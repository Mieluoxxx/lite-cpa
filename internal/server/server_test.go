package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/reqlog"
	"github.com/Mieluoxxx/lite-cpa/internal/server"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
	"github.com/tidwall/gjson"
)

func TestChatCompletionsToOpenAIUpstream(t *testing.T) {
	translator.RegisterBuiltin()

	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host:         "127.0.0.1",
		Port:         port,
		APIKeys:      []string{"sk-test"},
		RequestRetry: 1,
		MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name:    "mock",
			BaseURL: up.URL,
			APIKey:  "sk-up",
			Models:  []config.ModelAlias{{Name: "mock-model", Alias: "mock-model", Speed: "fast"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() {
		_ = srv.Shutdown(t.Context())
	})
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(`{"model":"mock-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	if gjson.GetBytes(raw, "choices.0.message.content").String() != "pong" {
		t.Fatalf("body %s", raw)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("upstream path %q", gotPath)
	}
	if gotAuth != "Bearer sk-up" {
		t.Fatalf("auth %q", gotAuth)
	}
	if gjson.Get(gotBody, "model").String() != "mock-model" {
		t.Fatalf("upstream body %s", gotBody)
	}
	if got := gjson.Get(gotBody, "service_tier"); got.Exists() {
		t.Fatalf("service_tier should be absent for Chat Completions; got %q; body=%s", got.String(), gotBody)
	}
}

func TestResponsesGPTInjectsServiceTierAndVerbosity(t *testing.T) {
	translator.RegisterBuiltin()

	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}]}`))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAIResponses: []config.Provider{{
			BaseURL: up.URL + "/v1",
			APIKey:  "sk-up",
			Models:  []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5", Speed: "fast", Verbosity: "low"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5","input":"hi","stream":false}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}

	if got := gjson.GetBytes(gotBody, "service_tier").String(); got != "priority" {
		t.Fatalf("service_tier = %q, want priority; body=%s", got, gotBody)
	}
	if got := gjson.GetBytes(gotBody, "text.verbosity").String(); got != "low" {
		t.Fatalf("text.verbosity = %q, want low; body=%s", got, gotBody)
	}
}

func TestResponsesNonGPTStripsClientVerbosity(t *testing.T) {
	translator.RegisterBuiltin()

	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","model":"grok-4.5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}]}`))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAIResponses: []config.Provider{{
			BaseURL: up.URL + "/v1",
			APIKey:  "sk-up",
			Models:  []config.ModelAlias{{Name: "grok-4.5", Alias: "grok-4.5", Verbosity: "low"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/responses",
		strings.NewReader(`{"model":"grok-4.5","text":{"verbosity":"high"},"input":"hi","stream":false}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}

	if got := gjson.GetBytes(gotBody, "text.verbosity"); got.Exists() {
		t.Fatalf("text.verbosity should be stripped for non-GPT Responses; got %q; body=%s", got.String(), gotBody)
	}
}

