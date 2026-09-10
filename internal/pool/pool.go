package pool

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
)

// BuildRegistry constructs a model registry from config upstream sections.
func BuildRegistry(cfg *config.Config) *registry.Registry {
	r := registry.New()
	now := time.Now().Unix()

	for i, p := range cfg.AnthropicMessages {
		name := providerName(p.Name, "anthropic", i)
		keys := expandProvider("claude", name, p.BaseURL, p.APIKey, p.ProxyURL, p.Priority, p.Headers, p.APIKeyEntries, cfg.ProxyURL, p.FailoverMode)
		for _, m := range p.Models {
			alias := m.ResolvedAlias()
			if alias == "" {
				continue
			}
			r.RegisterModel(alias, &registry.ModelInfo{
				ID: alias, Created: now, Type: "claude",
			}, keysForModel(keys, m.Name, m.ResolvedSpeed(), m.ResolvedVerbosity()))
		}
	}

	for i, p := range cfg.OpenAIResponses {
		name := providerName(p.Name, "responses", i)
		keys := expandProvider("openai-response", name, p.BaseURL, p.APIKey, p.ProxyURL, p.Priority, p.Headers, p.APIKeyEntries, cfg.ProxyURL, p.FailoverMode)
		for _, m := range p.Models {
			alias := m.ResolvedAlias()
			if alias == "" {
				continue
			}
			r.RegisterModel(alias, &registry.ModelInfo{
				ID: alias, Created: now, Type: "openai-response",
			}, keysForModel(keys, m.Name, m.ResolvedSpeed(), m.ResolvedVerbosity()))
		}
	}

	for i, p := range cfg.OpenAICompletions {
		name := providerName(p.Name, "compat", i)
		keys := expandProvider("openai", name, p.BaseURL, p.APIKey, p.ProxyURL, p.Priority, p.Headers, p.APIKeyEntries, cfg.ProxyURL, p.FailoverMode)
		for _, m := range p.Models {
			alias := m.ResolvedAlias()
			if alias == "" {
				continue
			}
			r.RegisterModel(alias, &registry.ModelInfo{
				ID: alias, Created: now, Type: "openai",
			}, keysForModel(keys, m.Name, m.ResolvedSpeed(), m.ResolvedVerbosity()))
		}
	}
	for i, p := range cfg.OpenAIImages {
		name := providerName(p.Name, "image", i)
		keys := expandProvider("openai-image", name, p.BaseURL, p.APIKey, p.ProxyURL, p.Priority, p.Headers, p.APIKeyEntries, cfg.ProxyURL, p.FailoverMode)
		for _, m := range p.Models {
			alias := m.ResolvedAlias()
			if alias == "" {
				continue
			}
			r.RegisterModel(alias, &registry.ModelInfo{
				ID: alias, Created: now, Type: "openai-image",
			}, keysForModel(keys, m.Name, m.ResolvedSpeed(), m.ResolvedVerbosity()))
		}
	}
	return r
}

func providerName(name, fallbackPrefix string, index int) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	return fmt.Sprintf("%s-%d", fallbackPrefix, index)
}

func expandProvider(provider, name, baseURL, flatKey, flatProxy string, flatPriority int, headers map[string]string, entries []config.APIKeyEntry, globalProxy, failoverMode string) []registry.UpstreamKey {
	baseURL = trimSlash(baseURL)
	failoverMode = config.NormalizeFailoverMode(failoverMode)
	expanded := config.ExpandKeys(flatKey, entries)
	out := make([]registry.UpstreamKey, 0, len(expanded))
	// proxy is provider-level (flatProxy), then global fallback
	proxy := flatProxy
	if proxy == "" {
		proxy = globalProxy
	}
	for i, e := range expanded {
		h := map[string]string{}
		for k, v := range headers {
			h[k] = v
		}
		out = append(out, registry.UpstreamKey{
			ID:            fmt.Sprintf("%s-%d", name, i),
			Name:          name,
			Provider:      provider,
			BaseURL:       baseURL,
			APIKey:        e.APIKey,
			Priority:      flatPriority,
			EntryPriority: e.Priority,
			Headers:       h,
			ProxyURL:      proxy,
			FailoverMode:  failoverMode,
		})
	}
	return out
}

