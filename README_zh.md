# lite-cpa

[English](README.md)

轻量 Go 网关，在 **OpenAI Chat Completions**、**OpenAI Responses**、**Anthropic Messages** 之间做协议转换。

单二进制 · 配置多 Key · 可选请求记录（SQLite / Postgres）· 无控制面。

基于 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI)（Luis Pater / Router-For.ME）。

**作者：** Mieluoxxx

## 功能

- 三协议互转：`chat` ↔ `responses` ↔ `claude`
- 命名上游 + 多 Key 轮询 / 失败重试
- 可选 **渠道亲和性**（new-api 式规则粘滞：按 header/body 钉住上游 key）
- 按上游配置 header（含 `User-Agent`）
- 可选请求记录：SQLite 或 Postgres，带过期清理
- 支持二进制 / Docker / Compose 部署

## 文档

- [AGENTS.md](AGENTS.md) — **config 说明 + 部署**（二进制 / Docker / Compose）
- 中文设计说明：[格式转换是如何实现的](docs/格式转换是如何实现的.md)
- English: [Protocol Conversion](docs/Protocol-Conversion.md)
- [渠道亲和、重试与故障切换](docs/渠道亲和与重试.md)
- English: [Channel Affinity and Retry](docs/Channel-Affinity-and-Retry.md)
- Wiki：https://github.com/Mieluoxxx/lite-cpa/wiki  
  （GitHub 要求先在网页创建第一个 Wiki 页面后，才能用 git 推送 wiki）

### 用 AI 助手部署

复制下面这段给编程助手（Claude / Cursor / Codex 等）：

```text
请阅读 https://github.com/Mieluoxxx/lite-cpa/blob/main/AGENTS.md ，并按该文档在本机部署 lite-cpa。严格遵循其中的「Configuring config.yaml」与「Deployment」：从 config.example.yaml 生成 config.yaml，填写网关 api-keys 与至少一个上游 provider，优先用 docker compose 启动（或本地二进制）。官方 API 用 failover-mode: key，中转站用 provider。
```


## 快速开始

```bash
cp config.example.yaml config.yaml
# 填写 api-keys 与上游凭证

go run ./cmd/lite-cpa --config config.yaml

go build -trimpath -ldflags='-s -w' -o lite-cpa ./cmd/lite-cpa
./lite-cpa --config config.yaml
```

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer sk-lite-gateway' \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}'
```

### 接口

| Method | Path | 客户端格式 |
|---|---|---|
| GET | `/healthz` | 健康检查 |
| GET | `/v1/models` | 模型列表 |
| POST | `/v1/chat/completions` | openai-completions |
| POST | `/v1/responses` | openai-responses |
| POST | `/v1/messages` | anthropic-messages |

## 配置

完整字段说明见 [AGENTS.md](AGENTS.md#configuring-configyaml)。

Provider 字段顺序：

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

# 可选请求记录
request-log:
  enabled: false
  backend: "sqlite"       # sqlite | postgres
  retention: "168h"       # Go duration；每小时清理
  store-body: false       # 默认只记元数据；流式响应不存 body
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

未配置 `headers.User-Agent` 时，Go 默认发送 `Go-http-client/1.1`。


### 模型 Fast 模式

`speed: fast` 是模型级策略：Anthropic 上游会收到 `speed: "fast"` 与 `Anthropic-Beta: fast-mode-2026-02-01`；OpenAI 上游会收到 `service_tier: "priority"`。某模型未配置时，lite-cpa 会移除客户端携带的 fast tier 字段，客户端不能自行改变速度或计费。只应在确认上游支持其原生 fast tier 时配置。

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

### 模型 verbosity

`verbosity` 是 GPT-5 系列的模型级提示参数，可在不改动 prompt 的前提下控制回复篇幅。当客户端未设置时，lite-cpa 会以 `text.verbosity` 注入到 Responses API 请求中；客户端已设置的 `text.verbosity` 始终优先。在 `openai-response` 上游的 `models` 下配置：

```yaml
openai-responses:
  - name: isok
    base-url: https://ai.isok.dev/v1
    api-key: sk-...
    models:
      - name: gpt-5.6-luna
        verbosity: low   # low | medium | high
