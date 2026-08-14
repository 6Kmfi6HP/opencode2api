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

## 请求校验

### temperature

| 入口 | 合法范围（闭区间） |
| --- | --- |
| `/v1/messages` | `0..1` |
| `/v1/chat/completions` | `0..2` |
| `/v1/responses` | `0..2` |

- 缺省（`nil`）、`0`、上界均合法；负数、越上界、`NaN`/`Inf` 在调用上游前被拒绝，不 clamp。
- 拒绝时返回 HTTP 400，`Content-Type: application/json`，形状与入口协议一致：
  - Claude：`{"type":"error","error":{"type":"invalid_request_error","message":...}}`
  - Chat/Responses：`{"error":{"type":"invalid_request_error","message":...,"param":"temperature"}}`
- 流式请求同样先返回普通 JSON 400，不会开始 SSE。

### 文件输入

- `document`（Anthropic）/ `input_file`（Responses）是 best-effort file part，映射为 `{type:"file",file:{...}}`。并非所有上游模型支持 file 模态，上游拒绝时会透传明确错误。
- 在受支持位置识别为文件输入但缺少任何可用 payload（无 `file_data`/`file_id`/`file_url` 或 document 无可用 `source`）时返回协议形状 HTTP 400 `invalid_request_error`，不会把 wrapper JSON 伪装成文本。

## Chat Completions

### 准确支持

- `model`
- `messages`
- `stream`
- `temperature`（闭区间 `0..2`）
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
- `instructions`、`messages`（使用 Chat content 形状，非 Responses `input` item 形状）、`previous_response_id`
- 显式零值的 `temperature`（闭区间 `0..2`）、`top_p`、`frequency_penalty`、`presence_penalty`
- `max_output_tokens`、`stop`、`user`、`parallel_tool_calls`、`stream_options`、`store`
- 函数工具、项目已有的内置工具、`tool_choice`、`reasoning`、`metadata`
- Anthropic-style `tool_result`（`call_id`，缺省时用 `tool_use_id`；`content` 支持 string、字符串数组、`{type:"text"|"input_text"|"output_text",text}` blocks；`is_error:true` 加 `Error: ` 前缀）
- 正常终态 `response.completed`；长度截断终态 `response.incomplete`，reason 为 `max_output_tokens`

### Best-effort

- Responses 会通过 Chat Completions 上游实现；内置工具被编码为函数工具后再还原。
- 仅在上游实际返回 reasoning 时生成 reasoning output item。
- `input` 中的 top-level item 或 message content 可使用 `input_file`；支持 flat 字段 `file_data`、`file_id`、`file_url`、`filename` 以及 nested `input_file` object，并映射为 `{type:"file",file:{...}}`。模型不支持 file 模态时上游可能拒绝。

### 不支持

- `include` 及未在上面列出的可选 Responses 字段；这些字段不会用占位值伪装成已支持。
- `input` 中受支持位置的 `input_file` 若缺少任何可用 payload，会返回 HTTP 400，不会序列化为文本。

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
- `stop_sequences`、`temperature`（闭区间 `0..1`）/ `top_p` / `top_k`（包括显式零值）
- `metadata.user_id`：若为 JSON 串则只转发 `session_id`（避免 `device_id` 外泄）；否则原样转发
- 文本、base64/URL image、`tool_use`、`tool_result`（包括 `is_error`）；合法的 tool result 在普通用户内容之前的顺序会被保留
- `tool_result` 中的 image 转为紧随其后的 `role=user` + `image_url`，tool 文本保留字符串并标注 `[image attached]`；`tool_result` 中的 document 同样转为紧随其后的 user file part 并标注 `[document attached]`
- `tool_choice` 的 `auto`、`any`、`tool`、`none`；`disable_parallel_tool_use:true` 映射为上游 `parallel_tool_calls=false`
- `output_config.effort` → 上游 `reasoning_effort`；`thinking.type=adaptive` 视为 enabled
- JSON Schema 约束字段（包括 `additionalProperties`、`format`）
- stop reason、usage 以及流式 content block 配对

### Best-effort / 显式丢弃（可观测）

- `document`（`source.type=base64`，默认 `application/pdf`；`source.type=url`）映射为 Chat content part `{type:"file",file:{...}}`，可保留 block/title 作为 `filename`。模型不支持 file 模态时上游可能拒绝；document 缺少可用 payload 时返回 HTTP 400。
- thinking 会在没有 signature 时继续输出，以提高客户端兼容性。代理不会伪造 signature 或发送假的 `signature_delta`。
- 请求历史中的 thinking `signature` 没有 Chat Completions 等价物，会被丢弃；代理仅统计历史中非空 signature block 数量到 `request_plan`（`history_signature_count`），不记录签名内容。
- `redacted_thinking.data` 是不可解释的加密数据，无法无损转成请求侧 reasoning；请求历史中的 redacted data 会被丢弃。native Anthropic 响应中明确存在的 signature / redacted data 由第二批私有 roundtrip 字段（`_opencode2api_anthropic_content`）保留，仅用于 Claude Messages 往返，Chat/Responses 公共 payload 不会泄漏这些私有字段。
- 无 Chat Completions 等价物的字段不进上游 body，但会绑定并记入 `request_plan` / body summary：`context_management`、`cache_control`、`anthropic-beta`、带 `type` 且无 `input_schema` 的 server tools（如 `web_search_*`）。
- `cache_control` breakpoints 不会被透传或实现；Chat 上游的自动前缀缓存无法表达 Anthropic TTL/breakpoint，`cached_tokens` usage 仅在上游提供时映射。请求中的 `cache_control` 计入 `cache_control_blocks` 并丢弃。

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
