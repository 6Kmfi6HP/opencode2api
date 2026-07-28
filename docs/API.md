# API 兼容说明

服务默认监听 `http://127.0.0.1:8000`，客户端不需要传入真实 OpenAI 或 Anthropic API key，但可以通过 `Authorization` 指定 OpenCode 上游模式。

## 鉴权与上游选择

- 无 `Authorization`，或 `Bearer public`
  - 走 public Zen 免费模型。
  - `/v1/models` 只返回 `-free` 模型和免费别名。
- `Bearer <opencode-api-key>`
  - 默认走 Zen。
  - 如果请求的是仅存在于 Go 目录中的模型，代理会自动切到 Go。
- `Bearer zen:<opencode-api-key>`
  - 强制走 Zen。
- `Bearer go:<opencode-api-key>`
  - 优先走 Go 订阅目录。
  - 对同时存在于 Zen 和 Go 的模型，也会按 Go 路径请求。

## 路由

| 路由 | 方法 | 说明 |
| --- | --- | --- |
| `/v1/models` | `GET` | 返回上游模型和本地别名模型 |
| `/v1/chat/completions` | `POST` | OpenAI Chat Completions 兼容入口 |
| `/v1/responses` | `POST` | OpenAI Responses 兼容入口 |
| `/v1/messages` | `POST` | Anthropic Messages 兼容入口 |
| `/health` | `GET` | 健康检查 |
| `/api/config` | `GET`/`POST` | 管理面板配置接口 |
| `/api/stats` | `GET`/`DELETE` | token 统计接口 |
| `/api/reload` | `POST` | 刷新 OpenCode 会话和模型列表 |

`GET /v1/models` 的返回会随鉴权模式变化：

- `public` 只显示免费 Zen 模型。
- 默认或 `zen:` 模式显示 Zen 目录。
- `go:` 模式显示 Go 目录，并附带 public 可用的免费模型。

## Chat Completions

### 准确支持

- `model`
- `messages`
- `stream`
- `temperature`
- `max_tokens`
- `top_p`
- `thinking`
- `reasoning_effort`
- `extra_body`
- `tools`
- `tool_choice`

流式响应会原样保留合法的 usage-only 尾块（`choices: []`）以及完整 usage details。

### Best-effort

- 上游 Anthropic 响应会转换 stop reason、usage、reasoning、refusal 和工具调用。
- 不同上游模型对 `thinking` / `reasoning_effort` 的支持可能不同。

### 不支持

- 本项目未声明支持的 Chat Completions beta 字段不会被合成或伪造。

`model` 会先经过 `model_alias` 解析。`reasoning_effort` 会按 `reasoning_effort_map` 转换。

## Responses API

### 准确支持

- `input`、`instructions`、`messages`、`previous_response_id`
- 显式零值的 `temperature`、`top_p`、`frequency_penalty`、`presence_penalty`
- `max_output_tokens`、`stop`、`user`、`parallel_tool_calls`、`stream_options`
- 函数工具、项目已有的内置工具、`tool_choice`、`reasoning`、`metadata`
- 正常终态 `response.completed`；长度截断终态 `response.incomplete`，reason 为 `max_tokens`

### Best-effort

- Responses 会通过 Chat Completions 上游实现；内置工具被编码为函数工具后再还原。
- 仅在上游实际返回 reasoning 时生成 reasoning output item。

### 不支持

- 未在上面列出的可选 Responses 字段不会用占位值伪装成已支持。

示例：

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "input": "Write one short sentence.",
    "stream": false
  }'
```

## Anthropic Messages

### 准确支持

- `system`、`stop_sequences`、采样参数（包括显式零值）和 `metadata.user_id`
- 文本、base64/URL image、`tool_use`、`tool_result`（包括 `is_error`）
- `tool_choice` 的 `auto`、`any`、`tool`、`none`
- JSON Schema 约束字段（包括 `additionalProperties`、`format`）
- stop reason、usage 以及流式 content block 配对

### Best-effort

- thinking 会在没有 signature 时继续输出，以提高客户端兼容性。代理不会伪造 signature 或发送假的 `signature_delta`。
- `redacted_thinking.data` 是不可解释的加密数据，无法无损转成 reasoning；代理忽略该内容且不记录其数据。

### 不支持

- Anthropic beta、server tools、fallback 等本项目未承诺的扩展。

示例：

```bash
curl http://127.0.0.1:8000/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

## 流式响应

`stream: true` 时服务会使用 SSE 返回，并在内部清理空 delta、空 finish reason 和不需要的 reasoning 字段。Responses 和 Anthropic 流式接口会把上游 Chat Completions chunk 转换成对应事件。
