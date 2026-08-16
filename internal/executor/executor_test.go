package executor

import (
	"context"
	"errors"
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
			name:      "OpenAI completions provider removes priority",
			key:       registry.UpstreamKey{Provider: "openai", Speed: "fast"},
			body:      `{"model":"gpt-5","service_tier":"flex"}`,
			wantPath:  "service_tier",
			wantValue: "",
		},
		{
			name:      "OpenAI responses GPT provider forces priority",
			key:       registry.UpstreamKey{Provider: "openai-response", Speed: "fast"},
			body:      `{"model":"gpt-5","service_tier":"auto"}`,
			wantPath:  "service_tier",
			wantValue: "priority",
		},
		{
			name:      "OpenAI responses non-GPT provider removes priority",
			key:       registry.UpstreamKey{Provider: "openai-response", Speed: "fast"},
			body:      `{"model":"claude-sonnet-5","service_tier":"auto"}`,
			wantPath:  "service_tier",
			wantValue: "",
		},
		{
			name:      "OpenAI responses GPT uppercase forces priority",
			key:       registry.UpstreamKey{Provider: "openai-response", Speed: "fast"},
			body:      `{"model":"GPT-5","service_tier":"auto"}`,
			wantPath:  "service_tier",
			wantValue: "priority",
		},
		{
			name:      "OpenAI responses GPT without fast removes client tier",
			key:       registry.UpstreamKey{Provider: "openai-response"},
			body:      `{"model":"gpt-5","service_tier":"priority"}`,
			wantPath:  "service_tier",
			wantValue: "",
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
			gotKey, gotBody := applySpeed(tt.key, gjson.Get(tt.body, "model").String(), []byte(tt.body))
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
		model     string // upstream model passed by Execute; empty = derive from body.model
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
			name:      "responses non-GPT ignores verbosity",
			to:        translator.FormatOpenAIResponse,
			verbosity: "low",
			body:      `{"model":"claude-sonnet-5","input":"hi"}`,
			want:      "",
		},
		{
			name:      "responses non-GPT strips client verbosity",
			to:        translator.FormatOpenAIResponse,
			verbosity: "low",
			body:      `{"model":"claude-sonnet-5","text":{"verbosity":"high"},"input":"hi"}`,
			want:      "",
		},
		{
			name:      "responses GPT uppercase model injects",
			to:        translator.FormatOpenAIResponse,
			verbosity: "low",
			body:      `{"model":"GPT-5","input":"hi"}`,
			want:      "low",
		},
		{
			name:      "responses uses passed upstream model not body.model",
			to:        translator.FormatOpenAIResponse,
			model:     "gpt-5",
			verbosity: "low",
			body:      `{"model":"my-alias","input":"hi"}`,
			want:      "low",
		},
		{
			name:      "responses GPT unconfigured keeps client verbosity",
			to:        translator.FormatOpenAIResponse,
			verbosity: "",
			body:      `{"model":"gpt-5","text":{"verbosity":"medium"},"input":"hi"}`,
			want:      "medium",
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
			model := tt.model
			if model == "" {
				model = gjson.Get(tt.body, "model").String()
			}
			got := applyModelVerbosity(tt.to, model, tt.verbosity, []byte(tt.body))
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

func TestEnsureResponsesTerminalConvertsStreamError(t *testing.T) {
	chunks := make(chan StreamChunk, 8)
	chunks <- StreamChunk{Payload: []byte("event: response.created\n")}
	chunks <- StreamChunk{Payload: []byte(`data: {"type":"response.created","sequence_number":4,"response":{"id":"resp_1","object":"response","model":"gpt-5","status":"in_progress","output":[]}}` + "\n")}
	chunks <- StreamChunk{Payload: []byte("\n")}
	chunks <- StreamChunk{Payload: []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":7,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"shell\",\"arguments\":\"{}\"}}\n\n")}
	chunks <- StreamChunk{Err: errors.New("unexpected EOF")}
	close(chunks)

	guarded := ensureResponsesTerminal(context.Background(), &StreamResult{Status: http.StatusOK, Chunks: chunks}, "gpt-5")
	body, logErrors, streamErrors := collectStream(t, guarded)
	if !strings.Contains(body, "response.output_item.done") {
		t.Fatalf("semantic event was not preserved: %q", body)
	}
	if got := strings.Count(body, "event: response.failed\n"); got != 1 {
		t.Fatalf("response.failed count = %d, want 1; body=%q", got, body)
	}
	payload := responseFailedPayload(t, body)
	if got := gjson.Get(payload, "sequence_number").Int(); got != 8 {
		t.Fatalf("sequence_number = %d, want 8; payload=%s", got, payload)
	}
	if got := gjson.Get(payload, "response.id").String(); got != "resp_1" {
		t.Fatalf("response.id = %q, want resp_1; payload=%s", got, payload)
	}
	if got := gjson.Get(payload, "response.status").String(); got != "failed" {
		t.Fatalf("response.status = %q, want failed; payload=%s", got, payload)
	}
	if got := gjson.Get(payload, "response.error.code").String(); got != "server_error" {
		t.Fatalf("response.error.code = %q, want server_error; payload=%s", got, payload)
	}
	if len(logErrors) != 1 || logErrors[0] != "unexpected EOF" {
		t.Fatalf("log errors = %#v, want unexpected EOF", logErrors)
	}
	if len(streamErrors) != 0 {
		t.Fatalf("stream errors = %#v, want none after response.failed", streamErrors)
	}
}

