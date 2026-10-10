package translator_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Mieluoxxx/lite-cpa/internal/translator"
	"github.com/tidwall/gjson"
)

func TestMain(m *testing.M) {
	translator.RegisterBuiltin()
	m.Run()
}

func TestChatToClaudeRequest(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	out := translator.TranslateRequest(translator.FormatOpenAI, translator.FormatClaude, "claude-sonnet-4", in, false)
	if gjson.GetBytes(out, "model").String() != "claude-sonnet-4" {
		t.Fatalf("model: %s", out)
	}
	if !gjson.GetBytes(out, "messages").IsArray() {
		t.Fatalf("messages missing: %s", out)
	}
	if gjson.GetBytes(out, "messages.0.role").String() != "user" {
		t.Fatalf("role: %s", out)
	}
}

func TestChatToResponsesRequest(t *testing.T) {
	in := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"max_tokens":100,"temperature":0.2}`)
	out := translator.TranslateRequest(translator.FormatOpenAI, translator.FormatOpenAIResponse, "gpt-5", in, false)
	if gjson.GetBytes(out, "model").String() != "gpt-5" {
		t.Fatalf("model: %s", out)
	}
	if gjson.GetBytes(out, "instructions").String() != "sys" {
		t.Fatalf("instructions: %s", out)
	}
	if gjson.GetBytes(out, "max_output_tokens").Int() != 100 {
		t.Fatalf("max_output_tokens: %s", out)
	}
	if gjson.GetBytes(out, "temperature").Float() != 0.2 {
		t.Fatalf("temperature must be preserved (not Codex-stripped): %s", out)
	}
	if !gjson.GetBytes(out, "input").IsArray() {
		t.Fatalf("input missing: %s", out)
	}
}

func TestResponsesIdentity(t *testing.T) {
	in := []byte(`{"model":"gpt-5","input":"hello","max_output_tokens":50}`)
	out := translator.TranslateRequest(translator.FormatOpenAIResponse, translator.FormatOpenAIResponse, "gpt-5", in, true)
	if gjson.GetBytes(out, "stream").Bool() != true {
		t.Fatalf("stream: %s", out)
	}
	if gjson.GetBytes(out, "max_output_tokens").Int() != 50 {
		t.Fatalf("max_output_tokens dropped: %s", out)
	}
}

func TestResponsesToChatNonStream(t *testing.T) {
	resp := []byte(`{"id":"resp_1","object":"response","model":"gpt-5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":1},"output_tokens":2,"total_tokens":3}}`)
	out := translator.TranslateNonStream(context.Background(), translator.FormatOpenAIResponse, translator.FormatOpenAI, "gpt-5", nil, nil, resp, new(any))
	if gjson.GetBytes(out, "object").String() != "chat.completion" {
		t.Fatalf("object: %s", out)
	}
	if gjson.GetBytes(out, "choices.0.message.content").String() != "hello" {
		t.Fatalf("content: %s", out)
	}
	if got := gjson.GetBytes(out, "usage.prompt_tokens_details.cached_tokens").Int(); got != 1 {
		t.Fatalf("cached tokens: got %d want 1: %s", got, out)
	}
}

func TestResponsesToChatStreamPreservesCachedTokens(t *testing.T) {
	var param any
	chunks := translator.TranslateStream(
		context.Background(),
		translator.FormatOpenAIResponse,
		translator.FormatOpenAI,
		"gpt-5",
		nil,
		nil,
		[]byte(`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":900},"output_tokens":10,"total_tokens":1010}}}`),
		&param,
	)
	for _, chunk := range chunks {
		payload := bytes.TrimSpace(bytes.TrimPrefix(chunk, []byte("data:")))
		if usage := gjson.GetBytes(payload, "usage"); usage.Exists() {
			if got := usage.Get("prompt_tokens_details.cached_tokens").Int(); got != 900 {
				t.Fatalf("cached tokens: got %d want 900: %s", got, payload)
			}
			return
		}
	}
	t.Fatal("usage chunk missing")
}