func TestMultiKeyRotation(t *testing.T) {
	translator.RegisterBuiltin()
	var hits []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		hits = append(hits, auth)
		if auth == "Bearer sk-bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, RequestRetry: 2, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "mock", BaseURL: up.URL,
			APIKeyEntries: []config.APIKeyEntry{
				{APIKey: "sk-bad", Priority: 0},
				{APIKey: "sk-good", Priority: 1},
			},
			Models: []config.ModelAlias{{Name: "m", Alias: "m"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s hits=%v", resp.StatusCode, raw, hits)
	}
	if len(hits) < 2 {
		t.Fatalf("expected rotation, hits=%v", hits)
	}
}

func TestChannelAffinityStickyKey(t *testing.T) {
	translator.RegisterBuiltin()
	var hits []string
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		hits = append(hits, auth)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, RequestRetry: 3, MaxBodyBytes: 1 << 20,
		ChannelAffinity: config.ChannelAffinitySetting{
			Enabled:           boolPtr(true),
			DefaultTTLSeconds: 60,
			Rules: []config.ChannelAffinityRule{{
				Name: "session sticky",
				KeySources: []config.ChannelAffinityKeySource{
					{Type: "gjson", Path: "metadata.user_id"},
				},
				IncludeRuleName: true,
			}},
		},
		OpenAICompletions: []config.Provider{{
			Name: "mock", BaseURL: up.URL,
			APIKeyEntries: []config.APIKeyEntry{
				{APIKey: "sk-a", Priority: 0},
				{APIKey: "sk-b", Priority: 0},
			},
			Models: []config.ModelAlias{{Name: "m", Alias: "m"}},
		}},
	}
	// Apply defaults used by config.Load for affinity capacity.
	cfg.ChannelAffinity.MaxEntries = 1000
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	body := `{"model":"m","metadata":{"user_id":"sess-sticky-1"},"messages":[{"role":"user","content":"x"}]}`
	do := func() {
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d %s", resp.StatusCode, raw)
		}
	}
	// Several requests with the same affinity value should pin one upstream key.
	for range 6 {
		do()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 6 {
		t.Fatalf("hits=%v", hits)
	}
	first := hits[0]
	for i, h := range hits {
		if h != first {
			t.Fatalf("expected sticky key %q, hit[%d]=%q all=%v", first, i, h, hits)
		}
	}
}
func TestChannelAffinitySkipRetry(t *testing.T) {
	translator.RegisterBuiltin()
	var hits int
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		// First request succeeds (to establish sticky pin); later fail.
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, RequestRetry: 5, MaxBodyBytes: 1 << 20,
		ChannelAffinity: config.ChannelAffinitySetting{
			Enabled: boolPtr(true),
			Rules: []config.ChannelAffinityRule{{
				Name: "sticky skip",
				KeySources: []config.ChannelAffinityKeySource{
					{Type: "gjson", Path: "metadata.user_id"},
				},
				SkipRetryOnFailure: true,
			}},
		},
		OpenAICompletions: []config.Provider{{
			Name: "mock", BaseURL: up.URL,
			APIKeyEntries: []config.APIKeyEntry{
				{APIKey: "sk-a"}, {APIKey: "sk-b"}, {APIKey: "sk-c"},
			},
			Models: []config.ModelAlias{{Name: "m", Alias: "m"}},
		}},
	}
	cfg.ChannelAffinity.MaxEntries = 100
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	do := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","metadata":{"user_id":"u1"},"messages":[{"role":"user","content":"x"}]}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp1 := do()
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != 200 {
		t.Fatalf("warm status %d", resp1.StatusCode)
	}

	before := hits
	resp2 := do()
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("status %d", resp2.StatusCode)
	}
	// One sticky attempt only — no multi-key rotation.
	if hits-before != 1 {
		t.Fatalf("skip-retry should attempt sticky key once, delta=%d hits=%d", hits-before, hits)
	}
}