func keysForModel(keys []registry.UpstreamKey, upstreamModel, speed, verbosity string) []registry.UpstreamKey {
	// Attach upstream model name into a copy via ID suffix is not needed;
	// handlers resolve alias separately. Store upstream model in a synthetic header.
	out := make([]registry.UpstreamKey, len(keys))
	for i, k := range keys {
		out[i] = k
		out[i].Speed = speed
		out[i].Verbosity = verbosity
		if out[i].Headers == nil {
			out[i].Headers = map[string]string{}
		} else {
			h := make(map[string]string, len(k.Headers)+1)
			for kk, vv := range k.Headers {
				h[kk] = vv
			}
			out[i].Headers = h
		}
		if upstreamModel != "" {
			out[i].Headers["x-lite-upstream-model"] = upstreamModel
		}
	}
	return out
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Selector does round-robin across keys for a model with failure skip.
type Selector struct {
	mu     sync.RWMutex
	reg    *registry.Registry
	rr     sync.Map // model -> *uint64
	retry  int
	health *HealthState
}

func providerScore(model, provider string, keys []registry.UpstreamKey, scores map[string]ReliabilityScore) float64 {
	total := 0.0
	count := 0
	minEntry := int(^uint(0) >> 1)
	for _, key := range keys {
		if key.Name == provider && key.EntryPriority < minEntry {
			minEntry = key.EntryPriority
		}
	}
	for _, key := range keys {
		if key.Name != provider || key.EntryPriority != minEntry {
			continue
		}
		total += scores[makeKeyScope(model, key)].Value
		count++
	}
	if count == 0 {
		return 0.5
	}
	return total / float64(count)
}

// AvailabilityError reports that every eligible candidate is currently
// cooling down. Callers can expose RetryAt as Retry-After without parsing text.
type AvailabilityError struct {
	Model   string
	RetryAt time.Time
}

func (e *AvailabilityError) Error() string {
	return fmt.Sprintf("no available credentials for model %s (retry after %s)", e.Model, e.RetryAt.Format(time.RFC3339))
}

func NewSelector(reg *registry.Registry, retry int, health ...*HealthState) *Selector {
	var state *HealthState
	if len(health) > 0 {
		state = health[0]
	}
	return &Selector{reg: reg, retry: retry, health: state}
}

// RoutingStats returns a non-secret snapshot for diagnostics.
func (s *Selector) RoutingStats() []HealthSnapshot {
	if s.health == nil {
		return []HealthSnapshot{}
	}
	return s.health.Snapshot()
}

// SetRetry updates request-retry used by MaxAttempts.
func (s *Selector) SetRetry(retry int) {
	if retry < 0 {
		retry = 0
	}
	s.mu.Lock()
	s.retry = retry
	s.mu.Unlock()
}

// ResetRoundRobin clears per-model round-robin counters. Called on reload so
// selection starts fresh against the rebuilt key pools; without this, a model
// whose key set shrank/expanded/reordered would carry a stale counter and the
// first picks after reload could land on an arbitrary offset. Resetting makes
// post-reload selection deterministic and drops counters for models that no
// longer exist.
func (s *Selector) ResetRoundRobin() {
	s.rr.Range(func(key, _ any) bool {
		s.rr.Delete(key)
		return true
	})
}

// Pick chooses the next unused key for model. It preserves the legacy API and
// discards the health admission lease; server code should use PickWithLease so
// half-open probes are released after the attempt completes.
// preferSupplier (if non-empty) prefers remaining keys from that provider Name first.
// skipSuppliers excludes all keys under those provider Names (dead relay).
// Selection order: choose the lowest provider Priority first (round-robin among
// provider ties), then choose the lowest EntryPriority only within that provider.
func (s *Selector) Pick(model string, tried map[string]struct{}, preferSupplier string, skipSuppliers map[string]struct{}) (registry.UpstreamKey, string, error) {
	k, upstream, lease, err := s.PickWithLease(model, tried, preferSupplier, skipSuppliers)
	// Pick has no caller-owned lease, so release a possible half-open probe
	// immediately. Server request paths use PickWithLease instead.
	s.Release(lease)
	return k, upstream, err
}

// PickWithLease is Pick plus one atomic health admission. Health only filters
// candidates; provider priority and entry priority remain the hard ordering
// rules. A failed half-open probe is never silently replaced by a bypass.
func (s *Selector) PickWithLease(model string, tried map[string]struct{}, preferSupplier string, skipSuppliers map[string]struct{}) (registry.UpstreamKey, string, Lease, error) {
	return s.pickWithLease(model, tried, preferSupplier, skipSuppliers, false, 0)
}

// PickWithLeaseExploring advances the bounded exploration counter once for a
// non-affinity selection. Retries reuse the same request's normal ordering.
func (s *Selector) PickWithLeaseExploring(model string, tried map[string]struct{}, preferSupplier string, skipSuppliers map[string]struct{}) (registry.UpstreamKey, string, Lease, error) {
	explore, index := false, int64(0)
	if s.health != nil {
		explore, index = s.health.NextExploration(model)
	}
	return s.pickWithLease(model, tried, preferSupplier, skipSuppliers, explore, index)
}

func (s *Selector) pickWithLease(model string, tried map[string]struct{}, preferSupplier string, skipSuppliers map[string]struct{}, explore bool, exploreIndex int64) (registry.UpstreamKey, string, Lease, error) {
	s.mu.RLock()
	reg := s.reg
	s.mu.RUnlock()
	_, keys, ok := reg.Resolve(model)
	if !ok || len(keys) == 0 {
		return registry.UpstreamKey{}, "", Lease{}, fmt.Errorf("model not found: %s", model)
	}
	start := s.nextIndex(model, len(keys))
	ordered := make([]registry.UpstreamKey, 0, len(keys))
	for i := range len(keys) {
		ordered = append(ordered, keys[(start+i)%len(keys)])
	}
	shadowRecommended := ""
	adaptiveConfigured := s.health != nil && s.health.AdaptiveConfigured()
	var scores map[string]ReliabilityScore
	if adaptiveConfigured {
		scores = s.health.ScoreKeys(model, ordered)
	}

	pickCandidate := func(restrictSupplier string, blocked map[string]struct{}) (registry.UpstreamKey, bool) {
		shadowRecommended = ""
		selectedSupplier := ""
		bestProviderPri := int(^uint(0) >> 1)
		suppliers := make([]string, 0, len(ordered))
		seenSuppliers := make(map[string]struct{})
		eligible := make([]registry.UpstreamKey, 0, len(ordered))
		for _, k := range ordered {
			if tried != nil {
				if _, used := tried[k.ID]; used {
					continue
				}
			}
			if restrictSupplier != "" && k.Name != restrictSupplier {
				continue
			}
			if skipSuppliers != nil {
				if _, skip := skipSuppliers[k.Name]; skip {
					continue
				}
			}
			if _, blocked := blocked[k.ID]; blocked {
				continue
			}
			if s.health != nil && !s.health.IsEligible(model, k) {
				continue
			}
			eligible = append(eligible, k)
			if k.Priority < bestProviderPri {
				bestProviderPri = k.Priority
				suppliers = suppliers[:0]
				seenSuppliers = make(map[string]struct{})
			}
			if k.Priority == bestProviderPri {
				if _, seen := seenSuppliers[k.Name]; !seen {
					seenSuppliers[k.Name] = struct{}{}
					suppliers = append(suppliers, k.Name)
				}
			}
		}
		if len(suppliers) == 0 {
			return registry.UpstreamKey{}, false
		}
		selectedSupplier = suppliers[0]
		recommendedSupplier := selectedSupplier
		if adaptiveConfigured && len(suppliers) > 1 {
			if explore {
				bestSamples := math.Inf(1)
				for _, supplier := range suppliers {
					samples := s.health.ProviderSamples(model, supplier, eligible, scores)
					if samples < bestSamples {
						bestSamples = samples
						recommendedSupplier = supplier
					}
				}
			} else {
				bestScore := -1.0
				for _, supplier := range suppliers {
					score := providerScore(model, supplier, eligible, scores)
					if score > bestScore {
						bestScore = score
						recommendedSupplier = supplier
					}
				}
			}
			if s.health.AdaptiveEnabled() {
				selectedSupplier = recommendedSupplier
			}
		}

		candidates := make([]registry.UpstreamKey, 0, len(ordered))
		bestEntryPri := int(^uint(0) >> 1)
		for _, k := range ordered {
			if k.Name != selectedSupplier {
				continue
			}
			if tried != nil {
				if _, used := tried[k.ID]; used {
					continue
				}
			}
			if _, blocked := blocked[k.ID]; blocked {
				continue
			}
			if s.health != nil && !s.health.IsEligible(model, k) {
				continue
			}
			if k.EntryPriority < bestEntryPri {
				bestEntryPri = k.EntryPriority
				candidates = candidates[:0]
			}
			if k.EntryPriority == bestEntryPri {
				candidates = append(candidates, k)
			}
		}
		if len(candidates) == 0 {
			return registry.UpstreamKey{}, false
		}
		if s.health != nil && adaptiveConfigured {
			recommended := s.health.OrderKeysWithScores(model, candidates, scores, explore)
			if s.health.ShadowEnabled() {
				recommendCandidates := candidates
				if recommendedSupplier != selectedSupplier {
					recommendCandidates = make([]registry.UpstreamKey, 0, len(ordered))
					bestPriority := int(^uint(0) >> 1)
					for _, candidate := range ordered {
						if candidate.Name != recommendedSupplier {
							continue
						}
						if tried != nil {
							if _, used := tried[candidate.ID]; used {
								continue
							}
						}
						if _, blocked := blocked[candidate.ID]; blocked {
							continue
						}
						if !s.health.IsEligible(model, candidate) {
							continue
						}
						if candidate.EntryPriority < bestPriority {
							bestPriority = candidate.EntryPriority
							recommendCandidates = recommendCandidates[:0]
						}
						if candidate.EntryPriority == bestPriority {
							recommendCandidates = append(recommendCandidates, candidate)
						}
					}
				}
				recommended = s.health.OrderKeysWithScores(model, recommendCandidates, scores, false)
				if len(recommended) > 0 {
					shadowRecommended = recommended[0].ID
				}
			} else if explore && len(recommended) > 0 {
				unknown := make([]registry.UpstreamKey, 0, len(recommended))
				for _, candidate := range recommended {
					if scores[makeKeyScope(model, candidate)].Samples == 0 {
						unknown = append(unknown, candidate)
					}
				}
				if len(unknown) > 0 {
					return unknown[exploreIndex%int64(len(unknown))], true
				}
				candidates = recommended
			}
			if !s.health.ShadowEnabled() && !explore {
				candidates = recommended
			}
		}
		return candidates[0], true
	}

	tryPick := func(restrictSupplier string) (registry.UpstreamKey, Lease, bool) {
		blocked := make(map[string]struct{})
		for len(blocked) < len(ordered) {
			k, ok := pickCandidate(restrictSupplier, blocked)
			if !ok {
				return registry.UpstreamKey{}, Lease{}, false
			}
			lease, admitted := s.acquire(model, k)
			if admitted {
				if shadowRecommended != "" && s.health != nil {
					s.health.RecordShadow(model, k.ID, shadowRecommended)
				}
				return k, lease, true
			}
			blocked[k.ID] = struct{}{}
		}
		return registry.UpstreamKey{}, Lease{}, false
	}

	if preferSupplier != "" {
		if k, lease, ok := tryPick(preferSupplier); ok {
			_, upstream, err := withUpstreamModel(k, model)
			return k, upstream, lease, err
		}
	}
	if k, lease, ok := tryPick(""); ok {
		_, upstream, err := withUpstreamModel(k, model)
		return k, upstream, lease, err
	}
	if next := s.nextHealthRetry(model, keys, tried, skipSuppliers); !next.IsZero() {
		return registry.UpstreamKey{}, "", Lease{}, &AvailabilityError{Model: model, RetryAt: next}
	}
	return registry.UpstreamKey{}, "", Lease{}, fmt.Errorf("no available credentials for model %s", model)
}

// Acquire admits an explicitly preferred key through the same health gate used
// by ordinary selection. Affinity must not bypass cooldown or half-open limits.
func (s *Selector) Acquire(model string, key registry.UpstreamKey) (Lease, bool) {
	return s.acquire(model, key)
}

func (s *Selector) acquire(model string, key registry.UpstreamKey) (Lease, bool) {
	if s.health == nil {
		return Lease{}, true
	}
	return s.health.Acquire(model, key)
}

// Release returns a half-open admission slot. It is safe for old requests to
// release after reload because the lease carries the health generation.
func (s *Selector) Release(lease Lease) {
	if s.health != nil {
		s.health.Release(lease)
	}
}

// Finish settles one admitted attempt exactly once.
func (s *Selector) Finish(lease Lease, result AttemptResult) {
	if s.health != nil {
		s.health.Finish(lease, result)
	}
}

func (s *Selector) nextHealthRetry(model string, keys []registry.UpstreamKey, tried map[string]struct{}, skipSuppliers map[string]struct{}) time.Time {
	if s.health == nil {
		return time.Time{}
	}
	return s.health.NextRetry(model, keys, tried, skipSuppliers)
}

func withUpstreamModel(k registry.UpstreamKey, fallback string) (registry.UpstreamKey, string, error) {
	upstreamModel := fallback
	if k.Headers != nil {
		if m := k.Headers["x-lite-upstream-model"]; m != "" {
			upstreamModel = m
		}
	}
	return k, upstreamModel, nil
}

func (s *Selector) nextIndex(model string, n int) int {
	if n <= 0 {
		return 0
	}
	v, _ := s.rr.LoadOrStore(model, new(uint64))
	counter := v.(*uint64)
	return int(atomic.AddUint64(counter, 1)-1) % n
}

func (s *Selector) MaxAttempts(model string) int {
	s.mu.RLock()
	reg := s.reg
	retry := s.retry
	s.mu.RUnlock()
	_, keys, ok := reg.Resolve(model)
	if !ok {
		return 1
	}
	max := 1 + retry
	if max > len(keys) {
		max = len(keys)
	}
	if max < 1 {
		max = 1
	}
	return max
}
