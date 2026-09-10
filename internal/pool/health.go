package pool

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/registry"
)

// AttemptOutcome is the health-relevant result of one dispatched upstream attempt.
type AttemptOutcome string

const (
	OutcomeSuccess      AttemptOutcome = "success"
	OutcomeAuth         AttemptOutcome = "auth"
	OutcomeRateLimited  AttemptOutcome = "rate_limited"
	OutcomeTimeout      AttemptOutcome = "timeout"
	OutcomeUpstream     AttemptOutcome = "upstream"
	OutcomeCanceled     AttemptOutcome = "canceled"
	OutcomeRequestError AttemptOutcome = "request_error"
)

// AttemptResult is recorded independently of request logging. Request logs may
// be disabled or dropped under load; routing health must still be updated.
type AttemptResult struct {
	RequestID         string
	Attempt           int
	Model             string
	KeyID             string
	Provider          string
	Upstream          string
	ActualModel       string
	Status            int
	Outcome           AttemptOutcome
	DurationMS        int64
	FirstChunkMS      int64
	OutputTokens      int64
	FirstChunkKnown   bool
	OutputTokensKnown bool
	RetryAfter        time.Duration
	ProviderScoped    bool
	StartedAt         time.Time
}

// Lease identifies one admission. Finish is the only operation that can
// settle it; the token makes duplicate or stale releases harmless.
type Lease struct {
	token      uint64
	generation uint64
}

type leaseRecord struct {
	generation    uint64
	keyScope      string
	providerScope string
	keyProbe      bool
	providerProbe bool
}

type healthEntry struct {
	model               string
	keyID               string
	provider            string
	upstream            string
	attempts            int64
	successes           int64
	failures            int64
	canceled            int64
	lastOutcome         AttemptOutcome
	lastStatus          int
	lastAttemptAt       time.Time
	lastFailureAt       time.Time
	lastRequestID       string
	lastAttempt         int
	lastDuration        int64
	lastFirstChunk      int64
	lastOutput          int64
	lastFirstChunkKnown bool
	lastOutputKnown     bool
	lastActualModel     string
	scoreSuccess        float64
	scoreFailure        float64
	scoreUpdatedAt      time.Time
	cooldownUntil       time.Time
	probeToken          uint64
}

// HealthSnapshot is a safe, non-secret view of routing health.
type HealthSnapshot struct {
	Model                 string         `json:"model"`
	KeyID                 string         `json:"key_id"`
	Provider              string         `json:"provider"`
	Upstream              string         `json:"upstream"`
	Attempts              int64          `json:"attempts"`
	Successes             int64          `json:"successes"`
	Failures              int64          `json:"failures"`
	Canceled              int64          `json:"canceled"`
	LastOutcome           AttemptOutcome `json:"last_outcome"`
	LastStatus            int            `json:"last_status"`
	LastAttemptAt         time.Time      `json:"last_attempt_at"`
	LastRequestID         string         `json:"last_request_id,omitempty"`
	LastAttempt           int            `json:"last_attempt,omitempty"`
	LastDurationMS        int64          `json:"last_duration_ms"`
	LastFirstChunkMS      *int64         `json:"last_first_chunk_ms,omitempty"`
	LastOutputTokens      *int64         `json:"last_output_tokens,omitempty"`
	LastActualModel       string         `json:"last_actual_model,omitempty"`
	CooldownUntil         *time.Time     `json:"cooldown_until,omitempty"`
	ProviderCooldownUntil *time.Time     `json:"provider_cooldown_until,omitempty"`
	Reliability           *float64       `json:"reliability,omitempty"`
	ScoreSamples          float64        `json:"score_samples"`
}

type ReliabilityScore struct {
	Value   float64 `json:"value"`
	Samples float64 `json:"samples"`
}

type ShadowRecommendation struct {
	Model          string `json:"model"`
	ActualKey      string `json:"actual_key"`
	RecommendedKey string `json:"recommended_key"`
	Observations   int64  `json:"observations"`
}

// HealthState keeps short-lived cross-request key health in process memory.
// It intentionally does not persist credentials or raw API keys.
type HealthState struct {
	mu                    sync.Mutex
	generation            uint64
	nextToken             uint64
	keys                  map[string]*healthEntry
	providers             map[string]*healthEntry
	leases                map[uint64]leaseRecord
	halfLife              time.Duration
	strategy              string
	shadow                bool
	exploration           map[string]int64
	shadowRecommendations map[string]*ShadowRecommendation
}

