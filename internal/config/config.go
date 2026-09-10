package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the slim lite-cpa configuration surface.
type Config struct {
	Host    string   `yaml:"host"`
	Port    int      `yaml:"port"`
	APIKeys []string `yaml:"api-keys"`

	// MaxBodyBytes limits inbound request bodies (0 = 32MiB default).
	MaxBodyBytes int64 `yaml:"max-body-bytes"`

	// RequestRetry is credential rotation attempts after the first failure.
	RequestRetry int `yaml:"request-retry"`

	// ProxyURL is a global outbound proxy (optional).
	ProxyURL string `yaml:"proxy-url"`

	// Debug enables verbose logging to stderr.
	Debug bool `yaml:"debug"`

	// RequestLog is optional request recording (sqlite or postgres) with retention.
	RequestLog RequestLogConfig `yaml:"request-log"`

	// ChannelAffinity pins successful upstream keys by request affinity keys
	// (new-api style rule stickiness; process-local memory cache).
	ChannelAffinity ChannelAffinitySetting `yaml:"channel-affinity"`

	// Routing controls optional same-priority adaptive key selection.
	Routing RoutingConfig `yaml:"routing"`

	AnthropicMessages []Provider `yaml:"anthropic-messages"`
	OpenAIResponses   []Provider `yaml:"openai-responses"`
	OpenAICompletions []Provider `yaml:"openai-completions"`
	OpenAIImages      []Provider `yaml:"openai-images"`
}

// RoutingConfig controls the opt-in reliability-aware selector.
// Strategy defaults to priority, preserving the historical provider/key order.
type RoutingConfig struct {
	Strategy string `yaml:"strategy,omitempty"`  // priority | adaptive
	HalfLife string `yaml:"half-life,omitempty"` // Go duration; default 72h
	Shadow   bool   `yaml:"shadow,omitempty"`    // collect scores without changing picks
}

func (r RoutingConfig) HalfLifeDuration() time.Duration {
	d, err := time.ParseDuration(r.HalfLife)
	if err != nil || d <= 0 {
		return 72 * time.Hour
	}
	return d
}

// ChannelAffinityKeySource extracts a sticky identity from the request.
// Type: request_header | gjson  (aliases: header | body).
type ChannelAffinityKeySource struct {
	Type string `yaml:"type"`           // request_header | gjson
	Key  string `yaml:"key,omitempty"`  // header name when type=request_header
	Path string `yaml:"path,omitempty"` // gjson path when type=gjson
}

// ChannelAffinityRule is an expanded sticky rule (usually generated from model families).
type ChannelAffinityRule struct {
	Name               string                     `yaml:"name,omitempty"`
	ModelRegex         []string                   `yaml:"model-regex,omitempty"`
	PathRegex          []string                   `yaml:"path-regex,omitempty"`
	UserAgentInclude   []string                   `yaml:"user-agent-include,omitempty"`
	KeySources         []ChannelAffinityKeySource `yaml:"key-sources,omitempty"`
	ValueRegex         string                     `yaml:"value-regex,omitempty"`
	TTLSeconds         int                        `yaml:"ttl-seconds,omitempty"`
	SkipRetryOnFailure bool                       `yaml:"skip-retry-on-failure,omitempty"`
	IncludeModelName   bool                       `yaml:"include-model-name,omitempty"`
	IncludeRuleName    bool                       `yaml:"include-rule-name,omitempty"`
}

// ChannelAffinitySetting is sticky upstream-key routing.
//
// YAML forms (enabled by default when omitted):
//
//	channel-affinity: true
//	channel-affinity: false
//	channel-affinity: [claude, gpt, gemini, grok, glm, kimi, qwen, minimax]
//	channel-affinity:
//	  models: [claude, grok]
//	  default-ttl-seconds: 600
//
// Advanced: set rules: [...] to fully override generated family rules.
type ChannelAffinitySetting struct {
	Enabled           *bool                 `yaml:"enabled,omitempty"`
	Models            []string              `yaml:"models,omitempty"` // model families
	SwitchOnSuccess   *bool                 `yaml:"switch-on-success,omitempty"`
	MaxEntries        int                   `yaml:"max-entries,omitempty"`
	DefaultTTLSeconds int                   `yaml:"default-ttl-seconds,omitempty"`
	Rules             []ChannelAffinityRule `yaml:"rules,omitempty"` // advanced override
}

