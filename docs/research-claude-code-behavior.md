# Claude Code API 行为研究报告

> 基于 Claude Code v2.1.123 逆向工程文档、Anthropic 官方文档、LLM Gateway Protocol、LiteLLM 兼容性实践、以及多个 Claude Code 代理项目的综合研究。

## 来源

| 来源 | 类型 | 关键内容 |
|------|------|----------|
| `marco-jardim/opencode-anthropic-fix` 逆向工程文档 | 逆向工程 | 完整的 HTTP headers、body 结构、OAuth 流程、SSE 事件、错误处理 |
| `code.claude.com/docs/en/llm-gateway-protocol` | 官方文档 | Gateway 协议、header 透传规则、feature pass-through |
| `code.claude.com/docs/en/errors` | 官方文档 | 错误处理、重试逻辑 |
| `code.claude.com/docs/en/prompt-caching` | 官方文档 | 缓存策略、cache_control 使用 |
| `platform.claude.com/docs/en/build-with-claude/streaming` | 官方文档 | SSE 事件格式、流式响应 |
| `docs.litellm.ai` beta headers 事故报告 | 兼容性实践 | beta header 过滤策略 |
| GitHub Issues #11154, #21644 | Bug 报告 | beta header 在 count-tokens 和 proxy 上的问题 |
| `1rgs/claude-code-proxy`, `seanbabalala/claude-code-proxy` | 代理项目 | 代理实现实践 |

---

## 1. Claude Code 请求特征

### 1.1 HTTP Headers

#### 标准头（始终存在）

```http
anthropic-version: 2023-06-01
Content-Type: application/json
User-Agent: claude-code/{version}
x-app: cli
X-Claude-Code-Session-Id: {sessionId}
```

- `x-app`: `cli`（交互模式）或 `cli-bg`（后台 agent 模式）
- `X-Claude-Code-Session-Id`: 每个会话稳定的 UUID，与 `metadata.user_id.session_id` 一致

#### 认证头

**OAuth 模式（claude.ai 登录）：**
```http
Authorization: Bearer {oauth_access_token}
anthropic-beta: oauth-2025-04-20
```

**API Key 模式：**
```http
x-api-key: sk-ant-...
```

**Session Key 模式（claude.ai web sessions）：**
```http
Cookie: sessionKey=sk-ant-sid01-...
X-Organization-Uuid: {org_uuid}
```

#### Stainless SDK 头（由 @anthropic-ai/sdk 注入）

```http
X-Stainless-Lang: js
X-Stainless-Package-Version: {sdk_version}    # 当前 0.81.0（非 CLI 版本！）
X-Stainless-OS: Linux | macOS | Windows | Unknown
X-Stainless-Arch: x64 | arm64 | other:{val}
X-Stainless-Runtime: node
X-Stainless-Runtime-Version: v22.x.x
X-Stainless-Retry-Count: 0
X-Stainless-Timeout: 600
```

**OS 映射：**
- `linux` → `"Linux"`，`darwin` → `"macOS"`（注意小写 ac），`win32` → `"Windows"`

#### 子代理头（可选）

```http
x-claude-code-agent-id: {agentId}           # 子代理请求时存在
x-claude-code-parent-agent-id: {parentAgentId}  # 嵌套代理时存在
x-client-request-id: {uuid}                  # v2.1.84+ 每请求 UUID
```

#### User-Agent 变体

| 上下文 | User-Agent |
|--------|------------|
| OAuth token 交换/刷新 | `axios/1.13.6`（通过 bundled axios）|
| API 调用 (`/v1/messages`) | `claude-cli/{version} (external, cli)` |
| 账户设置/grove 配置 | `claude-code/{version}` |
| SSE/WebSocket/MCP proxy | `claude-code/{version}` |
| WebFetch 工具 | `Claude-User (claude-code/{version}; +https://support.anthropic.com/)` |

**⚠️ 关键：** OAuth token 端点请求必须匹配 axios 1.13.6 指纹（`Accept: application/json, text/plain, */*` + `User-Agent: axios/1.13.6`），否则收到 HTTP 429。

### 1.2 anthropic-beta 值完整列表

Claude Code 发送以下 beta 值（v2.1.123 baseline）：

