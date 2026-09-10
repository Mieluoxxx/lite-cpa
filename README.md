# lite-cpa

[中文文档](README_zh.md)

Slim Go gateway for protocol conversion among **OpenAI Chat Completions**, **OpenAI Responses**, and **Anthropic Messages**.

Single binary · config multi-key pools · optional request recording (SQLite / Postgres) · no control plane.

Based on [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) by Luis Pater / Router-For.ME.

**Author:** Mieluoxxx

## Features

- Cross-format conversion: `chat` ↔ `responses` ↔ `claude`
- Named upstream providers with multi-key round-robin and retry
- Optional **channel affinity** (new-api style sticky keys by header/body rule)
- Per-provider headers (including `User-Agent`)
- Optional request log: SQLite or Postgres with retention cleanup
- Binary / Docker / Compose deployment

## Docs

- [AGENTS.md](AGENTS.md) — config reference + **deployment** (binary / Docker / Compose)
- [Protocol conversion design](docs/Protocol-Conversion.md)
- Chinese design notes: [格式转换是如何实现的](docs/格式转换是如何实现的.md)
- [Channel affinity, retry, failover](docs/Channel-Affinity-and-Retry.md)
- Chinese: [渠道亲和与重试](docs/渠道亲和与重试.md)
- Wiki: https://github.com/Mieluoxxx/lite-cpa/wiki  
  (GitHub requires creating the first Wiki page in the web UI before git push works.)

### Deploy with an AI assistant

Copy-paste to your coding agent (Claude / Cursor / Codex / etc.):

```text
Read https://github.com/Mieluoxxx/lite-cpa/blob/main/AGENTS.md and deploy lite-cpa on this machine using that document. Follow its "Configuring config.yaml" and "Deployment" sections: create config/config.yaml from config/config.example.yaml, fill its gateway api-keys and at least one upstream provider, then start with docker compose (preferred) or a local binary. Prefer failover-mode key for official APIs and provider for relays. Verify with /healthz and one sample chat/completions request. Do not commit secrets.
```


## Quick start

```bash
cp config/config.example.yaml config/config.yaml
# edit config/config.yaml (api-keys and upstream credentials)

go run ./cmd/lite-cpa --config config/config.yaml

go build -trimpath -ldflags='-s -w' -o lite-cpa ./cmd/lite-cpa
./lite-cpa --config config/config.yaml
```

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer sk-lite-gateway' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}'
```

### Endpoints

| Method | Path | Client format |
|---|---|---|
| GET | `/healthz` | health |
| GET | `/v1/models` | model list |
| POST | `/v1/chat/completions` | openai-completions |
| POST | `/v1/responses` | openai-responses |
| POST | `/v1/messages` | anthropic-messages |

## Configuration

Full reference: [AGENTS.md](AGENTS.md#configuring-configyaml).

Provider field order:

1. `name`
2. `proxy-url`
3. `priority`
4. `failover-mode`
5. `headers`
6. `base-url`
7. `api-key`
8. `models`

```yaml
host: ""
port: 8317
api-keys:
  - "sk-lite-gateway"
request-retry: 2
debug: false

# Optional request recording
request-log:
  enabled: false
  backend: "sqlite"       # sqlite | postgres
  retention: "168h"       # Go duration; hourly cleanup
  store-body: false       # metadata only by default; streams never store resp body
  sqlite:
    path: "logs/requests.db"
  # postgres:
  #   dsn: "postgres://litecpa:litecpa@postgres:5432/litecpa?sslmode=disable"

anthropic-messages:
  - name: anthropic-official
    proxy-url: ""
    priority: 0
    failover-mode: key
    headers:
      User-Agent: "claude-cli/2.1.63 (external, cli)"
    base-url: "https://api.anthropic.com"
    api-key: "sk-ant-..."
    models:
      - name: claude-sonnet-4
        alias: claude-sonnet-4
    # api-key-entries:
    #   - api-key: sk-1
    #     priority: 0

openai-responses:
  - name: openai-responses
    proxy-url: ""
    priority: 0
    failover-mode: key
    headers:
      User-Agent: "openai-python/1.40.0"
    base-url: "https://api.openai.com/v1"
    api-key: "sk-..."
    models:
      - name: gpt-5
        alias: gpt-5

openai-completions:
  - name: deepseek
    proxy-url: ""
    priority: 0
    failover-mode: key
    headers:
      User-Agent: "curl/8.7.1"
    base-url: "https://api.deepseek.com/v1"
    api-key: "sk-..."
    models:
      - name: deepseek-chat
        alias: deepseek-chat
```

### User-Agent

If `headers.User-Agent` is unset, Go sends the default `Go-http-client/1.1`.


### Model fast mode

`speed: fast` is a per-model policy. Anthropic upstreams receive `speed: "fast"` and `Anthropic-Beta: fast-mode-2026-02-01`; OpenAI upstreams receive `service_tier: "priority"`. When omitted for a model, lite-cpa removes client-supplied fast-tier fields, so clients cannot change speed or billing. Configure it only for upstreams that support their native fast tier.

```yaml
anthropic-messages:
  - name: anthropic-fast
    models:
      - name: claude-sonnet-4
        speed: fast
    # ...

openai-responses:
  - name: openai-fast
    models:
      - name: gpt-5
        speed: fast
    # ...