// UnmarshalYAML accepts bool | family list | mapping.
func (s *ChannelAffinitySetting) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var b bool
		if err := value.Decode(&b); err == nil {
			s.Enabled = &b
			return nil
		}
		var name string
		if err := value.Decode(&name); err == nil {
			name = strings.TrimSpace(name)
			if name == "" {
				return nil
			}
			s.Models = []string{name}
			return nil
		}
		return fmt.Errorf("channel-affinity: expected bool or model family name")
	case yaml.SequenceNode:
		// Prefer []string. Also accept a single nested sequence: [[claude, gpt, ...]].
		var models []string
		if err := value.Decode(&models); err == nil {
			s.Models = models
			return nil
		}
		if len(value.Content) == 1 && value.Content[0].Kind == yaml.SequenceNode {
			if err := value.Content[0].Decode(&models); err == nil {
				s.Models = models
				return nil
			}
		}
		return fmt.Errorf("channel-affinity: expected list of model families")
	case yaml.MappingNode:
		type raw struct {
			Enabled           *bool                 `yaml:"enabled"`
			Models            []string              `yaml:"models"`
			SwitchOnSuccess   *bool                 `yaml:"switch-on-success"`
			MaxEntries        int                   `yaml:"max-entries"`
			DefaultTTLSeconds int                   `yaml:"default-ttl-seconds"`
			Rules             []ChannelAffinityRule `yaml:"rules"`
		}
		var r raw
		if err := value.Decode(&r); err != nil {
			return err
		}
		s.Enabled = r.Enabled
		s.Models = r.Models
		s.SwitchOnSuccess = r.SwitchOnSuccess
		s.MaxEntries = r.MaxEntries
		s.DefaultTTLSeconds = r.DefaultTTLSeconds
		s.Rules = r.Rules
		return nil
	case yaml.DocumentNode:
		if len(value.Content) == 1 {
			return s.UnmarshalYAML(value.Content[0])
		}
	}
	return fmt.Errorf("channel-affinity: unsupported yaml node")
}

// EnabledOrDefault returns true unless explicitly set to false.
func (s ChannelAffinitySetting) EnabledOrDefault() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// SwitchOnSuccessOrDefault returns true unless explicitly set to false.
func (s ChannelAffinitySetting) SwitchOnSuccessOrDefault() bool {
	if s.SwitchOnSuccess == nil {
		return true
	}
	return *s.SwitchOnSuccess
}

// ResolvedRules returns expanded sticky rules (families or advanced rules).
func (s ChannelAffinitySetting) ResolvedRules() []ChannelAffinityRule {
	if !s.EnabledOrDefault() {
		return nil
	}
	if len(s.Rules) > 0 {
		return append([]ChannelAffinityRule(nil), s.Rules...)
	}
	models := s.Models
	if len(models) == 0 {
		models = DefaultChannelAffinityModels()
	}
	return ExpandAffinityModels(models)
}

// RequestLogConfig controls optional request recording.
// Disabled by default. When enabled, metadata is stored; bodies are optional.
type RequestLogConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Backend   string `yaml:"backend"`   // "sqlite" | "postgres"
	Retention string `yaml:"retention"` // Go duration, e.g. "168h", "7d" not valid — use 168h
	StoreBody bool   `yaml:"store-body"`

	SQLite   SQLiteLogConfig   `yaml:"sqlite"`
	Postgres PostgresLogConfig `yaml:"postgres"`
}

type SQLiteLogConfig struct {
	Path string `yaml:"path"` // default logs/requests.db
}

type PostgresLogConfig struct {
	DSN string `yaml:"dsn"`
}

// Provider is a named upstream entry with multi-key support.
// Preferred config field order:
//
//	name, proxy-url, priority, failover-mode, headers, speed, base-url, api-key, models
//
// Headers may include User-Agent to override Go's default "Go-http-client/1.1".
type Provider struct {
	Name     string `yaml:"name"`
	ProxyURL string `yaml:"proxy-url"`
	Priority int    `yaml:"priority"`
	// FailoverMode is per-provider: "key" (default) or "provider".
	// "provider" skips all keys under this name after one retriable failure
	// (useful for relay sites where all keys die together).
	FailoverMode string            `yaml:"failover-mode,omitempty"`
	Headers      map[string]string `yaml:"headers"`
	// Speed is deprecated and no longer applied at provider level. Kept only as
	// a parse sentinel so legacy configs fail validation with a migration error
	// instead of silently losing the fast tier. Configure models[].speed instead.
	Speed         string        `yaml:"speed,omitempty"`
	BaseURL       string        `yaml:"base-url"`
	APIKey        string        `yaml:"api-key"`
	APIKeyEntries []APIKeyEntry `yaml:"api-key-entries"`
	Models        []ModelAlias  `yaml:"models"`
}

