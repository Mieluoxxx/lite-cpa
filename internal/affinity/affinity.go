// Package affinity implements rule-based upstream key stickiness.
//
// Identity order:
//  1. strong sticky session headers (see cli_sessions.go catalog)
//  2. protocol body field by path:
//     - /v1/messages → metadata.user_id (Claude-normalized)
//     - /v1/responses or /chat/completions → prompt_cache_key
//  3. remaining rule KeySources as fallback
//  4. weak sticky headers (e.g. X-Client-Request-Id) as last resort — such
//     values may change per request, so their pins use a short TTL
//
// Stickiness only engages when such a field is present. Storage is process-local.
package affinity

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
	"github.com/tidwall/gjson"
)

// Match is the result of evaluating affinity rules against one request.
type Match struct {
	// Matched is true when a rule produced a non-empty affinity value.
	// CacheKey/TTL/SkipRetry are valid when Matched is true even if Found is false.
	Matched bool
	// Found is true when a preferred key ID is currently cached.
	Found     bool
	CacheKey  string
	KeyID     string
	SkipRetry bool
	TTL       time.Duration
	RuleName  string
	// Weak is true when the identity came from a weak source (e.g.
	// X-Client-Request-Id); such pins are capped to weakPinTTL.
	Weak bool
}

// Manager owns rule evaluation and the sticky cache.
type Manager struct {
	cfgMu           sync.RWMutex
	enabled         bool
	switchOnSuccess bool
	defaultTTL      time.Duration
	maxEntries      int
	rules           []config.ChannelAffinityRule
	regexCache      sync.Map // pattern -> *regexp.Regexp (or false for invalid)
	mu              sync.Mutex
	entries         map[string]cacheEntry
	approx          atomic.Int64
	janitorOnce     sync.Once
	stopJanitor     chan struct{}

	// stats counters (hot path: atomics only)
	stLookups  atomic.Int64 // rules that produced an identity (Matched=true)
	stHits     atomic.Int64 // lookups that found a pinned key
	stRecords  atomic.Int64 // pins written
	stClears   atomic.Int64 // pins deleted
	stPrefMiss atomic.Int64 // pins that existed but their key was unusable
}

// Stats is a point-in-time snapshot of affinity counters. Counters only —
// no affinity values or cache keys are exposed.
type Stats struct {
	Entries          int64   `json:"entries"`
	MatchedLookups   int64   `json:"matched_lookups"`
	CacheHits        int64   `json:"cache_hits"`
	HitRate          float64 `json:"hit_rate"`
	Records          int64   `json:"records"`
	Clears           int64   `json:"clears"`
	PreferredUnavail int64   `json:"preferred_unavailable"`
}

type cacheEntry struct {
	keyID     string
	expiresAt time.Time
}

// New builds a Manager from config. Affinity is on by default when Enabled is omitted.
func New(setting config.ChannelAffinitySetting) *Manager {
	m := &Manager{
		entries:     make(map[string]cacheEntry),
		stopJanitor: make(chan struct{}),
	}
	m.applySetting(setting)
	return m
}

// Reconfigure updates sticky rules/settings in place. Existing cache entries are
// kept; pins whose key IDs disappear after registry reload simply miss.
func (m *Manager) Reconfigure(setting config.ChannelAffinitySetting) {
	if m == nil {
		return
	}
	m.applySetting(setting)
}

func (m *Manager) applySetting(setting config.ChannelAffinitySetting) {
	ttlSec := setting.DefaultTTLSeconds
	if ttlSec <= 0 {
		ttlSec = 600
	}
	maxEntries := setting.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 100_000
	}
	enabled := setting.EnabledOrDefault()
	rules := setting.ResolvedRules()

	m.cfgMu.Lock()
	m.enabled = enabled
	m.switchOnSuccess = setting.SwitchOnSuccessOrDefault()
	m.defaultTTL = time.Duration(ttlSec) * time.Second
	m.maxEntries = maxEntries
	m.rules = rules
	m.cfgMu.Unlock()

	if enabled {
		m.janitorOnce.Do(func() {
			go m.janitor()
		})
	}
}

// Close stops the background janitor. Safe to call multiple times / on no-op managers.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	select {
	case <-m.stopJanitor:
	default:
		close(m.stopJanitor)
	}
}