const (
	authCooldown      = 5 * time.Minute
	rateLimitCooldown = 30 * time.Second
	timeoutCooldown   = 15 * time.Second
	upstreamCooldown  = 10 * time.Second
)

func NewHealthState() *HealthState {
	return &HealthState{
		generation:            1,
		keys:                  make(map[string]*healthEntry),
		providers:             make(map[string]*healthEntry),
		leases:                make(map[uint64]leaseRecord),
		halfLife:              72 * time.Hour,
		strategy:              "priority",
		exploration:           make(map[string]int64),
		shadowRecommendations: make(map[string]*ShadowRecommendation),
	}
}

// ConfigureRouting updates the opt-in adaptive selector without touching
// accumulated observations. Invalid values fall back to deterministic priority.
func (h *HealthState) ConfigureRouting(strategy string, halfLife time.Duration, shadow bool) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if halfLife > 0 {
		h.halfLife = halfLife
	}
	if strategy != "adaptive" {
		strategy = "priority"
	}
	h.strategy = strategy
	h.shadow = shadow
	h.mu.Unlock()
}

func (h *HealthState) adaptiveEnabledLocked() bool {
	return h.strategy == "adaptive" && !h.shadow
}

// AdaptiveEnabled reports whether adaptive ordering is active rather than shadow-only.
func (h *HealthState) AdaptiveEnabled() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.adaptiveEnabledLocked()
}

func (h *HealthState) AdaptiveConfigured() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.strategy == "adaptive"
}

func (h *HealthState) ShadowEnabled() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shadow
}

// NextExploration advances once per non-affinity selection. Every tenth turn
// requests an unknown same-tier candidate when one exists.
func (h *HealthState) NextExploration(model string) (bool, int64) {
	if h == nil || !h.AdaptiveConfigured() {
		return false, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.exploration[model]++
	count := h.exploration[model]
	return count%10 == 0, count / 10
}

func (h *HealthState) RecordShadow(model, actualKey, recommendedKey string) {
	if h == nil || !h.ShadowEnabled() || actualKey == "" || recommendedKey == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	item := h.shadowRecommendations[model]
	if item == nil {
		item = &ShadowRecommendation{Model: model}
		h.shadowRecommendations[model] = item
	}
	item.ActualKey = actualKey
	item.RecommendedKey = recommendedKey
	item.Observations++
}

func (h *HealthState) ShadowSnapshot() []ShadowRecommendation {
	if h == nil {
		return []ShadowRecommendation{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]ShadowRecommendation, 0, len(h.shadowRecommendations))
	for _, item := range h.shadowRecommendations {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Model < result[j].Model })
	return result
}

// Generation returns the current routing-state generation.
func (h *HealthState) Generation() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.generation
}

// Reset drops all health state. Existing leases from the old generation cannot
// modify the new state, which keeps in-flight requests safe across reloads.
func (h *HealthState) Reset() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.generation++
	h.nextToken = 0
	h.keys = make(map[string]*healthEntry)
	h.providers = make(map[string]*healthEntry)
	h.leases = make(map[uint64]leaseRecord)
	h.exploration = make(map[string]int64)
	h.shadowRecommendations = make(map[string]*ShadowRecommendation)
	h.mu.Unlock()
}

// ActiveLeases reports in-flight health admissions for tests and diagnostics.
func (h *HealthState) ActiveLeases() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.leases)
}

