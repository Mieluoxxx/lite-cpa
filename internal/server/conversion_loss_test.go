package server_test

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/server"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
)

// TestProxyLogsConversionLoss covers the wiring: a client request converted for
// a different upstream protocol reports on stderr what the conversion dropped,
// because the client cannot see it in the response.
func TestProxyLogsConversionLoss(t *testing.T) {
	translator.RegisterBuiltin()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(claudeSSEFixture()))
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host:         "127.0.0.1",
		Port:         port,
		APIKeys:      []string{"sk-test"},
		RequestRetry: 1,
		MaxBodyBytes: 1 << 20,
		AnthropicMessages: []config.Provider{{
			Name:    "claude-mock",
			BaseURL: up.URL,
			APIKey:  "sk-up",
			Models:  []config.ModelAlias{{Name: "claude-sonnet-4", Alias: "claude-sonnet-4"}},
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	waitHTTP(t, "http://127.0.0.1:"+strconv.Itoa(port)+"/healthz")

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	body := `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"temperature":0.9,"top_p":0.5,"response_format":{"type":"json_object"}}`
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions", strings.NewReader(body))
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
	if raw, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}

	out := logged.String()
	if !strings.Contains(out, "response_format_unsupported") {
		t.Fatalf("missing the response_format loss in %q", out)
	}
	// temperature and top_p are carried now, so this request must not report
	// them; a regression here means the sampling parameters were dropped again.
	for _, code := range []string{"top_p_unsupported", "temperature_unsupported"} {
		if strings.Contains(out, code) {
			t.Fatalf("unexpected %s in %q", code, out)
		}
	}
}