func TestModelsAndAuth(t *testing.T) {
	translator.RegisterBuiltin()
	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "x", BaseURL: "http://127.0.0.1:9", APIKey: "k",
			Models: []config.ModelAlias{{Name: "a", Alias: "alias-a"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("want 401 got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	data, _ := payload["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("models: %#v", payload)
	}
}

func TestOpenAIToAnthropicNonStream(t *testing.T) {
	translator.RegisterBuiltin()

	var gotStream bool
	var gotPath, gotSpeed, gotBeta string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotStream = gjson.GetBytes(body, "stream").Bool()
		gotSpeed = gjson.GetBytes(body, "speed").String()
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(claudeSSEFixture()))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		AnthropicMessages: []config.Provider{{
			BaseURL: up.URL,
			APIKey:  "sk-ant",
			Headers: map[string]string{"anthropic-beta": "oauth-2025-04-20"},
			Models:  []config.ModelAlias{{Name: "claude-sonnet-4", Alias: "claude-sonnet-4", Speed: "fast"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type %q body %s", ct, raw)
	}
	if gjson.GetBytes(raw, "object").String() != "chat.completion" {
		t.Fatalf("want chat.completion, got %s", raw)
	}
	if gjson.GetBytes(raw, "choices.0.message.content").String() != "hello-from-claude" {
		t.Fatalf("content: %s", raw)
	}
	// Must not return data: [DONE] as the whole body.
	if strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("non-stream body leaked stream marker: %s", raw)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path %q", gotPath)
	}
	if !gotStream {
		t.Fatal("expected cross-format nonstream client to request Anthropic stream=true for NonStream aggregator")
	}
	if gotSpeed != "fast" {
		t.Fatalf("upstream speed %q, want fast", gotSpeed)
	}
	if !strings.Contains(gotBeta, "fast-mode-2026-02-01") {
		t.Fatalf("Anthropic-Beta %q missing fast-mode token", gotBeta)
	}
}

func TestResponsesToAnthropicNonStream(t *testing.T) {
	translator.RegisterBuiltin()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(claudeSSEFixture()))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		AnthropicMessages: []config.Provider{{
			BaseURL: up.URL,
			APIKey:  "sk-ant",
			Models:  []config.ModelAlias{{Name: "claude-sonnet-4", Alias: "claude-sonnet-4"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/responses",
		strings.NewReader(`{"model":"claude-sonnet-4","input":"hi","stream":false}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	if gjson.GetBytes(raw, "object").String() != "response" {
		t.Fatalf("want response object, got %s", raw)
	}
	if strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("non-stream body is stream residue: %s", raw)
	}
}

func TestOpenAIToAnthropicStreamSSEDelimiters(t *testing.T) {
	translator.RegisterBuiltin()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Multi-line SSE events with blank-line delimiters.
		_, _ = w.Write([]byte(claudeSSEFixture()))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		AnthropicMessages: []config.Provider{{
			BaseURL: up.URL,
			APIKey:  "sk-ant",
			Models:  []config.ModelAlias{{Name: "claude-sonnet-4", Alias: "claude-sonnet-4"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	body := string(raw)
	if !strings.Contains(body, "\n\n") {
		t.Fatalf("SSE body missing event delimiters \\n\\n: %q", body)
	}
	// Framed data lines
	if !strings.Contains(body, "data: ") {
		t.Fatalf("expected data: frames: %q", body)
	}
	// At least one JSON chunk with assistant content or role
	if !strings.Contains(body, "chat.completion.chunk") && !strings.Contains(body, `"delta"`) {
		t.Fatalf("unexpected stream body: %s", body)
	}
}

func TestOpenAISameFormatStreamDONE(t *testing.T) {
	translator.RegisterBuiltin()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n",
		))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "mock", BaseURL: up.URL, APIKey: "sk-up",
			Models: []config.ModelAlias{{Name: "m", Alias: "m"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing terminal [DONE]: %q", body)
	}
	// Exactly one DONE marker preferred
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("want one [DONE], got body %q", body)
	}
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("missing chunks: %q", body)
	}
}

func TestResponsesSameFormatStreamEventAssociation(t *testing.T) {
	translator.RegisterBuiltin()

	// Upstream emits multi-field SSE events: event: then data: then blank line.
	upstreamBody := "" +
		"event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n" +
		"\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n" +
		"\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n" +
		"\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAIResponses: []config.Provider{{
			BaseURL: up.URL + "/v1",
			APIKey:  "sk-up",
			Models:  []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, raw)
	}
	body := string(raw)

	// event: and data: must remain associated within the same event (not each
	// line closed as its own \n\n event).
	if !strings.Contains(body, "event: response.created\ndata: {\"type\":\"response.created\"") {
		t.Fatalf("event/data association broken for response.created:\n%s", body)
	}
	if !strings.Contains(body, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\"") {
		t.Fatalf("event/data association broken for delta:\n%s", body)
	}
	if !strings.Contains(body, "event: response.completed\ndata: {\"type\":\"response.completed\"") {
		t.Fatalf("event/data association broken for completed:\n%s", body)
	}
	// Events should be separated by blank lines
	if !strings.Contains(body, "}\n\nevent: response.output_text.delta") &&
		!strings.Contains(body, "}\n\nevent: response.output_text.delta") {
		// allow \r\n variants; core association already checked
		if strings.Count(body, "\n\n") < 2 {
			t.Fatalf("expected multi-event SSE delimiters: %q", body)
		}
	}
}

func TestResponsesStreamUnexpectedEOFEmitsFailedAndLogsCause(t *testing.T) {
	translator.RegisterBuiltin()

	var hitsMu sync.Mutex
	hits := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		hits++
		hitsMu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream response writer does not support hijacking")
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack upstream connection: %v", err)
			return
		}
		body := "event: response.created\n" +
			"data: {\"type\":\"response.created\",\"sequence_number\":1,\"response\":{\"id\":\"resp_cut\",\"object\":\"response\",\"model\":\"gpt-5\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
			"event: response.output_item.done\n" +
			"data: {\"type\":\"response.output_item.done\",\"sequence_number\":2,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"shell\",\"arguments\":\"{}\"}}\n\n"
		_, _ = io.WriteString(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = io.WriteString(rw, strconv.FormatInt(int64(len(body)), 16)+"\r\n"+body+"\r\n")
		_ = rw.Flush()
		_ = conn.Close() // Deliberately omit the terminating zero-length chunk.
	}))
	t.Cleanup(up.Close)

	logger, err := reqlog.Open(config.RequestLogConfig{
		Enabled: true, Backend: "sqlite", Retention: "1h",
		SQLite: config.SQLiteLogConfig{Path: filepath.Join(t.TempDir(), "requests.db")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, RequestRetry: 2, MaxBodyBytes: 1 << 20,
		OpenAIResponses: []config.Provider{{
			Name: "truncated", BaseURL: up.URL + "/v1", APIKey: "sk-up",
			Models: []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5"}},
		}},
	}
	srv := server.New(cfg, logger)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHTTP(t, baseURL+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read downstream stream: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, "response.output_item.done") {
		t.Fatalf("partial semantic output was not preserved: %q", body)
	}
	if got := strings.Count(body, "event: response.failed\n"); got != 1 {
		t.Fatalf("response.failed count = %d, want 1; body=%q", got, body)
	}
	if !strings.Contains(body, `"response":{"id":"resp_cut"`) || !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("response.failed did not preserve response identity: %q", body)
	}
	hitsMu.Lock()
	gotHits := hits
	hitsMu.Unlock()
	if gotHits != 1 {
		t.Fatalf("upstream hits = %d, want 1 after semantic output", gotHits)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		logs, listErr := logger.List(t.Context(), reqlog.ListFilter{Limit: 10})
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, record := range logs.Items {
			if record.Upstream == "truncated" && record.StatusCode == http.StatusOK && record.Outcome == reqlog.OutcomeError && !record.UsageComplete && strings.Contains(record.Error, "unexpected EOF") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing unexpected EOF request log: %#v", logs.Items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResponsesStreamClientCancelKeepsWireStatusAndOutcome(t *testing.T) {
	translator.RegisterBuiltin()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cancel\",\"status\":\"in_progress\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(up.Close)

	logger, err := reqlog.Open(config.RequestLogConfig{
		Enabled: true, Backend: "sqlite", Retention: "1h",
		SQLite: config.SQLiteLogConfig{Path: filepath.Join(t.TempDir(), "requests.db")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	port := freePort(t)
	srv := server.New(&config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAIResponses: []config.Provider{{
			Name: "cancel-upstream", BaseURL: up.URL + "/v1", APIKey: "sk-up",
			Models: []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5"}},
		}},
	}, logger)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHTTP(t, baseURL+"/healthz")

	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		resp.Body.Close()
		t.Fatalf("read initial stream event: %v", err)
	}
	cancel()
	_ = resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		logs, listErr := logger.List(t.Context(), reqlog.ListFilter{Limit: 10})
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(logs.Items) == 1 {
			record := logs.Items[0]
			if record.StatusCode != http.StatusOK || record.Outcome != reqlog.OutcomeClientCanceled || record.UsageComplete {
				t.Fatalf("canceled record = %#v", record)
			}
			stats, statsErr := logger.Stats(t.Context(), reqlog.ListFilter{})
			if statsErr != nil {
				t.Fatal(statsErr)
			}
			if stats.Total != 1 || stats.Success != 0 || stats.Errors != 0 || stats.Canceled != 1 || stats.UsageIncomplete != 1 {
				t.Fatalf("canceled stats = %#v", stats)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing canceled request log: %#v", logs.Items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResponsesStreamOverloadRetriesAndLogsAttempt(t *testing.T) {
	translator.RegisterBuiltin()
	var badHits, goodHits int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\ndata: {\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"busy\"}}\n\n"))
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		goodHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\"}}\n\n"))
	}))
	t.Cleanup(good.Close)

	logger, err := reqlog.Open(config.RequestLogConfig{
		Enabled: true, Backend: "sqlite", Retention: "1h",
		SQLite: config.SQLiteLogConfig{Path: filepath.Join(t.TempDir(), "requests.db")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, RequestRetry: 1, MaxBodyBytes: 1 << 20,
		OpenAIResponses: []config.Provider{
			{Name: "overloaded", BaseURL: bad.URL + "/v1", APIKey: "sk-bad", Priority: 0, FailoverMode: "provider", Models: []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5"}}},
			{Name: "healthy", BaseURL: good.URL + "/v1", APIKey: "sk-good", Priority: 1, FailoverMode: "provider", Models: []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5"}}},
		},
	}
	srv := server.New(cfg, logger)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHTTP(t, baseURL+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "response.completed") {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	if badHits != 1 || goodHits != 1 {
		t.Fatalf("upstream hits overloaded=%d healthy=%d, want 1 each", badHits, goodHits)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		logs, listErr := logger.List(t.Context(), reqlog.ListFilter{Limit: 10})
		if listErr != nil {
			t.Fatal(listErr)
		}
		var sawOverload, sawSuccess bool
		for _, record := range logs.Items {
			sawOverload = sawOverload || (record.Upstream == "overloaded" && record.StatusCode == http.StatusServiceUnavailable && strings.Contains(record.Error, "server_is_overloaded"))
			sawSuccess = sawSuccess || (record.Upstream == "healthy" && record.StatusCode == http.StatusOK)
		}
		if sawOverload && sawSuccess {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing overload or success records: %#v", logs.Items)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClaudeUpstreamToResponsesClientStream(t *testing.T) {
	// Claude upstream SSE → Responses client: event: must stay with data:,
	// and a terminal response.completed (or message_stop-derived) event exists.
	translator.RegisterBuiltin()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(claudeSSEFixture()))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		AnthropicMessages: []config.Provider{{
			BaseURL: up.URL,
			APIKey:  "sk-ant",
			Models:  []config.ModelAlias{{Name: "claude-sonnet-4", Alias: "claude-sonnet-4"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/responses",
		strings.NewReader(`{"model":"claude-sonnet-4","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, "event: response.created\ndata: ") {
		t.Fatalf("missing associated response.created event:\n%s", body)
	}
	if !strings.Contains(body, "event: response.completed\ndata: ") &&
		!strings.Contains(body, "event: response.incomplete\ndata: ") {
		t.Fatalf("missing terminal response event:\n%s", body)
	}
	// event and data should not be split into separate \n\n-terminated events
	if strings.Contains(body, "event: response.created\n\ndata: ") {
		t.Fatalf("event/data split into separate SSE events:\n%s", body)
	}
}

func TestResponsesUpstreamToClaudeClientStream(t *testing.T) {
	// Responses upstream SSE → Claude client via two-hop state.
	translator.RegisterBuiltin()

	upstreamBody := "" +
		"event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\",\"created_at\":1,\"status\":\"in_progress\",\"output\":[]}}\n" +
		"\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n" +
		"\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n" +
		"\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAIResponses: []config.Provider{{
			BaseURL: up.URL,
			APIKey:  "sk-up",
			Models:  []config.ModelAlias{{Name: "gpt-5", Alias: "gpt-5"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/messages",
		strings.NewReader(`{"model":"gpt-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, "event: message_start\ndata: ") {
		t.Fatalf("missing associated message_start:\n%s", body)
	}
	if !strings.Contains(body, "event: message_stop\ndata: ") &&
		!strings.Contains(body, "event: message_delta\ndata: ") {
		t.Fatalf("missing terminal/progress claude events:\n%s", body)
	}
	if strings.Contains(body, "event: message_start\n\ndata: ") {
		t.Fatalf("event/data split into separate SSE events:\n%s", body)
	}
}

func claudeSSEFixture() string {
	return "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n" +
		"\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n" +
		"\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello-from-claude\"}}\n" +
		"\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n" +
		"\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n" +
		"\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n" +
		"\n"
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server not ready: %s", url)
}

func TestProviderFailoverSkipsSupplier(t *testing.T) {
	translator.RegisterBuiltin()
	var hits []string
	var mu sync.Mutex
	// Two upstream servers: A always 500 (dead relay), B healthy.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, "dead:"+r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	t.Cleanup(dead.Close)
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, "live:"+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(live.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"},
		RequestRetry: 5, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{
			{
				Name: "relay-a", BaseURL: dead.URL, Priority: 0,
				FailoverMode: "provider",
				APIKeyEntries: []config.APIKeyEntry{
					{APIKey: "sk-a1"}, {APIKey: "sk-a2"}, {APIKey: "sk-a3"},
				},
				Models: []config.ModelAlias{{Name: "m", Alias: "m"}},
			},
			{
				Name: "relay-b", BaseURL: live.URL, Priority: 1,
				// default key mode — only matters if it fails
				APIKeyEntries: []config.APIKeyEntry{
					{APIKey: "sk-b1"},
				},
				Models: []config.ModelAlias{{Name: "m", Alias: "m"}},
			},
		},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s hits=%v", resp.StatusCode, raw, hits)
	}
	mu.Lock()
	defer mu.Unlock()
	// provider mode: only one hit on dead relay (first key), then jump to live.
	deadHits := 0
	liveHits := 0
	for _, h := range hits {
		if strings.HasPrefix(h, "dead:") {
			deadHits++
		}
		if strings.HasPrefix(h, "live:") {
			liveHits++
		}
	}
	if deadHits != 1 {
		t.Fatalf("expected exactly 1 dead-relay probe, hits=%v", hits)
	}
	if liveHits != 1 {
		t.Fatalf("expected live success, hits=%v", hits)
	}
}

func TestKeyFailoverStaysOnSupplier(t *testing.T) {
	translator.RegisterBuiltin()
	var hits []string
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		hits = append(hits, auth)
		mu.Unlock()
		if auth == "Bearer sk-a1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"},
		RequestRetry: 3, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "relay", BaseURL: up.URL,
			FailoverMode: "key",
			APIKeyEntries: []config.APIKeyEntry{
				{APIKey: "sk-a1", Priority: 0},
				{APIKey: "sk-a2", Priority: 0},
			},
			Models: []config.ModelAlias{{Name: "m", Alias: "m"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d hits=%v", resp.StatusCode, hits)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) < 2 || hits[0] != "Bearer sk-a1" || hits[1] != "Bearer sk-a2" {
		t.Fatalf("expected key rotation within supplier, hits=%v", hits)
	}
}

func boolPtr(v bool) *bool { return &v }
