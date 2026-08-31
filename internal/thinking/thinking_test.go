package thinking

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestMapToClaudeEffortKeepsXHighDistinct(t *testing.T) {
	got, ok := MapToClaudeEffort("xhigh", true)
	if !ok || got != "xhigh" {
		t.Fatalf("xhigh = %q ok=%v, want xhigh", got, ok)
	}
	got, ok = MapToClaudeEffort("max", true)
	if !ok || got != "max" {
		t.Fatalf("max = %q ok=%v, want max", got, ok)
	}
	got, ok = MapToClaudeEffort("max", false)
	if !ok || got != "high" {
		t.Fatalf("max without support = %q ok=%v, want high", got, ok)
	}
}

func TestApplyThinkingClaudeSuffix(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		body   string
		want   map[string]string
		absent []string
	}{
		{
			name:  "level high",
			model: "claude-opus-4-8(high)",
			body:  `{"model":"claude-opus-4-8","temperature":0}`,
			want: map[string]string{
				"thinking.type":        "adaptive",
				"output_config.effort": "high",
				"temperature":          "1",
			},
			absent: []string{"thinking.budget_tokens"},
		},
		{
			name:  "level xhigh",
			model: "claude-opus-4-8(xhigh)",
			body:  `{"model":"claude-opus-4-8"}`,
			want: map[string]string{
				"thinking.type":        "adaptive",
				"output_config.effort": "xhigh",
			},
		},
		{
			name:  "level max",
			model: "claude-opus-4-8(max)",
			body:  `{"model":"claude-opus-4-8"}`,
			want: map[string]string{
				"thinking.type":        "adaptive",
				"output_config.effort": "max",
			},
		},
		{
			name:  "auto omits effort",
			model: "claude-opus-4-8(auto)",
			body:  `{"model":"claude-opus-4-8","output_config":{"effort":"low"}}`,
			want: map[string]string{
				"thinking.type": "adaptive",
			},
			absent: []string{"output_config.effort"},
		},
		{
			name:  "none disables",
			model: "claude-opus-4-8(none)",
			body:  `{"model":"claude-opus-4-8","thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`,
			want: map[string]string{
				"thinking.type": "disabled",
			},
			absent: []string{"thinking.budget_tokens", "output_config.effort"},
		},
		{
			name:  "numeric budget",
			model: "claude-sonnet-4(2048)",
			body:  `{"model":"claude-sonnet-4"}`,
			want: map[string]string{
				"thinking.type":          "enabled",
				"thinking.budget_tokens": "2048",
			},
			absent: []string{"output_config.effort"},
		},
		{
			name:  "forced tool strips thinking",
			model: "claude-opus-4-8(high)",
			body:  `{"model":"claude-opus-4-8","tool_choice":{"type":"any"},"temperature":0.2}`,
			want: map[string]string{
				"temperature": "0.2",
			},
			absent: []string{"thinking", "output_config.effort"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := ApplyThinking([]byte(tt.body), tt.model, "claude", "claude", "claude")
			if err != nil {
				t.Fatalf("ApplyThinking: %v", err)
			}
			for path, want := range tt.want {
				got := gjson.GetBytes(out, path)
				if !got.Exists() || got.String() != want {
					t.Fatalf("%s = %q, want %q; body=%s", path, got.String(), want, out)
				}
			}
			for _, path := range tt.absent {
				if gjson.GetBytes(out, path).Exists() {
					t.Fatalf("%s should be absent; body=%s", path, out)
				}
			}
		})
	}
}

func TestApplyThinkingOpenAISuffix(t *testing.T) {
	out, err := ApplyThinking([]byte(`{"model":"gpt-5"}`), "gpt-5(high)", "openai", "openai", "openai")
	if err != nil {
		t.Fatalf("ApplyThinking: %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want high; body=%s", got, out)
	}

	out, err = ApplyThinking([]byte(`{"model":"gpt-5"}`), "gpt-5(high)", "openai", "openai-response", "openai-response")
	if err != nil {
		t.Fatalf("ApplyThinking: %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high; body=%s", got, out)
	}
}

func TestSanitizeClaudeTemperatureWithoutSuffix(t *testing.T) {
	body := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},"temperature":0}`)
	out, err := ApplyThinking(body, "claude-opus-4-8", "claude", "claude", "claude")
	if err != nil {
		t.Fatalf("ApplyThinking: %v", err)
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 1 {
		t.Fatalf("temperature = %v, want 1; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "medium" {
		t.Fatalf("effort should pass through, got %q", got)
	}
}
