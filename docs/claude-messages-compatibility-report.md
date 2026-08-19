# /v1/messages → /v1/chat/completions 协议转换兼容性报告

> 生成日期：2026-08-14
> 分析范围：opencode2api 网关的 Anthropic Messages API (Claude Code 入口) → OpenAI Chat Completions (上游) 转换
> 优先级目标：**Claude Code 兼容性**
> 原则：**无法实现的功能不返回 400 错误，静默忽略即可**

---

## 目录

1. [转换架构概览](#1-转换架构概览)
2. [请求侧兼容性](#2-请求侧兼容性)
3. [响应侧兼容性（非流式）](#3-响应侧兼容性非流式)
4. [流式响应兼容性](#4-流式响应兼容性)
5. [Thinking / Extended Thinking](#5-thinking--extended-thinking)
6. [错误处理与 400 策略](#6-错误处理与-400-策略)
7. [Claude Code 特定兼容性](#7-claude-code-特定兼容性)
8. [兼容性缺口汇总与建议](#8-兼容性缺口汇总与建议)

---

## 1. 转换架构概览

```
Claude Code 客户端
    │
    ├─ POST /v1/messages (Anthropic Messages 格式)
    │
    ├─ claudeMessagesHandler()          ← 入口
    │   ├─ extractUpstreamAuth()         ← 提取认证 (x-api-key / Bearer)
    │   ├─ json.Unmarshal → ClaudeRequest
    │   ├─ validateRequestTemperature()  ← temperature 0..1 校验
    │   ├─ validateClaudeDocumentBlocks() ← document payload 校验
    │   ├─ convertClaudeRequest()        ← Claude → OpenAI 请求转换
    │   │   ├─ claudeToOpenAIMessages()  ← messages + system → chat messages
    │   │   ├─ claudeToOpenAITools()     ← tools → function tools
    │   │   ├─ convertClaudeToolChoice() ← tool_choice 映射
    │   │   └─ ExtraBody 合并 (top_k, stop, parallel_tool_calls, user)
    │   ├─ buildUpstreamBody()           ← 组装最终 Chat Completions 请求
    │   │
    │   ├─ 上游 (OpenCode /v1/chat/completions)
    │   │
    │   ├─ 非流式: openAIToClaudeResponse()  ← Chat 响应 → Claude 响应
    │   └─ 流式: claudeStreamHandler()     ← Chat SSE → Claude SSE
    │       ├─ message_start → ping → content_block_* → message_delta → message_stop
    │       └─ 15s keepalive ping
```

### 数据来源

| 来源 | 类型 | 覆盖内容 |
|------|------|----------|
| 代码分析 | 直接 | `internal/app/claude.go`, `internal/app/chat.go`, `internal/app/anthropic_protocol.go`, `internal/app/chat_protocol.go` |
| Anthropic 官方文档 | wigolo fetch | Messages API 参考(62K)、stop_reasons(8K)、extended-thinking(13K)、prompt-caching(33K)、versioning(2K) |
| Claude Code 行为研究 | 子代理 | `docs/research-claude-code-behavior.md` (36K, 913行) |
| 开源代理项目研究 | 子代理 | `docs/research-proxy-projects.md` (32K, 678行) |
| 测试覆盖 | 直接 | 250+ 测试函数覆盖协议转换 |

---

## 2. 请求侧兼容性

### 2.1 ClaudeRequest 结构体字段覆盖

| Anthropic API 字段 | 结构体字段 | 转换处理 | 状态 |
|-------------------|-----------|---------|------|
| `model` | `Model` | `resolveModel()` + `mapPublicToFreeModel()` | ✅ |
| `messages` | `Messages` | `claudeToOpenAIMessages()` | ✅ |
| `system` | `System` (any) | `extractClaudeSystemText()` → 合并为首条 system message | ✅ |
| `max_tokens` | `MaxTokens` (*int) | 直接映射，可被 `max_tokens_cap` 裁剪 | ✅ |
| `temperature` | `Temperature` (*float64) | 校验 0..1，直接映射 | ✅ |
| `top_p` | `TopP` (*float64) | 直接映射 | ✅ |
| `top_k` | `TopK` (*int) | → `ExtraBody["top_k"]` | ✅ |
| `stream` | `Stream` (bool) | 直接映射 | ✅ |
| `tools` | `Tools` ([]ClaudeTool) | `claudeToOpenAITools()` → function tools | ✅ |
| `tool_choice` | `ToolChoice` (any) | `convertClaudeToolChoice()` | ✅ |
| `stop_sequences` | `StopSequences` ([]string) | → `ExtraBody["stop"]` | ✅ |
| `metadata` | `Metadata` (any) | `narrowClaudeMetadataUser()` → `ExtraBody["user"]` | ✅ |
| `thinking` | `Thinking` (any) | 归一化 adaptive→enabled，转发到上游 | ✅ |
| `output_config` | `OutputConfig` (any) | 提取 `effort` → `ReasoningEffort` | ✅ |
| `context_management` | `ContextManagement` (any) | 解析到结构体，**不转发** | ✅ (正确忽略) |

**结构体中缺失但 Claude Code 可能发送的字段：**

| 字段 | Claude Code 使用场景 | 当前行为 | 是否需要修改 |
|------|-------------------|---------|-------------|
| `service_tier` | 优先 vs 标准服务等级 | JSON unmarshal 自动忽略 | ❌ 不需要（正确忽略） |
| `speed` | `"fast"` 快速模式 | JSON unmarshal 自动忽略 | ❌ 不需要（正确忽略） |
| `task_budget` | 子代理预算限制 | JSON unmarshal 自动忽略 | ❌ 不需要（正确忽略） |
| `mcp_servers` | MCP 服务器配置 | JSON unmarshal 自动忽略 | ❌ 不需要（正确忽略） |

> **结论**：Claude Code 发送的所有请求字段要么已正确处理，要么被 JSON unmarshal 静默忽略。**无 400 风险。**

### 2.2 Content Block 类型处理

**请求侧（Claude → OpenAI 转换）已处理的 block 类型：**

| Block 类型 | 转换为 | 状态 |
|-----------|--------|------|
| `text` | `{type:"text", text:"..."}` content part | ✅ |
| `image` (base64/url) | `{type:"image_url", image_url:{url:...}}` | ✅ |
| `document` (base64/url) | `{type:"file", file:{...}}` | ✅ |
| `thinking` | 提取 thinking 文本 → `reasoning_content` | ✅ |
| `tool_use` | `tool_calls` 数组 | ✅ |
| `tool_result` | `{role:"tool", tool_call_id, content}` 消息 | ✅ |

**已知不处理的 block 类型（`claudeUnsupportedBlockTypes` 中列出）：**

| Block 类型 | 处理方式 | 400？ | 状态 |
|-----------|---------|-------|------|
| `redacted_thinking` | 计入 `unsupported_blocks`，不阻止请求 | ❌ 不返回 400 | ✅ 正确忽略 |
| `search_result` | 同上 | ❌ | ✅ |
| `server_tool_use` | 同上 | ❌ | ✅ |
| `web_search_tool_result` | 同上 | ❌ | ✅ |
| `container_upload` | 同上 | ❌ | ✅ |

**Anthropic API 中新增但未在扫描列表中的 block 类型：**

| Block 类型 | 当前行为 | 400？ | 建议 |
|-----------|---------|-------|------|
| `code_execution_tool_use` | 静默忽略（不在 `claudeUnsupportedBlockTypes` 中） | ❌ | ⚠️ 添加到扫描列表（可观测性） |
| `code_execution_tool_result` | 同上 | ❌ | ⚠️ 同上 |
| `mcp_tool_use` | 同上 | ❌ | ⚠️ 同上 |
| `mcp_tool_result` | 同上 | ❌ | ⚠️ 同上 |
| `bash_code_execution_tool_result` | 同上 | ❌ | ⚠️ 同上 |
| `web_fetch_tool_result` | 同上 | ❌ | ⚠️ 同上 |
| `tool_reference` | 同上 | ❌ | ⚠️ 同上 |

> **结论**：所有不支持的 block 类型都被静默忽略，**不返回 400**。但部分新类型未在 `claudeUnsupportedBlockTypes` 中列出，影响可观测性（日志中不会计数）。建议添加。

### 2.3 system 字段处理

| 场景 | 当前行为 | 状态 |
|------|---------|------|
| 字符串 system | 提取为 system message | ✅ |
| 数组 system（text blocks） | 合并为单个 system message（`\n` 连接） | ✅ |
| system block 中的 `cache_control` | 丢弃，计入 `cache_control_blocks` | ✅ (正确忽略) |
| Mid-conversation `role:system` message | 合并到 leading system message | ✅ |
| Billing header 在 system prompt 中 | 作为文本保留在 system message 中 | ✅ (可接受) |

### 2.4 tool_choice 映射

| Anthropic | OpenAI | 状态 |
|-----------|--------|------|
| `{type:"auto"}` | `"auto"` | ✅ |
| `{type:"any"}` | `"required"` | ✅ |
| `{type:"none"}` | `"none"` | ✅ |
| `{type:"tool", name:"X"}` | `{type:"function", function:{name:"X"}}` | ✅ |
| `disable_parallel_tool_use:true` | → `ExtraBody["parallel_tool_calls"]=false` | ✅ |

### 2.5 metadata.user_id 处理

| 场景 | 当前行为 | 状态 |
|------|---------|------|
| JSON 字符串含 `session_id` | 提取 `session_id`，丢弃 `device_id`/`account_uuid` | ✅ |
| 非 JSON 字符串 | 原样转发 | ✅ |
| 空值 | 不设置 `ExtraBody["user"]` | ✅ |

---

## 3. 响应侧兼容性（非流式）

### 3.1 ClaudeResponse 结构体

```go
type ClaudeResponse struct {
    ID         string          `json:"id"`
    Type       string          `json:"type"`
    Role       string          `json:"role"`
    Content    []ClaudeContent `json:"content"`
    Model      string          `json:"model"`
    StopReason string          `json:"stop_reason"`
    Usage      ClaudeUsage     `json:"usage,omitempty"`
}
```

**缺失字段：**

| Anthropic API 响应字段 | 当前状态 | 影响 | 建议优先级 |
|---------------------|---------|------|-----------|
| `stop_sequence` | ❌ 缺失 | Claude Code 期望在 `stop_reason:"stop_sequence"` 时读取此字段 | 🔴 高 |
| `stop_details` | ❌ 缺失 | `stop_reason:"refusal"` 时提供拒绝详情 | 🟡 中 |
| `id` 格式 | ✅ `msg_` 前缀 | 使用 `normalizeClaudeMessageID()` 规范化 | ✅ |

### 3.2 stop_reason 映射

| 上游 (OpenAI) | 输出 (Claude) | Anthropic 官方值 | 状态 |
|-------------|-------------|----------------|------|
| `stop` | `end_turn` | `end_turn` | ✅ |
| `length` | `max_tokens` | `max_tokens` | ✅ |
| `tool_calls` / `function_call` | `tool_use` | `tool_use` | ✅ |
| `content_filter` | `refusal` | `refusal` | ✅ |
| — | — | `stop_sequence` | ⚠️ 未映射（上游无对应） |
| — | — | `pause_turn` | ⚠️ 未映射（上游无对应） |
| — | — | `model_context_window_exceeded` | ⚠️ 未映射 |

> **结论**：`stop_sequence` 场景下（当 Claude Code 发送了 `stop_sequences` 且上游命中），当前不会返回 `stop_reason:"stop_sequence"`。但由于 `stop_sequences` 被转发到上游作为 `stop` 参数，上游返回 `finish_reason:"stop"`，我们映射为 `end_turn` 而非 `stop_sequence`。Claude Code 可能期望 `stop_sequence` 来读取 `stop_sequence` 字段确定哪个停止序列被命中。这是一个**低优先级缺口**，因为 Claude Code 主要使用 `stop_sequences` 作为可选功能。

### 3.3 Usage 字段映射

| 上游 (OpenAI) | 输出 (Claude) | 状态 |
|-------------|-------------|------|
| `prompt_tokens` | `input_tokens`（当存在命中/`cached_tokens` 时扣除命中部分，符合 Anthropic 三者互斥语义；Anthropic 风格 `cache_read_input_tokens` 来源不扣） | ✅ |
| `completion_tokens` | `output_tokens` | ✅ |
| `total_tokens` | 计算 `input_tokens + output_tokens` | ✅ |
| `prompt_tokens_details.cached_tokens` | `cache_read_input_tokens` | ✅ |
| `cache_creation_input_tokens` | `cache_creation_input_tokens` | ✅ (如有) |
| `completion_tokens_details` | `output_tokens_details` | ✅ |
| `server_tool_use` | `server_tool_use` | ✅ (如有) |
| `service_tier` | `service_tier` | ✅ (如有) |
| `inference_geo` | `inference_geo` | ✅ (如有) |

### 3.4 Content Block 输出（响应侧）

| 类型 | 非流式 | 流式 | 状态 |
|------|--------|------|------|
| `text` | ✅ ClaudeContent{Type:"text", Text:...} | ✅ content_block_start + text_delta | ✅ |
| `thinking` | ✅ ClaudeContent{Type:"thinking", Thinking:...} | ✅ content_block_start + thinking_delta | ✅ |
| `tool_use` | ✅ ClaudeContent{Type:"tool_use", ID, Name, Input} | ✅ content_block_start + input_json_delta | ✅ |
| `redacted_thinking` | ✅ 私有 roundtrip（如有） | ❌ 不发送 | ✅ (正确，无法伪造) |
| `signature_delta` | N/A | ❌ 不发送 | ✅ (正确，无法伪造) |

---

## 4. 流式响应兼容性

### 4.1 SSE 事件序列

当前实现的事件流：
```
event: message_start    → {type, message:{id, type, role, content:[], model, stop_reason:null, usage:{input_tokens:0, output_tokens:0}}}
event: ping              → {type:"ping"}
event: content_block_start → {type, index, content_block:{type, ...}}
event: content_block_delta → {type, index, delta:{type, ...}}
event: content_block_stop  → {type, index, content_block:{type}}
event: message_delta     → {type, delta:{stop_reason}, usage:{output_tokens}}
event: message_stop      → {type:"message_stop"}
```

**与 Anthropic 官方 SSE 规范对比：**

| 事件 | Anthropic 规范 | 当前实现 | 差异 |
|------|-------------|---------|------|
| `message_start` | ✅ 必须第一个 | ✅ | ✅ |
| `message_start.message.stop_sequence` | `null` 或匹配的停止序列 | ❌ 缺失 | 🔴 应添加 `stop_sequence: null` |
| `message_start.message.usage.input_tokens` | 实际 input token 数 | `0`（fullUsage 为空） | 🟡 低优先级 |
| `message_start.message.usage.output_tokens` | `1` 或更多 | `0` | 🟡 低优先级 |
| `ping` | 可选，keepalive | ✅ 紧随 message_start 发送 | ✅ |
| `content_block_start` | ✅ | ✅ | ✅ |
| `content_block_delta` (text_delta) | ✅ | ✅ | ✅ |
| `content_block_delta` (thinking_delta) | ✅ | ✅ | ✅ |
| `content_block_delta` (signature_delta) | ✅ | ❌ 不发送 | ✅ (正确，无法伪造) |
| `content_block_delta` (input_json_delta) | ✅ | ✅ | ✅ |
| `content_block_stop` | ✅ | ✅ | ✅ |
| `message_delta` | ✅ 含 `stop_reason` | ✅ | ✅ |
| `message_delta.delta.stop_sequence` | `null` 或匹配 | ❌ 缺失 | 🔴 应添加 |
| `message_delta.usage` | 累积 usage | ✅ `buildClaudeDeltaUsage()` | ✅ |
| `message_stop` | ✅ 最后一个 | ✅ | ✅ |
| `error` | 流式错误 | ✅ `emitClaudeError()` | ✅ |

### 4.2 Keepalive / 心跳

| 行为 | 当前实现 | Claude Code 期望 | 状态 |
|------|---------|----------------|------|
| Keepalive 间隔 | 15 秒 | 300 秒超时 | ✅ |
| Keepalive 事件类型 | `event: ping` + `data: {type:"ping"}` | 接受 ping 事件 | ✅ |
| 首个 token 前的 keepalive | ✅ 发送 ping（不发送 message_start） | 期望 keepalive | ✅ |

### 4.3 流式 Block 管理

| 场景 | 当前实现 | 状态 |
|------|---------|------|
| Text block 打开/关闭 | `textBlockOpen` 状态跟踪 | ✅ |
| Thinking block 打开/关闭 | `thinkingBlockOpen` 状态跟踪 | ✅ |
| Thinking → Text 切换 | 先 closeThinkingBlock()，再打开 text | ✅ |
| Text → Thinking 切换 | 先 closeTextBlock()，再打开 thinking | ✅ |
| 并行 tool_calls | `toolCallAccumulator` map 按 upstream index 跟踪 | ✅ |
| Tool block 顺序 | `toolCallOrder` 保持出现顺序 | ✅ |
| Empty reply fallback | `emitEmptyTextFallback()` — reasoning → text | ✅ |

### 4.4 Usage 在流式中的处理

| 场景 | 当前实现 | 状态 |
|------|---------|------|
| `stream_options.include_usage` | 自动添加到上游请求 | ✅ |
| Usage-only trailing chunk | 正确解析 usage，不处理 choices | ✅ |
| Usage after finish_reason | 等待 usage chunk 后再发 message_delta | ✅ |
| Cumulative usage merge | `mergeUsageMaps()` 递归合并 | ✅ |

---

## 5. Thinking / Extended Thinking

### 5.1 请求侧 Thinking 处理

| 场景 | 当前实现 | 状态 |
|------|---------|------|
| `thinking.type:"enabled"` + `budget_tokens` | 转发到上游 `thinking` 字段 | ✅ |
| `thinking.type:"adaptive"` | 归一化为 `enabled`，保留 budget/effort | ✅ |
| `thinking.type:"disabled"` | 不转发 thinking | ✅ |
| `output_config.effort` | 提取 → `reasoning_effort` | ✅ |
| `budget_tokens` → `reasoning_effort` 推导 | `reasoningEffortFromThinking()` | ✅ |

### 5.2 响应侧 Thinking 处理

| 场景 | 当前实现 | Claude Code 期望 | 状态 |
|------|---------|----------------|------|
| Thinking 内容输出 | `reasoning_content` → `thinking` block | ✅ | ✅ |
| Thinking signature | ❌ 不伪造 | 期望但无法实现 | ✅ (正确策略) |
| `redacted_thinking` | 私有 roundtrip 保留（如有） | 期望但上游无 | ✅ |
| 请求历史中的 signature | 丢弃，仅计数 | 不影响功能 | ✅ |
| 请求历史中的 redacted data | 丢弃 | 不影响功能 | ✅ |

### 5.3 Thinking 失败恢复

| 场景 | 当前行为 | Claude Code 行为 | 状态 |
|------|---------|----------------|------|
| 上游拒绝 `thinking` 字段 | 透传上游 400 错误 | 自动重试并禁用 thinking | ✅ |
| 上游拒绝 `adaptive` 类型 | 透传上游 400 错误 | 自动重试并禁用 | ✅ |

> **关键发现**（来自 Claude Code 行为研究）：Claude Code 对 thinking 字段拒绝的 400 错误**会自动重试并禁用**。我们的行为是透传上游错误，这是正确的——Claude Code 会自行处理恢复。

---

## 6. 错误处理与 400 策略

### 6.1 当前返回 400 的场景

| 场景 | 是否返回 400 | 是否应该 | 建议 |
|------|------------|---------|------|
| JSON 解析失败 | ✅ 400 | ✅ 应该 | 保持 |
| temperature 超出 0..1 | ✅ 400 | ✅ 应该 | 保持 |
| document 缺少可用 payload | ✅ 400 | ⚠️ 可讨论 | 可考虑降级为忽略 |
| 上游 400 | 透传上游状态码 | ✅ 应该 | 保持 |

### 6.2 不返回 400 的场景（正确忽略）

| 场景 | 当前行为 | 状态 |
|------|---------|------|
| 不认识的请求字段 | JSON unmarshal 自动忽略 | ✅ |
| `context_management` | 解析到结构体，不转发 | ✅ |
| `cache_control` | 丢弃，仅计数 | ✅ |
| `anthropic-beta` header | 仅计数，不转发 | ✅ |
| Server tools (web_search等) | 跳过，记入 `skipped_server_tools` | ✅ |
| 不支持的 content block 类型 | 计入 `unsupported_blocks`，不阻止请求 | ✅ |
| `service_tier`, `speed`, `task_budget` | JSON unmarshal 自动忽略 | ✅ |

### 6.3 错误响应格式

| 场景 | 格式 | 状态 |
|------|------|------|
| 协议校验错误 (temperature) | `{"type":"error","error":{"type":"invalid_request_error","message":"..."}}` | ✅ |
| 上游错误 (Claude 协议) | `{"type":"error","error":{"type":"...","message":"..."}}` | ✅ |
| 流式错误 | `event: error` + `{"type":"error","error":{"type":"api_error","message":"..."}}` | ✅ |
| 上游 Anthropic 格式错误 | `anthropicProtocolError` 透传 type/message | ✅ |

### 6.4 错误透传策略

| 场景 | 当前行为 | Claude Code 期望 | 状态 |
|------|---------|----------------|------|
| 上游 400 错误 | 透传状态码 + 重新包装为 Claude 格式 | 期望原样透传 | ⚠️ 可能影响恢复 |
| 上游 5xx 错误 | 透传状态码 + 通用 "upstream error" | 匹配错误措辞 | ✅ 可接受 |
| 流式 in-band error | `emitClaudeError()` | `event: error` | ✅ |

> **关键发现**（来自 Claude Code 行为研究）：Claude Code 的重试逻辑**匹配上游错误措辞**。如果代理将上游错误包装在自己的信封中，可能破坏恢复路径。但我们的上游是 OpenAI 格式，错误格式不同，所以重新包装为 Claude 格式是必要的。Claude Code 对 thinking 字段拒绝会自动重试，对 context_management 拒绝不重试——两者都是正确行为。

---

## 7. Claude Code 特定兼容性

### 7.1 SSE 流式转发

| 要求 | 当前实现 | 状态 |
|------|---------|------|
| 逐字节流式转发（不缓冲完整响应） | ✅ 逐行读取 + flush | ✅ |
| Keepalive ping 转发 | ✅ 15秒间隔自动 ping | ✅ |
| 300秒空闲超时 | 15秒 ping 间隔远小于 300秒 | ✅ |

### 7.2 Beta Header 处理

| 要求 | 当前实现 | 状态 |
|------|---------|------|
| 不允许列表 beta headers | 不转发到上游（上游是 OpenAI 格式） | ✅ |
| 视为开放列表 | 仅计数，不解析具体值 | ✅ |
| body 字段与 beta header 成对处理 | 同时移除 body 字段和 header | ✅ |

### 7.3 工具名称处理

| 要求 | 当前实现 | 状态 |
|------|---------|------|
| PascalCase 工具名透传 | 透传到上游 | ✅ |
| `mcp_` 前缀 | 透传 | ✅ |
| `todowrite` 黑名单 | 透传 | ✅ |

### 7.4 metadata.user_id 处理

| 要求 | 当前实现 | 状态 |
|------|---------|------|
| JSON 字符串解析 | `narrowClaudeMetadataUser()` | ✅ |
| 提取 `session_id` | ✅ | ✅ |
| 丢弃 `device_id` | ✅ | ✅ |

### 7.5 端点覆盖

| Claude Code 调用的端点 | 当前实现 | 状态 |
|---------------------|---------|------|
| `POST /v1/messages` | ✅ `claudeMessagesHandler` | ✅ |
| `POST /v1/messages?beta=true` | ✅ 同一 handler | ✅ |
| `POST /v1/messages/count_tokens` | ❌ 未实现 | 🟡 可选（Claude Code 可降级） |
| `GET /v1/models` | ✅ `listModelsHandler` | ✅ |
| `GET /v1/models/{id}` | ❌ 未实现 | 🟡 可选 |

---

## 8. 兼容性缺口汇总与建议

### 8.1 需要代码修改的缺口

| # | 缺口 | 严重性 | 影响范围 | 建议修改 |
|---|------|--------|---------|---------|
| **G1** | 非流式响应缺少 `stop_sequence` 字段 | 🔴 高 | 所有 Claude 响应 | 在 `ClaudeResponse` 结构体中添加 `StopSequence` 字段，值为 `null` |
| **G2** | 流式 `message_start` 缺少 `stop_sequence: null` | 🔴 高 | 流式响应 | 在 `ensureMessageStart` 中添加 `"stop_sequence": nil` |
| **G3** | 流式 `message_delta` 缺少 `stop_sequence` 字段 | 🔴 高 | 流式响应 | 在 `message_delta` 的 `delta` 中添加 `"stop_sequence": nil` |
| **G4** | 新增 server tool block 类型未在扫描列表中 | 🟡 中 | 可观测性 | 将 `code_execution_tool_use`, `mcp_tool_use`, `mcp_tool_result` 等添加到 `claudeUnsupportedBlockTypes` |
| **G5** | `stop_sequence` 场景的 stop_reason 映射 | 🟡 中 | stop_sequences 功能 | 当 `finish_reason:"stop"` 且请求包含 `stop_sequences` 时，考虑映射为 `stop_sequence` |
| **G6** | 非流式响应缺少 `stop_details` 字段 | 🟡 中 | refusal 场景 | 在 `ClaudeResponse` 中添加 `StopDetails` 字段 |

### 8.2 不需要代码修改的缺口（正确忽略）

| # | 缺口 | 原因 |
|---|------|------|
| `service_tier` / `speed` / `task_budget` 字段 | JSON unmarshal 自动忽略，正确行为 |
| `mcp_servers` 字段 | 同上 |
| Thinking signature 不伪造 | 无法伪造加密签名，正确策略 |
| `signature_delta` 不发送 | 同上 |
| `redacted_thinking` 请求历史丢弃 | 无法无损转换，正确策略 |
| `cache_control` 丢弃 | OpenAI 上游不支持，正确忽略 |
| `anthropic-beta` header 不转发 | 上游是 OpenAI 格式，正确忽略 |
| `context_management` 不转发 | 同上 |
| Server tools 跳过 | 同上 |
| `count_tokens` 端点未实现 | 可选功能，Claude Code 可降级 |

### 8.3 低优先级 / 可选改进

| # | 改进 | 优先级 | 说明 |
|---|------|--------|------|
| `message_start` 中 `input_tokens` 为 0 | 🟢 低 | OpenAI 上游在流式首 chunk 时不提供 input_tokens |
| `request-id` 响应头 | 🟢 低 | Claude Code 用于调试，不影响功能 |
| `anthropic-ratelimit-*` 响应头 | 🟢 低 | Claude Code 用于速率限制感知 |
| `count_tokens` 端点 | 🟢 低 | Claude Code 可降级为估算 |
| `GET /v1/models/{id}` | 🟢 低 | 可选功能 |

### 8.4 与其他代理项目的对比

| 特性 | opencode2api | LiteLLM | new-api | clawgate | copilot2api |
|------|:-----------:|:-------:|:-------:|:--------:|:-----------:|
| `/v1/messages` 入口 | ✅ | ✅ | ✅ | ✅ | ✅ |
| Claude → OpenAI 转换 | ✅ | ✅ | ✅ | ✅ | ✅ |
| 流式 SSE 转换 | ✅ | ✅ | ✅ | ✅ | ✅ |
| Thinking 处理 | ✅ (无签名) | ⚠️ (签名丢失) | ✅ (to-content) | ✅ | ✅ |
| `stop_sequence` 字段 | ❌ | ? | ? | ? | ? |
| Keepalive ping | ✅ (15s) | ✅ | ? | ✅ | ✅ |
| Error 透传 | ✅ | ✅ | ✅ | ✅ | ✅ |
| `cache_control` 处理 | ✅ (丢弃) | ⚠️ (条件保留) | ✅ | ❌ | ❌ |
| Server tools 跳过 | ✅ | ❌ | ? | ❌ | ❌ |
| `context_management` | ✅ (忽略) | ❌ | ? | ❌ | ❌ |

---

## 附录

### A. 研究文件索引

| 文件 | 大小 | 内容 |
|------|------|------|
| `docs/research-claude-code-behavior.md` | 36K | Claude Code v2.1.123 完整行为分析 |
| `docs/research-proxy-projects.md` | 32K | 16 个开源代理项目的转换实现分析 |
| `docs/responses-compatibility-analysis.md` | (已有) | Responses API 兼容性分析 |

### B. 代码文件索引

| 文件 | 行数 | 关键函数 |
|------|------|---------|
| `internal/app/anthropic_protocol.go` | 120 | `convertClaudeRequest()`, `convertClaudeToolChoice()` |
| `internal/app/chat_protocol.go` | 130 | `normalizeFinishReason()`, `writeProtocolValidation400()` |
| `internal/app/claude.go` | 拆分后 | `claudeMessagesHandler()`, `claudeToOpenAIMessages()`, `claudeStreamHandler()`, `openAIToClaudeResponse()` |

### C. Anthropic stop_reason 完整枚举

| 值 | 含义 | 上游映射 | 当前状态 |
|----|------|---------|---------|
| `end_turn` | 正常完成 | `stop` | ✅ |
| `max_tokens` | 达到 max_tokens | `length` | ✅ |
| `stop_sequence` | 命中停止序列 | `stop` (未区分) | ⚠️ |
| `tool_use` | 工具调用 | `tool_calls` | ✅ |
| `pause_turn` | 服务端工具迭代限制 | 无对应 | N/A |
| `refusal` | 模型拒绝 | `content_filter` | ✅ |
| `model_context_window_exceeded` | 上下文窗口超限 | 无对应 | N/A |

---

*本报告基于代码分析、Anthropic 官方文档、Claude Code 逆向工程研究和开源项目调研综合生成。*
