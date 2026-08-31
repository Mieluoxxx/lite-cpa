// Package affinity — CLI session identity catalog.
//
// This file is the single source of truth for coding-CLI sticky identifiers.
// Channel affinity consults it before protocol body fields and custom rule
// KeySources. Keep entries evidence-based; mark Confidence when reverse-engineered.
//
// Extraction policy (unchanged overall):
//  1. sticky session headers (this catalog)
//  2. protocol-native body field by path (with Claude user_id normalization)
//  3. remaining rule KeySources
//
// No identity field → no stickiness (no message-hash fallback).
package affinity

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// Confidence grades how sure we are about a CLI's sticky identity.
type Confidence string

const (
	// ConfidenceHigh: observed in upstream CLI / CPA / new-api production paths.
	ConfidenceHigh Confidence = "high"
	// ConfidenceMedium: documented by the project or a popular fork; not fully verified here.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceLow: inferred / protocol-only; keep for docs, weak as a dedicated signal.
	ConfidenceLow Confidence = "low"
)

// CLISessionSource describes how one coding CLI labels a multi-turn session.
//
// Headers and BodyPaths are ordered preferred-first. Values are matched
// case-insensitively for headers (net/http.Header.Get).
type CLISessionSource struct {
	// ID is a stable machine name (claude-code, codex, …).
	ID string
	// Name is a human label.
	Name string
	// Confidence of the mapping.
	Confidence Confidence
	// Headers are request headers that carry a stable session / thread id.
	// Prefer hyphenated forms; include underscore variants for nginx / older CLIs.
	Headers []string
	// BodyPaths are gjson paths that carry a stable session id for this CLI.
	// Protocol-generic paths (prompt_cache_key, metadata.user_id) are also
	// applied globally in extractAffinityValue; list CLI-specific ones here.
	BodyPaths []string
	// Notes are short implementation notes (not user docs).
	Notes string
}

