# Channel Affinity, Retry, and Failover

How lite-cpa pins sessions to upstream API keys, which CLI identity fields it trusts, when it retries, and how relay sites should fail over.

Source of truth for sticky identity extraction:

```text
internal/affinity/cli_sessions.go
```

Config reference and deployment notes: [AGENTS.md](../AGENTS.md). Chinese: [渠道亲和与重试](渠道亲和与重试.md).

---

## Why affinity exists

Multi-key and multi-provider pools improve availability, but **prompt cache and conversation state are usually bound to one upstream credential**. If consecutive turns of the same CLI session land on different keys (or different relays), cache hits drop and behavior can diverge.

Channel affinity is a **success memory**:

1. Extract a **session identity** from the request (headers, then body).
2. After a **successful** upstream call, remember which `UpstreamKey.ID` served that identity.
3. On later requests with the same identity, prefer that key first.

It is **not** ordinary load balancing. Priority and round-robin still apply when there is no pin, or when the pin is cleared / expired.

---

## What gets pinned

lite-cpa is API-key only (no OAuth accounts). The sticky mapping is:

```text
session identity  →  UpstreamKey.ID   (e.g. laysath-0)
```

`UpstreamKey.ID` is `{provider-name}-{index}` from config expansion—not the provider display name alone.

Cache key shape:

```text
cacheKey ≈ "{rule-name}:{affinity-value}"  →  UpstreamKey.ID
TTL default: 600 seconds (process memory only; no Redis)
```

Binding rules:

| Event | Action |
|---|---|
| Lookup hit | Prefer that key on the first attempt (if still in the model pool) |
| Lookup miss | Normal priority / round-robin |
| Upstream **success** | `Record` pin (`switch-on-success` default true → pin the key that actually worked) |
| Sticky key **failure** | `Clear` pin for that cache key |

No identity field → affinity never engages.

---

## When a request is sticky

Two gates must both pass.

### 1. Model family must match

Default families (affinity on and no custom `rules`):

`claude`, `gpt`, `gemini`, `grok`, `glm`, `kimi`, `qwen`, `minimax`.

Match is **substring**, case-insensitive (`proxy-claude-x` matches `claude`).

YAML:

```yaml
channel-affinity: true                 # all default families
channel-affinity: [claude, gpt, grok]  # subset
channel-affinity: false                # off
channel-affinity:
  models: [claude, grok]
  default-ttl-seconds: 600
```

Advanced: set `rules:` to fully override generated family rules. Empty `rules` with enabled affinity expands from `models` / defaults.

### 2. Session identity must be present

Extraction order (runtime):

```text
1. Sticky session headers  (StickySessionHeaders — first non-empty wins)
2. Protocol body by path
3. Remaining rule KeySources (custom rules only; catalog headers already tried)
```

There is **no message-hash fallback**. Missing identity → pure RR / priority.

#### Header priority (runtime)

First non-empty header wins. Product-stable ids first; weak / sometimes per-request last:

| Priority | Headers | Typical CLI |
|---|---|---|
| 1 | `X-Claude-Code-Session-Id` | Claude Code |
| 2 | `x-opencode-session` | OpenCode |
| 3 | `x-session-affinity` | **MiMo Code**, OpenCode / Pi affinity |
| 4 | `session-id` / `Session-Id` / `session_id` | Codex, Pi |
| 5 | `thread-id` / `Thread-Id` / `thread_id`, `Conversation_id` | Codex thread / WS |
| 6 | `X-Session-Id` | Generic / OpenCode / Pi openrouter format |
| 7 | `x-parent-session-id` | Sub-agent parent (weaker pin) |
| 8 | `X-Amp-Thread-Id` | Amp CLI |
| 9 | `X-Client-Request-Id` | Pi (Codex may send per-request UUIDs—keep last) |

`net/http` header matching is case-insensitive; both hyphenated and underscored spellings are listed because reverse proxies differ.

Weak headers (`X-Client-Request-Id`) are deferred: they are only used when no strong header, protocol body field, or custom key source yields an identity, and pins made from a weak value are capped to a 60s TTL — the value may be per-request, so a full-TTL pin would flood the cache with one-shot keys.

