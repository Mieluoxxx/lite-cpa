package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestChannelAffinityYAMLForms(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		enabled bool
		models  []string
	}{
		{
			name:    "omitted defaults enabled",
			yaml:    "port: 1\napi-keys: [k]\nopenai-completions: [{name: x, base-url: http://x, api-key: a, models: [{name: m}]}]\n",
			enabled: true,
		},
		{
			name:    "bool true",
			yaml:    "port: 1\napi-keys: [k]\nchannel-affinity: true\nopenai-completions: [{name: x, base-url: http://x, api-key: a, models: [{name: m}]}]\n",
			enabled: true,
		},
		{
			name:    "bool false",
			yaml:    "port: 1\napi-keys: [k]\nchannel-affinity: false\nopenai-completions: [{name: x, base-url: http://x, api-key: a, models: [{name: m}]}]\n",
			enabled: false,
		},
		{
			name:    "family list",
			yaml:    "port: 1\napi-keys: [k]\nchannel-affinity: [claude, grok]\nopenai-completions: [{name: x, base-url: http://x, api-key: a, models: [{name: m}]}]\n",
			enabled: true,
			models:  []string{"claude", "grok"},
		},
		{
			name:    "nested list sugar",
			yaml:    "port: 1\napi-keys: [k]\nchannel-affinity:\n  - [claude, gpt, grok]\nopenai-completions: [{name: x, base-url: http://x, api-key: a, models: [{name: m}]}]\n",
			enabled: true,
			models:  []string{"claude", "gpt", "grok"},
		},
		{
			name:    "mapping models",
			yaml:    "port: 1\napi-keys: [k]\nchannel-affinity:\n  models: [kimi, qwen]\n  default-ttl-seconds: 120\nopenai-completions: [{name: x, base-url: http://x, api-key: a, models: [{name: m}]}]\n",
			enabled: true,
			models:  []string{"kimi", "qwen"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			cfg.applyDefaults()
			if got := cfg.ChannelAffinity.EnabledOrDefault(); got != tc.enabled {
				t.Fatalf("enabled=%v want %v", got, tc.enabled)
			}
			if tc.enabled {
				if len(cfg.ChannelAffinity.Rules) == 0 {
					t.Fatal("expected expanded rules")
				}
			} else if len(cfg.ChannelAffinity.Rules) != 0 {
				t.Fatalf("disabled should not expand rules, got %d", len(cfg.ChannelAffinity.Rules))
			}
			if tc.models != nil {
				if strings.Join(cfg.ChannelAffinity.Models, ",") != strings.Join(tc.models, ",") {
					t.Fatalf("models=%v want %v", cfg.ChannelAffinity.Models, tc.models)
				}
			}
		})
	}
}

func TestExpandAffinityModelsCoversGrok(t *testing.T) {
	rules := ExpandAffinityModels([]string{"grok", "claude"})
	if len(rules) != 2 {
		t.Fatalf("rules=%d", len(rules))
	}
	if rules[0].Name != "grok sticky" || rules[0].SkipRetryOnFailure {
		t.Fatalf("grok rule %+v", rules[0])
	}
	if rules[1].Name != "claude sticky" || rules[1].SkipRetryOnFailure {
		t.Fatalf("claude rule %+v", rules[1])
	}
}

func TestProviderFailoverModeNormalize(t *testing.T) {
	var cfg Config
	raw := `
port: 1
api-keys: [k]
openai-completions:
  - name: relay
    base-url: http://x
    api-key: a
    failover-mode: site
    models: [{name: m}]
  - name: official
    base-url: http://y
    api-key: b
    models: [{name: m}]
`
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()
	if cfg.OpenAICompletions[0].FailoverMode != "provider" {
		t.Fatalf("site alias -> %q", cfg.OpenAICompletions[0].FailoverMode)
	}
	if cfg.OpenAICompletions[1].FailoverMode != "key" {
		t.Fatalf("default -> %q", cfg.OpenAICompletions[1].FailoverMode)
	}
}

func TestModelSpeedNormalizeAndValidate(t *testing.T) {
	var cfg Config
	raw := `
port: 1
api-keys: [k]
openai-completions:
  - name: official
    base-url: http://x
    api-key: a
    models:
      - name: m
        speed: " FAST "
      - name: m2
`
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()
	if got := cfg.OpenAICompletions[0].Models[0].ResolvedSpeed(); got != "fast" {
		t.Fatalf("speed = %q, want fast", got)
	}
	if got := cfg.OpenAICompletions[0].Models[1].ResolvedSpeed(); got != "" {
		t.Fatalf("unset speed = %q, want empty", got)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate fast model speed: %v", err)
	}

	cfg.OpenAICompletions[0].Models[0].Speed = "turbo"
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "speed must be fast") {
		t.Fatalf("validate invalid model speed = %v, want speed validation error", err)
	}
}

func TestProviderSpeedMigrationError(t *testing.T) {
	var cfg Config
	raw := `
port: 1
api-keys: [k]
openai-completions:
  - name: legacy
    speed: fast
    base-url: http://x
    api-key: a
    models: [{name: m}]
`
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("validate provider-level speed = %v, want migration error", err)
	}
}

func TestModelVerbosityNormalizeAndValidate(t *testing.T) {
	var cfg Config
	raw := `
port: 1
api-keys: [k]
openai-responses:
  - name: isok
    base-url: http://x
    api-key: a
    models:
      - name: gpt-5.6-luna
        verbosity: " LOW "
      - name: gpt-5.6-terra
        verbosity: medium
      - name: gpt-5.6-sol
`
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate valid verbosity: %v", err)
	}
	if got := cfg.OpenAIResponses[0].Models[0].ResolvedVerbosity(); got != "low" {
		t.Fatalf("verbosity = %q, want low", got)
	}
	if got := cfg.OpenAIResponses[0].Models[1].ResolvedVerbosity(); got != "medium" {
		t.Fatalf("verbosity = %q, want medium", got)
	}
	if got := cfg.OpenAIResponses[0].Models[2].ResolvedVerbosity(); got != "" {
		t.Fatalf("unset verbosity = %q, want empty", got)
	}

	cfg.OpenAIResponses[0].Models[0].Verbosity = "verbose"
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "verbosity must be low, medium or high") {
		t.Fatalf("validate invalid verbosity = %v, want verbosity validation error", err)
	}
}