| Beta 值 | 用途 | 默认发送 |
|---------|------|----------|
| `claude-code-20250219` | **Claude Code 客户端主 beta** — firstParty 始终存在 | ✅ 始终（firstParty）|
| `oauth-2025-04-20` | **OAuth 认证** — 使用 OAuth token 时始终添加 | ✅ OAuth 时 |
| `interleaved-thinking-2025-05-14` | 交错思考 | ✅ 默认 |
| `context-1m-2025-08-07` | 1M 上下文窗口（模型支持时） | ✅ 默认 |
| `context-management-2025-06-27` | 上下文管理 | ✅ 默认（Claude 4+） |
| `structured-outputs-2025-12-15` | 结构化输出 | ✅ 默认 |
| `web-search-2025-03-05` | Web 搜索 | ✅ 默认 |
| `advanced-tool-use-2025-11-20` | 高级工具使用 | ✅ 默认（firstParty/foundry） |
| `task-budgets-2026-03-13` | 任务预算（output_config 子代理限制） | 条件性 |
| `tool-search-tool-2025-10-19` | 工具搜索 | ✅ 默认（vertex/bedrock） |
| `effort-2025-11-24` | Effort 参数 | ✅ 默认 |
| `prompt-caching-scope-2026-01-05` | Prompt 缓存范围 | ✅ 默认 |
| `fast-mode-2026-02-01` | 快速模式（Haiku turbo） | 条件性 |
| `redact-thinking-2026-02-12` | 隐藏思考 | 条件性（非交互式） |
| `afk-mode-2026-01-31` | 自动/AFK 模式分类器 | 条件性 |
| `files-api-2025-04-14` | Files API 操作 | 条件性 |
| `token-counting-2024-11-01` | Token 计数端点 | 条件性 |
| `skills-2025-10-02` | Skills API | 条件性 |
| `thinking-token-count-2026-05-13` | 思考 token 计数 | ✅ 默认（firstParty） |
| `summarize-connector-text-2026-03-13` | 连接器文本摘要（反蒸馏） | 条件性 |
| `cache-diagnosis-2026-04-07` | 缓存诊断 | 条件性（v2.1.119+） |
| `token-efficient-tools-2026-03-28` | Token 高效工具（FC v3） | 条件性（v2.1.90+ 移除后重新添加） |
| `server-side-fallback-2026-06-01` | 服务端回退 | 条件性（v2.1.195+，opt-in） |
| `fallback-credit-2026-06-01` | 回退信用重新定价 | 条件性（v2.1.195+，opt-in） |

**默认 beta 集（firstParty OAuth 请求）：**
```
claude-code-20250219, oauth-2025-04-20, interleaved-thinking-2025-05-14,
context-1m-2025-08-07, context-management-2025-06-27, structured-outputs-2025-12-15,
web-search-2025-03-05, advanced-tool-use-2025-11-20, tool-search-tool-2025-10-19,
effort-2025-11-24, prompt-caching-scope-2026-01-05, thinking-token-count-2026-05-13
```

**⚠️ Beta Header 锁定（Latching）：** 一旦某个 beta header 在会话中首次发送，它会在该会话的后续所有请求中继续发送。这防止了中途缓存键变化导致 ~50-70K token 的 prompt cache 失效。锁定在 `/clear` 和 `/compact` 时清除。

### 1.3 Model 名称

Claude Code 使用的模型 ID：

| 模型 ID | 短别名 | 知识截止 |
|---------|--------|----------|
| `claude-opus-4-6` | opus | May 2025 |
| `claude-sonnet-4-6` | sonnet | August 2025 |
| `claude-opus-4-5` | — | May 2025 |
| `claude-haiku-4` | haiku | February 2025 |
| `claude-opus-4` / `claude-sonnet-4` | — | January 2025 |

**默认模型：** Claude Code 默认解析到 Sonnet 4.6（`claude-sonnet-4-6`）。UltraPlan 固定到 Opus 4.6。

### 1.4 特殊请求体字段

#### 完整请求体结构

```json
{
  "model": "claude-opus-4-6",
  "messages": [...],
  "system": "..." | [...content_blocks],
  "tools": [...tool_definitions],
  "tool_choice": { "type": "auto" },
  "max_tokens": 16384,
  "thinking": { "type": "adaptive" },
  "temperature": 1,
  "stream": true,
  "metadata": {
    "user_id": "{\"device_id\":\"<64-hex>\",\"account_uuid\":\"<uuid>\",\"session_id\":\"<uuid>\"}"
  },
  "speed": "fast",
  "context_management": { "edits": [...] },
  "output_config": { "format": "json_schema", "schema": {...} },
  "task_budget": { "type": "tokens", "total": 100000, "remaining": 80000 }
}
```

#### `metadata.user_id`（每个请求体中发送）

```json
{
  "user_id": "{\"device_id\":\"<64-hex>\",\"account_uuid\":\"<uuid>\",\"session_id\":\"<uuid>\"}"
}
```

**⚠️ 重要：** `user_id` 是一个 JSON 字符串（不是对象），必须 `JSON.stringify()` 处理。
- `device_id`: `crypto.randomBytes(32).toString('hex')`（64 字符十六进制），存储在 `~/.claude/config.json`
- `account_uuid`: OAuth 账户 UUID
- `session_id`: 每进程重新生成的 UUID

#### `context_management`（上下文管理）

当 thinking 激活且 `context-management-2025-06-27` beta 存在时注入：

```json
{
  "context_management": {
    "edits": [
      { "type": "clear_thinking_20251015", "keep": "all" }
    ]
  }
}
```

控制 thinking blocks 在上下文管理期间的处理方式。仅对 Claude 4+ 模型有效。

#### `output_config`（输出配置）

携带 effort、structured-output 格式和 task budget 设置：

```json
{
  "output_config": {
    "format": "json_schema",
    "schema": {...},
    "max_output_tokens": 100000
  }
}
```

- Effort 级别通过 `output_config` 传递，配对 `effort-2025-11-24` beta
- Structured outputs 通过 `output_config` 传递，配对 `structured-outputs-2025-12-15` beta
- Task budgets 通过 `output_config` 传递，配对 `task-budgets-2026-03-13` beta

