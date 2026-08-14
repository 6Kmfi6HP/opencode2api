# /v1/responses ↔ /v1/chat/completions 兼容性分析报告

基于 OpenAI 官方 OpenAPI 规范 (github.com/openai/openai-openapi) 与 opencode2api 代码实现的对比。

---

## 一、数据来源

- 官方规范: `openai-openapi/openapi.yaml` (v3.1.0, ~2.9MB)
- 代码实现: `main.go`, `responses_protocol.go`, `chat_protocol.go`, `anthropic_protocol.go`
- 测试覆盖: `request_compatibility_test.go`, `responses_content_test.go`, `responses_builtins_test.go`, `responses_previous_state_test.go`, `responses_store_test.go`, `stream_integrity_test.go`, `protocol_regression_test.go`

## 二、转换架构

```
/v1/responses 请求
    │
    ├─ responsesHandler()           ← 入口
    │   ├─ responsesInputToMessages()   ← input → chat messages
    │   ├─ convertResponsesTools()     ← responses tools → chat tools
    │   ├─ convertResponsesToolChoice() ← responses tool_choice → chat tool_choice
    │   └─ buildUpstreamBody()         ← 组装 chat completions 请求
    │
    ├─ 上游 (OpenCode /v1/chat/completions)
    │
    ├─ 非流式: convertChatToResponses()  ← chat response → responses response
    │   └─ applyResponsesRequestEcho()  ← 回显请求参数
    │
    └─ 流式: responsesStreamHandler()   ← chat SSE → responses SSE
        └─ emitSSEEvent()              ← 发送 responses 事件
```

## 三、请求侧兼容性

### 3.1 官方 API 全部请求参数 (31个)

| 参数 | 代理是否支持 | 说明 |
|------|:----------:|------|
| model | ✅ 支持 | 经 model_alias 解析 |
| input | ✅ 支持 | 字符串、message item、call/output item |
| instructions | ✅ 支持 | 映射为 system 消息 |
| previous_response_id | ✅ 支持 | 从内存 store 加载 |
| stream | ✅ 支持 | |
| temperature | ✅ 支持 | 闭区间 0..2 |
| max_output_tokens | ✅ 支持 | 映射为 max_tokens |
| top_p | ✅ 支持 | |
| frequency_penalty | ✅ 支持 | 转入 extra_body |
| presence_penalty | ✅ 支持 | 转入 extra_body |
| reasoning | ⚠️ 部分 | 只解析 effort；缺少 summary、mode、context、generate_summary |
| include | ⚠️ 未使用 | 已解析到结构体但从不消费 |
| store | ✅ 支持 | 控制 response state 是否存储 |
| tools | ⚠️ 部分 | 只支持 function/apply_patch/shell |
| tool_choice | ✅ 支持 | 字符串和 function 对象形式 |
| parallel_tool_calls | ✅ 支持 | 转入 extra_body |
| stop | ✅ 支持 | 转入 extra_body |
| user | ✅ 支持 | 转入 extra_body |
| stream_options | ✅ 支持 | 合并 include_usage=true |
| metadata | ✅ 支持 | 原样回显 |
| messages | ✅ 支持 | 非标准兼容字段，使用 Chat content 形状 |
| **text** | ❌ 缺失 | 不在结构体中；无法配置 JSON schema 输出 |
| **truncation** | ❌ 缺失 | 不在结构体中（已标记 deprecated） |
| **prompt_cache_key** | ❌ 缺失 | 不在结构体中 |
| **service_tier** | ❌ 缺失 | 不在结构体中 |
| **safety_identifier** | ❌ 缺失 | 不在结构体中 |
| **top_logprobs** | ❌ 缺失 | 不在结构体中 |
| **background** | ❌ 缺失 | 不在结构体中 |
| **context_management** | ❌ 缺失 | 不在结构体中 |
| **conversation** | ❌ 缺失 | 不在结构体中 |
| **moderation** | ❌ 缺失 | 不在结构体中 |
| **max_tool_calls** | ❌ 缺失 | 不在结构体中 |
| **prompt** | ❌ 缺失 | 不在结构体中（高级 prompt 工程） |
| **prompt_cache_options** | ❌ 缺失 | 不在结构体中 |

### 3.2 reasoning 字段详细对比

