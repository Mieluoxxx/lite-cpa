package translator_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Mieluoxxx/lite-cpa/internal/translator"
)

// Bodies carrying every field whose fate the aliases and the code table cover.
const (
	lossChatBody = `{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"temperature":0.9,"top_p":0.5,"n":3,"seed":7,"stop":["a"],"response_format":{"type":"json_object"},"presence_penalty":1,"frequency_penalty":1,"logit_bias":{"1":2},"logprobs":true,"user":"u","stream":false,"parallel_tool_calls":false,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"auto"}`

	lossResponsesBody = `{"model":"x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"instructions":"be terse","max_output_tokens":100,"temperature":0.5,"top_p":0.5,"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"minimal"},"text":{"format":{"type":"json_object"}},"store":false,"previous_response_id":"r1","include":["reasoning.encrypted_content"],"truncation":"auto","metadata":{"k":"v"},"stream":false}`

	lossClaudeBody = `{"model":"x","max_tokens":100,"system":"sys","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"temperature":0.5,"top_p":0.5,"top_k":5,"stop_sequences":["a"],"metadata":{"user_id":"u"},"thinking":{"type":"enabled","budget_tokens":2000},"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},"stream":false}`
)

// TestInspectRequestLossMatrix pins what each cross-protocol conversion drops.
// A field listed here is one the upstream request really does go without; a
// field missing from a row is one the conversion carries, possibly under
// another name.
func TestInspectRequestLossMatrix(t *testing.T) {
	translator.RegisterBuiltin()
	cases := []struct {
		name string
		from translator.Format
		to   translator.Format
		body string
		want []string
	}{
		{
			name: "chat to responses",
			from: translator.FormatOpenAI, to: translator.FormatOpenAIResponse, body: lossChatBody,
			want: []string{
				"choice_count_unsupported n",
				"field_unsupported user",
				"logit_bias_unsupported logit_bias",
				"logprobs_unsupported logprobs",
				"penalty_unsupported frequency_penalty",
				"penalty_unsupported presence_penalty",
				"response_format_unsupported response_format",
				"seed_unsupported seed",
				"stop_unsupported stop",
			},
		},
		{
			name: "chat to claude",
			from: translator.FormatOpenAI, to: translator.FormatClaude, body: lossChatBody,
			want: []string{
				"choice_count_unsupported n",
				"field_unsupported user",
				"logit_bias_unsupported logit_bias",
				"logprobs_unsupported logprobs",
				"parallel_tool_calls_unsupported parallel_tool_calls",
				"penalty_unsupported frequency_penalty",
				"penalty_unsupported presence_penalty",
				"response_format_unsupported response_format",
				"seed_unsupported seed",
			},
		},
		{
			name: "responses to chat",
			from: translator.FormatOpenAIResponse, to: translator.FormatOpenAI, body: lossResponsesBody,
			want: []string{
				"continuation_unsupported previous_response_id",
				"field_unsupported include",
				"field_unsupported metadata",
				"field_unsupported store",
				"field_unsupported truncation",
				"response_format_unsupported text",
			},
		},
		{
			name: "responses to claude",
			from: translator.FormatOpenAIResponse, to: translator.FormatClaude, body: lossResponsesBody,
			want: []string{
				"continuation_unsupported previous_response_id",
				"field_unsupported include",
				"field_unsupported store",
				"field_unsupported truncation",
				"parallel_tool_calls_unsupported parallel_tool_calls",
				"response_format_unsupported text",
			},
		},
		{
			name: "claude to chat",
			from: translator.FormatClaude, to: translator.FormatOpenAI, body: lossClaudeBody,
			want: []string{
				"field_unsupported metadata",
				"top_k_unsupported top_k",
			},
		},
		{
			name: "claude to responses",
			from: translator.FormatClaude, to: translator.FormatOpenAIResponse, body: lossClaudeBody,
			want: []string{
				"field_unsupported metadata",
				"stop_unsupported stop_sequences",
				"top_k_unsupported top_k",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := []byte(tc.body)
			target := translator.TranslateRequest(tc.from, tc.to, "m", source, false)
			got := describe(translator.InspectRequestLoss(tc.from, tc.to, source, target))
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("losses\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

// TestInspectRequestLossSkipsSameFormat guards the passthrough path: a request
// whose source and target format match is not converted and cannot lose.
func TestInspectRequestLossSkipsSameFormat(t *testing.T) {
	source := []byte(lossChatBody)
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatOpenAIResponse, translator.FormatClaude} {
		if diags := translator.InspectRequestLoss(format, format, source, []byte(`{"model":"x"}`)); len(diags) != 0 {
			t.Fatalf("%s: expected no losses, got %v", format, describe(diags))
		}
	}
}

// TestInspectRequestLossSkipsEmptyValues covers fields a client sent as null or
// empty, which a target omitting them has not lost.
func TestInspectRequestLossSkipsEmptyValues(t *testing.T) {
	source := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"n":null,"stop":[],"response_format":{}}`)
	diags := translator.InspectRequestLoss(translator.FormatOpenAI, translator.FormatClaude, source, []byte(`{"model":"x","messages":[]}`))
	if len(diags) != 0 {
		t.Fatalf("expected no losses, got %v", describe(diags))
	}
}

// TestLossRecorderDedupes pins the retry path: the same request converted twice
// against the same target must not log twice.
func TestLossRecorderDedupes(t *testing.T) {
	rec := translator.NewLossRecorder()
	ctx := translator.WithLossRecorder(context.Background(), rec)
	source := []byte(lossChatBody)
	target := translator.TranslateRequest(translator.FormatOpenAI, translator.FormatClaude, "m", source, false)
	translator.RecordLosses(ctx, translator.FormatOpenAI, translator.FormatClaude, source, target)
	once := rec.Diagnostics()
	if len(once) == 0 {
		t.Fatal("expected the first conversion to record losses")
	}
	for range 3 {
		translator.RecordLosses(ctx, translator.FormatOpenAI, translator.FormatClaude, source, target)
	}
	if got := rec.Diagnostics(); !slices.Equal(got, once) {
		t.Fatalf("recorded %v, want %v", describe(got), describe(once))
	}
	if strings.Contains(rec.Summary(), "truncated=true") {
		t.Fatalf("recorder truncated below its cap: %q", rec.Summary())
	}
}

// TestLossRecorderCaps proves a request cannot grow loss state without bound.
func TestLossRecorderCaps(t *testing.T) {
	rec := translator.NewLossRecorder()
	ctx := translator.WithLossRecorder(context.Background(), rec)
	var body strings.Builder
	body.WriteString(`{"model":"x","messages":[{"role":"user","content":"hi"}]`)
	for i := range 100 {
		fmt.Fprintf(&body, `,"unknown_field_%d":"v"`, i)
	}
	body.WriteString("}")
	translator.RecordLosses(ctx, translator.FormatOpenAI, translator.FormatClaude, []byte(body.String()), []byte(`{"model":"x","messages":[]}`))
	if got := len(rec.Diagnostics()); got != 32 {
		t.Fatalf("kept %d diagnostics, want 32", got)
	}
	if !strings.Contains(rec.Summary(), "truncated=true") {
		t.Fatalf("summary missing truncation: %q", rec.Summary())
	}
}

// TestRecordLossesWithoutRecorder keeps the executor independent of the caller:
// no recorder armed means no work and no panic.
func TestRecordLossesWithoutRecorder(t *testing.T) {
	translator.RecordLosses(context.Background(), translator.FormatOpenAI, translator.FormatClaude, []byte(lossChatBody), []byte(`{}`))
	translator.RecordLosses(nil, translator.FormatOpenAI, translator.FormatClaude, []byte(lossChatBody), []byte(`{}`))
}

func describe(diags []translator.Diag) []string {
	out := make([]string, 0, len(diags))
	for _, diag := range diags {
		out = append(out, diag.Code+" "+diag.Path)
	}
	slices.Sort(out)
	return out
}