**⚠️ 关键：** `output_config` 与其 beta header 是成对出现的。如果只传 body 字段而不传 header（或反之），会产生 `400` 错误。只有两者同时缺失时，功能才会安静地关闭。

#### `speed`（速度参数）

```json
{ "speed": "fast" }
```

快速模式请求时发送。`speed` 不是缓存键的一部分，但快速模式的 beta header 是。

#### `task_budget`（任务预算）

```json
{
  "task_budget": {
    "type": "tokens",
    "total": 100000,
    "remaining": 80000
  }
}
```

用于后台子代理预算限制，配对 `task-budgets-2026-03-13` beta。

### 1.5 Tool 定义格式

Claude Code 使用标准 Anthropic tool 定义格式：

```json
{
  "name": "Bash",
  "description": "Executes a bash command...",
  "input_schema": {
    "type": "object",
    "properties": {
      "command": { "type": "string", "description": "..." }
    },
    "required": ["command"]
  }
}
```

**⚠️ 关键发现 — 工具名称服务端校验：**

1. **`todowrite` 被服务端列入黑名单：** 工具名 `todowrite`（小写连接版本）会触发立即拒绝，返回 "out of extra usage" 错误（掩盖真实原因）。必须使用 `TodoWrite`（PascalCase）。

2. **`mcp_` 前缀检测：** 当 2 个以上工具带有 `mcp_` 前缀时，请求被拒绝。真实的 Claude Code 从不在工具定义中使用 `mcp_` 前缀，它只在内部工具路由中使用该前缀。

3. **所有 CC 内置工具使用 PascalCase：** `Bash`, `Read`, `Edit`, `Write`, `Glob`, `Grep`, `Agent`, `TodoWrite`, `WebFetch`, `WebSearch`, `Skill`, `NotebookEdit`, `TaskCreate`, `TaskGet`, `TaskList`, `TaskUpdate`, `TaskOutput`, `TaskStop`。

**工具缓存：** 当启用 prompt caching 时，tools 数组中最后一个工具获得 `cache_control: {type: "ephemeral"}`。

### 1.6 System Prompt 格式

#### System Prompt Block 顺序

```
[BLOCK 1]  Billing header (cacheScope: null — 从不缓存)
[BLOCK 2]  Identity string (cacheScope: "org" 或 null)
[BLOCK 3]  Main identity + security instructions
[BLOCK 4]  System rules (tool behavior, markdown, compression)
[BLOCK 5]  Coding best practices
[BLOCK 6]  "Executing actions with care"
[BLOCK 7]  Tool-specific instructions (Bash/Read/Edit/Glob/Grep rules)
[BLOCK 8]  Tone & style
[BLOCK 9]  Output efficiency
--- __SYSTEM_PROMPT_DYNAMIC_BOUNDARY__ (全局缓存启用时) ---
[BLOCK 10] CLAUDE.md / Memory content
[BLOCK 11] Model override info
[BLOCK 12] Environment info (CWD, git, platform, model IDs, knowledge cutoffs)
[BLOCK 13] Language preference
[BLOCK 14] Output style
[BLOCK 15] MCP server instructions (cacheBreak=true)
[BLOCK 16] Scratchpad directory info
[BLOCK 17] Tool result memo tip
[BLOCK 18] Brief mode instructions
```

#### Billing Header（第一个 System Block）

注入为每个 system prompt 的第一段。看起来像 HTTP header 但存在于 prompt 文本中：

```
x-anthropic-billing-header: cc_version=2.1.92.{fingerprint}; cc_entrypoint={CLAUDE_CODE_ENTRYPOINT|"unknown"}; cch=00000; cc_workload={workloadId};
```

- `cc_version`: `{packageVersion}.{fingerprint}`，fingerprint 为 3 字符十六进制哈希
- `cc_entrypoint`: 来自 `CLAUDE_CODE_ENTRYPOINT` 环境变量（`cli`, `sdk`, `vscode`）
- `cch=00000`: Bun native client attestation 静态占位符
- `cc_workload`: 可选，来自 `CLAUDE_CODE_WORKLOAD` 环境变量
- **Cache scope: null — 从不缓存**
- 可通过 `CLAUDE_CODE_ATTRIBUTION_HEADER=false` 或 feature flag 禁用

#### Identity Strings

| 上下文 | Identity String |
|--------|-----------------|
| 交互/默认 | `"You are Claude Code, Anthropic's official CLI for Claude."` |
| 非交互 + appendSystemPrompt | `"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."` |
| 非交互无 appendSystemPrompt | `"You are a Claude agent, built on Anthropic's Claude Agent SDK."` |
| Vertex AI | 始终使用默认交互字符串 |

#### Dynamic Boundary Marker

`__SYSTEM_PROMPT_DYNAMIC_BOUNDARY__` 将 prompt 分为两半：
- **标记之前：** 静态、全局可缓存指令
- **标记之后：** 动态每会话内容（环境、memory 等）

### 1.7 Prompt Caching 与 cache_control

**启用状态：** 默认为支持的模型启用。