func TestEnsureResponsesTerminalConvertsCleanEOF(t *testing.T) {
	chunks := make(chan StreamChunk, 1)
	chunks <- StreamChunk{Payload: []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_clean\",\"status\":\"in_progress\"}}\n\n")}
	close(chunks)

	guarded := ensureResponsesTerminal(context.Background(), &StreamResult{Status: http.StatusOK, Chunks: chunks}, "gpt-5")
	body, logErrors, streamErrors := collectStream(t, guarded)
	if got := strings.Count(body, "event: response.failed\n"); got != 1 {
		t.Fatalf("response.failed count = %d, want 1; body=%q", got, body)
	}
	if len(logErrors) != 1 || logErrors[0] != responsesMissingTerminalError {
		t.Fatalf("log errors = %#v, want %q", logErrors, responsesMissingTerminalError)
	}
	if len(streamErrors) != 0 {
		t.Fatalf("stream errors = %#v, want none", streamErrors)
	}
}

func TestEnsureResponsesTerminalDoesNotDuplicateTerminalEvents(t *testing.T) {
	for _, terminalType := range []string{"response.completed", "response.incomplete", "response.failed"} {
		t.Run(terminalType, func(t *testing.T) {
			chunks := make(chan StreamChunk, 2)
			chunks <- StreamChunk{Payload: []byte("event: " + terminalType + "\ndata: {\"type\":\"" + terminalType + "\",\"sequence_number\":2,\"response\":{\"id\":\"resp_terminal\",\"status\":\"completed\"}}\n\n")}
			chunks <- StreamChunk{Err: errors.New("unexpected EOF")}
			close(chunks)

			guarded := ensureResponsesTerminal(context.Background(), &StreamResult{Status: http.StatusOK, Chunks: chunks}, "gpt-5")
			body, _, streamErrors := collectStream(t, guarded)
			wantFailed := 0
			if terminalType == "response.failed" {
				wantFailed = 1
			}
			if got := strings.Count(body, "event: response.failed\n"); got != wantFailed {
				t.Fatalf("response.failed count = %d, want %d; body=%q", got, wantFailed, body)
			}
			if len(streamErrors) != 1 || streamErrors[0].Error() != "unexpected EOF" {
				t.Fatalf("stream errors = %#v, want original EOF after terminal", streamErrors)
			}
		})
	}
}

func TestEnsureResponsesTerminalDoesNotDuplicateExplicitError(t *testing.T) {
	chunks := make(chan StreamChunk, 1)
	chunks <- StreamChunk{Payload: []byte("event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"busy\"}\n\n")}
	close(chunks)

	guarded := ensureResponsesTerminal(context.Background(), &StreamResult{Status: http.StatusOK, Chunks: chunks}, "gpt-5")
	body, _, _ := collectStream(t, guarded)
	if strings.Contains(body, "event: response.failed\n") {
		t.Fatalf("explicit error was followed by response.failed: %q", body)
	}
}

func TestEnsureResponsesTerminalSkipsFailureAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	chunks := make(chan StreamChunk, 1)
	chunks <- StreamChunk{Payload: []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cancelled\"}}\n\n")}
	close(chunks)

	guarded := ensureResponsesTerminal(ctx, &StreamResult{Status: http.StatusOK, Chunks: chunks}, "gpt-5")
	body, _, _ := collectStream(t, guarded)
	if strings.Contains(body, "event: response.failed\n") {
		t.Fatalf("canceled request received response.failed: %q", body)
	}
}

func collectStream(t *testing.T, result *StreamResult) (string, []string, []error) {
	t.Helper()
	var body strings.Builder
	var logErrors []string
	var streamErrors []error
	for chunk := range result.Chunks {
		body.Write(chunk.Payload)
		if chunk.LogError != "" {
			logErrors = append(logErrors, chunk.LogError)
		}
		if chunk.Err != nil {
			streamErrors = append(streamErrors, chunk.Err)
		}
	}
	return body.String(), logErrors, streamErrors
}

func responseFailedPayload(t *testing.T, body string) string {
	t.Helper()
	const marker = "event: response.failed\ndata: "
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("missing response.failed event: %q", body)
	}
	payload := body[start+len(marker):]
	if end := strings.IndexByte(payload, '\n'); end >= 0 {
		payload = payload[:end]
	}
	if !gjson.Valid(payload) {
		t.Fatalf("invalid response.failed payload: %q", payload)
	}
	return payload
}
