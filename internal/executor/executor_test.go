package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mieluoxxx/lite-cpa/internal/registry"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
	"github.com/tidwall/gjson"
)

func TestApplySpeed(t *testing.T) {
	tests := []struct {
		name       string
		key        registry.UpstreamKey
		body       string
		wantPath   string
		wantValue  string
		wantHeader string
	}{
		{
			name: "Claude provider forces fast and merges beta",
			key: registry.UpstreamKey{
				Provider: "claude",
				Speed:    "fast",
				Headers: map[string]string{
					"anthropic-beta": "oauth-2025-04-20,fast-mode-2026-02-01",
				},
			},
			body:       `{"speed":"standard"}`,
			wantPath:   "speed",
			wantValue:  "fast",
			wantHeader: "oauth-2025-04-20,fast-mode-2026-02-01",
		},
		{
			name:      "Claude provider without fast removes client speed",
			key:       registry.UpstreamKey{Provider: "claude"},
			body:      `{"speed":"fast"}`,
			wantPath:  "speed",
			wantValue: "",
		},
		{
			name:      "OpenAI completions provider forces priority",
			key:       registry.UpstreamKey{Provider: "openai", Speed: "fast"},
			body:      `{"service_tier":"flex"}`,
			wantPath:  "service_tier",
			wantValue: "priority",
		},
		{
			name:      "OpenAI responses provider forces priority",
			key:       registry.UpstreamKey{Provider: "openai-response", Speed: "fast"},
			body:      `{"service_tier":"auto"}`,
			wantPath:  "service_tier",
			wantValue: "priority",
		},
		{
			name:      "OpenAI provider without fast removes client tier",
			key:       registry.UpstreamKey{Provider: "openai"},
			body:      `{"service_tier":"priority"}`,
			wantPath:  "service_tier",
			wantValue: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKey, gotBody := applySpeed(tt.key, []byte(tt.body))
			value := gjson.GetBytes(gotBody, tt.wantPath)
			if tt.wantValue == "" {
				if value.Exists() {
					t.Fatalf("%s = %q, want absent; body=%s", tt.wantPath, value.String(), gotBody)
				}
			} else if value.String() != tt.wantValue {
				t.Fatalf("%s = %q, want %q; body=%s", tt.wantPath, value.String(), tt.wantValue, gotBody)
			}
			if tt.wantHeader != "" && gotKey.Headers["Anthropic-Beta"] != tt.wantHeader {
				t.Fatalf("Anthropic-Beta = %q, want %q", gotKey.Headers["Anthropic-Beta"], tt.wantHeader)
			}
		})
	}
}