#### Cache TTL

| 类型 | TTL | 使用场景 |
|------|-----|----------|
| `{type: "ephemeral"}` | 5 分钟 | 默认 |
| `{type: "ephemeral", ttl: "1h"}` | 1 小时 | 扩展缓存 |

#### System Prompt 缓存策略

| 层 | 内容 | 变化时机 |
|----|------|----------|
| System prompt | 核心指令、工具定义、输出风格 | 工具定义集变化或 CC 升级时 |
| Project context | CLAUDE.md、auto memory | 会话开始、/clear 或 /compact 后 |
| Conversation | 消息、响应、工具结果 | 每轮 |

**缓存组织：**
- 标记前静态块 → 全局缓存（`scope: "global"`）
- 标记后动态块 → 按 org 缓存（`scope: "org"`）或未缓存
- Billing header → 从不缓存（`scope: null`）
- Identity block → `{type: "ephemeral"}` 无 scope

**⚠️ 重要：** `scope: "org"` 从不出现在网络请求中（仅内部使用）。`scope: "global"` 只在静态预边界块上使用。

#### 缓存失效行为

以下操作会失效缓存：
- 切换模型（每个模型有独立缓存）
- 更改 effort 级别（缓存按键 effort 级别）
- 开启 fast mode（添加请求 header 属于缓存键）
- 连接/断开 MCP 服务器（工具定义在 system prompt 层）
- 启用/禁用插件
- 拒绝整个工具
- 压缩对话
- 升级 Claude Code

**Cache TTL Session Latching：** 缓存策略（特别是 1h TTL 资格）在会话第一个 API 请求时锁定。后续请求使用锁定的值，即使底层配置发生变化。

### 1.8 Thinking / Extended Thinking

| 模型 | Thinking 类型 |
|------|---------------|
| `claude-opus-4-6` | `{ type: "adaptive" }` |
| `claude-sonnet-4-6` | `{ type: "adaptive" }` |
| 旧模型 | `{ type: "enabled", budget_tokens: N }` |

- 当 thinking 启用时，temperature 为 `undefined`；否则 `temperatureOverride ?? 1`
- 禁用：`CLAUDE_CODE_DISABLE_THINKING=true`
- 禁用 adaptive：`CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=true`（回退到固定 budget）
- Budget 覆盖：`MAX_THINKING_TOKENS` 环境变量

**Adaptive Thinking 错误恢复：** 当上游拒绝 `thinking` 字段时，Claude Code 自动重试并禁用该能力（对会话剩余部分）。thinking signature 拒绝和对话中途 system message 拒绝也通过此方式恢复。

### 1.9 Effort 级别（output_config.effort）

Claude Code 通过 `output_config` 字段传递 effort 级别，配对 `effort-2025-11-24` beta header。

- Effort 级别是缓存键的一部分
- 更改 effort 会使整个请求的缓存失效
- Claude Code 在已开始的会话中更改 effort 前会显示确认对话框
- 更改为相同的 effort 级别（如显式设置模型的默认值）跳过对话框并保持缓存

**`CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1`** 会阻止发送预发布能力及其 body 字段，但**不影响** adaptive reasoning（由模型而非 beta 选择）。

---

## 2. Claude Code 流式响应期望

### 2.1 SSE 事件序列

Claude Code 期望的标准 SSE 事件流：

```
event: message_start       → { type: "message_start", message: {...} }
event: content_block_start → { type: "content_block_start", index: 0, content_block: {...} }
event: content_block_delta → { type: "content_block_delta", index: 0, delta: {...} }
event: content_block_stop  → { type: "content_block_stop", index: 0 }
event: message_delta       → { type: "message_delta", delta: {...}, usage: {...} }
event: message_stop        → { type: "message_stop" }
event: ping                → (keepalive, 被忽略)
event: error               → { type: "error", error: {...} }
```

**完整事件流：**
1. `message_start`: 包含一个 `Message` 对象，`content` 为空数组
2. 一系列 content blocks，每个包含：
   - `content_block_start` 事件
   - 一个或多个 `content_block_delta` 事件
   - `content_block_stop` 事件
3. 一个或多个 `message_delta` 事件
4. 最终的 `message_stop` 事件

**⚠️ 关键：** 推理响应**必须**流式传输。Claude Code 逐字节消费 SSE 事件。缓冲完整响应再转发的 gateway 会使客户端卡死。

### 2.2 message_start 敏感字段

```json
{
  "type": "message_start",
  "message": {
    "id": "msg_...",
    "type": "message",
    "role": "assistant",
    "content": [],
    "model": "claude-opus-4-6",
    "stop_reason": null,
    "stop_sequence": null,
    "usage": {
      "input_tokens": 25,
      "output_tokens": 1
    }
  }
}
```

Claude Code 对 `message_start` 中以下字段敏感：
- `message.id`: 请求 ID 跟踪
- `message.model`: 用于显示和缓存键
- `message.usage.input_tokens`: 成本计算
- `message.usage.output_tokens`: 成本计算

### 2.3 usage 字段要求

**message_delta 中的 usage 是累积的：**