#### Protocol body (no session header)

| Request path | Preferred body fields (in order) |
|---|---|
| contains `/messages` | `metadata.user_id` → `prompt_cache_key` |
| `/responses`, `/chat/completions`, `/completions` | `prompt_cache_key` → `metadata.user_id` |
| other | `prompt_cache_key` → `metadata.user_id` |

**Claude / OpenCode `metadata.user_id` normalization** (when that field is used):

| Format | Example | Stored affinity value |
|---|---|---|
| Legacy string | `user_{hash}_account__session_{uuid}` | `{uuid}` |
| JSON string | `{"device_id":"...","session_id":"uuid"}` | `{uuid}` |
| Other non-empty | arbitrary string | as-is |

---

## Coding CLI catalog

Catalog lives in `PriorityCLISessionSources` (`cli_sessions.go`). Runtime does **not** pick a CLI first then extract—it uses the shared header list + body paths above. The table documents **what each CLI actually emits** and what to preserve on the reverse proxy.

| CLI | Confidence | Primary sticky signal | Secondary | Notes |
|---|---|---|---|---|
| **claude-code** | high | body `metadata.user_id` (normalized); header `X-Claude-Code-Session-Id` | — | Claude Code session formats |
| **codex** | high | headers `session-id` / `session_id`, `thread-id`; body `prompt_cache_key` | `Conversation_id` | Do **not** treat `X-Client-Request-Id` alone as session (often per-request) |
| **pi** | high | `session_id`, `x-session-affinity`, `X-Session-Id` | `X-Client-Request-Id`, body `prompt_cache_key` | `sessionAffinityFormat`: openai vs openrouter |
| **oh-my-pi** | high | same as Pi | same as Pi | Shares Pi openai-shared stack |
| **opencode** | high | `x-opencode-session`, `x-session-affinity`, `X-Session-Id` | body Claude-style `metadata.user_id` | Read headers on **ingress** (may be stripped before real provider) |
| **kimi-code** | high | body **`prompt_cache_key`** (= session id) | `metadata.user_id` | No stable sticky HTTP header in-repo. **Never** use `X-Msh-Device-Id` |
| **mimo-code** | high | header **`x-session-affinity`** = sessionID | `x-parent-session-id`, OpenCode heritage headers | OpenCode fork; `User-Agent: mimocode/...` identifies client, not the pin key |
| **zcode** | medium | Claude-like `metadata.user_id` | common session headers | Claude-compatible surface |

### Reverse proxy: headers not to strip

At minimum, pass through:

```text
X-Claude-Code-Session-Id
x-opencode-session
x-session-affinity
x-parent-session-id
session-id / session_id
thread-id / thread_id
Conversation_id
X-Session-Id
X-Client-Request-Id
X-Amp-Thread-Id
```

Nginx drops headers with underscores unless `underscores_in_headers on`. Prefer hyphenated forms when you control the client; still accept underscored names from Codex / Pi.

Also: disable response buffering for streams (`proxy_buffering off`).

---

## Retry budget

Top-level `request-retry`:

```text
maxAttempts = min(1 + request-retry, number of keys for that model)
```

Example: `request-retry: 2` and 5 keys → at most **3** tries on that request.

Retriable upstream outcomes (rotate key):

- HTTP **401 / 403 / 429 / ≥500**
- Network / transport errors

Other 4xx return to the client without rotating.

No time-based backoff between tries on the same request—rotation is immediate.

---

## Failover mode (per provider `name`)

After a retriable failure, behavior depends on the **failed key’s** provider:

| `failover-mode` | Behavior | Typical use |
|---|---|---|
| `key` (default) | Mark this key tried; pick another unused key (may be same site) | Official multi-key, key-level limits |
| `provider` | Skip **all** keys under that `name` for the rest of the request | Relay / 中转站 (one site down ⇒ all keys dead) |

```yaml
- name: openai-official
  failover-mode: key
  base-url: https://api.openai.com/v1
  ...

- name: laysath
  failover-mode: provider
  base-url: https://ai.laysath.cn/v1
  ...
```