// Official Responses streams key function_call by call_id on output_item.added,
// then send function_call_arguments.delta with item_id only (item.id / fc_*).
// Those must accumulate onto the same chat tool_call index — otherwise clients
// receive name without arguments ("tool input was not fully received").
func TestResponsesToChatStreamToolCallItemID(t *testing.T) {
	events := []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5","created_at":1}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_123","type":"function_call","call_id":"call_abc","name":"Read","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_123","output_index":0,"delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_123","output_index":0,"delta":"\"conversation.md\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_123","output_index":0,"arguments":"{\"path\":\"conversation.md\"}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
	}

	var param any
	var args strings.Builder
	var sawName, sawID, finish string
	for _, ev := range events {
		chunks := translator.TranslateStream(context.Background(), translator.FormatOpenAIResponse, translator.FormatOpenAI, "gpt-5", nil, nil, []byte(ev), &param)
		for _, chunk := range chunks {
			line := bytes.TrimSpace(chunk)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[5:])
			if bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			root := gjson.ParseBytes(payload)
			if id := root.Get("choices.0.delta.tool_calls.0.id").String(); id != "" {
				sawID = id
			}
			if name := root.Get("choices.0.delta.tool_calls.0.function.name").String(); name != "" {
				sawName = name
			}
			if a := root.Get("choices.0.delta.tool_calls.0.function.arguments").String(); a != "" {
				args.WriteString(a)
			}
			if fr := root.Get("choices.0.finish_reason").String(); fr != "" && fr != "null" {
				finish = fr
			}
			// Guard: deltas must not invent a second tool_call index.
			if idx := root.Get("choices.0.delta.tool_calls.0.index"); idx.Exists() && idx.Int() != 0 {
				t.Fatalf("unexpected tool index %d in %s", idx.Int(), payload)
			}
		}
	}

	if sawID != "call_abc" {
		t.Fatalf("tool id: got %q want call_abc", sawID)
	}
	if sawName != "Read" {
		t.Fatalf("tool name: got %q want Read", sawName)
	}
	if args.String() != `{"path":"conversation.md"}` {
		t.Fatalf("arguments: got %q", args.String())
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason: got %q want tool_calls", finish)
	}
}

// TestSamplingControlsSurviveCrossProtocolConversion pins that temperature and
// top_p reach every upstream body. The Responses sources never read them, and
// the Chat/Claude pair forwarded top_p only when temperature was absent.
func TestSamplingControlsSurviveCrossProtocolConversion(t *testing.T) {
	translator.RegisterBuiltin()
	sources := map[translator.Format]string{
		translator.FormatOpenAI:         `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.3}`,
		translator.FormatOpenAIResponse: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"temperature":0.7,"top_p":0.3}`,
		translator.FormatClaude:         `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"temperature":0.7,"top_p":0.3}`,
	}
	for from, body := range sources {
		for _, to := range []translator.Format{translator.FormatOpenAI, translator.FormatOpenAIResponse, translator.FormatClaude} {
			if from == to {
				continue
			}
			out := translator.TranslateRequest(from, to, "m", []byte(body), false)
			if got := gjson.GetBytes(out, "temperature").Float(); got != 0.7 {
				t.Errorf("%s to %s: temperature = %v, want 0.7; body %s", from, to, got, out)
			}
			if got := gjson.GetBytes(out, "top_p").Float(); got != 0.3 {
				t.Errorf("%s to %s: top_p = %v, want 0.3; body %s", from, to, got, out)
			}
		}
	}
}

// TestNullSamplingControlsAreNotForwarded guards the gjson Exists trap: an
// explicit null exists, but turning it into temperature 0 changes sampling.
func TestNullSamplingControlsAreNotForwarded(t *testing.T) {
	translator.RegisterBuiltin()
	for _, to := range []translator.Format{translator.FormatOpenAIResponse, translator.FormatClaude} {
		out := translator.TranslateRequest(translator.FormatOpenAI, to, "m",
			[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":null,"top_p":null}`), false)
		if gjson.GetBytes(out, "temperature").Exists() {
			t.Errorf("chat to %s forwarded a null temperature: %s", to, out)
		}
		if gjson.GetBytes(out, "top_p").Exists() {
			t.Errorf("chat to %s forwarded a null top_p: %s", to, out)
		}
	}
}