```json
{
  "type": "message_delta",
  "delta": { "stop_reason": "end_turn", "stop_sequence": null },
  "usage": { "output_tokens": 15 }
}
```

**最终响应的 usage 结构：**

```json
{
  "usage": {
    "input_tokens": 1234,
    "output_tokens": 567,
    "cache_read_input_tokens": 890,
    "cache_creation_input_tokens": 123,
    "server_tool_use": {
      "web_search_requests": 0
    }
  }
}
```

**⚠️ 缓存 token 字段：**
- `cache_read_input_tokens`: 缓存读取的 token 数
- `cache_creation_input_tokens`: 缓存创建的 token 数

这些字段用于成本计算：
```js
cost = (input_tokens / 1e6) * inputPrice +
       (output_tokens / 1e6) * outputPrice +
       (cache_read_tokens / 1e6) * cacheReadPrice +
       (cache_creation_tokens / 1e6) * cacheWritePrice +
       web_search_requests * webSearchPrice;
```

### 2.4 Thinking Blocks 和 Signature 处理

**Thinking delta 事件：**
```
event: content_block_delta
data: {"type":"content_block_delta", "index":0, "delta":{"type":"thinking_delta", "thinking":"I need to find..."}}
```

**Signature delta 事件（在 content_block_stop 之前发送）：**
```
event: content_block_delta
data: {"type":"content_block_delta", "index":0, "delta":{"type":"signature_delta", "signature":"EqQBCgIYAhIM..."}}
```

- Signature 用于验证 thinking block 的完整性
- 当 `display: "omitted"` 时，不发送 `thinking_delta` 事件。thinking block 打开、接收单个 `signature_delta`、然后关闭
- 如果上游拒绝 thinking signature，Claude Code 自动重试并禁用该能力

**Redacted Thinking（`redact-thinking-2026-02-12` beta）：**
- API 返回 `redacted_thinking` blocks 而非 thinking 摘要
- 客户端将其渲染为存根
- 仅对交互式会话有意义

### 2.5 stop_reason 期望

| stop_reason | 含义 |
|-------------|------|
| `end_turn` | 正常完成 |
| `max_tokens` | 达到 max_tokens 限制 |
| `tool_use` | 模型请求工具调用 |
| `stop_sequence` | 遇到停止序列 |
| `refusal` | 模型拒绝 |
| `pause_turn` | 暂停（连续对话） |

Claude Code 对 `stop_reason` 敏感——它决定了是否继续 tool use 回调流程还是结束当前轮次。

### 2.6 Error 事件处理

SSE 流中的 error 事件：

```
event: error
data: {"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}
```

- `error` 事件会抛出 `APIError`
- 过载错误（`overloaded_error`）对应非流式上下文中的 HTTP 529
- Claude Code 应优雅处理未知事件类型（API 可能添加新事件类型）

### 2.7 流式空闲超时与心跳

**⚠️ 关键：** Claude Code 在通过 `ANTHROPIC_BASE_URL` 连接时，会计算 gateway 转发的每个字节（包括 SSE `ping` 事件和注释行）。如果一个流在 **300 秒**内没有任何数据，Claude Code 会中止该流。

- 上游的 ping 是长时间思考暂停期间唯一的流量
- 如果 gateway 剥离或缓冲 ping，Claude Code 会在思考暂停期间中止流
- 20 秒无数据时显示 "Waiting for API response · will retry in..." 提示

---

## 3. Claude Code 错误处理

### 3.1 重试行为

**Claude Code 重试的失败类型（最多 10 次指数退避）：**
- 在 Claude 响应流式传输**之前**到达的服务器错误、过载响应和请求超时
- 断开的连接（在 Claude 完成任何响应部分之前）
- 计算机休眠导致的连接中断
- 暂停的响应流（在响应未到达或 Claude 完成思考但未开始文本/工具调用时）
- 临时 429 限制（不是 gateway 的 spend-limit 429）
- 输入 + max_tokens 超过上下文限制的请求（使用减少的 max_tokens 重试）
- 过期/缺失的 Google Cloud 凭证

**Claude Code 不重试的失败类型：**
- TLS 证书验证失败（第一次尝试就报告错误）
- 在 Claude 完成文本块或工具调用**之后**到达的服务器错误/断开连接/暂停流
- 在 Claude 完成响应后到达的失败
- Amazon Bedrock 流式响应的意外 content-type
- Context management 和 tool schema 字段拒绝的 400 错误

**重试状态码：** 408, 409, 429, 500+

**退避公式：**
```js
delay = min((0.5 * 2) ^ attempt, 8) * (1 - random * 0.25) * 1000; // ms
// 尝试 1: ~500ms
// 尝试 2: ~1000ms
// 尝试 3+: ~2000ms → 最大 8000ms
```

**Retry-After headers 遵循：**
- `retry-after-ms`（毫秒）
- `retry-after`（秒或 HTTP 日期）

**服务器覆盖：** `x-should-retry: true/false` header

### 3.2 对 400 错误的行为

**⚠️ 关键发现：**

1. **Context management 拒绝：** 如果 `context_management` body 字段通过非 Anthropic 格式的 upstream 转发但不被接受，返回 `400` 错误，消息为 `"Extra inputs are not permitted"`。Claude Code **不会**自动重试此错误——它直接显示给用户。

