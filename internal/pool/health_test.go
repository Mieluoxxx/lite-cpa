package pool

import (
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
)

func TestHealthStateCooldownAndReset(t *testing.T) {
	h := NewHealthState()
	key := registry.UpstreamKey{ID: "hub-0", Name: "hub", Provider: "openai"}
	finishHealthAttempt(t, h, key, AttemptResult{
		Model:      "m",
		KeyID:      key.ID,
		Provider:   key.Provider,
		Upstream:   key.Name,
		Outcome:    OutcomeRateLimited,
		RetryAfter: time.Hour,
	})
	if _, ok := h.Acquire("m", key); ok {
		t.Fatal("rate-limited key should be cooling down")
	}
	snapshot := h.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Failures != 1 || snapshot[0].LastOutcome != OutcomeRateLimited {
		t.Fatalf("unexpected health snapshot: %#v", snapshot)
	}

	oldKey := registry.UpstreamKey{ID: "hub-1", Name: "hub", Provider: "openai"}
	oldLease, ok := h.Acquire("m", oldKey)
	if !ok {
		t.Fatal("expected an old-generation lease")
	}
	h.Reset()
	h.Finish(oldLease, AttemptResult{Model: "m", KeyID: oldKey.ID, Provider: oldKey.Provider, Upstream: oldKey.Name, Outcome: OutcomeSuccess})
	lease, ok := h.Acquire("m", key)
	if !ok {
		t.Fatal("reset should make the key eligible")
	}
	h.Release(lease)
	if got := h.Snapshot(); len(got) != 0 {
		t.Fatalf("reset should drop old health entries: %#v", got)
	}
}

func TestHealthStateProviderCooldownDoesNotAffectOtherProvider(t *testing.T) {
	h := NewHealthState()
	k1 := registry.UpstreamKey{ID: "relay-0", Name: "relay", Provider: "openai", FailoverMode: "provider"}
	k2 := registry.UpstreamKey{ID: "relay-1", Name: "relay", Provider: "openai", FailoverMode: "provider"}
	k3 := registry.UpstreamKey{ID: "other-0", Name: "other", Provider: "openai"}

	finishHealthAttempt(t, h, k1, AttemptResult{
		Model:          "m",
		KeyID:          k1.ID,
		Provider:       k1.Provider,
		Upstream:       k1.Name,
		Outcome:        OutcomeUpstream,
		ProviderScoped: true,
	})
	if _, ok := h.Acquire("m", k2); ok {
		t.Fatal("provider-scoped failure should block sibling keys")
	}
	if _, ok := h.Acquire("m", k3); !ok {
		t.Fatal("provider cooldown should not block another provider")
	}
}