// PriorityCLISessionSources is the supported set for lite-cpa channel affinity.
// Order is documentation order only; runtime header priority is StickySessionHeaders.
var PriorityCLISessionSources = []CLISessionSource{
	{
		ID:         "claude-code",
		Name:       "Claude Code",
		Confidence: ConfidenceHigh,
		Headers: []string{
			// Upstream Claude Code / gateway passthrough (CPA sets this outbound).
			"X-Claude-Code-Session-Id",
		},
		BodyPaths: []string{
			// Primary sticky id for Anthropic Messages.
			// Formats:
			//   legacy: user_{hash}_account__session_{uuid}
			//   json:   {"device_id":"...","account_uuid":"...","session_id":"uuid"}
			"metadata.user_id",
		},
		Notes: "Normalize metadata.user_id to the session UUID when the Claude formats match.",
	},
	{
		ID:         "codex",
		Name:       "OpenAI Codex CLI",
		Confidence: ConfidenceHigh,
		Headers: []string{
			// Modern Codex prefers hyphenated names (underscores dropped for proxies).
			// https://github.com/openai/codex/commit/7c7b4861d88960f7e3bd5b7f30f8351be666dd84
			"session-id",
			"thread-id",
			// Legacy / mixed casing still seen in the wild and in CPA passthrough.
			"Session-Id",
			"Session_id",
			"session_id",
			"Thread-Id",
			"Thread_id",
			"thread_id",
			// Conversation_id is used on websocket / some Responses paths.
			"Conversation_id",
			"conversation_id",
		},
		BodyPaths: []string{
			// Responses API prompt cache key — new-api codex rule uses this.
			"prompt_cache_key",
		},
		Notes: "Do not treat X-Client-Request-Id alone as Codex session (often per-request). Prefer session/thread headers or prompt_cache_key.",
	},
	{
		ID:         "pi",
		Name:       "Pi coding agent",
		Confidence: ConfidenceHigh,
		Headers: []string{
			// Pi / oh-my-pi sessionAffinityFormat=openai:
			//   session_id + x-client-request-id (+ x-session-affinity on completions)
			// sessionAffinityFormat=openrouter → x-session-id
			"session_id",
			"Session_id",
			"session-id",
			"X-Client-Request-Id",
			"x-client-request-id",
			"x-session-affinity",
			"X-Session-Affinity",
			"x-session-id",
			"X-Session-Id",
		},
		BodyPaths: []string{
			"prompt_cache_key",
		},
		Notes: "CPA historically keyed PI on X-Client-Request-Id; Pi 0.68+ also emits session_id / x-session-affinity when caching is on.",
	},
	{
		ID:         "oh-my-pi",
		Name:       "Oh My Pi",
		Confidence: ConfidenceHigh,
		Headers: []string{
			// Same stack as Pi (can1357/oh-my-pi openai-shared).
			"session_id",
			"Session_id",
			"session-id",
			"X-Client-Request-Id",
			"x-client-request-id",
			"x-session-affinity",
			"X-Session-Affinity",
			"x-session-id",
			"X-Session-Id",
		},
		BodyPaths: []string{
			"prompt_cache_key",
		},
		Notes: "Shares Pi's sessionAffinityFormat / promptCacheSessionHeader compat knobs.",
	},
	{
		ID:         "opencode",
		Name:       "OpenCode",
		Confidence: ConfidenceHigh,
		Headers: []string{
			// Trace / proxy affinity headers used by OpenCode and forks.
			"x-opencode-session",
			"X-Opencode-Session",
			"x-session-affinity",
			"X-Session-Affinity",
			"X-Session-Id",
			"x-session-id",
			"X-Parent-Session-Id", // weaker: parent of a sub-agent session
		},
		BodyPaths: []string{
			// Anthropic path: Claude-style user_id for prompt cache.
			// Format: user_{projectId}_account__session_{sessionId}
			"metadata.user_id",
			"prompt_cache_key",
		},
		Notes: "x-opencode-session may be stripped before the real provider; gateways should read it on ingress.",
	},
	{
		ID:         "kimi-code",
		Name:       "Kimi Code",
		Confidence: ConfidenceHigh,
		// Repo: https://github.com/MoonshotAI/kimi-code
		// AGENTS.md: Agent may take optional sessionId as a request-config hint
		// mapped to the provider's prompt_cache_key (not stored on Agent).
		// Legacy kimi-cli DeepWiki: prompt_cache_key := session_id.
		Headers: nil, // no stable outbound sticky HTTP header confirmed in-repo
		BodyPaths: []string{
			"prompt_cache_key", // primary: sessionId → prompt_cache_key
			"metadata.user_id",
		},
		Notes: "Sticky via body prompt_cache_key (= session id). Do not use X-Msh-Device-Id (device-scoped, shared across sessions).",
	},
	{
		ID:         "mimo-code",
		Name:       "Xiaomi MiMo Code",
		Confidence: ConfidenceHigh,
		// Repo: https://github.com/XiaomiMiMo/MiMo-Code (OpenCode fork)
		// packages/opencode/src/session/llm.ts always injects:
		//   headers["x-session-affinity"] = input.sessionID
		//   headers["x-parent-session-id"] = parent (sub-agent)
		//   User-Agent: mimocode/<version>
		// PR #1034 unified this (always send x-session-affinity + User-Agent).
		Headers: []string{
			"x-session-affinity",
			"X-Session-Affinity",
			"x-parent-session-id", // sub-agent parent; weaker for pin
			"X-Parent-Session-Id",
			// OpenCode heritage / optional
			"x-opencode-session",
			"X-Session-Id",
			"x-session-id",
		},
		BodyPaths: []string{
			"prompt_cache_key",
			"metadata.user_id",
		},
		Notes: "Primary sticky header is x-session-affinity = sessionID. OpenCode-derived; User-Agent prefix mimocode/.",
	},
	{
		ID:         "zcode",
		Name:       "ZCODE (Z.AI)",
		Confidence: ConfidenceMedium,
		Headers: []string{
			// ZCODE hooks Claude CLI events; treat like Claude Code when headers appear.
			"X-Claude-Code-Session-Id",
			"X-Session-Id",
			"session_id",
		},
		BodyPaths: []string{
			"metadata.user_id",
			"prompt_cache_key",
		},
		Notes: "Claude-compatible agent surface; metadata.user_id is the primary sticky signal.",
	},
}

// StickySessionHeaders is the deduplicated, priority-ordered header list used
// at runtime. First non-empty wins.
//
// Priority rationale:
//  1. Explicit product session headers (Claude Code / OpenCode / MiMo)
//  2. Codex / Pi session + thread (hyphenated then underscored)
//  3. Generic X-Session-Id / Thread-Id family
//  4. Parent-session (sub-agent; weaker pin)
//  5. X-Client-Request-Id last (PI sticky; Codex may send per-request UUIDs)
var StickySessionHeaders = buildStickySessionHeaders()

