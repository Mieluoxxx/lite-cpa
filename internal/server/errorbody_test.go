package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Mieluoxxx/lite-cpa/internal/config"
	"github.com/Mieluoxxx/lite-cpa/internal/server"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
	"github.com/tidwall/gjson"
)

// TestNormalizeErrorBodyExits drives both error response exits of Server.forward:
// a non-retriable status (400) and a retriable one with the retry budget spent
// (503 with request-retry 0). Only a flat top-level error object with a string
// message may be re-wrapped under "error"; every other shape passes through
// byte-identical, and large integers must survive the wrap.
func TestNormalizeErrorBodyExits(t *testing.T) {
	translator.RegisterBuiltin()

	flat := `{"object":"error","message":"model not found","code":12345678901234567890}`
	cases := []struct {
		name     string
		status   int
		upstream string
		want     string
	}{
		{"flat-400", http.StatusBadRequest, flat, `{"error":` + flat + `}`},
		{"flat-503", http.StatusServiceUnavailable, flat, `{"error":` + flat + `}`},
		{"nested-400", http.StatusBadRequest, `{"error":{"message":"boom"}}`, `{"error":{"message":"boom"}}`},
		{"plaintext-400", http.StatusBadRequest, `Method Not Allowed`, `Method Not Allowed`},
		{"array-400", http.StatusBadRequest, `[{"message":"boom"}]`, `[{"message":"boom"}]`},
		{"nested-level-400", http.StatusBadRequest, `{"data":{"message":"boom"}}`, `{"data":{"message":"boom"}}`},
		{"invalid-json-400", http.StatusBadRequest, `{"message":"boom"} trailing`, `{"message":"boom"} trailing`},
		{"non-string-400", http.StatusBadRequest, `{"message":123}`, `{"message":123}`},
	}

	byModel := make(map[string]int, len(cases))
	models := make([]config.ModelAlias, 0, len(cases))
	for i, c := range cases {
		byModel[c.name] = i
		models = append(models, config.ModelAlias{Name: c.name, Alias: c.name})
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := cases[byModel[gjson.GetBytes(body, "model").String()]]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(c.status)
		_, _ = io.WriteString(w, c.upstream)
	}))
	t.Cleanup(up.Close)

	port := freePort(t)
	cfg := &config.Config{
		Host: "127.0.0.1", Port: port, APIKeys: []string{"sk-test"}, MaxBodyBytes: 1 << 20,
		OpenAICompletions: []config.Provider{{
			Name: "mock", BaseURL: up.URL + "/v1", APIKey: "sk-up", Models: models,
		}},
	}
	srv := server.New(cfg, nil)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHTTP(t, base+"/healthz")

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
				strings.NewReader(`{"model":"`+c.name+`","messages":[{"role":"user","content":"hi"}]}`))
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
			if resp.StatusCode != c.status {
				t.Fatalf("status %d, want %d (body %s)", resp.StatusCode, c.status, raw)
			}
			if string(raw) != c.want {
				t.Fatalf("body %q, want %q", raw, c.want)
			}
		})
	}
}