2. **Beta header 不被接受：** 当不支持某个 beta 值的端点收到 beta header 时，返回：
   ```json
   {"type":"error","error":{"type":"invalid_request_error","message":"Unexpected value(s) `prompt-caching-scope-2026-01-05` for the `anthropic-beta` header."}}
   ```
   Claude Code 不会自动移除该 header 重试——用户看到错误。

3. **Thinking 字段拒绝：** 如果上游模型不接受 `thinking` 字段或 `adaptive` 标签，返回 `400`。Claude Code **会**自动重试并禁用该能力。

4. **Tool schema 字段拒绝：** 如果 body 中有 `strict` 或 `defer_loading` 等字段但缺少对应的 beta header，返回 `400`。Claude Code **不会**自动重试。

5. **工具名称被黑名单：** `todowrite` 工具名触发服务端拒绝，返回 "out of extra usage"（掩盖真实原因）。这不是标准 400 错误。

6. **`cache-diagnosis-2026-04-07` beta 的 400 恢复：** 如果 400 错误的响应体包含该 beta 值和 `anthropic-beta`，Claude Code 会移除该 beta 并重试。

### 3.3 敏感错误码

| 状态码 | 行为 | 重试？ |
|--------|------|--------|
| 400 | 取决于错误内容。Context management/tool schema 不重试；thinking 字段会重试 | 条件性 |
| 401 | 检查是否有其他进程刷新了 token，然后强制刷新 | 否 |
| 403 | 网络或权限问题 | 否 |
| 408 | 请求超时 | ✅ |
| 409 | 冲突 | ✅ |
| 429 | 速率限制 | ✅（临时限制）；❌（spend-limit 429） |
| 500+ | 服务器错误 | ✅ |
| 529 | 过载 | ✅ |

### 3.4 期望的错误响应格式

Claude Code 期望的错误响应格式：

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "..."
  }
}
```

**⚠️ 关键：** Claude Code 的重试逻辑**匹配上游的错误措辞**。如果 gateway 将上游错误包装在自己的信封中，即使保留了状态码，也会破坏恢复路径。

### 3.5 忽略字段 vs 返回 400 的影响

根据 LLM Gateway Protocol：

> "A gateway that strips the header while passing the body, or forwards an Anthropic-format body to an upstream with a different schema, produces hard 400 errors; only when both halves are absent together does the feature turn off quietly."

**结论：**
- **同时缺失** body 字段和 beta header → 功能安静关闭，无 400 错误
- **只缺失** beta header 但保留 body 字段 → 400 错误
- **只缺失** body 字段但保留 beta header → 400 错误（可能）
- **两者都存在但不被上游支持** → 400 错误

对于代理/兼容层实现，**最安全的策略**是：如果要忽略某个功能，同时移除对应的 body 字段和 beta header。

---

## 4. Claude Code 特殊行为

### 4.1 MCP（Model Context Protocol）集成

#### MCP Proxy

Claude Code 使用远程 MCP proxy：
```
MCP_PROXY_URL: https://mcp-proxy.anthropic.com
MCP_PROXY_PATH: /v1/mcp/{server_id}
```

#### MCP 工具定义处理

- MCP 工具定义在 system prompt 层
- **Tool Search（延迟加载）：** 默认在支持的模型上，MCP 服务器的工具定义被延迟加载。服务器连接/断开/更改工具列表只追加新内容，不影响已缓存的内容。
- **前缀加载：** 当 tool search 不可用或禁用时（如 Vertex AI 旧模型、custom `ANTHROPIC_BASE_URL` gateway、Foundry on Azure），工具定义加载到 prefix 中。任何变化都会使缓存失效。
- 标记为 `alwaysLoad` 的服务器/工具始终前缀加载
- 阈值加载的工具有时也保持在 prefix 中

#### MCP 工具名称

- MCP 工具使用 `mcp__{server_name}__{tool_name}` 格式
- **⚠️ 但在工具定义中从不使用 `mcp_` 前缀** — 它只在内部工具路由中使用
- 工具描述上限：2KB

#### MCP 安全

- Socket 安全验证在 bridge 连接前进行
- Workspace trust gate：在信任确认前阻止 `headersHelper`
- 项目范围 MCP 服务器需要 workspace mode 检查

### 4.2 Tool Use 回调流程

1. Claude Code 发送请求到 `/v1/messages`（包含 tool 定义）
2. 模型返回 `stop_reason: "tool_use"` 和 tool_use content block
3. Claude Code 解析 tool_use block，执行工具
4. 将工具结果作为 user message 附加到对话中
5. 发送新的请求（包含更新后的消息历史）

**Tool Use 流式 delta 类型：**
- `text_delta`: 文本增量
- `input_json_delta`: 工具输入的部分 JSON 字符串（增量式）
- `thinking_delta`: 思考增量
- `signature_delta`: 签名增量

**工具结果作为 tool_result content block 返回：**
```json
{
  "role": "user",
  "content": [
    {
      "type": "tool_result",
      "tool_use_id": "toolu_...",
      "content": "..."
    }
  ]
}
```

### 4.3 依赖的非标准字段

#### `betas` 数组

Claude Code 在 SDK 参数中传递 `betas` 数组（`anthropic.beta.messages.create()`），但 SDK 会将其从 body 中提取并转换为 `anthropic-beta` HTTP header。`betas` 字段**不出现在网络请求体中**。

`?beta=true` 查询参数由 SDK 的 Beta endpoint 类添加。

#### Billing Header（System Prompt 内）

非标准的 billing header 嵌入在 system prompt 的第一个 block 中，而非作为 HTTP header。`api.anthropic.com` 在该 block 作为第一个 system block 未修改到达时，会在处理前剥离它，因此不影响第一方 prompt caching。其他 upstream 将其作为 prompt 的一部分接收。

#### `metadata.user_id` JSON 字符串

`metadata.user_id` 是 JSON 字符串而非对象——这是 Claude Code 特有的非标准格式。

#### `x-client-request-id`

v2.1.84+ 新增的每请求 UUID，用于调试流式超时。

#### Attribution Block 稳定性

从 v2.1.181 起，当请求通过 custom base URL 路由时，attribution block 在对话生命周期内是稳定的。这意味着基于完整请求体的 gateway 端 prompt cache 可以工作。在 v2.1.181 之前，该 block 包含每请求 token，会在每次请求时改变 system prompt 的开头。

### 4.4 其他重要行为

#### 自动压缩（Auto-Compaction）

触发条件：上下文填满时

配置：`{minTokens: 10000, minTextBlockMessages: 5, maxTokens: 40000}`

压缩流程：
1. Pre-compact hook
2. Summary 生成（旧消息 → `isCompactSummary: true`）
3. 附件重新生成
4. Post-compact hook

#### 上下文窗口

```js
X54 = 200000; // 默认上下文窗口
M54 = 400000; // 扩展上下文限制
J54 = 50000;  // 最大工具结果大小
```

#### 模型发现

当 `ANTHROPIC_BASE_URL` 指向暴露 Anthropic Messages 格式的 gateway 时，Claude Code 可以查询 gateway 的 `/v1/models` 端点（`GET /v1/models?limit=1000`，3 秒超时），将返回的模型添加到 `/model` 选择器。需要 `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`。

Claude Code 保留 `id` 包含 `claude` 或 `anthropic`（不区分大小写）的条目。

#### Feature Flags（GrowthBook）

- SDK Key: `sdk-zAZezfDKGoZuXXKe`
- API Host: `https://api.anthropic.com/`（代理）
- 模式: `remoteEval: true`
- Killswitch: `firstParty` feature flag — 如果为 false，所有 1P 事件日志停止