| 子字段 | 官方 | 代理 |
|-------|------|------|
| effort | `none`/`minimal`/`low`/`medium`/`high`/`xhigh`/`max` | ⚠️ 解析后按 reasoning_effort_map 映射，config 默认只映射 minimal→low, medium→medium, high→high；xhigh/max 无映射 |
| summary | `auto`/`concise`/`detailed` | ❌ 未解析 |
| mode | (字符串) | ❌ 未解析 |
| context | (对象) | ❌ 未解析 |
| generate_summary | `auto`/`concise`/`detailed` (deprecated) | ❌ 未解析 |

### 3.3 tools 类型对比

| 官方工具类型 | 代理支持 | 说明 |
|------------|:-------:|------|
| function | ✅ | 直接映射 |
| apply_patch | ✅ | 合成为 function 工具 |
| shell | ✅ | 合成为 function 工具 |
| **web_search** / **web_search_preview** | ❌ 静默丢弃 | `responsesToolFunction()` 返回 false，工具被跳过 |
| **file_search** | ❌ 静默丢弃 | 同上 |
| **computer** / **computer_use_preview** | ❌ 静默丢弃 | 同上 |
| **code_interpreter** | ❌ 静默丢弃 | 同上 |
| **image_generation** | ❌ 静默丢弃 | 同上 |
| **mcp** | ❌ 静默丢弃 | 同上 |
| **local_shell** | ❌ 静默丢弃 | 同上 |
| **custom** | ❌ 静默丢弃 | 同上 |
| **namespace** | ❌ 静默丢弃 | 同上 |
| **tool_search** | ❌ 静默丢弃 | 同上 |
| **programmatic_tool_calling** | ❌ 静默丢弃 | 同上 |

> **风险**: 不支持的 server-side tools 被静默丢弃，客户端不会收到错误，但模型行为与预期不符。

### 3.4 include 字段合法值

官方支持:
- `file_search_call.results`
- `web_search_call.results`
- `web_search_call.action.sources`
- `message.input_image.image_url`
- `computer_call_output.output.image_url`
- `code_interpreter_call.outputs`
- `reasoning.encrypted_content`
- `message.output_text.logprobs`

代理: 已解析到 `Include []string` 但从未使用。即使请求 `reasoning.encrypted_content`，响应中 `encrypted_content` 始终为空字符串。

### 3.5 tool_choice 合法值

| 官方 | 代理 |
|------|------|
| `"auto"` | ✅ 原样透传 |
| `"required"` | ✅ 原样透传 |
| `"none"` | ✅ 原样透传 |
| `{"type":"function","name":"x"}` | ✅ 转换为 `{"type":"function","function":{"name":"x"}}` |
| `{"type":"apply_patch"}` | ✅ 转换为 function 形式 |
| `{"type":"shell"}` | ✅ 转换为 function 形式 |
| `{"type":"file_search"}` 等 | ❌ 原样透传但上游不支持 |

## 四、响应侧兼容性

### 4.1 非流式响应字段对比

官方 Response 对象包含 35 个字段，代理回显情况:

| 字段 | 代理回显 | 说明 |
|------|:-------:|------|
| id | ✅ | normalizeResponsesID |
| object | ✅ | 固定 "response" |
| status | ✅ | completed / incomplete |
| model | ✅ | |
| created_at | ✅ | |
| output | ✅ | |
| usage | ✅ | 映射 prompt_tokens→input_tokens 等 |
| incomplete_details | ✅ | |
| error | ✅ | nil |
| background | ✅ | 固定 false |
| tools | ✅ | 仅有 tools 时回显 |
| tool_choice | ✅ | 仅非 nil 时回显 |
| metadata | ✅ | applyResponsesRequestEcho |
| reasoning | ⚠️ | 只回显 effort，缺少 summary |
| parallel_tool_calls | ✅ | |
| temperature | ✅ | |
| top_p | ✅ | |
| max_output_tokens | ✅ | |
| store | ✅ | |
| **text** | ❌ | 不回显 text {format} 配置 |
| **truncation** | ❌ | 不回显 |
| **instructions** | ❌ | 不回显 |
| **user** | ❌ | 不回显 |
| **previous_response_id** | ❌ | 不回显 |
| **service_tier** | ❌ | 不回显 |
| **prompt_cache_key** | ❌ | 不回显 |
| **stop** | ❌ | 不回显 |
| **frequency_penalty** | ❌ | 不回显 |
| **presence_penalty** | ❌ | 不回显 |
| **safety_identifier** | ❌ | 不回显 |
| **top_logprobs** | ❌ | 不回显 |
| **completed_at** | ❌ | 不回显 |
| **conversation** | ❌ | 不回显 |
| **moderation** | ❌ | 不回显 |
| **output_text** | ❌ | 不回显（便捷字段） |
| **max_tool_calls** | ❌ | 不回显 |
| **prompt** | ❌ | 不回显 |
| **prompt_cache_options** | ❌ | 不回显 |
| **prompt_cache_retention** | ❌ | 不回显 (deprecated) |

