package server

import "testing"

func TestTokenUsageFromResponse(t *testing.T) {
	tests := []struct {
		name     string
		payload  []byte
		input    int64
		output   int64
		cached   int64
		complete bool
	}{
		{
			name:     "openai non-stream response",
			payload:  []byte(`{"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":40}}}`),
			input:    120,
			output:   30,
			cached:   40,
			complete: true,
		},
		{
			name:     "anthropic stream usage across events",
			payload:  []byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":20,\"cache_read_input_tokens\":30}}}\n\nevent: message_delta\ndata: {\"usage\":{\"output_tokens\":40}}\n\n"),
			input:    150,
			output:   40,
			cached:   30,
			complete: true,
		},
		{
			name:     "responses completed event",
			payload:  []byte("event: response.completed\ndata: {\"response\":{\"usage\":{\"input_tokens\":70,\"output_tokens\":25,\"input_tokens_details\":{\"cached_tokens\":12}}}}\n\n"),
			input:    70,
			output:   25,
			cached:   12,
			complete: true,
		},
		{
			name:    "terminal event without usage",
			payload: []byte("data: [DONE]\n\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := usageFromResponse(tt.payload)
			if got.inputTokens != tt.input || got.outputTokens != tt.output || got.cachedTokens != tt.cached {
				t.Fatalf("usage = (%d, %d, %d), want (%d, %d, %d)", got.inputTokens, got.outputTokens, got.cachedTokens, tt.input, tt.output, tt.cached)
			}
			if got.complete() != tt.complete {
				t.Fatalf("usage complete = %t, want %t", got.complete(), tt.complete)
			}
		})
	}
}

func TestTokenUsageMergePayload(t *testing.T) {
	var usage tokenUsage
	usage.mergePayload([]byte("data: {\"message\":{\"usage\":{\"input_tokens\":64}}}\n\n"))
	usage.mergePayload([]byte("data: {\"usage\":{\"output_tokens\":16}}\n\n"))

	if !usage.inputSeen || !usage.outputSeen {
		t.Fatalf("usage visibility input=%t output=%t, want both true", usage.inputSeen, usage.outputSeen)
	}
	if !usage.complete() {
		t.Fatal("merged usage should be complete")
	}
	if usage.inputTokens != 64 || usage.outputTokens != 16 {
		t.Fatalf("usage = (%d, %d), want (64, 16)", usage.inputTokens, usage.outputTokens)
	}
}