Same client model `alias` across providers is **merged** into one pool. Selection prefers lower provider `priority`, then lower entry `priority` within that provider, then round-robin.

---

## Affinity × retry × failover (one request)

```text
1. Lookup affinity → optional preferred key ID
2. attempt loop (≤ maxAttempts):
     pick preferred (once) or Selector.Pick
       - prefer same supplier when possible
       - skip suppliers marked dead (provider mode)
     Execute upstream
     on failure:
       Clear affinity pin if this was the sticky key
       if skip-retry-on-failure and sticky was used → stop
       if failover-mode=provider → skip whole name
       continue
     on success:
       Record affinity pin → return
3. return last error
```

### Defaults that matter for production

| Setting | Default | Rationale |
|---|---|---|
| Affinity enabled | on | Cache-friendly sessions |
| Affinity TTL | **600s** | Avoid hour-long sticky to dead relays |
| `skip-retry-on-failure` (family rules) | **false** | Failover can run; success re-pins |
| `switch-on-success` | true | Pin the key that actually worked |
| `failover-mode` | `key` unless set | Explicit `provider` on relays |

---

## Common pitfalls (“old channel still hit”)

1. **TTL still valid** — pin remains until 600s (or `default-ttl-seconds`) elapses.
2. **`skip-retry-on-failure: true`** (custom rules) — this request will not rotate after sticky failure.
3. **Relay with `failover-mode: key`** — rotates keys on the same dead site. Use `provider`.
4. **New high-priority provider added** — existing pins still prefer the old key until clear/expire.
5. **No identity field** — affinity never engages; pure RR. Common when the reverse proxy strips session headers or the client never sends `prompt_cache_key` / `metadata.user_id`.
6. **Process restart** — memory cache is empty (no Redis).
7. **Kimi** — stickiness needs body `prompt_cache_key`; device headers are not session keys.
8. **MiMo / OpenCode** — stickiness needs `x-session-affinity` (or `x-opencode-session`) on the gateway ingress.

lite-cpa currently has **no admin “clear affinity” API**. Short TTL + restart is the practical reset; failure already clears the pin for that identity.

---

## Recommended configs

### Official only

```yaml
request-retry: 2
openai-responses:
  - name: openai-official
    failover-mode: key
    base-url: https://api.openai.com/v1
    api-key: sk-...
    models: [{ name: gpt-5, alias: gpt-5 }]
```

### Official + relay (merged alias)

```yaml
request-retry: 3
openai-responses:
  - name: openai-official
    priority: 0
    failover-mode: key
    base-url: https://api.openai.com/v1
    api-key: sk-official
    models: [{ name: gpt-5, alias: gpt-5 }]

  - name: laysath
    priority: 1
    failover-mode: provider
    base-url: https://ai.laysath.cn/v1
    api-key-entries:
      - { api-key: sk-a }
      - { api-key: sk-b }
    models:
      - { name: gpt-5, alias: gpt-5 }
      - { name: grok-4.5, alias: grok-4.5 }
```

### Affinity knobs

```yaml
channel-affinity: true                    # or [claude, gpt, grok]
# channel-affinity: false
# channel-affinity:
#   models: [claude, grok]
#   default-ttl-seconds: 600
```

---

## Debugging

With `debug: true`, stderr logs affinity hit / clear / record and rotation.

`GET /api/affinity/stats` (gateway key required, same as `/api/logs`) returns counters only — `matched_lookups`, `cache_hits`, `hit_rate`, `records`, `clears`, `preferred_unavailable`, `entries`. No affinity values or cache keys are exposed.

Request-log (optional) stores provider name and status; it does not yet store affinity fingerprint fields.

To verify identity extraction for a CLI: send one request with the expected header/body field, enable debug, confirm `affinity recorded`, then a second request should log `affinity hit` with the same key id.

---

## Related

- CLI catalog implementation: `internal/affinity/cli_sessions.go`
- Hot path: `internal/server` → affinity Lookup / Record / Clear
- Full config reference: [AGENTS.md](../AGENTS.md#configuring-configyaml)
- Deployment: [AGENTS.md](../AGENTS.md#deployment)
- Chinese: [渠道亲和与重试](渠道亲和与重试.md)
