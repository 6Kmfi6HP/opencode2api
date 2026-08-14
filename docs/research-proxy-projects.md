# 开源项目处理 Anthropic Messages API 与 OpenAI Chat Completions API 转换的研究报告

> 研究日期：2026-08-14
> 研究方法：使用 wigolo 搜索引擎和页面抓取工具，系统调研 GitHub、PyPI、DeepWiki 等平台上的开源项目实现

---

## 目录

1. [LiteLLM](#litellm)
2. [one-api / new-api](#one-api--new-api)
3. [其他相关项目](#其他相关项目)
4. [关键发现](#关键发现)

---

## LiteLLM

**项目地址**：https://github.com/BerriAI/litellm
**文档**：https://docs.litellm.ai/docs/providers/anthropic
**语言**：Python（Rust 核心）
**定位**：统一的 AI 网关，通过 OpenAI 格式调用 100+ LLM 提供商

### 架构概述

LiteLLM 提供两条 Anthropic 相关的转换路径：

| 端点 | 格式 | 用途 |
|------|------|------|
| `/v1/chat/completions` | OpenAI 格式 | 统一入口，将 Anthropic 响应转换为 OpenAI 格式返回 |
| `/v1/messages` | Anthropic 原生格式 | 透传端点（passthrough），保持 Anthropic 原生格式 |

关键代码文件：
- `litellm/llms/anthropic/chat/transformation.py` — 主转换器（OpenAI → Anthropic 请求构建）
- `litellm/llms/anthropic/experimental_pass_through/adapters/transformation.py` — Anthropic Messages ↔ OpenAI 双向适配器（`AnthropicAdapter` / `LiteLLMAnthropicMessagesAdapter`）
- `litellm/llms/anthropic/experimental_pass_through/adapters/streaming_iterator.py` — 流式转换包装器（`AnthropicStreamWrapper`）

### LiteLLM 对 content blocks 的处理方式

LiteLLM 的 `LiteLLMAnthropicMessagesAdapter.translate_anthropic_messages_to_openai()` 方法逐条处理消息：

| Anthropic content block | OpenAI 映射 | 说明 |
|------------------------|-------------|------|
| `{"type": "text", "text": "..."}` | `{"type": "text", "text": "..."}` | 直接映射为 OpenAI content part |
| `{"type": "image", "source": {...}}` | `{"type": "image_url", "image_url": {"url": "data:..."}}` | 转换为 data URI 格式 |
| `{"type": "document", "source": {...}}` | `{"type": "image_url", "image_url": {"url": "data:..."}}` | PDF/文档也转为 image_url |
| `{"type": "tool_result", "tool_use_id": "...", "content": "..."}` | `{"role": "tool", "tool_call_id": "...", "content": "..."}` | 转为 tool 角色消息 |

**关键细节**：
- 当 `tool_result` 的 `content` 是数组时，LiteLLM 将所有 content items 合并为**单个** tool 消息，以避免创建多个相同 ID 的 tool_result 块（Anthropic 要求每个 `tool_use` 必须恰好有一个 `tool_result`）
- `system` 参数（字符串或 blocks 数组）被翻译为 OpenAI `messages` 数组的第一条 `role: "system"` 消息
- 中途出现的 `role: "system"` 消息也通过 `_translate_midturn_system_message_to_openai()` 处理

### LiteLLM 对 tool_use / tool_result 的转换策略

**请求方向（Anthropic → OpenAI）**：
- Anthropic `tools` 定义中的 `input_schema` → OpenAI `function.parameters`
- Anthropic `tool_choice`（`auto` / `any` / `tool`）→ OpenAI `tool_choice`（`auto` / `required` / 指定 function）
- 工具名长度差异处理：OpenAI 限制 64 字符，Anthropic 限制 128 字符
  - `truncate_tool_name()` 使用 `{55字符前缀}_{8字符hash}` 格式截断
  - 构建 `tool_name_mapping` 用于响应时恢复原始名称
- Anthropic 工具名必须匹配 `^[a-zA-Z0-9_-]{1,128}$`，非法字符需替换
  - 使用 per-request 的前向/反向映射解决冲突（避免 `foo/bar` 和 `foo_bar` 都映射到 `foo_bar`）

**响应方向（OpenAI → Anthropic）**：
- OpenAI `tool_calls`（assistant 消息上的 `function` 调用）→ Anthropic `tool_use` content blocks
- `convert_tool_use_to_openai_format()` 方法将 Anthropic tool_use 转为 `ChatCompletionToolCallChunk`
- 支持程序化工具调用（`caller` 字段透传）

### LiteLLM 对 thinking / reasoning 的处理

**这是 LiteLLM 已知的最严重问题。**

**核心发现**（来源：GitHub Issue #27946 + shekohex 的 gist 文档）：

1. **thinking 块存储方式**：当 LiteLLM 将 Anthropic `/v1/messages` 的 assistant 响应（含 `thinking` 块）转换为 OpenAI Chat Completions 格式时，thinking 块被存储在自定义的 `thinking_blocks` 字段中，而**标准的 `reasoning_content` 字段未被设置**。

2. **多轮对话失败**：这导致多轮对话中，上游 API（如 DeepSeek、OpenAI o-series）报错：
   ```
   The `reasoning_content` in the thinking mode must be passed back to the API.
   ```

3. **修复方案**（Issue #27946 提出的修复）：
   ```python
   if len(thinking_blocks) > 0:
       assistant_message["thinking_blocks"] = thinking_blocks
       first_thinking = thinking_blocks[0]
       assistant_message["reasoning_content"] = first_thinking.get("thinking", "")
   ```

4. **扩展思考（Extended Thinking）的签名问题**（来源：shekohex gist）：
   - Anthropic 的 thinking 块包含**加密签名**（signature），由 Anthropic 服务器生成
   - 多轮对话中，之前的 assistant 消息（含 thinking 块）必须原样发回 API
   - Anthropic **验证签名**以确保 thinking 块未被篡改
   - **格式转换会丢失或损坏签名**，导致：
     ```
     Invalid `signature` in `thinking` block
     Expected `thinking` or `redacted_thinking`, but found `text`.
     When `thinking` is enabled, a final `assistant` message must start with a thinking block
     ```

5. **失败的解决方案尝试**：
   - 尝试创建 `reasoning_adapter.py` 回调来缓存/恢复 thinking 块 → 失败（无法伪造有效签名）
   - 尝试注入带假签名的占位 thinking 块 → 失败（Anthropic 服务端验证签名）

6. **最终解决方案**：**双提供商配置** — 对 Anthropic 模型使用 `/v1/messages` 原生端点（避免格式转换），对其他模型使用 `/chat/completions`。

7. **关键结论**：**任何 Anthropic ↔ OpenAI 格式转换都会破坏扩展思考功能**，因为签名是加密的且无法伪造。

### LiteLLM 对 streaming 事件的转换

LiteLLM 使用 `AnthropicStreamWrapper` 将 OpenAI SSE 流转换为 Anthropic SSE 事件序列：

**OpenAI → Anthropic 事件映射**：

| OpenAI SSE | Anthropic SSE 事件 |
|-----------|-------------------|
| 第一个 chunk（含 id, model） | `message_start` |
| — | `ping`（心跳） |
| `delta.reasoning_content` chunks | `content_block_start`（type: thinking）+ `content_block_delta`（thinking_delta）× N + `content_block_stop` |
| `delta.content` text chunks | `content_block_start`（type: text）+ `content_block_delta`（text_delta）× N + `content_block_stop` |
| `delta.tool_calls` | `content_block_start`（type: tool_use）+ `content_block_delta`（input_json_delta）× N + `content_block_stop` |
| `finish_reason` + `usage` | `message_delta`（含 stop_reason, usage）+ `message_stop` |

**关键实现细节**：
- 流式转换是**有状态的** — 需要跟踪当前 block index、是否有 thinking block 打开、多工具执行状态
- 当从 thinking block 切换到 text block 时，需要先关闭 thinking block（`content_block_stop`），再打开新的 text block
- 支持异步和同步两种模式（`is_async` 参数）

### LiteLLM 对 cache_control 的处理

- `_add_cache_control_if_applicable()` 方法在转换时**有条件地保留** `cache_control`
- **仅在目标模型是 Anthropic Claude 模型或 Bedrock ARN 模型时才保留**
- 对于 OpenAI 等其他提供商，`cache_control` 会被丢弃
- LiteLLM 支持在 system 消息、tool 定义、对话历史中放置 `cache_control`
- Anthropic 不再需要 `anthropic-beta: prompt-caching-2024-07-31` header，使用 `cache_control` 即自动生效

### LiteLLM 的已知问题和限制

1. **thinking 块签名丢失**（最严重）：格式转换会破坏 extended thinking 的加密签名
2. **reasoning_content 未设置**（Issue #27946）：thinking 块存入 `thinking_blocks` 但未设 `reasoning_content`
3. **cache_control 丢失**：OpenAI 格式请求不携带 `cache_control`，转换到 Anthropic 时无缓存控制
4. **工具名长度限制**：OpenAI 64 字符 vs Anthropic 128 字符，需截断+映射
5. **工具名字符限制**：Anthropic 要求 `^[a-zA-Z0-9_-]{1,128}$`，MCP 工具名常含 `/` 或 `.`
6. **max_tokens 必填**：Anthropic 要求 `max_tokens`，LiteLLM 在未提供时默认设为 4096
7. **top_k 不支持**：OpenAI Chat API 不支持 `top_k`，转换时丢弃
8. **structured outputs**：需要添加 `anthropic-beta: structured-outputs-2025-11-13` header，通过创建工具+强制使用实现

---

## one-api / new-api

### one-api

**项目地址**：https://github.com/songquanpeng/one-api
**语言**：Go
**定位**：通过标准 OpenAI API 格式访问所有大模型的网关

#### 是否支持 Anthropic Messages API 入口

**不支持。** one-api 是以 OpenAI 格式为中心的网关。虽然它支持将 Anthropic Claude 作为**后端提供商**（即 OpenAI 格式请求 → 转换为 Anthropic 格式 → 调用 Claude API），但它**不提供 Anthropic Messages API 入口端点**（即不支持 `/v1/messages` 端点接收 Anthropic 格式请求）。

GitHub Issue #2322（"添加 Anthropic API 协议支持"）明确请求：
- 添加 `/anthropic/v1/messages` 路由端点
- 实现 Anthropic 协议适配器
- 支持请求直通到第三方 anthropic 端点（如 DeepSeek 的 `https://api.deepseek.com/anthropic/v1/messages`）

该 issue 的核心需求是让 Claude Code 通过 Anthropic 协议请求 one-api，但截至研究时，one-api 尚未原生支持此功能。

#### 对 Claude Code 的兼容性

由于不支持 Anthropic Messages API 入口，**one-api 无法直接作为 Claude Code 的后端**。用户需要：
- 使用 new-api（见下文）或其他支持 Anthropic Messages 入口的项目
- 或在 one-api 前面部署一个 Anthropic → OpenAI 的转换代理

### new-api

**项目地址**：https://github.com/QuantumNous/new-api（原 Calcium-Ion/new-api）
**文档**：https://docs.newapi.pro/en/docs
**语言**：Go
**定位**：下一代 LLM 网关和 AI 资产管理系统，完全兼容 one-api 数据库

#### 是否支持 Anthropic Messages API 入口

**支持。** new-api 是 one-api 的增强分支，明确支持：

| 功能 | 说明 |
|------|------|
| Claude Messages 端点 | `POST /v1/messages`（Claude Chat） |
| 格式转换 | OpenAI Compatible ⇄ Claude Messages（双向） |
| Thinking-to-content | 将 thinking 内容转为 content 的功能 |
| Claude thinking 模型 | 如 `claude-3-7-sonnet-20250219-thinking` |
| 缓存计费 | 支持 Claude 模型的 cache billing 统计 |

#### 转换处理方式

new-api 的核心能力之一是 **OpenAI Compatible ⇄ Claude Messages** 双向格式转换：

1. **入口端点**：`POST /v1/messages` 接收 Anthropic 格式请求
2. **后端路由**：可将请求路由到 OpenAI 兼容后端（通过格式转换）或 Anthropic 原生后端（直通）
3. **Claude thinking 模型**：通过模型名后缀（如 `-thinking`）启用思考模式
4. **Reasoning Effort 支持**：
   - OpenAI 系列：`o3-mini-high/medium/low`、`gpt-5-high/medium/low`
   - Claude 系列：`claude-3-7-sonnet-20250219-thinking`
   - Gemini 系列：`gemini-2.5-flash-thinking`、`gemini-2.5-pro-thinking-128`

#### 对 Claude Code 的兼容性

new-api 可以作为 Claude Code 的后端：
- 设置 `ANTHROPIC_BASE_URL` 指向 new-api 实例
- new-api 的 `/v1/messages` 端点接收 Anthropic 格式请求
- 支持将请求转换到 OpenAI 兼容后端或直通到 Anthropic 后端

#### 已知的兼容性问题和解决方案

1. **Thinking 模型映射**：通过模型名后缀 `-thinking` 来启用思考模式，但这是一种约定而非标准
2. **格式转换的通用问题**：与 LiteLLM 类似，thinking 块的签名问题在 OpenAI ⇄ Claude 转换中同样存在
3. **Thinking-to-content 功能**：new-api 提供了一种将 thinking 内容直接转为 content 的变通方案，绕过签名验证问题

---

## 其他相关项目

### 1. clawgate

**项目地址**：https://clawgate.org/
**语言**：Go（~6MB 单二进制）
**定位**：Anthropic → OpenAI 单向代理，专为 Claude Code 设计

**特点**：
- 单二进制，零依赖，跨平台
- 双模式：ChatGPT OAuth 订阅 或 OpenAI API Key
- 完整流式 SSE 支持（含错误处理、并行工具调用、reasoning model 支持）
- 完整 `tool_use` 往返转换，包括并行工具调用，MCP 工具开箱即用
- 支持任何 OpenAI Chat Completions 端点（OpenAI、Azure、vLLM、Ollama）

**使用方式**：
```bash
clawgate login && clawgate
ANTHROPIC_BASE_URL=http://localhost:8082 claude
```

### 2. ant2oai (a2o)

**项目地址**：https://github.com/WqyJh/a2o（PyPI: `ant2oai`）
**语言**：Python（FastAPI + httpx）
**定位**：轻量级 Anthropic → OpenAI 代理

**转换映射表**：

| Anthropic | OpenAI |
|-----------|--------|
| system（string/blocks） | 第一条 system 消息 |
| messages[].content blocks | messages[].content parts |
| tool_use content blocks | assistant 消息上的 tool_calls |
| tool_result content blocks | tool 角色消息 |
| thinking blocks | reasoning_content 字段 |
| max_tokens | max_tokens |
| stop_sequences | stop |
| Streaming SSE events | Streaming SSE chunks |

**特点**：
- 流式（SSE）和非流式支持
- 工具调用（function calling）完整往返转换
- 扩展思考/reasoning 内容支持
- 图片内容（base64 和 URL）
- 多 worker 部署 + 连接池

### 3. local-openai2anthropic (OA2A)

**项目地址**：https://github.com/dongfangzan/local-openai2anthropic（PyPI: `local-openai2anthropic`）
**语言**：Python 3.12+
**定位**：Anthropic 和 OpenAI 生态之间的双向桥接代理

**三种运行模式**：

| 模式 | 端点 | 用途 |
|------|------|------|
| Anthropic Proxy | `POST /v1/messages` | Claude SDK / Claude Code → 任意 OpenAI 后端 |
| OpenAI Passthrough | `POST /v1/chat/completions` | OpenAI 原生客户端直接透传 |
| Responses Bridge | `POST /v1/responses` | OpenAI Responses SDK → chat-completions-only 后端 |

**特点**：
- 交错思考（Interleaved Thinking）：支持 `thinking` 块 + `chat_template_kwargs` + `reasoning_effort`
- 通配符模型名映射（Anthropic 模型名 → 后端模型名）
- 服务端 Web 搜索（Tavily / 通晓）
- Daemon + Web Dashboard
- Docker Compose 集成 Claude Code

### 4. anthropic-proxy-rs

**项目地址**：https://github.com/m0n0x41d/anthropic-proxy-rs
**语言**：Rust（~3MB 二进制）
**定位**：高性能 Anthropic → OpenAI 代理

**特点**：
- 扩展思考模式自动路由：检测 `thinking` 参数，路由到 `REASONING_MODEL`
- 模型映射：`ANTHROPIC_PROXY_MODEL_MAP`
- API Key 透传：从 `x-api-key` header 提取
- 系统提示词清理：移除上游可能被 WAF 拦截的术语

**已知限制**（明确列出）：
- `tool_choice` 参数不支持（始终用 `auto`）
- `service_tier` 参数不支持
- `metadata` 参数不支持
- `context_management` 参数不支持
- `container` 参数不支持
- 响应中的 Citations 不支持
- `pause_turn` 和 `refusal` stop reasons 不支持
- Message Batches API / Files API / Admin API 不支持

### 5. anthropic_openai_bridge

**项目地址**：https://github.com/dliedtka/anthropic_openai_bridge
**语言**：Python（库，非代理）
**定位**：Python 库，使用 Anthropic Messages API 格式与 OpenAI 兼容服务通信

**特点**：
- 返回原生 `anthropic.types.Message` 对象
- 完整流式支持（`message_start` → `content_block_delta` → `message_stop`）
- 工具调用支持（`tool_use` ↔ `function_call`）
- 响应链式调用（支持将 `response.content` 直接传给后续请求）
- 异步支持

### 6. claude-code-proxy

**项目地址**：https://github.com/fuergaosi233/claude-code-proxy
**语言**：Python
**定位**：Claude Code → OpenAI 兼容 API 代理

**模型映射策略**：

| Claude 请求 | 映射到 | 环境变量 |
|------------|--------|---------|
| 含 "haiku" 的模型 | SMALL_MODEL | 默认: gpt-4o-mini |
| 含 "sonnet" 的模型 | MIDDLE_MODEL | 默认: BIG_MODEL |
| 含 "opus" 的模型 | BIG_MODEL | 默认: gpt-4o |

**特点**：
- 完整 `/v1/messages` 端点支持
- 自定义 HTTP headers 注入（`CUSTOM_HEADER_*`）
- 函数调用完整转换
- 流式 SSE 响应
- Base64 图片输入

### 7. claude-code-provider-proxy

**项目地址**：https://github.com/ujisati/claude-code-provider-proxy
**语言**：Python（FastAPI）
**定位**：Claude Code → OpenRouter/任意 OpenAI 兼容端点

**详细的转换映射文档**（`docs/mapping.md`）提供了字段级别的完整映射：

**请求参数映射**：

| Anthropic 参数 | OpenAI 参数 | 说明 |
|---------------|-------------|------|
| `model` | `model` | 动态模型映射 |
| `system`（string） | `messages[0]`（role: system） | 作为第一条消息 |
| `messages` | `messages` | 角色+内容结构需仔细转换 |
| `max_tokens` | `max_tokens` | 直接映射 |
| `stop_sequences` | `stop` | 直接映射 |
| `stream` | `stream` | 直接映射 |
| `temperature` | `temperature` | 直接映射 |
| `top_p` | `top_p` | 直接映射 |
| `top_k` | 不支持 | **丢弃**（OpenAI 不支持） |
| `metadata.user_id` | `user` | 映射到顶层 user 字段 |
| `tools` | `tools` | 工具定义转换 |
| `tool_choice` | `tool_choice` | 工具选择转换 |
| `stream_options` | 不直接支持 | 需代理处理 usage 上报 |

**Stop/Finish Reason 映射**：

| Anthropic `stop_reason` | OpenAI `finish_reason` |
|------------------------|----------------------|
| `end_turn` | `stop` |
| `max_tokens` | `length` |
| `tool_use` | `tool_calls` |

**Usage 映射**：

| Anthropic | OpenAI |
|-----------|--------|
| `input_tokens` | `prompt_tokens` |
| `output_tokens` | `completion_tokens` |

### 8. Olla

**项目地址**：https://github.com/thushan/olla
**文档**：https://thushan.github.io/olla/api-reference/anthropic/
**语言**：Go
**定位**：本地 LLM 基础设施网关，支持 Anthropic Messages API

**双模式架构**：

| 模式 | 说明 |
|------|------|
| Passthrough（优先） | 后端原生支持 Anthropic API 时，直接透传，零转换开销 |
| Translation（回退） | 后端仅支持 OpenAI 格式时，进行双向翻译 |

**Thinking 块处理**：
- 从后端的 `reasoning`/`reasoning_content` 字段构建 thinking 块
- 本地模型不产生 Anthropic 的 `signature`，所以 thinking 块不带签名
- 支持 `reasoning`（Ollama, LM Studio）和 `reasoning_content`（vLLM, SGLang, DeepSeek）两种字段名

### 9. copilot2api

**项目地址**：https://github.com/whtsky/copilot2api
**DeepWiki**：https://deepwiki.com/whtsky/copilot2api/5.3-anthropic-openai-conversion
**语言**：Go
**定位**：GitHub Copilot 代理，支持 Anthropic 格式

**流式转换状态机**（`StreamState`）：
- `messageStartSent`：确保 `message_start` 事件只发送一次
- `currentBlockIndex`：跟踪当前 Anthropic block index
- `thinkingBlockOpen` / `textBlockOpen`：跟踪当前打开的 block 类型
- `reasoningState`：跟踪 thinking/reasoning 块状态
- `toolCallsState`：多工具执行的 map（toolCallId → 状态）

**事件生成逻辑**：
1. 第一个 chunk → `message_start`
2. `delta.reasoning_content` → `content_block_start`(thinking) + `content_block_delta`(thinking_delta) + `content_block_stop`
3. `delta.content` → `content_block_start`(text) + `content_block_delta`(text_delta) + `content_block_stop`
   - 如果 thinking block 打开，先自动关闭
4. `delta.tool_calls` → `content_block_start`(tool_use) + `content_block_delta`(input_json_delta) + `content_block_stop`
5. `finish_reason` + `usage` → 关闭所有打开的 block + `message_delta` + `message_stop`

### 10. API7 AI Gateway

**文档**：https://docs.api7.ai/api7-gateway/ai-gateway/use-cases/protocol-conversion
**语言**：Lua/OpenResty
**定位**：网关层协议转换

**特点**：
- 自动检测：请求 URI 为 `/v1/messages` 时识别为 Anthropic 协议
- 请求转换：Anthropic 字段映射到 OpenAI 等价物
- 响应转换：OpenAI 响应字段映射回 Anthropic 格式
- 流式支持：OpenAI SSE chunk → Anthropic SSE 事件

**限制**：
- 自动检测依赖精确 URI `/v1/messages`
- 仅支持 Anthropic ↔ OpenAI 转换

### 11. Bifrost AI Gateway

**项目地址**：https://github.com/maximhq/bifrost
**语言**：Go
**定位**：高性能 AI 网关，统一 23+ 提供商

**已知 cache_control Bug**（Issue #2469）：
- 通过 `/v1/chat/completions`（OpenAI 端点）使用 Anthropic 模型时
- Bifrost 在 OpenAI → Anthropic 格式转换时**不会自动注入** `cache_control`
- 导致 prompt caching 不生效（`cached_tokens: 0`）
- 修复方案：在转换时自动为 system prompt block 添加 `cache_control: {"type": "ephemeral"}`

### 12. OpenClaw

**文档**：https://docs.openclaw.ai/reference/prompt-caching
**定位**：AI agent 平台

**cache_control 处理策略**：
- 对 `openrouter/anthropic/*` 模型，自动注入 Anthropic `cache_control` 标记
- 但仅在请求仍指向 OpenRouter 路由时注入
- 如果将模型重定向到任意 OpenAI 兼容代理 URL，则停止注入
- 已知 bug（Issue #100624）：model-fallback 时，Anthropic 特定的 `cache_control` 会泄漏到 OpenAI/DeepSeek 模型

### 13. AntiHub-ALL

**项目地址**：https://github.com/zhongruan0522/AntiHub-ALL
**DeepWiki**：https://deepwiki.com/zhongruan0522/AntiHub-ALL/7.1-anthropic-to-openai-conversion
**语言**：Python（FastAPI + Pydantic）
**定位**：多账号 API 代理，支持 Anthropic/OpenAI/Gemini 格式

**核心组件**：
- `AnthropicAdapter` 类 — 非流式请求/响应转换
- `convert_openai_stream_to_anthropic()` — 流式响应转换
- `KiroThinkingTagParser` — 内联 thinking 标签提取
- `SSEUsageTracker` — 流式 token 计数

**流式转换**：与 copilot2api 类似的状态机，从 OpenAI SSE 生成 Anthropic SSE 事件序列

### 14. anthropic_adapter

**项目地址**：https://github.com/abhiram1809/anthropic_adapter
**语言**：Python（FastAPI）
**定位**：Anthropic ↔ OpenAI 双向适配器

**特点**：
- 同时支持 `v1/chat/completions` 和 `v1/responses` 端点
- 根据 `OPENAI_BASE_URL` 自动检测使用哪种 API
- 内置 tiktoken token 计数器
- 多模态支持（文本、图片、工具调用）

---

## 关键发现

### 各项目共同处理的兼容性点

1. **System prompt 位置差异**
   - Anthropic：顶层 `system` 字段（string 或 blocks 数组）
   - OpenAI：`messages` 数组第一条 `role: "system"` 消息
   - **所有项目都将 Anthropic system 映射为 OpenAI 的第一条 system 消息**

2. **Content blocks vs 字符串**
   - Anthropic：`content` 可以是字符串或 content blocks 数组（`text`、`image`、`tool_use`、`tool_result`、`thinking`）
   - OpenAI：`content` 通常是字符串，也支持 content parts 数组（`text`、`image_url`）
   - 需要逐 block 类型转换

3. **Tool calling 格式差异**
   - Anthropic：`tool_use` / `tool_result` 作为 content blocks
   - OpenAI：`tool_calls` 在 assistant 消息上，`tool` 角色消息返回结果
   - `input_schema` → `parameters`、`name` → `function.name`

4. **Stop reason / finish reason 映射**
   - `end_turn` ↔ `stop`
   - `max_tokens` ↔ `length`
   - `tool_use` ↔ `tool_calls`
   - `stop_sequence` ↔ `stop`（触发 stop sequence）

5. **Usage 统计映射**
   - `input_tokens` ↔ `prompt_tokens`
   - `output_tokens` ↔ `completion_tokens`
   - `cache_creation_input_tokens` / `cache_read_input_tokens` — OpenAI 没有直接对应

6. **Streaming 事件结构差异**
   - Anthropic：结构化事件序列（`message_start` → `content_block_start` → `content_block_delta` → `content_block_stop` → `message_delta` → `message_stop`）
   - OpenAI：扁平的 `delta` chunks
   - **需要状态机进行有状态转换**

7. **max_tokens 必填性**
   - Anthropic：`max_tokens` 是必填参数
   - OpenAI：可选
   - 所有项目都处理了默认值设置

8. **模型名映射**
   - Claude 模型名（如 `claude-3-5-sonnet-20241022`）→ 后端模型名（如 `gpt-4o`）
   - 通常通过环境变量配置 BIG/MIDDLE/SMALL 模型

### 各项目在转换中的常见陷阱

1. **Extended Thinking 签名丢失（最严重）**
   - Anthropic 的 `thinking` 块包含**加密签名**
   - 任何 Anthropic ↔ OpenAI 格式转换都会丢失或损坏签名
   - 多轮对话中 Anthropic 会验证签名 → 请求失败
   - **无法伪造签名** — 尝试注入假签名会被服务端拒绝
   - **最佳实践**：对需要 extended thinking 的场景，使用原生 Anthropic `/v1/messages` 端点，避免格式转换

2. **reasoning_content 字段缺失**
   - LiteLLM 将 thinking 存入自定义 `thinking_blocks` 但未设标准 `reasoning_content`
   - 导致多轮对话中上游 API 报错
   - 需要**同时设置** `thinking_blocks` 和 `reasoning_content`

3. **cache_control 在转换中丢失**
   - OpenAI 格式不携带 `cache_control`
   - OpenAI → Anthropic 转换时，如果没有主动注入 `cache_control`，prompt caching 不生效
   - tool_result 消息上的 `cache_control` 在 `role:tool` → `tool_result` 转换中可能丢失
   - **最佳实践**：在转换时自动为 system prompt 和关键内容块注入 `cache_control`

4. **内容序列化不一致破坏缓存前缀**
   - 有 `cache_control` 的消息序列化为 block array，没有的序列化为 bare string
   - 上一轮标记的消息在下一轮变为未标记 → 序列化形式改变 → byte prefix diverge → 缓存读取失败
   - **最佳实践**：统一内容序列化（始终用 block array 或始终用 bare string），不受 cache_control 存在影响

5. **工具名长度和字符限制**
   - OpenAI：64 字符限制
   - Anthropic：128 字符限制，且必须匹配 `^[a-zA-Z0-9_-]$`
   - MCP 工具名常含 `/` 或 `.`
   - **最佳实践**：构建 per-request 的前向/反向映射，处理截断和字符替换

6. **tool_result 的 content 合并**
   - Anthropic 允许 `tool_result.content` 是数组（多个 content blocks）
   - OpenAI 的 `tool` 角色消息只能有一个 `content`
   - **最佳实践**：将多个 content items 合并为单个 tool 消息，避免重复 ID

7. **流式转换中的 block 切换**
   - 从 thinking block 切换到 text block 时，必须先发送 `content_block_stop` 关闭 thinking block
   - 多工具调用需要跟踪每个工具的 ID 和状态
   - **最佳实践**：使用状态机跟踪当前 block index、打开的 block 类型、工具调用状态

8. **top_k 参数丢弃**
   - Anthropic 支持 `top_k`，OpenAI 不支持
   - 所有项目都选择丢弃此参数

9. **stop_sequences vs stop**
   - Anthropic `stop_sequences`（数组）→ OpenAI `stop`（数组或字符串）
   - 需注意 OpenAI 返回 `finish_reason: "stop"` 时可能是触发了 stop sequence

### 对 Claude Code 兼容性的最佳实践

1. **优先使用原生 Anthropic 端点**
   - 如果后端支持 Anthropic Messages API（如 Anthropic 直连、AWS Bedrock、Vertex AI），使用 `/v1/messages` 透传，避免格式转换
   - LiteLLM 的双提供商配置是最佳范例：Anthropic 模型走 `/v1/messages`，其他模型走 `/chat/completions`

2. **Extended Thinking 处理**
   - **不要通过格式转换处理 thinking 块** — 签名会丢失
   - 如果必须转换，考虑 new-api 的 "thinking-to-content" 方案（将 thinking 内容直接转为 content）
   - 对于本地模型（无签名），可以从 `reasoning`/`reasoning_content` 字段构建 thinking 块

3. **模型名映射**
   - Claude Code 会发送 `claude-sonnet-4-...`、`claude-opus-4-...`、`claude-haiku-...` 等模型名
   - 需要映射到实际后端模型名
   - 支持通过 `ANTHROPIC_DEFAULT_SONNET_MODEL`、`ANTHROPIC_DEFAULT_OPUS_MODEL`、`ANTHROPIC_DEFAULT_HAIKU_MODEL` 环境变量配置

4. **Claude Code 特定行为**
   - Claude Code 使用 `x-api-key` header 进行认证
   - Claude Code 会发送 `anthropic-version` header
   - Claude Code 的系统提示词可能包含会被 WAF 拦截的术语（如 `rm -rf`）
   - 需要支持 `count_tokens` 端点（Claude Code 用于估算 token 数）

5. **并行工具调用**
   - Claude Code 可能发起并行工具调用
   - 流式响应中需要正确处理多个 `tool_use` block 的交错

### 哪些字段通常被忽略，哪些必须处理

**必须处理的字段**：

| 字段 | 原因 |
|------|------|
| `model` | 必须映射到后端模型名 |
| `messages` | 核心对话内容，必须逐条转换 |
| `max_tokens` | Anthropic 必填，OpenAI 可选 |
| `system` | 位置差异，必须转为第一条消息 |
| `tools` / `tool_choice` | 工具调用是 Claude Code 的核心功能 |
| `stream` | Claude Code 默认使用流式 |
| `stop_sequences` | 影响生成行为 |
| `temperature` / `top_p` | 采样参数 |
| `thinking` | 扩展思考（如果启用） |

**通常被忽略的字段**：

| 字段 | 原因 |
|------|------|
| `top_k` | OpenAI 不支持 |
| `metadata` | 除 `user_id` 外通常不映射 |
| `cache_control` | OpenAI 格式不携带，需主动注入 |
| `service_tier` | 非标准参数 |
| `context_management` | 新参数，多数项目未实现 |
| `container` | 新参数，多数项目未实现 |
| `stream_options` | Anthropic 特有，OpenAI 需不同处理 |
| `citations` | 响应中的引用，多数项目不支持 |
| `pause_turn` / `refusal` | stop_reason 值，多数项目不支持 |

**需要注意但容易出错的字段**：

| 字段 | 陷阱 |
|------|------|
| `thinking` 块的 `signature` | 加密签名，转换会丢失，导致多轮失败 |
| `tool_result.content`（数组） | 需合并为单个 tool 消息 |
| 工具名 | 长度/字符限制差异，需截断+映射 |
| `cache_control` | 在 `role:tool` 消息上可能丢失 |
| content 序列化 | block array vs bare string 影响缓存前缀 |

---

## 附录：项目对比总览

| 项目 | 语言 | 方向 | Claude Code 支持 | Thinking 支持 | Streaming | Tool Use | cache_control |
|------|------|------|-----------------|--------------|-----------|----------|---------------|
| LiteLLM | Python/Rust | 双向 | ✅（/v1/messages） | ⚠️（签名丢失） | ✅ | ✅ | ✅（条件保留） |
| new-api | Go | 双向 | ✅ | ✅（thinking-to-content） | ✅ | ✅ | ✅（计费） |
| one-api | Go | 仅后端 | ❌ | ❌ | ✅ | ✅ | ❌ |
| clawgate | Go | A→O | ✅ | ✅ | ✅ | ✅（并行） | ❌ |
| ant2oai | Python | A→O | ✅ | ✅ | ✅ | ✅ | ❌ |
| OA2A | Python | 双向 | ✅ | ✅ | ✅ | ✅ | ❌ |
| anthropic-proxy-rs | Rust | A→O | ✅ | ✅（自动路由） | ✅ | ✅ | ❌ |
| anthropic_openai_bridge | Python | A→O | 库 | ✅ | ✅ | ✅ | ❌ |
| claude-code-proxy | Python | A→O | ✅ | ❌ | ✅ | ✅ | ❌ |
| claude-code-provider-proxy | Python | A→O | ✅ | ❌ | ✅ | ✅ | ❌ |
| Olla | Go | 双向 | ✅ | ✅（无签名） | ✅ | ✅ | ❌ |
| copilot2api | Go | 双向 | ✅ | ✅（状态机） | ✅ | ✅ | ❌ |
| API7 Gateway | Lua | A→O | ✅ | ❌ | ✅ | ✅ | ❌ |
| Bifrost | Go | 双向 | ✅ | ❌ | ✅ | ✅ | ⚠️（bug） |
| AntiHub-ALL | Python | 双向 | ✅ | ✅（parser） | ✅ | ✅ | ❌ |
| anthropic_adapter | Python | 双向 | ✅ | ❌ | ✅ | ✅ | ❌ |

> A→O = Anthropic → OpenAI；双向 = 支持两个方向的转换