// Lookup evaluates rules in order and returns the first match.
// path should be the request URL path (e.g. /v1/messages).
func (m *Manager) Lookup(model, path string, headers http.Header, body []byte) Match {
	if m == nil {
		return Match{}
	}
	m.cfgMu.RLock()
	enabled := m.enabled
	rules := m.rules
	defaultTTL := m.defaultTTL
	m.cfgMu.RUnlock()
	if !enabled || len(rules) == 0 {
		return Match{}
	}
	ua := ""
	if headers != nil {
		ua = headers.Get("User-Agent")
	}
	for _, rule := range rules {
		if len(rule.ModelRegex) > 0 && !m.matchAnyRegex(rule.ModelRegex, model) {
			continue
		}
		if len(rule.PathRegex) > 0 && !m.matchAnyRegex(rule.PathRegex, path) {
			continue
		}
		if len(rule.UserAgentInclude) > 0 && !matchAnyIncludeFold(rule.UserAgentInclude, ua) {
			continue
		}
		value, weak, ok := extractAffinityValue(rule.KeySources, path, headers, body)
		if !ok {
			// no sticky identity field present → skip this rule
			continue
		}
		if rule.ValueRegex != "" && !m.matchAnyRegex([]string{rule.ValueRegex}, value) {
			continue
		}
		ttl := defaultTTL
		if rule.TTLSeconds > 0 {
			ttl = time.Duration(rule.TTLSeconds) * time.Second
		}
		if weak && ttl > weakPinTTL {
			ttl = weakPinTTL
		}
		cacheKey := buildCacheKey(rule, model, value)
		match := Match{
			Matched:   true,
			CacheKey:  cacheKey,
			SkipRetry: rule.SkipRetryOnFailure,
			TTL:       ttl,
			RuleName:  rule.Name,
			Weak:      weak,
		}
		m.stLookups.Add(1)
		if keyID, found := m.get(cacheKey); found {
			match.Found = true
			match.KeyID = keyID
			m.stHits.Add(1)
		}
		return match
	}
	return Match{}
}

// Record pins keyID for a previously matched CacheKey.
// When switchOnSuccess is true (default), always overwrites with the successful key.
func (m *Manager) Record(cacheKey, keyID string, ttl time.Duration) {
	if m == nil || cacheKey == "" || keyID == "" {
		return
	}
	m.cfgMu.RLock()
	enabled := m.enabled
	defaultTTL := m.defaultTTL
	m.cfgMu.RUnlock()
	if !enabled {
		return
	}
	if ttl <= 0 {
		ttl = defaultTTL
	}
	m.set(cacheKey, keyID, ttl)
	m.stRecords.Add(1)
}

// Clear drops a sticky binding (e.g. preferred key failed).
func (m *Manager) Clear(cacheKey string) {
	if m == nil || cacheKey == "" {
		return
	}
	m.mu.Lock()
	if _, ok := m.entries[cacheKey]; ok {
		delete(m.entries, cacheKey)
		m.approx.Add(-1)
		m.stClears.Add(1)
	}
	m.mu.Unlock()
}

// ResolvePreferred returns the key with ID == preferredID if present and not tried.
func ResolvePreferred(keys []registry.UpstreamKey, preferredID string, tried map[string]struct{}) (registry.UpstreamKey, bool) {
	if preferredID == "" {
		return registry.UpstreamKey{}, false
	}
	for _, k := range keys {
		if k.ID != preferredID {
			continue
		}
		if tried != nil {
			if _, used := tried[k.ID]; used {
				return registry.UpstreamKey{}, false
			}
		}
		return k, true
	}
	return registry.UpstreamKey{}, false
}

// weakPinTTL caps the pin lifetime for weak identity sources: their values may
// change per request (e.g. X-Client-Request-Id on some clients), so a full-TTL
// pin would flood the cache with one-shot keys.
const weakPinTTL = 60 * time.Second

// extractAffinityValue implements:
//  1. strong sticky session headers first (CLI catalog)
//  2. protocol-native body field (by path, Claude user_id normalized)
//  3. remaining configured key sources
//  4. weak sticky headers (sometimes per-request values) as last resort
//
// Match only when a field is present (non-empty).
func extractAffinityValue(sources []config.ChannelAffinityKeySource, path string, headers http.Header, body []byte) (value string, weak bool, ok bool) {
	// 1) sticky session headers (CLI catalog priority); keep the first weak
	// candidate around in case nothing stronger exists.
	hdrValue, hdrWeak, hdrFound := extractStickySessionHeader(headers)
	if hdrFound && !hdrWeak {
		return hdrValue, false, true
	}

	// 2) protocol body field
	if v, found := extractProtocolBodyValue(path, body); found {
		return v, false, true
	}

	// 3) configured sources (skip headers / body paths already tried)
	triedBody := map[string]struct{}{}
	for _, p := range protocolBodyPaths(path) {
		triedBody[p] = struct{}{}
	}
	for _, src := range sources {
		switch strings.ToLower(strings.TrimSpace(src.Type)) {
		case "request_header", "header":
			if isStickySessionHeader(src.Key) {
				continue // already tried
			}
			if v, found := extractHeader(headers, src.Key); found {
				return v, false, true
			}
		case "gjson", "body":
			if src.Path == "" {
				continue
			}
			if _, done := triedBody[src.Path]; done {
				continue
			}
			if v, found := extractGJSON(body, src.Path); found {
				if src.Path == "metadata.user_id" {
					if sid, ok := normalizeClaudeUserID(v); ok {
						return sid, false, true
					}
					continue
				}
				return v, false, true
			}
		}
	}

	// 4) weak sticky header fallback (short-TTL pin upstream)
	if hdrFound {
		return hdrValue, true, true
	}
	return "", false, false
}