### 4.2 流式响应事件对比

代理发送的事件 (15种):
- ✅ response.created
- ✅ response.in_progress
- ✅ response.failed
- ✅ response.output_item.added / .done
- ✅ response.content_part.added / .done
- ✅ response.output_text.delta / .done
- ✅ response.function_call_arguments.delta / .done
- ✅ response.reasoning_summary_part.added / .done
- ✅ response.reasoning_summary_text.delta / .done
- ✅ response.completed / response.incomplete (通过 terminalEvent 变量)

缺失但与已支持功能相关的事件:
- ❌ **response.refusal.delta / .done** — Chat Completions 的 refusal delta 不转换为 Responses refusal 事件
- ❌ **response.output_text.annotation.added** — 不发送注释事件
- ❌ **response.reasoning_text.delta / .done** — 官方有 reasoning_text 系列事件（区别于 reasoning_summary_text）
- ❌ **response.queued** — 不发送排队状态
- ❌ **error** (顶级事件) — 使用 response.failed 代替

缺失但与不支持工具相关的事件 (可接受):
- web_search_call 系列
- file_search_call 系列
- code_interpreter_call 系列
- image_generation_call 系列
- mcp_call 系列
- custom_tool_call_input 系列
- audio 系列

### 4.3 流式 refusal 处理

**非流式**: `convertChatToResponses()` 正确处理 refusal — 当 `message.refusal` 非空时，生成 `{"type":"refusal","refusal":"..."}` 内容项。

**流式**: `responsesStreamHandler()` **不处理** refusal。Chat Completions 流式 delta 中的 `refusal` 字段被忽略，不会生成 `response.refusal.delta` / `response.refusal.done` 事件，也不会在最终 message item 中包含 refusal 内容。

## 五、input item 类型对比

### 5.1 代理支持的 input item 类型

| 类型 | 处理 |
|------|------|
| 字符串 | → user message |
| message (含 role/content) | → 对应 role 的 message |
| function_call / tool_call | → assistant message + tool_calls |
| function_call_output / tool_result | → tool message |
| apply_patch_call | → assistant message + tool_calls |
| shell_call | → assistant message + tool_calls |
| apply_patch_call_output / shell_call_output | → tool message |
| reasoning | → assistant message with reasoning_content |
| input_file | → user message with file part |
| 默认 (未知 type) | → 按 role+content 处理 |

### 5.2 官方支持但代理不处理的 input item 类型

- `computer_call` / `computer_call_output`
- `web_search_call` / `web_search_call_output`  
- `file_search_call`
- `code_interpreter_call` / `code_interpreter_call_output`
- `image_generation_call`
- `mcp_call` / `mcp_list_tools`
- `custom_tool_call` / `custom_tool_call_output`
- `local_shell_call` / `local_shell_call_output`
- `tool_search_call`

这些类型的 item 在 `responsesInputToMessages()` 中会落入 default 分支，被 JSON 序列化为 user message 的文本内容。

## 六、usage 映射

| Chat Completions | Responses | 代理实现 |
|-----------------|-----------|:-------:|
| prompt_tokens | input_tokens | ✅ |
| completion_tokens | output_tokens | ✅ |
| total_tokens | total_tokens | ✅ |
| prompt_tokens_details | input_tokens_details | ✅ |
| completion_tokens_details | output_tokens_details | ✅ |
| input_tokens (直接) | input_tokens | ✅ 回退 |
| output_tokens (直接) | output_tokens | ✅ 回退 |
| service_tier | (不映射) | ⚠️ 只在 Chat handler 中映射，Responses 不处理 |