```

### 故障切换模式

挂在 **provider（`name`）** 上，不是全局。可重试错误（401/403/429/5xx）后：

```yaml
openai-completions:
  - name: relay-a
    base-url: https://a.example/v1
    failover-mode: provider   # 官方 → key；中转（如 ai.laysath.cn）→ provider
    api-key-entries:
      - api-key: sk-a1
      - api-key: sk-a2
    models:
      - { name: grok-4.5, alias: grok-4.5 }

  - name: relay-b
    base-url: https://b.example/v1
    # 省略 failover-mode => key
    api-key: sk-b1
    models:
      - { name: grok-4.5, alias: grok-4.5 }
```

| 模式 | 行为 |
|---|---|
| `key` | 换下一把未用 key（任意供应商） |
| `provider` | 跳过 **该 `name` 下全部 key**，切到另一家 |

多个 provider 使用相同客户端模型 alias 时会合并成一个 key 池。


### 渠道亲和

同一会话钉住同一上游 API key（保 prompt cache）。进程内存，**默认开启**。

默认 TTL **600s**；亲和失败允许换 key（`skip-retry` 默认 false）。

```yaml
# 默认全部：claude/gpt/gemini/grok/glm/kimi/qwen/minimax
# channel-affinity: true

# 只开部分模型族
channel-affinity: [claude, gpt, grok]

# 关闭
# channel-affinity: false
```

亲和身份总表：`internal/affinity/cli_sessions.go`。优先级：产品头（`X-Claude-Code-Session-Id`、`x-opencode-session`、`x-session-affinity`）→ Codex/Pi session/thread → `X-Session-Id` → `X-Client-Request-Id` → 协议 body（`/v1/messages` 归一化 `metadata.user_id`；responses/chat 用 `prompt_cache_key`）。详见 [渠道亲和与重试](docs/渠道亲和与重试.md)。无身份 → 普通轮询。

### 请求记录

| 字段 | 含义 |
|---|---|
| `enabled` | 默认 `false` |
| `backend` | `sqlite` 或 `postgres` |
| `retention` | Go duration，如 `168h`（7 天）；每小时清理 |
| `store-body` | 默认 `false`；为 true 时 body 截断 64KiB；**SSE 响应 body 不存储** |
| `sqlite.path` | 默认 `logs/requests.db` |
| `postgres.dsn` | postgres 后端必填 |

进程诊断日志仍只写 **stderr**（不进数据库）。

## Docker

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml

docker compose up -d --build
docker compose logs -f
docker compose down
```

挂载：

- `./config.yaml` → `/app/config.yaml`（只读）
- `./logs` → `/app/logs`（启用 sqlite 时）

监听 `8317`。默认时区 `Asia/Shanghai`（`TZ`）。

### Postgres 后端（可选）

1. 在 `docker-compose.yml` 取消注释 `postgres` 服务。
2. 在 `config.yaml` 中设置：

```yaml
request-log:
  enabled: true
  backend: postgres
  retention: "168h"
  store-body: false
  postgres:
    dsn: "postgres://litecpa:litecpa@postgres:5432/litecpa?sslmode=disable"
```

3. 取消注释 `lite-cpa` 的 `depends_on`。

### 普通 docker

```bash
docker build -t lite-cpa:local .
docker run --rm -p 8317:8317 \
  -v "$PWD/config.yaml:/app/config.yaml:ro" \
  -v "$PWD/logs:/app/logs" \
  lite-cpa:local
```

## 开发

```bash
gofmt -w .
go test ./...
go build -trimpath -ldflags='-s -w' -o lite-cpa ./cmd/lite-cpa
```

转换器目录：

```text
internal/translator/{from}/{to}/   # chat | claude | responses
```

## 性能说明

- 共享 `http.Transport`（HTTP/2、连接复用）
- 流式 scanner 上限 1MiB
- 每次尝试只做一次请求翻译
- 请求记录异步 + 有界队列；默认关闭
- 流式响应只记元数据（不累积完整响应 body）

## 贡献方式

欢迎贡献。

1. Fork 仓库并创建功能分支。
2. 保持改动小而聚焦。
3. 提交 PR 前运行：

```bash
gofmt -w .
go test ./...
go build -o /tmp/lite-cpa ./cmd/lite-cpa
```

4. 代码注释与 PR 描述优先使用英文。
5. 不要提交密钥（`config.yaml`、API Key、本地日志库）。
6. 涉及协议转换时，请补充或更新测试：
   - 同格式透传
   - 跨格式非流式
   - 跨格式流式（SSE 帧）

向 `main` 发起 Pull Request，并简要说明问题与修复。

## 许可证

MIT。版权声明：

- Luis Pater
- Router-For.ME（上游 CPA）
- Mieluoxxx

详见 [LICENSE](LICENSE)。
