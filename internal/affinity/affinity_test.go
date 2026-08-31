package affinity_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/affinity"
	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
)

func TestLookupGJSONAndRecord(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled:           boolPtr(true),
		DefaultTTLSeconds: 60,
		MaxEntries:        100,
		Models:            []string{"claude"},
	})
	t.Cleanup(m.Close)

	body := []byte(`{"model":"claude-sonnet-4","metadata":{"user_id":"session-abc"},"messages":[]}`)
	match := m.Lookup("claude-sonnet-4", "/v1/messages", nil, body)
	if !match.Matched {
		t.Fatal("expected rule match")
	}
	if match.Found {
		t.Fatal("cache should be empty")
	}
	if match.CacheKey != "claude sticky:session-abc" {
		t.Fatalf("cache key %q", match.CacheKey)
	}

	m.Record(match.CacheKey, "provider-0", match.TTL)
	match2 := m.Lookup("claude-sonnet-4", "/v1/messages", nil, body)
	if !match2.Found || match2.KeyID != "provider-0" {
		t.Fatalf("expected sticky hit, got %+v", match2)
	}
}

func TestSessionHeaderBeatsBody(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Models:  []string{"gpt"},
	})
	t.Cleanup(m.Close)

	h := http.Header{}
	h.Set("Session-Id", "hdr-sess")
	body := []byte(`{"model":"gpt-5","prompt_cache_key":"body-pc"}`)
	match := m.Lookup("gpt-5", "/v1/responses", h, body)
	if !match.Matched {
		t.Fatal("expected match")
	}
	if match.CacheKey != "gpt sticky:hdr-sess" {
		t.Fatalf("want session header first, got %q", match.CacheKey)
	}
}

func TestProtocolBodyByPath(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Models:  []string{"claude", "gpt"},
	})
	t.Cleanup(m.Close)

	// messages prefers metadata.user_id
	body := []byte(`{"prompt_cache_key":"pc","metadata":{"user_id":"uid-1"}}`)
	match := m.Lookup("claude-3", "/v1/messages", nil, body)
	if !match.Matched || match.CacheKey != "claude sticky:uid-1" {
		t.Fatalf("messages path: %+v", match)
	}

	// responses prefers prompt_cache_key
	match = m.Lookup("gpt-4", "/v1/responses", nil, body)
	if !match.Matched || match.CacheKey != "gpt sticky:pc" {
		t.Fatalf("responses path: %+v", match)
	}
}

func TestContainsModelName(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Models:  []string{"claude"},
	})
	t.Cleanup(m.Close)

	// family token appears anywhere in model name
	body := []byte(`{"metadata":{"user_id":"u1"}}`)
	match := m.Lookup("proxy-claude-sonnet", "/v1/messages", nil, body)
	if !match.Matched {
		t.Fatal("contains match expected for proxy-claude-sonnet")
	}
}

func TestNoIdentityNoSticky(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Models:  []string{"grok"},
	})
	t.Cleanup(m.Close)
	match := m.Lookup("grok-4.5", "/v1/responses", nil, []byte(`{"model":"grok-4.5"}`))
	if match.Matched {
		t.Fatalf("no session/body field should not sticky: %+v", match)
	}
}

func TestLookupHeaderSource(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Rules: []config.ChannelAffinityRule{{
			Name: "header sticky",
			KeySources: []config.ChannelAffinityKeySource{
				{Type: "request_header", Key: "Session-Id"},
			},
			IncludeRuleName: true,
		}},
	})
	t.Cleanup(m.Close)
	h := http.Header{}
	h.Set("Session-Id", "s1")
	match := m.Lookup("any", "/v1/chat/completions", h, []byte(`{}`))
	if !match.Matched || match.CacheKey != "header sticky:s1" {
		t.Fatalf("%+v", match)
	}
}

func TestClearAndTTLExpiry(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled:           boolPtr(true),
		DefaultTTLSeconds: 1,
		Rules: []config.ChannelAffinityRule{{
			Name: "r",
			KeySources: []config.ChannelAffinityKeySource{
				{Type: "gjson", Path: "prompt_cache_key"},
			},
			IncludeRuleName: true,
		}},
	})
	t.Cleanup(m.Close)
	body := []byte(`{"prompt_cache_key":"k"}`)
	match := m.Lookup("m", "/v1/responses", nil, body)
	if !match.Matched {
		t.Fatal("match")
	}
	m.Record(match.CacheKey, "id1", match.TTL)
	if !m.Lookup("m", "/v1/responses", nil, body).Found {
		t.Fatal("should be sticky")
	}
	m.Clear(match.CacheKey)
	if m.Lookup("m", "/v1/responses", nil, body).Found {
		t.Fatal("cleared")
	}
	m.Record(match.CacheKey, "id1", time.Millisecond*50)
	time.Sleep(80 * time.Millisecond)
	if m.Lookup("m", "/v1/responses", nil, body).Found {
		t.Fatal("ttl expired")
	}
}

func TestModelPathFilters(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Rules: []config.ChannelAffinityRule{{
			Name:       "codex",
			ModelRegex: []string{`(?i)gpt`},
			PathRegex:  []string{`/v1/responses`},
			KeySources: []config.ChannelAffinityKeySource{
				{Type: "gjson", Path: "prompt_cache_key"},
			},
			IncludeRuleName: true,
		}},
	})
	t.Cleanup(m.Close)
	body := []byte(`{"prompt_cache_key":"x"}`)
	if m.Lookup("claude-3", "/v1/responses", nil, body).Matched {
		t.Fatal("model filter")
	}
	if m.Lookup("gpt-5", "/v1/messages", nil, body).Matched {
		t.Fatal("path filter")
	}
	if !m.Lookup("gpt-5", "/v1/responses", nil, body).Matched {
		t.Fatal("should match")
	}
}