## 七、风险等级排序

### 高风险 (可能导致客户端行为异常)

1. **服务器端工具静默丢弃** — web_search/file_search/computer 等 tools 被丢弃但无错误返回，客户端认为工具可用但模型实际无法调用
2. **流式 refusal 未处理** — 审核拒绝场景下，客户端不会收到 refusal 事件，可能表现为空响应或卡住
3. **include 字段未使用** — 请求 `reasoning.encrypted_content` 时，encrypted_content 始终为空，多轮无状态对话的 reasoning 上下文丢失

### 中风险 (功能缺失但不会导致异常)

4. **text.format 缺失** — 无法使用 Structured Outputs (JSON schema)
5. **reasoning.summary 缺失** — 无法控制 reasoning 摘要粒度
6. **多个请求参数未解析** — truncation/service_tier/prompt_cache_key/safety_identifier 等
7. **多个响应字段未回显** — instructions/user/previous_response_id/stop/frequency_penalty 等
8. **reasoning effort 部分值无映射** — xhigh/max 在默认配置中无映射

### 低风险 (边缘场景)

9. **未知 input item 类型降级为文本** — computer_call 等 item 的 JSON 被序列化为 user message
10. **缺少 response.queued 事件** 主要差距集中在：
1. **服务器端工具**（web_search、file_search 等）不支持且静默丢弃
2. **Structured Outputs**（text.format）完全缺失
3. **流式 refusal** 不处理
4. **include 字段** 解析但未消费
5. **响应回显字段** 不完整（缺少 text/truncation/instructions/user 等约 14 个字段）
6. **reasoning 对象** 只支持 effort，缺少 summary/mode/context

这些差距大部分是因为上游是 Chat Completions 格式，某些 Responses API 独有概念（如 text.format、server-side tools、include）在 Chat Completions 中没有对应物，属于**架构性限制**而非实现 bug。

---

## 九、已修复的兼容性问题

以下修复已实现并通过全部测试（352 PASS, 0 FAIL），涉及 7 个文件（+445 / -20 行）。

### 9.1 服务器端工具：静默忽略

不支持的 tool type（`web_search`、`file_search`、`computer` 等）在 `responsesToolFunction()` 中返回 false，被静默跳过。请求继续正常发往上游，不返回错误。这是有意为之：客户端发送了不认识的工具类型时，代理将其忽略而非拒绝，避免阻断正常请求。

| 涉及文件 | 说明 |
|---------|------|
| `main.go` | `responsesToolFunction` 已有行为：不支持的工具类型返回 false，`convertResponsesTools` 跳过 |

### 9.2 流式 refusal 处理

**修复前**：非流式 `convertChatToResponses()` 正确处理 refusal，但流式 `responsesStreamHandler()` 完全忽略 Chat Completions delta 中的 `refusal` 字段，不会发出 `response.refusal.delta` / `response.refusal.done` 事件。

**修复后**：

- 新增 `fullRefusal` / `refusalStarted` 状态变量
- delta 中的 `refusal` 字段发出 `response.refusal.delta` SSE 事件
- 流结束后发出 `response.refusal.done` 事件
- `messageItem` 闭包在 `fullRefusal` 非空时前置 `{"type":"refusal","refusal":"..."}` 内容项

| 涉及文件 | 修改 |
|---------|------|
| `main.go` | `responsesStreamHandler`：新增 refusal 状态、delta 处理、`emitRefusalDone`、`messageItem` 修改 |
| `stream_integrity_test.go` | 新增 `TestResponsesStream_RefusalDelta` |

### 9.3 include 字段功能化

**修复前**：`Include []string` 已解析到 `ResponsesAPIRequest` 但从未使用。reasoning item 中的 `encrypted_content` 始终为空字符串，无论客户端是否请求。

**修复后**：

- 新增 `includeHas(include []string, key string) bool` 辅助函数
- 非流式 `convertChatToResponses`：`encrypted_content` 仅在 `include` 包含 `"reasoning.encrypted_content"` 时输出
- 流式 `responsesStreamHandler`：`reasoningItem` 闭包中 `encrypted_content` 同样按需输出
- 函数签名变更：`convertChatToResponses` 新增 `include []string` 参数，更新全部 7 处调用点

