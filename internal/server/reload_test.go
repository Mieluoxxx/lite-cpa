package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/server"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
)

func TestReloadSwapsAPIKeysAndModels(t *testing.T) {
	translator.RegisterBuiltin()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-old"}, RequestRetry: 0, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "mock", BaseURL: up.URL, APIKey: "sk-up",
			Models: []config.ModelAlias{{Name: "m1", Alias: "m1"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	if code := postChatReload(t, port, "sk-old", "m1"); code != 200 {
		t.Fatalf("old key before reload status=%d", code)
	}
	if code := postChatReload(t, port, "sk-new", "m1"); code != 401 {
		t.Fatalf("new key before reload status=%d want 401", code)
	}

	next := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-new"}, RequestRetry: 1, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "mock", BaseURL: up.URL, APIKey: "sk-up",
			Models: []config.ModelAlias{
				{Name: "m1", Alias: "m1"},
				{Name: "m2", Alias: "m2"},
			},
		}},
	}
	if err := srv.Reload(next); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if code := postChatReload(t, port, "sk-old", "m1"); code != 401 {
		t.Fatalf("old key after reload status=%d want 401", code)
	}
	if code := postChatReload(t, port, "sk-new", "m1"); code != 200 {
		t.Fatalf("new key after reload status=%d", code)
	}
	if code := postChatReload(t, port, "sk-new", "m2"); code != 200 {
		t.Fatalf("new model after reload status=%d", code)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range parsed.Data {
		ids[m.ID] = true
	}
	if !ids["m1"] || !ids["m2"] {
		t.Fatalf("models after reload: %v", ids)
	}
}

func TestReloadRejectsHostPortChange(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "x", BaseURL: "http://127.0.0.1:9", APIKey: "k",
			Models: []config.ModelAlias{{Name: "a", Alias: "a"}},
		}},
	}
	srv := server.New(cfg, nil)
	next := *cfg
	next.Port = port + 1
	if err := srv.Reload(&next); err == nil || !strings.Contains(err.Error(), "host/port") {
		t.Fatalf("want host/port error, got %v", err)
	}
}

func TestReloadRejectsRequestLogIdentityChange(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "x", BaseURL: "http://127.0.0.1:9", APIKey: "k",
			Models: []config.ModelAlias{{Name: "a", Alias: "a"}},
		}},
		RequestLog: config.RequestLogConfig{Enabled: false, Backend: "sqlite", Retention: "168h"},
	}
	srv := server.New(cfg, nil)
	next := *cfg
	next.RequestLog.Enabled = true
	next.RequestLog.SQLite.Path = "logs/other.db"
	if err := srv.Reload(&next); err == nil || !strings.Contains(err.Error(), "request-log") {
		t.Fatalf("want request-log error, got %v", err)
	}
}

func postChatReload(t *testing.T, port int, key, model string) int {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost,
		"http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

func TestReloadKeepsOldOnNil(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "x", BaseURL: "http://127.0.0.1:9", APIKey: "k",
			Models: []config.ModelAlias{{Name: "a", Alias: "a"}},
		}},
	}
	srv := server.New(cfg, nil)
	if err := srv.Reload(nil); err == nil {
		t.Fatal("expected error for nil config")
	}
}
