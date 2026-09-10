package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/executor"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
)

func TestForwardNormalStreamsSettleHealthSuccess(t *testing.T) {
	translator.RegisterBuiltin()
	cases := []struct {
		name     string
		provider string
		source   translator.Format
		body     string
		stream   string
	}{
		{
			name: "chat", provider: "openai", source: translator.FormatOpenAI,
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			stream: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n",
		},
		{
			name: "responses", provider: "openai-response", source: translator.FormatOpenAIResponse,
			body:   `{"model":"m","input":"hi","stream":true}`,
			stream: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n",
		},
		{
			name: "claude", provider: "claude", source: translator.FormatClaude,
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`,
			stream: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.stream))
			}))
			defer up.Close()

			provider := config.Provider{
				Name:    "upstream",
				BaseURL: up.URL,
				APIKey:  "sk-upstream",
				Models:  []config.ModelAlias{{Name: "m"}},
			}
			enabled := false
			cfg := &config.Config{RequestRetry: 0, ChannelAffinity: config.ChannelAffinitySetting{Enabled: &enabled}}
			switch tc.provider {
			case "openai":
				cfg.OpenAICompletions = []config.Provider{provider}
			case "openai-response":
				cfg.OpenAIResponses = []config.Provider{provider}
			case "claude":
				cfg.AnthropicMessages = []config.Provider{provider}
			}
			s := New(cfg, nil)
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			r := httptest.NewRequest(http.MethodPost, "/v1/stream", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			s.forward(w, r, "stream-"+tc.name, tc.name, "m", "m", []byte(tc.body), time.Now(),
				func(ctx context.Context, key registry.UpstreamKey, upstreamModel string) (any, error) {
					return executor.Execute(ctx, key, upstreamModel, tc.source, []byte(tc.body), true)
				})
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			items := s.health.Snapshot()
			if len(items) != 1 || items[0].Successes != 1 || items[0].Failures != 0 {
				t.Fatalf("health=%#v, want one success", items)
			}
		})
	}
}

func TestForwardThinkingSuffixSkipsCoolingKey(t *testing.T) {
	cfg := &config.Config{
		RequestRetry: 0,
		OpenAICompletions: []config.Provider{{
			Name: "mock", BaseURL: "https://mock.example",
			APIKeyEntries: []config.APIKeyEntry{{APIKey: "sk-a"}, {APIKey: "sk-b"}},
			Models:        []config.ModelAlias{{Name: "m"}},
		}},
	}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	selected := make([]string, 0, 2)
	serve := func(_ context.Context, key registry.UpstreamKey, _ string) (any, error) {
		selected = append(selected, key.ID)
		return nil, executor.StatusError{Code: http.StatusInternalServerError, Body: "busy"}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"m(high)"}`)
	s.forward(httptest.NewRecorder(), req, "suffix-1", "chat", "m(high)", "m", body, time.Now(), serve)
	s.selector.ResetRoundRobin()
	s.forward(httptest.NewRecorder(), req, "suffix-2", "chat", "m(high)", "m", body, time.Now(), serve)
	if len(selected) != 2 || selected[0] == selected[1] {
		t.Fatalf("selected=%v, want second request to skip cooling key", selected)
	}
}

func TestImagesStreamSettlesHealthSuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"AA==\"}\n\n"))
		_, _ = w.Write([]byte("event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"BB==\"}\n\n"))
	}))
	defer up.Close()
	cfg := &config.Config{
		RequestRetry: 0,
		OpenAIImages: []config.Provider{{Name: "images", BaseURL: up.URL, APIKey: "sk-image", Models: []config.ModelAlias{{Name: "gpt-image-2"}}}},
	}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"cat","stream":true}`))
	w := httptest.NewRecorder()
	s.handleImages(w, r, "generations")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	items := s.health.Snapshot()
	if len(items) != 1 || items[0].Successes != 1 || items[0].Failures != 0 {
		t.Fatalf("image health=%#v, want one success", items)
	}
}

func TestForwardPanicReleasesLease(t *testing.T) {
	cfg := &config.Config{RequestRetry: 0, OpenAICompletions: []config.Provider{{Name: "mock", BaseURL: "https://mock.example", APIKey: "sk-a", Models: []config.ModelAlias{{Name: "m"}}}}}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	var callbackCtx context.Context
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected upstream callback panic")
			}
		}()
		s.forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "panic", "chat", "m", "m", []byte(`{"model":"m"}`), time.Now(),
			func(ctx context.Context, _ registry.UpstreamKey, _ string) (any, error) {
				callbackCtx = ctx
				panic("upstream panic")
			})
	}()
	if got := s.health.ActiveLeases(); got != 0 {
		t.Fatalf("panic path leaked %d health lease(s)", got)
	}
	if callbackCtx == nil || callbackCtx.Err() != context.Canceled {
		t.Fatalf("panic path did not cancel attempt context: %v", callbackCtx)
	}
}

func TestRoutingAdaptiveReloadAndStats(t *testing.T) {
	cfg := &config.Config{RequestRetry: 0, OpenAICompletions: []config.Provider{{Name: "mock", BaseURL: "https://mock.example", APIKey: "sk-a", Models: []config.ModelAlias{{Name: "m"}}}}}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	next := *cfg
	next.Routing = config.RoutingConfig{Strategy: "adaptive", HalfLife: "24h", Shadow: false}
	if err := s.Reload(&next); err != nil {
		t.Fatal(err)
	}
	if !s.health.AdaptiveEnabled() {
		t.Fatal("adaptive routing was not enabled after reload")
	}
	w := httptest.NewRecorder()
	s.handleRoutingStats(w, httptest.NewRequest(http.MethodGet, "/api/routing/stats", nil))
	if !strings.Contains(w.Body.String(), `"strategy":"adaptive"`) || strings.Contains(w.Body.String(), `"shadow":true`) {
		t.Fatalf("routing stats=%s", w.Body.String())
	}
}