type APIKeyEntry struct {
	APIKey   string `yaml:"api-key"`
	Priority int    `yaml:"priority"`
}

type ModelAlias struct {
	Name  string `yaml:"name"`
	Alias string `yaml:"alias"`
	// Speed forces this model's supported fast tier ("fast"). Empty prevents
	// client-selected fast tiers. Model-scoped, same level as Verbosity.
	Speed string `yaml:"speed,omitempty"`
	// Verbosity is an optional Responses-API hint injected as text.verbosity
	// when the client did not set it (low | medium | high, GPT-5 series).
	Verbosity string `yaml:"verbosity,omitempty"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Port == 0 {
		c.Port = 8317
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 32 << 20
	}
	if c.RequestRetry < 0 {
		c.RequestRetry = 0
	}
	normalizeProviderFailovers(c.AnthropicMessages)
	normalizeProviderFailovers(c.OpenAIResponses)
	normalizeProviderFailovers(c.OpenAICompletions)
	normalizeModelSpeeds(c.AnthropicMessages)
	normalizeModelSpeeds(c.OpenAIResponses)
	normalizeModelSpeeds(c.OpenAICompletions)
	if c.RequestLog.Backend == "" {
		c.RequestLog.Backend = "sqlite"
	}
	if c.RequestLog.Retention == "" {
		c.RequestLog.Retention = "168h" // 7 days
	}
	if c.RequestLog.SQLite.Path == "" {
		c.RequestLog.SQLite.Path = "logs/requests.db"
	}
	if strings.TrimSpace(c.Routing.Strategy) == "" {
		c.Routing.Strategy = "priority"
	} else {
		c.Routing.Strategy = strings.ToLower(strings.TrimSpace(c.Routing.Strategy))
	}
	if strings.TrimSpace(c.Routing.HalfLife) == "" {
		c.Routing.HalfLife = "72h"
	}
	if c.ChannelAffinity.MaxEntries <= 0 {
		c.ChannelAffinity.MaxEntries = 100_000
	}
	if c.ChannelAffinity.DefaultTTLSeconds <= 0 {
		c.ChannelAffinity.DefaultTTLSeconds = 600
	}
	// Expand model families (or defaults) into rules when advanced rules are absent.
	if c.ChannelAffinity.EnabledOrDefault() && len(c.ChannelAffinity.Rules) == 0 {
		c.ChannelAffinity.Rules = c.ChannelAffinity.ResolvedRules()
	}
}

// DefaultChannelAffinityModels is the built-in sticky family list.
func DefaultChannelAffinityModels() []string {
	return []string{"claude", "gpt", "gemini", "grok", "glm", "kimi", "qwen", "minimax"}
}

// ExpandAffinityModels turns family names into sticky rules.
// Unknown names become a case-insensitive substring regex on the name itself.
func ExpandAffinityModels(models []string) []ChannelAffinityRule {
	out := make([]ChannelAffinityRule, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, raw := range models {
		family := strings.ToLower(strings.TrimSpace(raw))
		if family == "" {
			continue
		}
		if _, ok := seen[family]; ok {
			continue
		}
		seen[family] = struct{}{}
		rule, ok := affinityFamilyRule(family)
		if !ok {
			// fallback: treat token as model-name prefix
			rule = ChannelAffinityRule{
				Name:            family + " sticky",
				ModelRegex:      []string{`(?i)` + regexp.QuoteMeta(family)},
				KeySources:      defaultAffinityKeySources(),
				IncludeRuleName: true,
			}
		}
		out = append(out, rule)
	}
	return out
}

// DefaultChannelAffinityRules expands the default family list (compat helper).
func DefaultChannelAffinityRules() []ChannelAffinityRule {
	return ExpandAffinityModels(DefaultChannelAffinityModels())
}

// defaultAffinityKeySources is a fallback list for advanced/custom rules.
// Runtime extraction prefers the CLI catalog in internal/affinity/cli_sessions.go
// (sticky headers → protocol body). This list remains for custom rules that
// only set KeySources without relying on the catalog.
func defaultAffinityKeySources() []ChannelAffinityKeySource {
	return []ChannelAffinityKeySource{
		// Product / CLI session headers (see affinity.StickySessionHeaders)
		{Type: "request_header", Key: "X-Claude-Code-Session-Id"},
		{Type: "request_header", Key: "x-opencode-session"},
		{Type: "request_header", Key: "session-id"},
		{Type: "request_header", Key: "Session-Id"},
		{Type: "request_header", Key: "session_id"},
		{Type: "request_header", Key: "thread-id"},
		{Type: "request_header", Key: "Thread-Id"},
		{Type: "request_header", Key: "thread_id"},
		{Type: "request_header", Key: "X-Session-Id"},
		{Type: "request_header", Key: "x-session-affinity"},
		{Type: "request_header", Key: "X-Client-Request-Id"},
		// protocol fields (order refined at runtime by path)
		{Type: "gjson", Path: "prompt_cache_key"},
		{Type: "gjson", Path: "metadata.user_id"},
	}
}

func affinityFamilyRule(family string) (ChannelAffinityRule, bool) {
	// skipRetry false by default so failed sticky keys can rotate (relay-friendly).
	type fam struct {
		name      string
		regex     []string
		skipRetry bool
	}
	table := map[string]fam{
		"claude":   {name: "claude sticky", regex: []string{`(?i)claude`}, skipRetry: false},
		"gpt":      {name: "gpt sticky", regex: []string{`(?i)gpt`}, skipRetry: false},
		"openai":   {name: "gpt sticky", regex: []string{`(?i)gpt`}, skipRetry: false},
		"codex":    {name: "gpt sticky", regex: []string{`(?i)gpt`}, skipRetry: false},
		"gemini":   {name: "gemini sticky", regex: []string{`(?i)gemini`}, skipRetry: false},
		"grok":     {name: "grok sticky", regex: []string{`(?i)grok`}, skipRetry: false},
		"glm":      {name: "glm sticky", regex: []string{`(?i)glm`}, skipRetry: false},
		"kimi":     {name: "kimi sticky", regex: []string{`(?i)(kimi|moonshot)`}, skipRetry: false},
		"moonshot": {name: "kimi sticky", regex: []string{`(?i)(kimi|moonshot)`}, skipRetry: false},
		"qwen":     {name: "qwen sticky", regex: []string{`(?i)qwen`}, skipRetry: false},
		"minimax":  {name: "minimax sticky", regex: []string{`(?i)(minimax|abab)`}, skipRetry: false},
	}
	f, ok := table[family]
	if !ok {
		return ChannelAffinityRule{}, false
	}
	return ChannelAffinityRule{
		Name:               f.name,
		ModelRegex:         f.regex,
		KeySources:         defaultAffinityKeySources(),
		SkipRetryOnFailure: f.skipRetry,
		IncludeRuleName:    true,
	}, true
}

func (c *Config) validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Port)
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys must not be empty")
	}
	for i, k := range c.APIKeys {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("api-keys[%d] is empty", i)
		}
	}
	if len(c.AnthropicMessages) == 0 && len(c.OpenAIResponses) == 0 && len(c.OpenAICompletions) == 0 {
		return fmt.Errorf("at least one upstream provider is required")
	}
	strategy := strings.ToLower(strings.TrimSpace(c.Routing.Strategy))
	if strategy == "" {
		strategy = "priority"
	}
	if strategy != "priority" && strategy != "adaptive" {
		return fmt.Errorf("routing.strategy must be priority or adaptive, got %q", c.Routing.Strategy)
	}
	halfLifeValue := strings.TrimSpace(c.Routing.HalfLife)
	if halfLifeValue == "" {
		halfLifeValue = "72h"
	}
	if halfLife, err := time.ParseDuration(halfLifeValue); err != nil || halfLife <= 0 {
		return fmt.Errorf("routing.half-life must be a positive Go duration, got %q", c.Routing.HalfLife)
	}
	for _, group := range [][]Provider{c.AnthropicMessages, c.OpenAIResponses, c.OpenAICompletions} {
		for i, p := range group {
			switch NormalizeFailoverMode(p.FailoverMode) {
			case "key", "provider":
			default:
				name := strings.TrimSpace(p.Name)
				if name == "" {
					name = fmt.Sprintf("#%d", i)
				}
				return fmt.Errorf("provider %q failover-mode must be key or provider, got %q", name, p.FailoverMode)
			}
			if p.Speed != "" {
				name := strings.TrimSpace(p.Name)
				if name == "" {
					name = fmt.Sprintf("#%d", i)
				}
				return fmt.Errorf("provider %q speed is no longer supported; move it under models[].speed", name)
			}
			for _, m := range p.Models {
				name := strings.TrimSpace(p.Name)
				if name == "" {
					name = fmt.Sprintf("#%d", i)
				}
				if m.Speed != "" && NormalizeSpeed(m.Speed) == "" {
					return fmt.Errorf("provider %q model %q speed must be fast when set, got %q", name, m.Name, m.Speed)
				}
				if m.Verbosity != "" && NormalizeVerbosity(m.Verbosity) == "" {
					return fmt.Errorf("provider %q model %q verbosity must be low, medium or high when set, got %q", name, m.Name, m.Verbosity)
				}
			}
		}
	}

	if c.RequestLog.Enabled {
		switch strings.ToLower(strings.TrimSpace(c.RequestLog.Backend)) {
		case "sqlite":
			// ok
		case "postgres", "postgresql", "pg":
			if strings.TrimSpace(c.RequestLog.Postgres.DSN) == "" {
				return fmt.Errorf("request-log.postgres.dsn is required when backend is postgres")
			}
		default:
			return fmt.Errorf("request-log.backend must be sqlite or postgres")
		}
		if _, err := time.ParseDuration(c.RequestLog.Retention); err != nil {
			return fmt.Errorf("request-log.retention: %w", err)
		}
	}
	return nil
}

// NormalizeFailoverMode maps aliases to key|provider. Empty => key.
func NormalizeFailoverMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "key", "keys":
		return "key"
	case "provider", "supplier", "site":
		return "provider"
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func normalizeProviderFailovers(ps []Provider) {
	for i := range ps {
		ps[i].FailoverMode = NormalizeFailoverMode(ps[i].FailoverMode)
	}
}

// NormalizeSpeed normalizes a configured fast-tier hint. Only "fast" is accepted.
func NormalizeSpeed(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "fast" {
		return "fast"
	}
	return ""
}

// ResolvedSpeed returns the normalized fast-tier hint ("" when unset/invalid).
func (m ModelAlias) ResolvedSpeed() string { return NormalizeSpeed(m.Speed) }

func normalizeModelSpeeds(ps []Provider) {
	for i := range ps {
		for j := range ps[i].Models {
			ps[i].Models[j].Speed = NormalizeSpeed(ps[i].Models[j].Speed)
		}
	}
}

// ExpandKeys returns flat key entries for a provider (flat api-key or api-key-entries).
// Proxy is provider-level only; flatProxy is applied by the pool when expanding UpstreamKey.
// Entry Priority is relative within the provider (not global); flat api-key uses 0.
func ExpandKeys(flatKey string, entries []APIKeyEntry) []APIKeyEntry {
	if len(entries) > 0 {
		out := make([]APIKeyEntry, 0, len(entries))
		for _, e := range entries {
			e.APIKey = strings.TrimSpace(e.APIKey)
			if e.APIKey == "" {
				continue
			}
			out = append(out, e)
		}
		return out
	}
	flatKey = strings.TrimSpace(flatKey)
	if flatKey == "" {
		return nil
	}
	return []APIKeyEntry{{APIKey: flatKey, Priority: 0}}
}

func (m ModelAlias) ResolvedAlias() string {
	if strings.TrimSpace(m.Alias) != "" {
		return strings.TrimSpace(m.Alias)
	}
	return strings.TrimSpace(m.Name)
}

// NormalizeVerbosity normalizes a configured verbosity hint. Only low|medium|high
// (GPT-5 series) are accepted; empty or invalid values return "".
func NormalizeVerbosity(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "low", "medium", "high":
		return v
	}
	return ""
}

// ResolvedVerbosity returns the normalized verbosity hint ("" when unset/invalid).
func (m ModelAlias) ResolvedVerbosity() string { return NormalizeVerbosity(m.Verbosity) }