// Acquire admits a key, reserving one half-open probe when a cooldown expires.
// Key and provider gates are checked and reserved atomically.
func (h *HealthState) Acquire(model string, key registry.UpstreamKey) (Lease, bool) {
	if h == nil {
		return Lease{}, true
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	keyScope := makeKeyScope(model, key)
	providerScope := makeProviderScope(model, key)
	keyEntry := h.keys[keyScope]
	providerEntry := h.providers[providerScope]
	keyAllowed, keyProbe := admit(keyEntry, now)
	providerAllowed, providerProbe := admit(providerEntry, now)
	if !keyAllowed || !providerAllowed {
		return Lease{}, false
	}

	h.nextToken++
	lease := Lease{token: h.nextToken, generation: h.generation}
	h.leases[lease.token] = leaseRecord{
		generation:    h.generation,
		keyScope:      keyScope,
		providerScope: providerScope,
		keyProbe:      keyProbe,
		providerProbe: providerProbe,
	}
	if keyProbe {
		if keyEntry == nil {
			keyEntry = &healthEntry{model: model, keyID: key.ID, provider: key.Provider, upstream: key.Name}
			h.keys[keyScope] = keyEntry
		}
		keyEntry.probeToken = lease.token
	}
	if providerProbe {
		if providerEntry == nil {
			providerEntry = &healthEntry{model: model, provider: key.Provider, upstream: key.Name}
			h.providers[providerScope] = providerEntry
		}
		providerEntry.probeToken = lease.token
	}
	return lease, true
}

// IsEligible is a read-only health gate used to build one selection snapshot.
// Acquire remains the authoritative atomic admission immediately before dispatch.
func (h *HealthState) IsEligible(model string, key registry.UpstreamKey) bool {
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for _, entry := range []*healthEntry{h.keys[makeKeyScope(model, key)], h.providers[makeProviderScope(model, key)]} {
		if entry == nil {
			continue
		}
		if entry.probeToken != 0 || entry.cooldownUntil.After(now) {
			return false
		}
	}
	return true
}

func admit(entry *healthEntry, now time.Time) (bool, bool) {
	if entry == nil || entry.cooldownUntil.IsZero() {
		return true, false
	}
	if now.Before(entry.cooldownUntil) || entry.probeToken != 0 {
		return false, false
	}
	return true, true
}

// Finish settles a lease exactly once. A stale generation or duplicate token
// is ignored, so an old request cannot clear or rewrite post-reload state.
func (h *HealthState) Finish(lease Lease, result AttemptResult) {
	if h == nil || lease.token == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	record, ok := h.leases[lease.token]
	if !ok || record.generation != h.generation || lease.generation != h.generation {
		return
	}
	delete(h.leases, lease.token)
	h.clearProbe(record, lease.token)
	if result.Model == "" || result.KeyID == "" {
		return
	}

	now := time.Now()
	keyEntry := h.keys[record.keyScope]
	if keyEntry == nil {
		keyEntry = &healthEntry{
			model:    result.Model,
			keyID:    result.KeyID,
			provider: result.Provider,
			upstream: result.Upstream,
		}
		h.keys[record.keyScope] = keyEntry
	}
	keyEntry.attempts++
	keyEntry.lastOutcome = result.Outcome
	keyEntry.lastStatus = result.Status
	keyEntry.lastAttemptAt = now
	keyEntry.lastRequestID = result.RequestID
	keyEntry.lastAttempt = result.Attempt
	keyEntry.lastDuration = result.DurationMS
	keyEntry.lastFirstChunk = result.FirstChunkMS
	keyEntry.lastFirstChunkKnown = result.FirstChunkKnown
	keyEntry.lastOutput = result.OutputTokens
	keyEntry.lastOutputKnown = result.OutputTokensKnown
	keyEntry.lastActualModel = result.ActualModel
	if result.Outcome == OutcomeSuccess || isFailure(result.Outcome) {
		h.decayScoreLocked(keyEntry, now)
		if result.Outcome == OutcomeSuccess {
			keyEntry.scoreSuccess++
		} else {
			keyEntry.scoreFailure++
		}
	}

	switch result.Outcome {
	case OutcomeSuccess:
		keyEntry.successes++
		if !failureStartedAfter(keyEntry, result.StartedAt) {
			keyEntry.cooldownUntil = time.Time{}
		}
	case OutcomeCanceled:
		keyEntry.canceled++
	case OutcomeAuth, OutcomeRateLimited, OutcomeTimeout, OutcomeUpstream:
		keyEntry.failures++
		keyEntry.lastFailureAt = now
		if !result.ProviderScoped {
			until := now.Add(cooldownFor(result.Outcome, result.RetryAfter))
			if until.After(keyEntry.cooldownUntil) {
				keyEntry.cooldownUntil = until
			}
		}
	}

	if result.ProviderScoped && isFailure(result.Outcome) {
		providerEntry := h.providers[record.providerScope]
		if providerEntry == nil {
			providerEntry = &healthEntry{
				model:    result.Model,
				provider: result.Provider,
				upstream: result.Upstream,
			}
			h.providers[record.providerScope] = providerEntry
		}
		providerEntry.failures++
		providerEntry.lastOutcome = result.Outcome
		providerEntry.lastStatus = result.Status
		providerEntry.lastAttemptAt = now
		providerEntry.lastFailureAt = now
		until := now.Add(cooldownFor(result.Outcome, result.RetryAfter))
		if until.After(providerEntry.cooldownUntil) {
			providerEntry.cooldownUntil = until
		}
	}
	if result.Outcome == OutcomeSuccess {
		if providerEntry := h.providers[record.providerScope]; providerEntry != nil && !failureStartedAfter(providerEntry, result.StartedAt) {
			providerEntry.cooldownUntil = time.Time{}
		}
	}
}

func (h *HealthState) decayScoreLocked(entry *healthEntry, now time.Time) {
	if entry.scoreUpdatedAt.IsZero() {
		entry.scoreUpdatedAt = now
		return
	}
	if h.halfLife <= 0 || !now.After(entry.scoreUpdatedAt) {
		return
	}
	factor := math.Pow(0.5, now.Sub(entry.scoreUpdatedAt).Seconds()/h.halfLife.Seconds())
	entry.scoreSuccess *= factor
	entry.scoreFailure *= factor
	entry.scoreUpdatedAt = now
}

// Reliability returns a decay-weighted Beta(1,1) expectation and sample mass.
func (h *HealthState) Reliability(model string, key registry.UpstreamKey) (float64, float64) {
	if h == nil {
		return 0.5, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.keys[makeKeyScope(model, key)]
	if entry == nil {
		return 0.5, 0
	}
	h.decayScoreLocked(entry, time.Now())
	samples := entry.scoreSuccess + entry.scoreFailure
	return (entry.scoreSuccess + 1) / (samples + 2), samples
}

// ScoreKeys snapshots all candidate scores under one lock/clock observation.
func (h *HealthState) ScoreKeys(model string, keys []registry.UpstreamKey) map[string]ReliabilityScore {
	result := make(map[string]ReliabilityScore, len(keys))
	if h == nil {
		for _, key := range keys {
			result[makeKeyScope(model, key)] = ReliabilityScore{Value: 0.5}
		}
		return result
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for _, key := range keys {
		entry := h.keys[makeKeyScope(model, key)]
		if entry == nil {
			result[makeKeyScope(model, key)] = ReliabilityScore{Value: 0.5}
			continue
		}
		h.decayScoreLocked(entry, now)
		samples := entry.scoreSuccess + entry.scoreFailure
		result[makeKeyScope(model, key)] = ReliabilityScore{Value: (entry.scoreSuccess + 1) / (samples + 2), Samples: samples}
	}
	return result
}

// ProviderReliability averages key scores without crossing the provider-priority boundary.
func (h *HealthState) ProviderReliability(model, provider string, keys []registry.UpstreamKey) float64 {
	if h == nil {
		return 0.5
	}
	scores := h.ScoreKeys(model, keys)
	total := 0.0
	count := 0
	if len(keys) == 0 {
		return 0.5
	}
	for _, key := range keys {
		if key.Name != provider {
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

func (h *HealthState) ProviderSamples(model, provider string, keys []registry.UpstreamKey, scores map[string]ReliabilityScore) float64 {
	total := 0.0
	minEntry := int(^uint(0) >> 1)
	for _, key := range keys {
		if key.Name == provider && key.EntryPriority < minEntry {
			minEntry = key.EntryPriority
		}
	}
	for _, key := range keys {
		if key.Name == provider && key.EntryPriority == minEntry {
			total += scores[makeKeyScope(model, key)].Samples
		}
	}
	return total
}

// OrderKeys applies deterministic reliability ordering inside one provider and
// one entry-priority tier. Unknown keys retain their incoming order at the
// neutral Beta(1,1) prior, providing bounded cold-start exploration.
func (h *HealthState) OrderKeys(model string, keys []registry.UpstreamKey, explore ...bool) []registry.UpstreamKey {
	if h == nil || !h.AdaptiveEnabled() || len(keys) < 2 {
		return keys
	}
	return h.OrderKeysWithScores(model, keys, h.ScoreKeys(model, keys), len(explore) > 0 && explore[0])
}

func (h *HealthState) OrderKeysWithScores(model string, keys []registry.UpstreamKey, scores map[string]ReliabilityScore, explore bool) []registry.UpstreamKey {
	if h == nil || len(keys) < 2 {
		return keys
	}
	type scoredKey struct {
		key     registry.UpstreamKey
		score   float64
		samples float64
	}
	scored := make([]scoredKey, 0, len(keys))
	for _, key := range keys {
		score := scores[makeKeyScope(model, key)]
		scored = append(scored, scoredKey{key: key, score: score.Value, samples: score.Samples})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if explore && scored[i].samples != scored[j].samples {
			return scored[i].samples < scored[j].samples
		}
		return scored[i].score > scored[j].score
	})
	ordered := make([]registry.UpstreamKey, 0, len(scored))
	for _, item := range scored {
		ordered = append(ordered, item.key)
	}
	return ordered
}

func (h *HealthState) clearProbe(record leaseRecord, token uint64) {
	if record.keyProbe {
		if entry := h.keys[record.keyScope]; entry != nil && entry.probeToken == token {
			entry.probeToken = 0
		}
	}
	if record.providerProbe {
		if entry := h.providers[record.providerScope]; entry != nil && entry.probeToken == token {
			entry.probeToken = 0
		}
	}
}

func failureStartedAfter(entry *healthEntry, startedAt time.Time) bool {
	return !startedAt.IsZero() && entry.lastFailureAt.After(startedAt)
}

func cooldownFor(outcome AttemptOutcome, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	switch outcome {
	case OutcomeAuth:
		return authCooldown
	case OutcomeRateLimited:
		return rateLimitCooldown
	case OutcomeTimeout:
		return timeoutCooldown
	default:
		return upstreamCooldown
	}
}

func isFailure(outcome AttemptOutcome) bool {
	switch outcome {
	case OutcomeAuth, OutcomeRateLimited, OutcomeTimeout, OutcomeUpstream:
		return true
	default:
		return false
	}
}

// Release abandons a lease without changing health counters.
func (h *HealthState) Release(lease Lease) {
	if h == nil || lease.token == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	record, ok := h.leases[lease.token]
	if !ok || record.generation != h.generation || lease.generation != h.generation {
		return
	}
	delete(h.leases, lease.token)
	h.clearProbe(record, lease.token)
}

// NextRetry returns the earliest active cooldown for the supplied candidates.
func (h *HealthState) NextRetry(model string, keys []registry.UpstreamKey, tried map[string]struct{}, skipSuppliers map[string]struct{}) time.Time {
	if h == nil {
		return time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	var next time.Time
	for _, key := range keys {
		if tried != nil {
			if _, ok := tried[key.ID]; ok {
				continue
			}
		}
		if skipSuppliers != nil {
			if _, ok := skipSuppliers[key.Name]; ok {
				continue
			}
		}
		ready := time.Time{}
		for _, entry := range []*healthEntry{h.keys[makeKeyScope(model, key)], h.providers[makeProviderScope(model, key)]} {
			if entry != nil && entry.cooldownUntil.After(now) && entry.cooldownUntil.After(ready) {
				ready = entry.cooldownUntil
			}
		}
		if !ready.IsZero() && (next.IsZero() || ready.Before(next)) {
			next = ready
		}
	}
	return next
}

// Snapshot returns key-level health without exposing credentials.
func (h *HealthState) Snapshot() []HealthSnapshot {
	if h == nil {
		return []HealthSnapshot{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make([]HealthSnapshot, 0, len(h.keys))
	for _, entry := range h.keys {
		item := HealthSnapshot{
			Model:           entry.model,
			KeyID:           entry.keyID,
			Provider:        entry.provider,
			Upstream:        entry.upstream,
			Attempts:        entry.attempts,
			Successes:       entry.successes,
			Failures:        entry.failures,
			Canceled:        entry.canceled,
			LastOutcome:     entry.lastOutcome,
			LastStatus:      entry.lastStatus,
			LastAttemptAt:   entry.lastAttemptAt,
			LastRequestID:   entry.lastRequestID,
			LastAttempt:     entry.lastAttempt,
			LastDurationMS:  entry.lastDuration,
			LastActualModel: entry.lastActualModel,
		}
		h.decayScoreLocked(entry, now)
		samples := entry.scoreSuccess + entry.scoreFailure
		item.ScoreSamples = samples
		if samples > 0 {
			reliability := (entry.scoreSuccess + 1) / (samples + 2)
			item.Reliability = &reliability
		}
		if entry.lastFirstChunkKnown {
			value := entry.lastFirstChunk
			item.LastFirstChunkMS = &value
		}
		if entry.lastOutputKnown {
			value := entry.lastOutput
			item.LastOutputTokens = &value
		}
		if entry.cooldownUntil.After(now) {
			until := entry.cooldownUntil
			item.CooldownUntil = &until
		}
		if providerEntry := h.providers[makeProviderScopeFromEntry(entry)]; providerEntry != nil && providerEntry.cooldownUntil.After(now) {
			until := providerEntry.cooldownUntil
			item.ProviderCooldownUntil = &until
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].KeyID < out[j].KeyID
	})
	return out
}

func makeKeyScope(model string, key registry.UpstreamKey) string {
	return model + "\x00" + key.Provider + "\x00" + key.Name + "\x00" + key.ID
}

func makeProviderScope(model string, key registry.UpstreamKey) string {
	return model + "\x00" + key.Provider + "\x00" + key.Name
}

func makeProviderScopeFromEntry(entry *healthEntry) string {
	return entry.model + "\x00" + entry.provider + "\x00" + entry.upstream
}
