package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/executor"
	"github.com/Mieluoxxx/lite-cpa/internal/pool"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
)

func TestForwardDoesNotBindAffinityBeforeStreamCompletes(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		RequestRetry: 1,
		ChannelAffinity: config.ChannelAffinitySetting{
			Enabled:           &enabled,
			DefaultTTLSeconds: 60,
			Rules: []config.ChannelAffinityRule{{
				Name:       "session",
				KeySources: []config.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}},
			}},
		},
		OpenAICompletions: []config.Provider{{
			Name:    "mock",
			BaseURL: "https://mock.example",
			APIKeyEntries: []config.APIKeyEntry{
				{APIKey: "sk-a"},
				{APIKey: "sk-b"},
			},
			Models: []config.ModelAlias{{Name: "m"}},
		}},
	}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Advance the cursor so the failed stream uses the second key. Resetting the
	// cursor below makes a premature affinity write observable.
	if _, _, err := s.selector.Pick("m", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"m","metadata":{"user_id":"session-1"},"messages":[{"role":"user","content":"hi"}]}`)
	var selected []string
	serve := func(key registry.UpstreamKey) (any, error) {
		selected = append(selected, key.ID)
		chunks := make(chan executor.StreamChunk, 1)
		chunks <- executor.StreamChunk{Payload: []byte("data: partial\n\n")}
		close(chunks)
		complete := make(chan executor.StreamCompletion, 1)
		complete <- executor.StreamIncomplete
		close(complete)
		return &executor.StreamResult{Status: http.StatusOK, Chunks: chunks, Complete: complete}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(context.Background())
	first := httptest.NewRecorder()
	s.forward(first, req, "attempt-1", "chat", "m", "m", body, time.Now(),
		func(_ context.Context, key registry.UpstreamKey, _ string) (any, error) { return serve(key) })

	if len(selected) != 1 || selected[0] != "mock-1" {
		t.Fatalf("first selected=%v, want [mock-1]", selected)
	}
	if items := s.health.Snapshot(); len(items) != 1 || items[0].Failures != 0 || items[0].LastOutcome != pool.OutcomeRequestError {
		t.Fatalf("incomplete stream health=%#v, want non-health request error", items)
	}

	// Remove the health cooldown and reset round-robin. With no successful stream,
	// affinity must be empty, so the next normal pick starts at mock-0.
	s.health.Reset()
	s.selector.ResetRoundRobin()
	second := httptest.NewRecorder()
	s.forward(second, req, "attempt-2", "chat", "m", "m", body, time.Now(),
		func(_ context.Context, key registry.UpstreamKey, _ string) (any, error) { return serve(key) })

	if len(selected) != 2 || selected[1] != "mock-0" {
		t.Fatalf("after incomplete stream selected=%v, want second mock-0", selected)
	}
}

func TestForwardStreamWriteFailureDoesNotWaitForCompletion(t *testing.T) {
	cfg := &config.Config{
		RequestRetry: 0,
		OpenAICompletions: []config.Provider{{
			Name:    "mock",
			BaseURL: "https://mock.example",
			APIKey:  "sk-a",
			Models:  []config.ModelAlias{{Name: "m"}},
		}},
	}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	chunks := make(chan executor.StreamChunk, 1)
	producerDone := make(chan struct{})
	producerCtxDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for range 100 {
			select {
			case chunks <- executor.StreamChunk{Payload: []byte("data: partial\n\n")}:
			case <-producerCtxDone:
				return
			}
		}
		close(chunks)
	}()
	// Deliberately never signal completion: a downstream write failure must
	// cancel the attempt rather than wait on this channel forever.
	completion := make(chan executor.StreamCompletion)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := &streamWriteFailureWriter{cancellationWriter: cancellationWriter{writeErr: errors.New("client gone")}}
	done := make(chan struct{})
	go func() {
		s.forward(w, r, "write-fail", "chat", "m", "m", []byte(`{"model":"m"}`), time.Now(),
			func(ctx context.Context, _ registry.UpstreamKey, _ string) (any, error) {
				go func() {
					<-ctx.Done()
					close(producerCtxDone)
				}()
				return &executor.StreamResult{Status: http.StatusOK, Chunks: chunks, Complete: completion}, nil
			})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward waited for an incomplete stream after client write failure")
	}
	if w.writes == 0 {
		t.Fatal("stream test did not reach downstream Write")
	}
	items := s.health.Snapshot()
	if len(items) != 1 || items[0].Canceled != 1 || items[0].Failures != 0 {
		t.Fatalf("write failure health=%#v, want canceled=1 failures=0", items)
	}
	select {
	case <-producerDone:
	case <-time.After(time.Second):
		t.Fatal("producer did not observe attempt cancellation")
	}
}

func TestForwardHealthUsesResolvedModelKey(t *testing.T) {
	cfg := &config.Config{
		RequestRetry: 0,
		OpenAICompletions: []config.Provider{{
			Name:    "mock",
			BaseURL: "https://mock.example",
			APIKey:  "sk-a",
			Models:  []config.ModelAlias{{Name: "m"}},
		}},
	}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	s.forward(w, r, "suffix-health", "chat", "m(custom)", "m", []byte(`{"model":"m(custom)"}`), time.Now(),
		func(context.Context, registry.UpstreamKey, string) (any, error) {
			return nil, executor.StatusError{Code: http.StatusInternalServerError, Body: "boom"}
		})
	items := s.health.Snapshot()
	if len(items) != 1 || items[0].Model != "m" {
		t.Fatalf("health model=%#v, want resolved model m", items)
	}
}

func TestAdaptiveAffinityStillPrefersThenRespectsCooldown(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		RequestRetry: 1,
		Routing:      config.RoutingConfig{Strategy: "adaptive", HalfLife: "72h"},
		ChannelAffinity: config.ChannelAffinitySetting{
			Enabled: &enabled,
			Rules:   []config.ChannelAffinityRule{{Name: "session", KeySources: []config.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}}}},
		},
		OpenAICompletions: []config.Provider{{Name: "mock", BaseURL: "https://mock.example", APIKeyEntries: []config.APIKeyEntry{{APIKey: "sk-a"}, {APIKey: "sk-b"}}, Models: []config.ModelAlias{{Name: "m"}}}},
	}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	body := []byte(`{"model":"m","metadata":{"user_id":"adaptive-session"}}`)
	selected := make([]string, 0, 3)
	call := func(failSticky bool) {
		s.forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "adaptive-affinity", "chat", "m", "m", body, time.Now(),
			func(_ context.Context, key registry.UpstreamKey, _ string) (any, error) {
				selected = append(selected, key.ID)
				if failSticky && len(selected) == 2 && key.ID == selected[0] {
					return nil, executor.StatusError{Code: http.StatusTooManyRequests, Body: "busy"}
				}
				return &executor.Result{Status: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
			})
	}
	call(false)
	first := selected[0]
	call(true)
	if len(selected) != 3 || selected[1] != first || selected[2] == first {
		t.Fatalf("adaptive affinity sequence=%v, want sticky then cooled fallback", selected)
	}
}

func TestForwardSilentStreamStopsOnClientCancel(t *testing.T) {
	cfg := &config.Config{
		RequestRetry:      0,
		OpenAICompletions: []config.Provider{{Name: "mock", BaseURL: "https://mock.example", APIKey: "sk-a", Models: []config.ModelAlias{{Name: "m"}}}},
	}
	s := New(cfg, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	chunks := make(chan executor.StreamChunk)
	completion := make(chan executor.StreamCompletion)
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.forward(w, r, "silent-cancel", "chat", "m", "m", []byte(`{"model":"m"}`), time.Now(),
			func(context.Context, registry.UpstreamKey, string) (any, error) {
				close(started)
				return &executor.StreamResult{Status: http.StatusOK, Chunks: chunks, Complete: completion}, nil
			})
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("silent stream did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("silent stream did not stop after client cancellation")
	}
}

func TestReloadBlocksOldAffinityWriteback(t *testing.T) {
	enabled := true
	affinity := config.ChannelAffinitySetting{
		Enabled: &enabled,
		Rules: []config.ChannelAffinityRule{{
			Name:       "session",
			KeySources: []config.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}},
		}},
	}
	base := &config.Config{
		RequestRetry:    0,
		ChannelAffinity: affinity,
		OpenAICompletions: []config.Provider{{
			Name: "old", BaseURL: "https://old.example", APIKey: "sk-old", Models: []config.ModelAlias{{Name: "m"}},
		}},
	}
	s := New(base, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	body := []byte(`{"model":"m","metadata":{"user_id":"reload-session"}}`)
	s.forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "warm-reload", "chat", "m", "m", body, time.Now(),
		func(context.Context, registry.UpstreamKey, string) (any, error) {
			return &executor.Result{Status: http.StatusOK, Body: []byte(`{"warm":true}`)}, nil
		})
	if old := s.affinity.Lookup("m", "/v1/chat/completions", nil, body); !old.Found || old.KeyID != "old-0" {
		t.Fatalf("old request did not start from old pin: %#v", old)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "old-request", "chat", "m", "m", body, time.Now(),
			func(context.Context, registry.UpstreamKey, string) (any, error) {
				close(started)
				<-release
				return &executor.Result{Status: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
			})
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old request did not start")
	}
	next := &config.Config{
		RequestRetry:    0,
		ChannelAffinity: affinity,
		OpenAICompletions: []config.Provider{{
			Name: "new", BaseURL: "https://new.example", APIKey: "sk-new", Models: []config.ModelAlias{{Name: "m"}},
		}},
	}
	if err := s.Reload(next); err != nil {
		t.Fatal(err)
	}
	s.forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "new-request", "chat", "m", "m", body, time.Now(),
		func(context.Context, registry.UpstreamKey, string) (any, error) {
			return &executor.Result{Status: http.StatusOK, Body: []byte(`{"new":true}`)}, nil
		})
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old request did not finish")
	}
	match := s.affinity.Lookup("m", "/v1/chat/completions", nil, body)
	if !match.Found || match.KeyID != "new-0" {
		t.Fatalf("reload affinity=%#v, want new-0", match)
	}
	items := s.health.Snapshot()
	if len(items) != 1 || items[0].Successes != 1 || items[0].Failures != 0 {
		t.Fatalf("old success changed new health: %#v", items)
	}
}

func TestReloadBlocksOldAffinityClear(t *testing.T) {
	enabled := true
	affinity := config.ChannelAffinitySetting{Enabled: &enabled, Rules: []config.ChannelAffinityRule{{Name: "session", KeySources: []config.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}}}}}
	base := &config.Config{RequestRetry: 0, ChannelAffinity: affinity, OpenAICompletions: []config.Provider{{Name: "old", BaseURL: "https://old.example", APIKey: "sk-old", Models: []config.ModelAlias{{Name: "m"}}}}}
	s := New(base, nil)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	body := []byte(`{"model":"m","metadata":{"user_id":"reload-failure"}}`)
	s.forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "warm-old", "chat", "m", "m", body, time.Now(),
		func(context.Context, registry.UpstreamKey, string) (any, error) {
			return &executor.Result{Status: http.StatusOK, Body: []byte(`{"old":true}`)}, nil
		})
	if old := s.affinity.Lookup("m", "/v1/chat/completions", nil, body); !old.Found || old.KeyID != "old-0" {
		t.Fatalf("old failure request did not start from old pin: %#v", old)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		s.forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "old-failure", "chat", "m", "m", body, time.Now(),
			func(context.Context, registry.UpstreamKey, string) (any, error) {
				close(started)
				<-release
				return nil, executor.StatusError{Code: http.StatusInternalServerError, Body: "old failure"}
			})
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old failure request did not start")
	}
	next := &config.Config{RequestRetry: 0, ChannelAffinity: affinity, OpenAICompletions: []config.Provider{{Name: "new", BaseURL: "https://new.example", APIKey: "sk-new", Models: []config.ModelAlias{{Name: "m"}}}}}
	if err := s.Reload(next); err != nil {
		t.Fatal(err)
	}
	s.forward(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "new-failure", "chat", "m", "m", body, time.Now(),
		func(context.Context, registry.UpstreamKey, string) (any, error) {
			return &executor.Result{Status: http.StatusOK, Body: []byte(`{"new":true}`)}, nil
		})
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old failure request did not finish")
	}
	match := s.affinity.Lookup("m", "/v1/chat/completions", nil, body)
	if !match.Found || match.KeyID != "new-0" {
		t.Fatalf("old failure cleared new affinity: %#v", match)
	}
	items := s.health.Snapshot()
	if len(items) != 1 || items[0].Successes != 1 || items[0].Failures != 0 {
		t.Fatalf("old failure changed new health: %#v", items)
	}
}

type streamWriteFailureWriter struct {
	cancellationWriter
	writes int
}

func (w *streamWriteFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.cancellationWriter.Write(p)
}

func (w *streamWriteFailureWriter) Flush() {}
