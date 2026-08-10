package pool_test

import (
	"testing"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/pool"
)

func TestPickProviderPriorityBeatsEntryPriority(t *testing.T) {
	cfg := &config.Config{
		RequestRetry: 3,
		OpenAIResponses: []config.Provider{
			{
				Name:     "laysath-input",
				Priority: 0,
				BaseURL:  "https://a.example",
				APIKey:   "sk-input",
				Models:   []config.ModelAlias{{Name: "gpt-5.6-terra"}},
			},
			{
				Name:     "laysath",
				Priority: 1,
				BaseURL:  "https://b.example",
				APIKey:   "sk-laysath",
				Models:   []config.ModelAlias{{Name: "gpt-5.6-terra"}},
			},
			{
				Name:         "AI-HUB",
				Priority:     3,
				BaseURL:      "https://c.example",
				FailoverMode: "key",
				APIKeyEntries: []config.APIKeyEntry{
					{APIKey: "sk-hub-0", Priority: 0},
					{APIKey: "sk-hub-1", Priority: 1},
					{APIKey: "sk-hub-2", Priority: 1},
				},
				Models: []config.ModelAlias{{Name: "gpt-5.6-terra"}},
			},
		},
	}
	reg := pool.BuildRegistry(cfg)
	sel := pool.NewSelector(reg, cfg.RequestRetry)

	// First pick must be highest provider priority, not AI-HUB entry priority 1.
	k, _, err := sel.Pick("gpt-5.6-terra", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != "laysath-input" {
		t.Fatalf("first pick = %s/%s, want laysath-input", k.Name, k.ID)
	}

	// After provider-level skip of laysath-input, next is laysath (provider 1),
	// never AI-HUB (provider 3) even though its keys have entry priority 1.
	tried := map[string]struct{}{k.ID: {}}
	skip := map[string]struct{}{"laysath-input": {}}
	k2, _, err := sel.Pick("gpt-5.6-terra", tried, "", skip)
	if err != nil {
		t.Fatal(err)
	}
	if k2.Name != "laysath" {
		t.Fatalf("after skip laysath-input got %s/%s, want laysath", k2.Name, k2.ID)
	}
}

func TestPickEntryPriorityWithinProvider(t *testing.T) {
	cfg := &config.Config{
		OpenAIResponses: []config.Provider{
			{
				Name:     "hub",
				Priority: 0,
				BaseURL:  "https://hub.example",
				APIKeyEntries: []config.APIKeyEntry{
					{APIKey: "sk-low", Priority: 2},
					{APIKey: "sk-high", Priority: 0},
					{APIKey: "sk-mid", Priority: 1},
				},
				Models: []config.ModelAlias{{Name: "m"}},
			},
		},
	}
	reg := pool.BuildRegistry(cfg)
	sel := pool.NewSelector(reg, 2)

	k, _, err := sel.Pick("m", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if k.APIKey != "sk-high" || k.EntryPriority != 0 {
		t.Fatalf("want sk-high entry 0, got key=%s entry=%d", k.APIKey, k.EntryPriority)
	}

	tried := map[string]struct{}{k.ID: {}}
	k2, _, err := sel.Pick("m", tried, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if k2.APIKey != "sk-mid" || k2.EntryPriority != 1 {
		t.Fatalf("want sk-mid entry 1, got key=%s entry=%d", k2.APIKey, k2.EntryPriority)
	}
}

func TestPickDoesNotCompareEntryPriorityAcrossProviders(t *testing.T) {
	cfg := &config.Config{
		OpenAIResponses: []config.Provider{
			{
				Name:     "first",
				Priority: 0,
				BaseURL:  "https://first.example",
				APIKeyEntries: []config.APIKeyEntry{
					{APIKey: "sk-first-low", Priority: 2},
					{APIKey: "sk-first-high", Priority: 1},
				},
				Models: []config.ModelAlias{{Name: "m"}},
			},
			{
				Name:     "second",
				Priority: 0,
				BaseURL:  "https://second.example",
				APIKeyEntries: []config.APIKeyEntry{
					{APIKey: "sk-second", Priority: 0},
				},
				Models: []config.ModelAlias{{Name: "m"}},
			},
		},
	}
	sel := pool.NewSelector(pool.BuildRegistry(cfg), 0)

	k, _, err := sel.Pick("m", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != "first" || k.APIKey != "sk-first-high" {
		t.Fatalf("pick = %s/%s, want first/sk-first-high", k.Name, k.APIKey)
	}
}

func TestExpandStoresProviderAndEntryPriority(t *testing.T) {
	cfg := &config.Config{
		OpenAIResponses: []config.Provider{
			{
				Name:     "hub",
				Priority: 3,
				BaseURL:  "https://hub.example",
				APIKeyEntries: []config.APIKeyEntry{
					{APIKey: "sk-a", Priority: 1},
					{APIKey: "sk-b"}, // entry 0
				},
				Models: []config.ModelAlias{{Name: "m"}},
			},
		},
	}
	reg := pool.BuildRegistry(cfg)
	_, keys, ok := reg.Resolve("m")
	if !ok || len(keys) != 2 {
		t.Fatalf("keys=%v ok=%v", keys, ok)
	}
	byKey := map[string]struct{ pri, entry int }{}
	for _, k := range keys {
		byKey[k.APIKey] = struct{ pri, entry int }{k.Priority, k.EntryPriority}
	}
	if got := byKey["sk-a"]; got.pri != 3 || got.entry != 1 {
		t.Fatalf("sk-a = %+v, want provider 3 entry 1", got)
	}
	if got := byKey["sk-b"]; got.pri != 3 || got.entry != 0 {
		t.Fatalf("sk-b = %+v, want provider 3 entry 0", got)
	}
}

func TestModelSpeedVerbosityPropagateToKeys(t *testing.T) {
	cfg := &config.Config{
		OpenAIResponses: []config.Provider{
			{
				Name:     "isok",
				Priority: 0,
				BaseURL:  "https://isok.example",
				APIKey:   "sk-a",
				Models: []config.ModelAlias{
					{Name: "gpt-5.6-luna", Speed: "fast", Verbosity: "low"},
					{Name: "gpt-5.6-sol", Speed: "fast", Verbosity: "high"},
					{Name: "gpt-5.6-terra", Verbosity: "low"},
					{Name: "grok-4.5"},
				},
			},
		},
	}
	reg := pool.BuildRegistry(cfg)

	cases := map[string]struct{ speed, verbosity string }{
		"gpt-5.6-luna":  {"fast", "low"},
		"gpt-5.6-sol":   {"fast", "high"},
		"gpt-5.6-terra": {"", "low"},
		"grok-4.5":      {"", ""},
	}
	for alias, want := range cases {
		_, keys, ok := reg.Resolve(alias)
		if !ok || len(keys) == 0 {
			t.Fatalf("resolve %s: ok=%v keys=%d", alias, ok, len(keys))
		}
		for _, k := range keys {
			if k.Speed != want.speed || k.Verbosity != want.verbosity {
				t.Fatalf("%s key speed=%q verbosity=%q, want speed=%q verbosity=%q", alias, k.Speed, k.Verbosity, want.speed, want.verbosity)
			}
		}
	}
}

// TestResetRoundRobin verifies the counter map is cleared: after picks
// advance the counter, ResetRoundRobin makes the next pick land on index 0.
func TestResetRoundRobin(t *testing.T) {
	cfg := &config.Config{
		OpenAIResponses: []config.Provider{{
			Name:     "hub",
			Priority: 0,
			BaseURL:  "https://hub.example",
			APIKeyEntries: []config.APIKeyEntry{
				{APIKey: "sk-0"},
				{APIKey: "sk-1"},
				{APIKey: "sk-2"},
			},
			Models: []config.ModelAlias{{Name: "m"}},
		}},
	}
	sel := pool.NewSelector(pool.BuildRegistry(cfg), 0)

	// Advance the round-robin counter a couple of times.
	for i := 0; i < 3; i++ {
		if _, _, err := sel.Pick("m", nil, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// Reset: the very next pick should be back at index 0 (sk-0).
	sel.ResetRoundRobin()
	k, _, err := sel.Pick("m", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if k.APIKey != "sk-0" {
		t.Fatalf("after reset first pick = %s, want sk-0", k.APIKey)
	}
}