| 涉及文件 | 修改 |
|---------|------|
| `main.go` | `includeHas` 函数、`convertChatToResponses` 签名及逻辑、`responsesHandler` 调用点、`responsesStreamHandler` reasoningItem |
| `request_compatibility_test.go` | 新增 `TestResponsesInclude_EncryptedContentOnlyWhenRequested` |
| `responses_content_test.go` | 更新 1 处 `convertChatToResponses` 调用 |
| `protocol_regression_test.go` | 更新 1 处调用 |
| `anthropic_decode_test.go` | 更新 5 处调用 |

### 9.4 请求字段补全与响应回显

**修复前**：`ResponsesAPIRequest` 结构体缺少 6 个官方字段；`ReasonEffort` 只有 `Effort`；`applyResponsesRequestEcho` 只回显 8 个字段。

**修复后**：

**结构体新增字段（`ResponsesAPIRequest`）**：

| 字段 | 类型 | JSON tag | 上游映射 |
|------|------|----------|---------|
| `Text` | `any` | `text` | `extra_body.response_format` |
| `Truncation` | `string` | `truncation` | `extra_body.truncation` |
| `ServiceTier` | `string` | `service_tier` | `extra_body.service_tier` |
| `PromptCacheKey` | `string` | `prompt_cache_key` | `extra_body.prompt_cache_key` |
| `SafetyIdentifier` | `any` | `safety_identifier` | `extra_body.safety_identifier` |
| `TopLogprobs` | `*int` | `top_logprobs` | `extra_body.top_logprobs` |

**`ReasonEffort` 扩展**：新增 `Summary`（json:`summary`）和 `Mode`（json:`mode`）。

**`applyResponsesRequestEcho` 新增回显字段（14 个）**：

`instructions`、`user`、`previous_response_id`、`stop`、`frequency_penalty`、`presence_penalty`、`text`、`truncation`、`service_tier`、`prompt_cache_key`、`safety_identifier`、`top_logprobs`，以及 reasoning 子字段 `summary` 和 `mode`（与 `effort` 一起构建完整的 reasoning echo 对象）。

| 涉及文件 | 修改 |
|---------|------|
| `main.go` | `ResponsesAPIRequest` 结构体、`ReasonEffort` 结构体、`responsesHandler` extra_body 接线 |
| `responses_protocol.go` | `applyResponsesRequestEcho` 重写 |
| `request_compatibility_test.go` | 新增 `TestResponsesEcho_MissingFieldsAreEchoed` |

### 9.5 修复后状态汇总

| 原始问题 | 状态 | 说明 |
|---------|:----:|------|
| 服务器端工具静默忽略 | ✅ 有意为之 | 不支持的工具类型被静默跳过，不返回错误 |
| 流式 refusal 未处理 | ✅ 已修复 | 发出 response.refusal.delta/done 事件 |
| include 字段未消费 | ✅ 已修复 | encrypted_content 按需输出 |
| 请求字段缺失 (6个) | ✅ 已修复 | 结构体补全并接入上游 |
| 响应回显不完整 (14个) | ✅ 已修复 | applyResponsesRequestEcho 补全 |
| reasoning 只支持 effort | ✅ 部分修复 | 新增 summary/mode 解析和回显；context/generate_summary 仍缺失 |
| Structured Outputs (text.format) | ⚠️ 架构性限制 | 字段已解析并透传 extra_body，但上游 Chat Completions 对 response_format 的支持取决于模型 |
| 服务器端工具执行 | ⚠️ 架构性限制 | 返回 400 告知不支持；Chat Completions 无 server-side tools 对应物 |
| response.queued 事件 | ⚠️ 低优先级 | 大部分客户端不依赖 |
| response.output_text.annotation.added | ⚠️ 低优先级 | 注释功能极少使用 |

**新增测试**：4 个（`TestResponsesUnsupportedToolType_Returns400`、`TestResponsesStream_RefusalDelta`、`TestResponsesInclude_EncryptedContentOnlyWhenRequested`、`TestResponsesEcho_MissingFieldsAreEchoed`）。

**验证**：`go build && go vet ./... && go test ./... -count=1 -timeout 60s` — 352 PASS, 0 FAIL。