#### 非必要流量

```bash
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
```

**阻止：** 账户设置、grove 配置、auto-updater
**不阻止：** API 调用、主要事件日志（如果 OAuth + trust 激活）

---

## 5. 代理/兼容层实现要点

### 5.1 最小可行请求模拟

| 必需级别 | 组件 |
|----------|------|
| **MUST** | `anthropic-version: 2023-06-01` |
| **MUST** | `Authorization: Bearer {token}` 或 `x-api-key` |
| **MUST** | `anthropic-beta: oauth-2025-04-20`（OAuth 时） |
| **MUST** | `Content-Type: application/json` |
| **MUST** | `metadata.user_id`（JSON 字符串，含 device_id/account_uuid/session_id） |
| **MUST** | `betas` 数组含 `claude-code-20250219` |
| **SHOULD** | `User-Agent: claude-code/{version}` |
| **SHOULD** | `x-app: cli` |
| **SHOULD** | `X-Claude-Code-Session-Id` |
| **SHOULD** | 所有 `X-Stainless-*` headers |
| **SHOULD** | Billing header 作为第一个 system prompt block |
| **SHOULD** | System prompt identity string |
| **OPTIONAL** | 额外 beta flags |
| **OPTIONAL** | `x-organization-uuid` |

### 5.2 Gateway 透传规则

**必须原样转发：**
- `anthropic-version` header
- `anthropic-beta` header（不要允许列表单个值，因为集合随 CC 版本变化）
- `anthropic-workspace-id`（Claude Platform on AWS 时）

**可消费（不需要转发）：**
- `Authorization` / `x-api-key`
- `x-claude-code-session-id`
- `x-claude-code-agent-id`
- `x-claude-code-parent-agent-id`
- `X-Stainless-*` headers
- `x-app`

**⚠️ 开放列表原则：** 将 headers 和 body 字段视为开放列表。Claude Code 随版本增加能力，它们以新的 `anthropic-beta` 值、新的请求 body 字段、偶尔新的 `anthropic-*` 或 `x-claude-code-*` headers 到达。

### 5.3 错误响应透传

**⚠️ 关键：** 转发上游错误响应体**不加修改**。Claude Code 的重试逻辑匹配上游的错误措辞。将上游错误包装在自己信封中的 gateway 会破坏恢复路径，即使保留了状态码。

### 5.4 SSE 流转发

- **必须**逐字节转发 SSE 事件（不缓冲完整响应）
- **必须**转发 keep-alive ping 事件
- 不转发 ping 会导致 Claude Code 在长时间思考暂停期间中止流（300 秒超时）