// protocolBodyPaths returns body gjson paths preferred for the request path.
// messages → Claude metadata.user_id; responses/chat → prompt_cache_key.
func protocolBodyPaths(path string) []string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/messages"):
		return []string{"metadata.user_id", "prompt_cache_key"}
	case strings.Contains(p, "/responses"), strings.Contains(p, "/chat/completions"), strings.Contains(p, "/completions"):
		return []string{"prompt_cache_key", "metadata.user_id"}
	default:
		// unknown path: try both, session headers already handled
		return []string{"prompt_cache_key", "metadata.user_id"}
	}
}

func extractHeader(headers http.Header, key string) (string, bool) {
	if headers == nil || key == "" {
		return "", false
	}
	v := strings.TrimSpace(headers.Get(key))
	if v == "" {
		return "", false
	}
	return v, true
}

func extractGJSON(body []byte, path string) (string, bool) {
	if path == "" || len(body) == 0 {
		return "", false
	}
	res := gjson.GetBytes(body, path)
	if !res.Exists() {
		return "", false
	}
	switch res.Type {
	case gjson.String, gjson.Number, gjson.True, gjson.False:
		v := strings.TrimSpace(res.String())
		if v != "" {
			return v, true
		}
	default:
		v := strings.TrimSpace(res.Raw)
		if v != "" && v != "null" {
			return v, true
		}
	}
	return "", false
}

func buildCacheKey(rule config.ChannelAffinityRule, model, value string) string {
	parts := make([]string, 0, 4)
	if rule.IncludeRuleName && rule.Name != "" {
		parts = append(parts, rule.Name)
	}
	if rule.IncludeModelName && model != "" {
		parts = append(parts, model)
	}
	parts = append(parts, value)
	return strings.Join(parts, ":")
}

func (m *Manager) matchAnyRegex(patterns []string, s string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re := m.compiled(p)
		if re != nil && re.MatchString(s) {
			return true
		}
	}
	return false
}

func (m *Manager) compiled(pattern string) *regexp.Regexp {
	if v, ok := m.regexCache.Load(pattern); ok {
		if v == nil {
			return nil
		}
		return v.(*regexp.Regexp)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		m.regexCache.Store(pattern, nil)
		return nil
	}
	actual, _ := m.regexCache.LoadOrStore(pattern, re)
	if actual == nil {
		return nil
	}
	return actual.(*regexp.Regexp)
}

func matchAnyIncludeFold(patterns []string, s string) bool {
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func (m *Manager) get(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiresAt) {
		delete(m.entries, key)
		m.approx.Add(-1)
		return "", false
	}
	return e.keyID, true
}

func (m *Manager) set(key, keyID string, ttl time.Duration) {
	now := time.Now()
	m.cfgMu.RLock()
	maxEntries := m.maxEntries
	m.cfgMu.RUnlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[key]; !exists {
		if len(m.entries) >= maxEntries {
			// Cheap eviction: drop one expired entry, else drop an arbitrary key.
			evicted := false
			for k, e := range m.entries {
				if now.After(e.expiresAt) {
					delete(m.entries, k)
					m.approx.Add(-1)
					evicted = true
					break
				}
			}
			if !evicted {
				for k := range m.entries {
					delete(m.entries, k)
					m.approx.Add(-1)
					break
				}
			}
		}
		m.approx.Add(1)
	}
	m.entries[key] = cacheEntry{keyID: keyID, expiresAt: now.Add(ttl)}
}

func (m *Manager) janitor() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopJanitor:
			return
		case <-ticker.C:
			m.purgeExpired()
		}
	}
}

func (m *Manager) purgeExpired() {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, e := range m.entries {
		if now.After(e.expiresAt) {
			delete(m.entries, k)
			m.approx.Add(-1)
		}
	}
}

// Len returns the approximate number of sticky entries (for tests/stats).
func (m *Manager) Len() int {
	if m == nil {
		return 0
	}
	return int(m.approx.Load())
}

// Stats returns a snapshot of the affinity counters.
func (m *Manager) Stats() Stats {
	if m == nil {
		return Stats{}
	}
	lookups := m.stLookups.Load()
	hits := m.stHits.Load()
	rate := 0.0
	if lookups > 0 {
		rate = float64(hits) / float64(lookups)
	}
	return Stats{
		Entries:          m.approx.Load(),
		MatchedLookups:   lookups,
		CacheHits:        hits,
		HitRate:          rate,
		Records:          m.stRecords.Load(),
		Clears:           m.stClears.Load(),
		PreferredUnavail: m.stPrefMiss.Load(),
	}
}

// MarkPreferredUnavailable records that a pin existed but its key was not
// usable (vanished after a reload, or already tried this request).
func (m *Manager) MarkPreferredUnavailable() {
	if m == nil {
		return
	}
	m.stPrefMiss.Add(1)
}