func TestDisabledNoOp(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(false),
		Models:  []string{"claude"},
	})
	t.Cleanup(m.Close)
	match := m.Lookup("claude-x", "/v1/messages", nil, []byte(`{"metadata":{"user_id":"u"}}`))
	if match.Matched {
		t.Fatal("disabled manager must not match")
	}
}

func TestResolvePreferred(t *testing.T) {
	keys := []registry.UpstreamKey{{ID: "a"}, {ID: "b"}}
	k, ok := affinity.ResolvePreferred(keys, "b", nil)
	if !ok || k.ID != "b" {
		t.Fatalf("got %+v ok=%v", k, ok)
	}
	tried := map[string]struct{}{"b": {}}
	if _, ok := affinity.ResolvePreferred(keys, "b", tried); ok {
		t.Fatal("tried key must be skipped")
	}
}

func TestDefaultFamiliesIncludeGrok(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{})
	t.Cleanup(m.Close)
	body := []byte(`{"model":"grok-4.5","prompt_cache_key":"pc-1"}`)
	match := m.Lookup("grok-4.5", "/v1/responses", nil, body)
	if !match.Matched {
		t.Fatal("expected grok family match")
	}
	if match.RuleName != "grok sticky" {
		t.Fatalf("rule %q", match.RuleName)
	}
	if match.SkipRetry {
		t.Fatal("grok should allow retry by default")
	}
	m.Record(match.CacheKey, "grok-0", match.TTL)
	match2 := m.Lookup("grok-4.5", "/v1/chat/completions", nil, body)
	if !match2.Found || match2.KeyID != "grok-0" {
		t.Fatalf("sticky miss %+v", match2)
	}
}

func TestWeakHeaderDeferredToStrongHeader(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Models:  []string{"gpt"},
	})
	t.Cleanup(m.Close)

	h := http.Header{}
	h.Set("Session-Id", "hdr-sess")
	h.Set("X-Client-Request-Id", "req-1")
	body := []byte(`{"prompt_cache_key":"body-pc"}`)
	match := m.Lookup("gpt-5", "/v1/responses", h, body)
	if !match.Matched || match.CacheKey != "gpt sticky:hdr-sess" || match.Weak {
		t.Fatalf("strong header must beat weak header: %+v", match)
	}
}

func TestWeakHeaderDeferredToBody(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Models:  []string{"gpt"},
	})
	t.Cleanup(m.Close)

	h := http.Header{}
	h.Set("X-Client-Request-Id", "req-1")
	body := []byte(`{"prompt_cache_key":"body-pc"}`)
	match := m.Lookup("gpt-5", "/v1/responses", h, body)
	if !match.Matched || match.CacheKey != "gpt sticky:body-pc" || match.Weak {
		t.Fatalf("protocol body must beat weak header: %+v", match)
	}
}

func TestWeakHeaderOnlyShortTTL(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled:           boolPtr(true),
		DefaultTTLSeconds: 600,
		Models:            []string{"gpt"},
	})
	t.Cleanup(m.Close)

	h := http.Header{}
	h.Set("X-Client-Request-Id", "req-1")
	match := m.Lookup("gpt-5", "/v1/responses", h, []byte(`{}`))
	if !match.Matched || !match.Weak {
		t.Fatalf("weak-only identity must match: %+v", match)
	}
	if match.TTL != 60*time.Second {
		t.Fatalf("weak pin TTL must be capped to 60s, got %v", match.TTL)
	}
	m.Record(match.CacheKey, "gpt-0", match.TTL)
	if next := m.Lookup("gpt-5", "/v1/responses", h, []byte(`{}`)); !next.Found || next.KeyID != "gpt-0" {
		t.Fatalf("weak pin should stick within TTL: %+v", next)
	}
}

func TestStatsCounters(t *testing.T) {
	m := affinity.New(config.ChannelAffinitySetting{
		Enabled: boolPtr(true),
		Models:  []string{"gpt"},
	})
	t.Cleanup(m.Close)

	body := []byte(`{"prompt_cache_key":"pc-1"}`)
	if match := m.Lookup("gpt-5", "/v1/responses", nil, body); !match.Matched || match.Found {
		t.Fatalf("first lookup: %+v", match)
	}
	m.Record("gpt sticky:pc-1", "gpt-0", time.Minute)
	if next := m.Lookup("gpt-5", "/v1/responses", nil, body); !next.Found || next.KeyID != "gpt-0" {
		t.Fatalf("second lookup: %+v", next)
	}
	// rule matched but no identity field → not a matched lookup
	if miss := m.Lookup("gpt-5", "/v1/responses", nil, []byte(`{}`)); miss.Matched {
		t.Fatalf("no identity must not match: %+v", miss)
	}
	m.Clear("gpt sticky:pc-1")
	m.MarkPreferredUnavailable()

	st := m.Stats()
	if st.MatchedLookups != 2 || st.CacheHits != 1 || st.Records != 1 || st.Clears != 1 || st.PreferredUnavail != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if st.HitRate != 0.5 || st.Entries != 0 {
		t.Fatalf("rate=%v entries=%v", st.HitRate, st.Entries)
	}
}

func boolPtr(v bool) *bool { return &v }