func TestSemanticSSEError(t *testing.T) {
	tests := []struct {
		name  string
		event [][]byte
		want  string
	}{
		{
			name: "error event with overload code",
			event: [][]byte{
				[]byte("event: error"),
				[]byte(`data: {"error":{"code":"server_is_overloaded","message":"try again"}}`),
			},
			want: "server_is_overloaded",
		},
		{
			name: "failed response event",
			event: [][]byte{
				[]byte("event: response.failed"),
				[]byte(`data: {"type":"response.failed","response":{"error":{"code":"overloaded_error","message":"busy"}}}`),
			},
			want: "overloaded_error",
		},
		{
			name: "normal response event",
			event: [][]byte{
				[]byte("event: response.created"),
				[]byte(`data: {"type":"response.created","response":{"status":"in_progress"}}`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := semanticSSEError(tt.event)
			if tt.want == "" {
				if got != "" {
					t.Fatalf("semanticSSEError() = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("semanticSSEError() = %q, want containing %q", got, tt.want)
			}
		})
	}
}

func TestApplyModelVerbosity(t *testing.T) {
	tests := []struct {
		name      string
		to        translator.Format
		verbosity string
		body      string
		want      string // "" = absent
	}{
		{
			name:      "responses upstream injects low",
			to:        translator.FormatOpenAIResponse,
			verbosity: "low",
			body:      `{"model":"gpt-5.6-luna","input":"hi"}`,
			want:      "low",
		},
		{
			name:      "responses upstream injects high",
			to:        translator.FormatOpenAIResponse,
			verbosity: "high",
			body:      `{"model":"gpt-5.6-sol","input":"hi"}`,
			want:      "high",
		},
		{
			name:      "client text.verbosity wins",
			to:        translator.FormatOpenAIResponse,
			verbosity: "low",
			body:      `{"model":"gpt-5.6-luna","text":{"verbosity":"high"},"input":"hi"}`,
			want:      "high",
		},
		{
			name:      "empty verbosity leaves body unchanged",
			to:        translator.FormatOpenAIResponse,
			verbosity: "",
			body:      `{"model":"gpt-5.6-luna","input":"hi"}`,
			want:      "",
		},
		{
			name:      "chat completions upstream ignores verbosity",
			to:        translator.FormatOpenAI,
			verbosity: "low",
			body:      `{"model":"deepseek-v4-flash","messages":[]}`,
			want:      "",
		},
		{
			name:      "claude upstream ignores verbosity",
			to:        translator.FormatClaude,
			verbosity: "low",
			body:      `{"model":"claude-sonnet-4","messages":[]}`,
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyModelVerbosity(tt.to, tt.verbosity, []byte(tt.body))
			value := gjson.GetBytes(got, "text.verbosity")
			if tt.want == "" {
				if value.Exists() {
					t.Fatalf("text.verbosity = %q, want absent; body=%s", value.String(), got)
				}
			} else if value.String() != tt.want {
				t.Fatalf("text.verbosity = %q, want %q; body=%s", value.String(), tt.want, got)
			}
		})
	}
}

func TestExecuteResponsesInjectsModelVerbosity(t *testing.T) {
	baseKey := registry.UpstreamKey{
		Provider:  "openai-response",
		APIKey:    "sk-test",
		Verbosity: "low",
	}

	run := func(t *testing.T, key registry.UpstreamKey, payload []byte, stream bool) []byte {
		var gotBody []byte
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/responses" {
				t.Errorf("upstream path = %q, want /responses", r.URL.Path)
			}
			gotBody, _ = io.ReadAll(r.Body)
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
			} else {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","model":"gpt-5.6-luna","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`))
			}
		}))
		defer srv.Close()
		key.BaseURL = srv.URL

		res, err := Execute(context.Background(), key, "gpt-5.6-luna", translator.FormatOpenAIResponse, payload, stream)
		if err != nil {
			t.Fatalf("Execute(stream=%v): %v", stream, err)
		}
		if stream {
			for ch := range res.(*StreamResult).Chunks {
				if ch.Err != nil {
					t.Fatalf("stream chunk error: %v", ch.Err)
				}
			}
		}
		return gotBody
	}

	// Configured low -> text.verbosity: low, non-stream.
	body := run(t, baseKey, []byte(`{"model":"gpt-5.6-luna","input":"hi"}`), false)
	if got := gjson.GetBytes(body, "text.verbosity").String(); got != "low" {
		t.Fatalf("non-stream verbosity = %q, want low; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "stream").Bool(); got {
		t.Fatalf("non-stream should carry stream=false; body=%s", body)
	}

	// Configured low -> text.verbosity: low, streaming.
	body = run(t, baseKey, []byte(`{"model":"gpt-5.6-luna","input":"hi"}`), true)
	if got := gjson.GetBytes(body, "text.verbosity").String(); got != "low" {
		t.Fatalf("stream verbosity = %q, want low; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "stream").Bool(); !got {
		t.Fatalf("stream should carry stream=true; body=%s", body)
	}

	// Explicit client text.verbosity always wins.
	body = run(t, baseKey, []byte(`{"model":"gpt-5.6-luna","text":{"verbosity":"high"},"input":"hi"}`), false)
	if got := gjson.GetBytes(body, "text.verbosity").String(); got != "high" {
		t.Fatalf("client verbosity = %q, want high preserved; body=%s", got, body)
	}

	// Unconfigured model stays untouched.
	noVerb := baseKey
	noVerb.Verbosity = ""
	body = run(t, noVerb, []byte(`{"model":"grok-4.5","input":"hi"}`), false)
	if got := gjson.GetBytes(body, "text.verbosity"); got.Exists() {
		t.Fatalf("unconfigured model must not get verbosity; body=%s", body)
	}
}