func buildStickySessionHeaders() []string {
	// Explicit priority list — do not derive solely from PriorityCLISessionSources
	// merge order, which is documentation-oriented.
	preferred := []string{
		// Product-specific (stable multi-turn ids)
		"X-Claude-Code-Session-Id",
		"x-opencode-session",
		"X-Opencode-Session",
		// MiMo Code / OpenCode always-on affinity header (= sessionID)
		"x-session-affinity",
		"X-Session-Affinity",

		// Codex / Pi primary session
		"session-id",
		"Session-Id",
		"Session_id",
		"session_id",

		// Thread / conversation
		"thread-id",
		"Thread-Id",
		"Thread_id",
		"thread_id",
		"Conversation_id",
		"conversation_id",

		// Generic session
		"X-Session-Id",
		"x-session-id",
		"X-Session-ID",

		// Parent of a sub-agent session (weaker)
		"x-parent-session-id",
		"X-Parent-Session-Id",

		// Amp CLI (CPA; not in priority list but cheap to honor)
		"X-Amp-Thread-Id",

		// Weak / sometimes per-request — keep last
		"X-Client-Request-Id",
		"x-client-request-id",
	}

	seen := make(map[string]struct{}, len(preferred)+16)
	out := make([]string, 0, len(preferred)+16)
	add := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" {
			return
		}
		key := strings.ToLower(h)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, h)
	}
	for _, h := range preferred {
		add(h)
	}
	// Append any catalog headers we forgot above.
	for _, cli := range PriorityCLISessionSources {
		for _, h := range cli.Headers {
			add(h)
		}
	}
	return out
}

// sessionHeaderKeySet is a lower-cased set of StickySessionHeaders for O(1) checks.
var sessionHeaderKeySet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(StickySessionHeaders))
	for _, h := range StickySessionHeaders {
		m[strings.ToLower(h)] = struct{}{}
	}
	return m
}()

// claudeSessionPattern matches Claude Code legacy metadata.user_id:
//
//	user_{hash}_account__session_{uuid}
var claudeSessionPattern = regexp.MustCompile(`(?i)_session_([a-f0-9-]+)$`)

// weakStickySessionHeaders are sticky headers whose value may change per
// request (catalog note: "Weak / sometimes per-request — keep last"). They are
// deferred behind strong headers, protocol body fields and custom key sources,
// and their pins are capped to a short TTL.
var weakStickySessionHeaders = map[string]struct{}{
	"x-client-request-id": {},
}

func isWeakStickySessionHeader(key string) bool {
	_, ok := weakStickySessionHeaders[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

// extractStickySessionHeader returns the first sticky session header value.
// Strong (stable multi-turn) headers win; a value from a weak header is
// reported with weak=true and only consumed when no stronger identity exists.
func extractStickySessionHeader(headers http.Header) (value string, weak bool, ok bool) {
	if headers == nil {
		return "", false, false
	}
	for _, k := range StickySessionHeaders {
		v, found := extractHeader(headers, k)
		if !found {
			continue
		}
		if isWeakStickySessionHeader(k) {
			if value == "" {
				value, weak, ok = v, true, true
			}
			continue
		}
		return v, false, true
	}
	return value, weak, ok
}

// isStickySessionHeader reports whether key is a known sticky session header.
func isStickySessionHeader(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	_, ok := sessionHeaderKeySet[strings.ToLower(key)]
	return ok
}

// normalizeClaudeUserID extracts a stable session id from Claude / OpenCode
// metadata.user_id values. Returns (value, true) when non-empty.
//
//	legacy string: user_xxx_account__session_{uuid}  → uuid
//	json string:   {"session_id":"uuid",...}         → uuid
//	other:         as-is
func normalizeClaudeUserID(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	// Legacy Claude Code suffix.
	if m := claudeSessionPattern.FindStringSubmatch(raw); len(m) >= 2 {
		if sid := strings.TrimSpace(m[1]); sid != "" {
			return sid, true
		}
	}
	// JSON object embedded as a string (Claude Code newer format).
	if strings.HasPrefix(raw, "{") {
		if sid := strings.TrimSpace(gjson.Get(raw, "session_id").String()); sid != "" {
			return sid, true
		}
	}
	return raw, true
}

// extractProtocolBodyValue reads the path-preferred protocol body field and
// normalizes Claude-style metadata.user_id when that path is used.
func extractProtocolBodyValue(path string, body []byte) (string, bool) {
	for _, expr := range protocolBodyPaths(path) {
		v, ok := extractGJSON(body, expr)
		if !ok {
			continue
		}
		if expr == "metadata.user_id" {
			return normalizeClaudeUserID(v)
		}
		return v, true
	}
	return "", false
}

// CLISessionSourceByID returns a catalog entry by ID.
func CLISessionSourceByID(id string) (CLISessionSource, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, s := range PriorityCLISessionSources {
		if s.ID == id {
			return s, true
		}
	}
	return CLISessionSource{}, false
}