```

### Model verbosity

`verbosity` is a per-model hint for the GPT-5 series that controls reply length without touching the prompt. It is injected as `text.verbosity` into Responses API requests when the client did not set it; a client-set `text.verbosity` always wins. Configure it under `models` on `openai-response` upstreams:

```yaml
openai-responses:
  - name: isok
    base-url: https://ai.isok.dev/v1
    api-key: sk-...
    models:
      - name: gpt-5.6-luna
        verbosity: low   # low | medium | high
```

### Failover mode

Per **provider** (`name`), not global. After a retriable error (401/403/429/5xx):

```yaml
openai-completions:
  - name: relay-a
    base-url: https://a.example/v1
    failover-mode: provider   # official → key; relay (e.g. ai.laysath.cn) → provider
    api-key-entries:
      - api-key: sk-a1
      - api-key: sk-a2
    models:
      - { name: grok-4.5, alias: grok-4.5 }

  - name: relay-b
    base-url: https://b.example/v1
    # failover-mode omitted => key
    api-key: sk-b1
    models:
      - { name: grok-4.5, alias: grok-4.5 }
```

| mode | behavior |
|---|---|
| `key` | try next unused key of any supplier |
| `provider` | skip **all keys under this `name`**, jump to another supplier |

Same client model alias across providers is merged into one pool.


### Channel affinity

Pin the same upstream API key for a session (prompt cache). Memory only. **On by default**.

Default TTL is **600s**; sticky failure allows key rotation (`skip-retry` false).

```yaml
# all defaults (claude/gpt/gemini/grok/glm/kimi/qwen/minimax)
# channel-affinity: true

# only some families
channel-affinity: [claude, gpt, grok]

# off
# channel-affinity: false
```

Sticky identity catalog: `internal/affinity/cli_sessions.go`. First present wins: product headers (`X-Claude-Code-Session-Id`, `x-opencode-session`, `x-session-affinity`) → Codex/Pi session/thread → `X-Session-Id` → `X-Client-Request-Id` → protocol body (`/v1/messages` normalizes `metadata.user_id`; responses/chat use `prompt_cache_key`). Details: [Channel Affinity and Retry](docs/Channel-Affinity-and-Retry.md). No identity → normal round-robin.

### Optional adaptive routing

The default `routing.strategy: priority` preserves the existing provider/key order. `routing.strategy: adaptive` ranks only within the same provider and entry-priority tier using decay-weighted Beta reliability; affinity and cooldown gates remain authoritative. `half-life` defaults to `72h`; `shadow: true` collects scores and recommendations without changing picks. Cold-start keys use a neutral prior and at most one deterministic same-tier exploration every ten non-affinity selections; quota estimation and randomized Thompson sampling are intentionally not enabled here.

### Request log

| Field | Meaning |
|---|---|
| `enabled` | default `false` |
| `backend` | `sqlite` or `postgres` |
| `retention` | Go duration, e.g. `168h` (7 days). Cleaned hourly. |
| `store-body` | default `false`; if true, bodies capped at 64KiB; **SSE response bodies are not stored** |
| `sqlite.path` | default `logs/requests.db` |
| `postgres.dsn` | required when backend is postgres |

Process diagnostics still go to **stderr** only (not the DB).

## Docker

```bash
cp config/config.example.yaml config/config.yaml
# edit config/config.yaml

docker compose up -d --build
docker compose logs -f
docker compose down
```

Mounts:

- `./config` → `/app/config` (read-only)
- `./logs` → `/app/logs` (sqlite path when enabled)

Listens on port `8317`. Default timezone `Asia/Shanghai` (`TZ`).

### Postgres backend (optional)

1. Uncomment the `postgres` service in `docker-compose.yml`.
2. Set in `config/config.yaml`:

```yaml
request-log:
  enabled: true
  backend: postgres
  retention: "168h"
  store-body: false
  postgres:
    dsn: "postgres://litecpa:litecpa@postgres:5432/litecpa?sslmode=disable"
```

3. Uncomment `depends_on` for `lite-cpa`.

### Plain docker

```bash
docker build -t lite-cpa:local .
docker run --rm -p 8317:8317 \
  -v "$PWD/config:/app/config:ro" \
  -v "$PWD/logs:/app/logs" \
  lite-cpa:local
```

## Development

```bash
gofmt -w .
go test ./...
go build -trimpath -ldflags='-s -w' -o lite-cpa ./cmd/lite-cpa
```

Translator layout:

```text
internal/translator/{from}/{to}/   # chat | claude | responses
```

## Performance notes

- Shared `http.Transport` (HTTP/2, idle connection reuse)
- Stream scanner cap 1MiB
- Single request translation per attempt
- Request log is async with a bounded queue; disabled by default
- Streaming responses: metadata only (no response body accumulation)

## Contributing

Contributions are welcome.

1. Fork the repository and create a feature branch.
2. Keep changes small and focused.
3. Run checks before opening a PR:

```bash
gofmt -w .
go test ./...
go build -o /tmp/lite-cpa ./cmd/lite-cpa
```

4. Prefer English for code comments and PR descriptions.
5. Do not commit secrets (`config.yaml`, API keys, local log DBs).
6. For protocol conversion changes, add or update tests covering:
   - same-format passthrough
   - cross-format non-stream
   - cross-format stream (SSE framing)

Open a pull request against `main` with a short summary of the problem and the fix.

## License

MIT. Copyright notices:

- Luis Pater
- Router-For.ME (upstream CPA)
- Mieluoxxx

See [LICENSE](LICENSE).
