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

func finishHealthAttempt(t *testing.T, h *HealthState, key registry.UpstreamKey, result AttemptResult) {
	t.Helper()
	lease, ok := h.Acquire(result.Model, key)
	if !ok {
		t.Fatalf("acquire %s: key is unexpectedly unavailable", key.ID)
	}
	h.Finish(lease, result)
}
