package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/reqlog"
	"github.com/Mieluoxxx/lite-cpa/internal/server"
)

func TestDashboardAliases(t *testing.T) {
	port := freePort(t)
	srv := server.New(&config.Config{
		Host:         "127.0.0.1",
		Port:         port,
		APIKeys:      []string{"sk-test"},
		MaxBodyBytes: 1 << 20,
	}, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHTTP(t, baseURL+"/healthz")

	var dashboard string
	for _, path := range []string{"/dashboard", "/dashboard.html"} {
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status %d body %s", path, resp.StatusCode, body)
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s Content-Type %q", path, resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(string(body), "lite-cpa · 请求日志") {
			t.Fatalf("GET %s did not return dashboard HTML", path)
		}
		if dashboard != "" && dashboard != string(body) {
			t.Fatal("dashboard aliases returned different content")
		}
		dashboard = string(body)
	}

	assetPath := regexp.MustCompile(`href="(/dashboard/assets/[^\"]+\.css)"`).FindStringSubmatch(dashboard)
	if len(assetPath) != 2 {
		t.Fatalf("dashboard HTML does not reference a bundled stylesheet: %s", dashboard)
	}
	cssResp, err := http.Get(baseURL + assetPath[1])
	if err != nil {
		t.Fatalf("GET bundled CSS: %v", err)
	}
	cssBody, readErr := io.ReadAll(cssResp.Body)
	cssResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read bundled CSS: %v", readErr)
	}
	if cssResp.StatusCode != http.StatusOK {
		t.Fatalf("GET bundled CSS status %d body %s", cssResp.StatusCode, cssBody)
	}
	if !strings.HasPrefix(cssResp.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("GET bundled CSS Content-Type %q", cssResp.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(cssResp.Header.Get("Cache-Control"), "public, max-age=") {
		t.Fatalf("GET bundled CSS Cache-Control %q", cssResp.Header.Get("Cache-Control"))
	}
}

func TestClearLogs(t *testing.T) {
	logger, err := reqlog.Open(config.RequestLogConfig{
		Enabled: true, Backend: "sqlite", Retention: "1h",
		SQLite: config.SQLiteLogConfig{Path: filepath.Join(t.TempDir(), "requests.db")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	logger.Record(reqlog.Record{
		RequestID: "clear-me", Timestamp: time.Now().UTC(), Method: "POST",
		Path: "/v1/responses", StatusCode: http.StatusOK,
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		logs, listErr := logger.List(t.Context(), reqlog.ListFilter{Limit: 10})
		if listErr != nil {
			t.Fatal(listErr)
		}
		if logs.Total == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request log was not persisted before clear")
		}
		time.Sleep(10 * time.Millisecond)
	}

	port := freePort(t)
	srv := server.New(&config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
	}, logger)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHTTP(t, baseURL+"/healthz")

	req, err := http.NewRequest(http.MethodDelete, baseURL+"/api/logs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || result.Deleted != 1 {
		t.Fatalf("status=%d deleted=%d want 200/1", resp.StatusCode, result.Deleted)
	}

	logs, err := logger.List(t.Context(), reqlog.ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if logs.Total != 0 || len(logs.Items) != 0 {
		t.Fatalf("logs remain after clear: total=%d items=%d", logs.Total, len(logs.Items))
	}
}

func TestFilteredLogStats(t *testing.T) {
	logger, err := reqlog.Open(config.RequestLogConfig{
		Enabled: true, Backend: "sqlite", Retention: "1h",
		SQLite: config.SQLiteLogConfig{Path: filepath.Join(t.TempDir(), "requests.db")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	now := time.Now().UTC()
	for _, record := range []reqlog.Record{
		{RequestID: "gpt-relay", Timestamp: now, Method: "POST", Path: "/v1/responses", StatusCode: http.StatusTooManyRequests, Model: "gpt-x", Upstream: "laysath", InputTokens: 200, OutputTokens: 20, CachedTokens: 50, UsageComplete: true, Error: "rate limited"},
		{RequestID: "gpt-official", Timestamp: now.Add(time.Second), Method: "POST", Path: "/v1/responses", StatusCode: http.StatusOK, Model: "gpt-x", Upstream: "openai", InputTokens: 300, OutputTokens: 30, CachedTokens: 10, UsageComplete: true},
		{RequestID: "claude-relay", Timestamp: now.Add(2 * time.Second), Method: "POST", Path: "/v1/messages", StatusCode: http.StatusOK, Model: "claude-x", Upstream: "laysath", InputTokens: 100, OutputTokens: 10, CachedTokens: 5, UsageComplete: true},
	} {
		logger.Record(record)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		logs, listErr := logger.List(t.Context(), reqlog.ListFilter{Limit: 10})
		if listErr != nil {
			t.Fatal(listErr)
		}
		if logs.Total == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request logs were not persisted before stats query")
		}
		time.Sleep(10 * time.Millisecond)
	}

	port := freePort(t)
	srv := server.New(&config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
	}, logger)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHTTP(t, baseURL+"/healthz")

	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/logs/stats?model=gpt-x&upstream=laysath", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var stats reqlog.Stats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || stats.Total != 1 || stats.Errors != 1 || stats.Success != 0 {
		t.Fatalf("filtered status=%d total=%d errors=%d success=%d", resp.StatusCode, stats.Total, stats.Errors, stats.Success)
	}
	if stats.InputTokens != 200 || stats.OutputTokens != 20 || stats.CachedTokens != 50 {
		t.Fatalf("filtered tokens input=%d output=%d cached=%d", stats.InputTokens, stats.OutputTokens, stats.CachedTokens)
	}
}