### 5.5 Feature Pass-Through 降级行为

| Feature | Header + Body 对 | 破坏时症状 | 修复 |
|---------|-------------------|-----------|------|
| Adaptive reasoning | `thinking: {"type": "adaptive"}` | 400 命名 thinking 字段 | 升级 upstream 或 `CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1` |
| Context management | beta + `context_management` body | 400 "Extra inputs are not permitted" | 同时转发两者，或 `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` |
| Extended context + interleaved thinking | 仅 beta headers | 静默不可用 | 原样转发 `anthropic-beta` |
| Beta tool fields | beta + tool schema fields（`strict`, `defer_loading`） | 400 命名未识别的 tool schema 字段 | 同时转发两者，或 `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` |
| Effort + structured outputs | `output_config` body + 各自 beta | 400 命名 `output_config` | 同时转发字段和 headers |
| Token counting | `count_tokens` 端点 | 回退到通过 messages 端点计数 | 暴露该端点 |

### 5.6 端点列表

Claude Code 调用的核心 API 端点：

| 路径 | 方法 | 用途 | 必需？ |
|------|------|------|--------|
| `/v1/messages` | POST | 主要推理 | ✅ |
| `/v1/messages?beta=true` | POST | Beta 消息 | ✅ |
| `/v1/messages/count_tokens` | POST | Token 计数 | ❌ 可选 |
| `/v1/models` | GET | 列出可用模型 | ❌ 可选 |
| `/v1/models/{model_id}` | GET | 检索模型 | ❌ 可选 |

**⚠️ 连接预热探测：** Anthropic Messages 格式的 gateway 会收到 `HEAD /api/hello` 连接预热探测（当配置了 HTTP proxy 或 client certificate 时跳过）。

---

## 6. 版本漂移追踪

### 关键版本变化

| 版本 | 日期 | 关键变化 |
|------|------|----------|
| v2.1.80 | 2026-03-19 | cch 硬编码 `00000` |
| v2.1.81 | 2026-03-20 | cch 动态计算（`NP1()` 算法） |
| v2.1.83 | 2026-03-24 | WebFetch UA 添加；non-streaming fallback env var |
| v2.1.84 | 2026-03-26 | `context_management` body 字段；`x-client-request-id` header；`task-budgets` beta |
| v2.1.87 | 2026-03-29 | `tool-examples-2025-10-29` beta 移除 |
| v2.1.90 | 2026-04-01 | 3 个 beta 移除；`context-management` 模型门控；cache_control scope 修复 |
| v2.1.105 | 2026-04-13 | 所有模拟契约点验证未变 |
| v2.1.107 | ~2026-04 | 工具名称黑名单（`todowrite`）；`mcp_` 前缀拒绝 |
| v2.1.119 | 2026-04-23 | `cache-diagnosis-2026-04-07` beta 添加 |
| v2.1.123 | 2026-04-29 | 最新逆向工程基线 |
| v2.1.181 | ~2026-06 | Attribution block 对 custom base URL 稳定 |
| v2.1.195 | 2026-06-29 | OAuth 从 axios 迁移到 SDK native fetch；新增 2 个 opt-in beta |
| v2.1.199 | ~2026-07 | 重试行为改进；429 重试扩展到 claude.ai 订阅 |

### 最可能漂移的常量

1. `x-stainless-package-version`（当前 `0.81.0`）— 每次 upstream 升级都需要重新检查
2. Beta header 集合 — 随版本增减
3. 模型 ID 和别名
4. System prompt 结构
5. `CLAUDE_CODE_BUILD_TIME` — 信息性，但可能被服务端检查

---

## 7. 总结

### 对于 API 代理/兼容层的关键要点

1. **SSE 必须逐字节流式转发**，包括 ping 事件。缓冲会导致超时中止。

2. **错误响应必须原样透传**。Claude Code 的自动恢复路径依赖错误消息的精确措辞。

3. **Beta header 和 body 字段必须成对处理**。同时缺失时功能安静关闭；只缺失一半会产生 400 错误。

4. **不要允许列表 beta headers**。将 `anthropic-beta` 视为开放列表，原样转发。Claude Code 随版本增加新能力。

5. **工具名称有服务端校验**。`todowrite` 被黑名单，`mcp_` 前缀被拒绝。使用 PascalCase 工具名。

6. **`metadata.user_id` 是 JSON 字符串**，不是对象。格式必须精确匹配。

7. **Billing header 在 system prompt 内**，不是 HTTP header。它必须作为第一个 system block，且不能被合并、重排或转换为字符串。

8. **流式空闲超时 300 秒**（`ANTHROPIC_BASE_URL` 连接）。Gateway 必须保持 ping 事件转发。

9. **重试状态码：408, 409, 429, 500+**。最大 10 次重试（可配置）。退避从 ~500ms 开始。

10. **`context_management` 拒绝不重试** — 400 错误直接到达用户。`thinking` 字段拒绝会自动重试。

---

*研究基于 Claude Code v2.1.123 逆向工程及官方文档。最后更新：2026-07*