func TestSelectorSkipsCoolingKey(t *testing.T) {
	cfg := &config.Config{OpenAIResponses: []config.Provider{{
		Name:    "hub",
		BaseURL: "https://hub.example",
		APIKeyEntries: []config.APIKeyEntry{
			{APIKey: "sk-a"},
			{APIKey: "sk-b"},
		},
		Models: []config.ModelAlias{{Name: "m"}},
	}}}
	reg := BuildRegistry(cfg)
	_, keys, ok := reg.Resolve("m")
	if !ok || len(keys) != 2 {
		t.Fatalf("resolve keys: ok=%v keys=%d", ok, len(keys))
	}
	h := NewHealthState()
	finishHealthAttempt(t, h, keys[0], AttemptResult{
		Model:    "m",
		KeyID:    keys[0].ID,
		Provider: keys[0].Provider,
		Upstream: keys[0].Name,
		Outcome:  OutcomeUpstream,
	})

	sel := NewSelector(reg, 0, h)
	key, _, err := sel.Pick("m", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != keys[1].ID {
		t.Fatalf("selected cooling key %q, want %q", key.ID, keys[1].ID)
	}
}

func TestHealthStateHalfOpenLeaseOwnership(t *testing.T) {
	h := NewHealthState()
	key := registry.UpstreamKey{ID: "hub-0", Name: "hub", Provider: "openai"}
	finishHealthAttempt(t, h, key, AttemptResult{Model: "m", KeyID: key.ID, Provider: key.Provider, Upstream: key.Name, Outcome: OutcomeUpstream})
	h.mu.Lock()
	h.keys[makeKeyScope("m", key)].cooldownUntil = time.Now().Add(-time.Second)
	h.mu.Unlock()

	first, ok := h.Acquire("m", key)
	if !ok {
		t.Fatal("first half-open probe should be admitted")
	}
	h.Release(first)
	second, ok := h.Acquire("m", key)
	if !ok {
		t.Fatal("second half-open probe should be admitted after release")
	}
	// A stale duplicate release must not clear the second probe's slot.
	h.Release(first)
	if _, ok := h.Acquire("m", key); ok {
		t.Fatal("stale lease release cleared a newer probe")
	}
	h.Finish(second, AttemptResult{Model: "m", KeyID: key.ID, Provider: key.Provider, Upstream: key.Name, Outcome: OutcomeSuccess})
	if _, ok := h.Acquire("m", key); !ok {
		t.Fatal("successful half-open probe should restore availability")
	}
}

func TestHealthStateOldSuccessCannotClearNewerFailure(t *testing.T) {
	h := NewHealthState()
	key := registry.UpstreamKey{ID: "hub-0", Name: "hub", Provider: "openai"}
	older, ok := h.Acquire("m", key)
	if !ok {
		t.Fatal("older lease not admitted")
	}
	newer, ok := h.Acquire("m", key)
	if !ok {
		t.Fatal("newer lease not admitted")
	}
	h.Finish(newer, AttemptResult{Model: "m", KeyID: key.ID, Provider: key.Provider, Upstream: key.Name, Outcome: OutcomeUpstream})
	h.Finish(older, AttemptResult{Model: "m", KeyID: key.ID, Provider: key.Provider, Upstream: key.Name, Outcome: OutcomeSuccess, StartedAt: time.Now().Add(-time.Hour)})
	if _, ok := h.Acquire("m", key); ok {
		t.Fatal("old success cleared a newer failure cooldown")
	}
}

func TestHealthStateFinishIsIdempotent(t *testing.T) {
	h := NewHealthState()
	key := registry.UpstreamKey{ID: "hub-0", Name: "hub", Provider: "openai"}
	lease, ok := h.Acquire("m", key)
	if !ok {
		t.Fatal("lease not admitted")
	}
	result := AttemptResult{Model: "m", KeyID: key.ID, Provider: key.Provider, Upstream: key.Name, Outcome: OutcomeSuccess}
	h.Finish(lease, result)
	h.Finish(lease, result)
	items := h.Snapshot()
	if len(items) != 1 || items[0].Attempts != 1 || items[0].Successes != 1 {
		t.Fatalf("non-idempotent finish: %#v", items)
	}
}

func TestHealthLeaseReleasedAfterPanic(t *testing.T) {
	h := NewHealthState()
	key := registry.UpstreamKey{ID: "hub-0", Name: "hub", Provider: "openai"}
	lease, ok := h.Acquire("m", key)
	if !ok {
		t.Fatal("lease not admitted")
	}
	func() {
		defer func() { _ = recover() }()
		defer h.Release(lease)
		panic("test panic")
	}()
	if next, ok := h.Acquire("m", key); !ok {
		t.Fatalf("lease was not released after panic: %#v", next)
	} else {
		h.Release(next)
	}
}

func TestHealthStateProviderProbeSuccessRestoresPool(t *testing.T) {
	h := NewHealthState()
	k1 := registry.UpstreamKey{ID: "relay-0", Name: "relay", Provider: "openai", FailoverMode: "provider"}
	k2 := registry.UpstreamKey{ID: "relay-1", Name: "relay", Provider: "openai", FailoverMode: "provider"}
	finishHealthAttempt(t, h, k1, AttemptResult{Model: "m", KeyID: k1.ID, Provider: k1.Provider, Upstream: k1.Name, Outcome: OutcomeUpstream, ProviderScoped: true})
	h.mu.Lock()
	h.providers[makeProviderScope("m", k1)].cooldownUntil = time.Now().Add(-time.Second)
	h.mu.Unlock()
	probe, ok := h.Acquire("m", k2)
	if !ok {
		t.Fatal("provider half-open probe not admitted")
	}
	h.Finish(probe, AttemptResult{Model: "m", KeyID: k2.ID, Provider: k2.Provider, Upstream: k2.Name, Outcome: OutcomeSuccess})
	lease, ok := h.Acquire("m", k1)
	if !ok {
		t.Fatal("provider success did not restore sibling key")
	}
	h.Release(lease)
}

func TestSelectorAdaptiveOrdersSamePriorityByReliability(t *testing.T) {
	cfg := &config.Config{OpenAIResponses: []config.Provider{{
		Name: "hub", BaseURL: "https://hub.example",
		APIKeyEntries: []config.APIKeyEntry{{APIKey: "sk-bad"}, {APIKey: "sk-good"}},
		Models:        []config.ModelAlias{{Name: "m"}},
	}}}
	reg := BuildRegistry(cfg)
	_, keys, ok := reg.Resolve("m")
	if !ok || len(keys) != 2 {
		t.Fatalf("resolve keys: %v %d", ok, len(keys))
	}
	h := NewHealthState()
	h.ConfigureRouting("adaptive", 72*time.Hour, false)
	for i := 0; i < 4; i++ {
		finishHealthAttempt(t, h, keys[0], AttemptResult{Model: "m", KeyID: keys[0].ID, Provider: keys[0].Provider, Upstream: keys[0].Name, Outcome: OutcomeRequestError})
		finishHealthAttempt(t, h, keys[1], AttemptResult{Model: "m", KeyID: keys[1].ID, Provider: keys[1].Provider, Upstream: keys[1].Name, Outcome: OutcomeSuccess})
	}
	// Request errors are intentionally not reliability failures, so add one real
	// failure and immediately clear its operational cooldown for this scoring test.
	finishHealthAttempt(t, h, keys[0], AttemptResult{Model: "m", KeyID: keys[0].ID, Provider: keys[0].Provider, Upstream: keys[0].Name, Outcome: OutcomeUpstream})
	h.mu.Lock()
	h.keys[makeKeyScope("m", keys[0])].cooldownUntil = time.Time{}
	h.mu.Unlock()
	sel := NewSelector(reg, 0, h)
	key, _, err := sel.Pick("m", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != keys[1].ID {
		t.Fatalf("adaptive key=%s, want %s", key.ID, keys[1].ID)
	}
}

func TestSelectorAdaptiveShadowKeepsPriorityOrder(t *testing.T) {
	cfg := &config.Config{OpenAIResponses: []config.Provider{{
		Name: "hub", BaseURL: "https://hub.example",
		APIKeyEntries: []config.APIKeyEntry{{APIKey: "sk-a"}, {APIKey: "sk-b"}},
		Models:        []config.ModelAlias{{Name: "m"}},
	}}}
	h := NewHealthState()
	h.ConfigureRouting("adaptive", 72*time.Hour, true)
	reg := BuildRegistry(cfg)
	_, keys, _ := reg.Resolve("m")
	finishHealthAttempt(t, h, keys[0], AttemptResult{Model: "m", KeyID: keys[0].ID, Provider: keys[0].Provider, Upstream: keys[0].Name, Outcome: OutcomeRequestError})
	finishHealthAttempt(t, h, keys[1], AttemptResult{Model: "m", KeyID: keys[1].ID, Provider: keys[1].Provider, Upstream: keys[1].Name, Outcome: OutcomeSuccess})
	sel := NewSelector(reg, 0, h)
	key, _, err := sel.Pick("m", nil, "", nil)
	if err != nil || key.ID != keys[0].ID {
		t.Fatalf("shadow selection=%s err=%v, want priority key %s", key.ID, err, keys[0].ID)
	}
	recommendations := h.ShadowSnapshot()
	if len(recommendations) != 1 || recommendations[0].ActualKey != keys[0].ID || recommendations[0].RecommendedKey != keys[1].ID {
		t.Fatalf("shadow recommendation=%#v", recommendations)
	}
}

func TestSelectorAdaptiveDoesNotCrossProviderPriority(t *testing.T) {
	cfg := &config.Config{OpenAIResponses: []config.Provider{
		{Name: "official", Priority: 0, BaseURL: "https://official.example", APIKey: "sk-official", Models: []config.ModelAlias{{Name: "m"}}},
		{Name: "relay", Priority: 1, BaseURL: "https://relay.example", APIKey: "sk-relay", Models: []config.ModelAlias{{Name: "m"}}},
	}}
	reg := BuildRegistry(cfg)
	_, keys, _ := reg.Resolve("m")
	h := NewHealthState()
	h.ConfigureRouting("adaptive", 72*time.Hour, false)
	for i := 0; i < 4; i++ {
		recordScoredFailure(t, h, "m", keys[0])
		finishHealthAttempt(t, h, keys[1], AttemptResult{Model: "m", KeyID: keys[1].ID, Provider: keys[1].Provider, Upstream: keys[1].Name, Outcome: OutcomeSuccess})
	}
	h.mu.Lock()
	h.keys[makeKeyScope("m", keys[0])].cooldownUntil = time.Time{}
	h.mu.Unlock()
	sel := NewSelector(reg, 0, h)
	key, _, err := sel.Pick("m", nil, "", nil)
	if err != nil || key.Name != "official" {
		t.Fatalf("adaptive crossed provider priority: key=%+v err=%v", key, err)
	}
}

func TestSelectorAdaptiveColdStartRetainsDeterministicExploration(t *testing.T) {
	cfg := &config.Config{OpenAIResponses: []config.Provider{{
		Name: "hub", BaseURL: "https://hub.example",
		APIKeyEntries: []config.APIKeyEntry{{APIKey: "sk-a"}, {APIKey: "sk-b"}},
		Models:        []config.ModelAlias{{Name: "m"}},
	}}}
	h := NewHealthState()
	h.ConfigureRouting("adaptive", 72*time.Hour, false)
	sel := NewSelector(BuildRegistry(cfg), 0, h)
	first, _, err := sel.Pick("m", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := sel.Pick("m", map[string]struct{}{first.ID: {}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("cold-start exploration repeated key %s", first.ID)
	}
}

func TestShadowRecommendationSkipsCoolingKey(t *testing.T) {
	cfg := &config.Config{OpenAIResponses: []config.Provider{{
		Name: "hub", BaseURL: "https://hub.example",
		APIKeyEntries: []config.APIKeyEntry{{APIKey: "sk-cooling"}, {APIKey: "sk-good"}},
		Models:        []config.ModelAlias{{Name: "m"}},
	}}}
	reg := BuildRegistry(cfg)
	_, keys, _ := reg.Resolve("m")
	h := NewHealthState()
	h.ConfigureRouting("adaptive", 72*time.Hour, true)
	finishHealthAttempt(t, h, keys[0], AttemptResult{Model: "m", KeyID: keys[0].ID, Provider: keys[0].Provider, Upstream: keys[0].Name, Outcome: OutcomeUpstream})
	finishHealthAttempt(t, h, keys[1], AttemptResult{Model: "m", KeyID: keys[1].ID, Provider: keys[1].Provider, Upstream: keys[1].Name, Outcome: OutcomeSuccess})
	sel := NewSelector(reg, 0, h)
	key, _, err := sel.Pick("m", nil, "", nil)
	if err != nil || key.ID != keys[1].ID {
		t.Fatalf("cooling key was selected: %s err=%v", key.ID, err)
	}
	recommendations := h.ShadowSnapshot()
	if len(recommendations) != 1 || recommendations[0].RecommendedKey == keys[0].ID {
		t.Fatalf("cooling key was recommended: %#v", recommendations)
	}
}

func TestSelectorAdaptiveExploresUnknownKeyEveryTenthPick(t *testing.T) {
	cfg := &config.Config{OpenAIResponses: []config.Provider{{
		Name: "hub", BaseURL: "https://hub.example",
		APIKeyEntries: []config.APIKeyEntry{{APIKey: "sk-known"}, {APIKey: "sk-unknown"}},
		Models:        []config.ModelAlias{{Name: "m"}},
	}}}
	reg := BuildRegistry(cfg)
	_, keys, _ := reg.Resolve("m")
	h := NewHealthState()
	h.ConfigureRouting("adaptive", 72*time.Hour, false)
	for i := 0; i < 5; i++ {
		finishHealthAttempt(t, h, keys[0], AttemptResult{Model: "m", KeyID: keys[0].ID, Provider: keys[0].Provider, Upstream: keys[0].Name, Outcome: OutcomeSuccess})
	}
	sel := NewSelector(reg, 0, h)
	last := ""
	for i := 0; i < 10; i++ {
		key, _, lease, err := sel.PickWithLeaseExploring("m", nil, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		last = key.ID
		sel.Finish(lease, AttemptResult{Model: "m", KeyID: key.ID, Provider: key.Provider, Upstream: key.Name, Outcome: OutcomeSuccess})
	}
	if last != keys[1].ID {
		t.Fatalf("tenth pick=%s, want unknown key %s", last, keys[1].ID)
	}
}

func TestHealthReliabilityHalfLifeDecay(t *testing.T) {
	h := NewHealthState()
	key := registry.UpstreamKey{ID: "hub-0", Name: "hub", Provider: "openai"}
	h.mu.Lock()
	h.keys[makeKeyScope("m", key)] = &healthEntry{
		model: "m", keyID: key.ID, provider: key.Provider, upstream: key.Name,
		scoreSuccess: 3, scoreFailure: 1, scoreUpdatedAt: time.Now().Add(-24 * time.Hour),
	}
	h.halfLife = 24 * time.Hour
	h.mu.Unlock()
	score, samples := h.Reliability("m", key)
	if score < 0.62 || score > 0.63 || samples < 1.99 || samples > 2.01 {
		t.Fatalf("score=%f samples=%f, want ~0.625/2", score, samples)
	}
}

func TestSelectorAdaptiveDoesNotCrossEntryPriority(t *testing.T) {
	cfg := &config.Config{OpenAIResponses: []config.Provider{{
		Name: "hub", BaseURL: "https://hub.example",
		APIKeyEntries: []config.APIKeyEntry{{APIKey: "sk-low", Priority: 0}, {APIKey: "sk-good", Priority: 1}},
		Models:        []config.ModelAlias{{Name: "m"}},
	}}}
	reg := BuildRegistry(cfg)
	_, keys, _ := reg.Resolve("m")
	h := NewHealthState()
	h.ConfigureRouting("adaptive", 72*time.Hour, false)
	for i := 0; i < 4; i++ {
		recordScoredFailure(t, h, "m", keys[0])
		finishHealthAttempt(t, h, keys[1], AttemptResult{Model: "m", KeyID: keys[1].ID, Provider: keys[1].Provider, Upstream: keys[1].Name, Outcome: OutcomeSuccess})
	}
	sel := NewSelector(reg, 0, h)
	key, _, err := sel.Pick("m", nil, "", nil)
	if err != nil || key.EntryPriority != 0 {
		t.Fatalf("adaptive crossed entry priority: key=%+v err=%v", key, err)
	}
}

func finishHealthAttempt(t *testing.T, h *HealthState, key registry.UpstreamKey, result AttemptResult) {
	t.Helper()
	lease, ok := h.Acquire(result.Model, key)
	if !ok {
		t.Fatalf("acquire %s: key is unexpectedly unavailable", key.ID)
	}
	h.Finish(lease, result)
}

func recordScoredFailure(t *testing.T, h *HealthState, model string, key registry.UpstreamKey) {
	t.Helper()
	lease, ok := h.Acquire(model, key)
	if !ok {
		t.Fatalf("acquire %s: key is unexpectedly unavailable", key.ID)
	}
	h.Finish(lease, AttemptResult{Model: model, KeyID: key.ID, Provider: key.Provider, Upstream: key.Name, Outcome: OutcomeUpstream})
	h.mu.Lock()
	h.keys[makeKeyScope(model, key)].cooldownUntil = time.Time{}
	h.mu.Unlock()
}
