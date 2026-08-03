# API 兼容说明

服务默认监听 `http://127.0.0.1:8000`，客户端不需要传入真实 OpenAI 或 Anthropic API key，但可以通过 `Authorization` 指定 OpenCode 上游模式。

## 鉴权与上游选择

- 无 `Authorization`，或 `Bearer public`
  - 走 public Zen 免费模型。
  - `/v1/models` 只返回免费模型，且 ID 会去掉上游 `-free` 后缀（请求时会自动映射回 `-free`）。
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
| `/v1/models` | `GET` | 返回权限范围内的模型；`-free` 后缀会隐藏，已配置别名会替换对应上游模型 ID |
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

- 字符串 `input`；含 `input_text` / `input_image` 的 message item；函数及内置工具的 call/output item
- `instructions`、`messages`、`previous_response_id`
- 显式零值的 `temperature`、`top_p`、`frequency_penalty`、`presence_penalty`
- `max_output_tokens`、`stop`、`user`、`parallel_tool_calls`、`stream_options`、`store`
- 函数工具、项目已有的内置工具、`tool_choice`、`reasoning`、`metadata`
- 正常终态 `response.completed`；长度截断终态 `response.incomplete`，reason 为 `max_output_tokens`

### Best-effort

- Responses 会通过 Chat Completions 上游实现；内置工具被编码为函数工具后再还原。
- 仅在上游实际返回 reasoning 时生成 reasoning output item。

### 不支持

- `input_file` 等未在准确支持列表中的 input item 类型。
- `include` 及未在上面列出的可选 Responses 字段；这些字段不会用占位值伪装成已支持。

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

### 鉴权

- `Authorization: Bearer <key>` 与 `x-api-key: <key>` 同权；Bearer 优先。
- 有效 opencode key：`sk-` 前缀且长度 > 15；`go:` / `zen:` 前缀路由同样适用于两种头。
- Anthropic 真 key（`sk-ant-`）不会转发上游，回落 public。
- 占位短 key（如 Claude Code 默认 `sk-local`）因长度不足走 public。

### 准确支持

- `system`（顶层）与消息内 `role=system` 合并为上游**唯一首条** system（`\n\n` 拼接，顶层在前）
- `stop_sequences`、`temperature` / `top_p` / `top_k`（包括显式零值）
- `metadata.user_id`：若为 JSON 串则只转发 `session_id`（避免 `device_id` 外泄）；否则原样转发
- 文本、base64/URL image、`tool_use`、`tool_result`（包括 `is_error`）；合法的 tool result 在普通用户内容之前的顺序会被保留
- `tool_result` 中的 image 转为紧随其后的 `role=user` + `image_url`，tool 文本保留字符串并标注 `[image attached]`
- `tool_choice` 的 `auto`、`any`、`tool`、`none`；`disable_parallel_tool_use:true` 映射为上游 `parallel_tool_calls=false`
- `output_config.effort` → 上游 `reasoning_effort`；`thinking.type=adaptive` 视为 enabled
- JSON Schema 约束字段（包括 `additionalProperties`、`format`）
- stop reason、usage 以及流式 content block 配对

### Best-effort / 显式丢弃（可观测）

- thinking 会在没有 signature 时继续输出，以提高客户端兼容性。代理不会伪造 signature 或发送假的 `signature_delta`。
- `redacted_thinking.data` 是不可解释的加密数据，无法无损转成 reasoning；代理忽略该内容且不记录其数据。
- 无 Chat Completions 等价物的字段不进上游 body，但会绑定并记入 `request_plan` / body summary：`context_management`、`cache_control`、`anthropic-beta`、带 `type` 且无 `input_schema` 的 server tools（如 `web_search_*`）。

### 不支持（不实现语义）

- prompt caching、context management、server tools 的真实能力
- 转发 `anthropic-beta` 到上游

示例：

```bash
curl http://127.0.0.1:8000/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: sk-your-opencode-key" \
  -d '{
    "model": "gpt-4o-mini",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

## 流式响应

`stream: true` 时服务会使用 SSE 返回，并在内部清理空 delta、空 finish reason 和不需要的 reasoning 字段。Responses 和 Anthropic 流式接口会把上游 Chat Completions chunk 转换成对应事件。
