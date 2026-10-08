# Changelog

## v0.14.28

- Align conversion layer with Bifrost reference (`fix(protocol)`, PR #41): 转换层对齐 maximhq/bifrost (Apache-2.0) 语义，修复 stop reason / thinking / tool 映射多处偏差：
  - stop reason：`responsesOutcome` 折叠 `finish_reason=length` 与 Anthropic `model_context_window_exceeded` 为 `incomplete/max_output_tokens`，`content_filter` → `incomplete/content_filter`，`pause_turn`/`compaction` 不再误报 completed；`normalizeFinishReason` 映射 `model_context_window_exceeded` → `length`，`pause_turn`/`compaction` 返回空让 chat choices 不再谎报完成；流式/非流式 Claude 路径 `model_context_window_exceeded` → `max_tokens`、`content_filter` → Anthropic `refusal`；stop_sequence 往返（请求 stop_sequences 命中响应尾部即恢复 `stop_sequence` + 回显序列）。
  - thinking：adaptive thinking 仅对 adaptive-only 模型（Opus 4.7+/Sonnet 5+/Fable）保留、不再强制 normalize 为 enabled+budget（该类模型直接拒绝）；`reasoning.encrypted_content` 独立 `redacted_thinking` 块（不再塞 `thinking.signature`），summary 每段独立 thinking 块；`ReasoningDetails` 结构化携带 thinking 签名与 redacted 载荷并在 chat-to-anthropic 出站回放。
  - tool：新增 `sanitizeToolUseID`（Anthropic `^[a-zA-Z0-9_-]+$` 字符集，64 截断，FNV-1a 确定性映射保 `tool_calls[].id`/`tool_call_id` 配对）；空 ID tool_use 跳过、孤儿 tool_result 降级 user 文本（不再必 400）；`tool_choice` 收窄 `allowed_tools`、`parallel_tool_calls` → `disable_parallel_tool_use`、参数经 `parseToolCallArguments` 压实。
  - 内容杂项：对话中段 system/developer 内联为 system-reminder user 轮（tool_result 块保持首位）；file part → Anthropic document 块；非允许 scheme 图片 URL（如 file://）丢弃；part 级 `cache_control` 透传；usage 合并 cache_creation 5m/1h 明细与 `server_tool_use.web_search_requests`，`mergeUsage` 按 max 合并嵌套计数（事件序无关）。
  - 合并解决：与 main 上 `9ca44b4`（F1-F6）/`829eacd`（responses 透传恢复）冲突时采用已落定的 F1 语义，PR 自带 thinking 断言同步更新。
  - 验证：`go test ./...`、`go vet`、`gofmt` 全过；三协议真实测试全过（muse-spark chat/responses/messages + thinking 推理链路）；Claude Code 长上下文稳态 99.8%、pi 三协议稳态 98-99.9%。

## v0.14.26

- Map downstream sessions to dedicated upstream sessions (`feat(session)`): 网关此前全部流量共用一个进程全局 `x-opencode-session`，在上游侧形成"超级会话"——所有下游 agent 钉在同一个 provider 粘性上、计费归因混合、一次 429 触发的本地解绑影响全体。已查证上游 zen handler（anomalyco/opencode v2）只读 `x-opencode-session` 且该值直接决定 provider 粘性与计费归因（`stickyId = sessionId ?: workspaceID ?: ip`），真实 opencode 客户端每个 agent 会话独立发 affinity 值（`parentID ?? fork.sessionID ?? session.id`）。新增有界映射表（下游会话身份 → 专属 `ses_`，`internal/app/opencode.go`）：
  - `ocSessionMap`：256 上限 + 滑动 2h TTL + LRU 淘汰（对齐 `stickyMaxEntries`，TTL 远大于 sticky 出口 15min 防止出口未换 session 先转），锁内创建防并发重复，`newOCSessionID()` 生成（满足上游免费层 `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$` 校验），`refreshOCSession`（admin reload）一并清空。
  - `downstreamSessionKey`：客户端会话头（`X-Claude-Code-Session-Id`/`Thread-Id`/`Session-Id` 大小写不敏感，复用 `sessionHeaderValue`）> body `user`（chat `req.User` / Claude `metadata.user`）> token 哈希作用域前缀防跨账号同名串扰；全程 sha256 8 字节 hex，原文不入日志。下游显式 `x-opencode-session` 不进映射、原样透传（客户端自有语义最高优先级）。
  - 解析顺序（`callOpenCodeEndpoint`）：显式透传 > 映射命中/新建 > 全局 sessionID > 临时随机（不回写）。`normalizedTransportScope` 输入自然变专属值，sticky 出口 `oc:` 后缀、keypool 路由零改动自动按下游会话隔离，429 解绑只影响出事会话。
  - 开关 `OPENCODE2API_OC_SESSION_MAP=off/0/false/no` 回退单一全局 session 行为；`/api/config` 暴露 `oc_session_map_size` 可观测；`buildOCRequest`/`systemone` 路径 `ocScope` 统一 `normalizedTransportScope` 哈希（与主路径一致）。
  - `projectID`/`clientVersion`/三头同值双发/prompt_cache_key 内容派生全部不变——上游 project 头零路由语义（纯来源标注），缓存身份与 session 头正交。
  - 验证：gofmt/vet/`go test ./...` 全过含 `-race`；ultracode 工作流三视角对抗评审 0 findings；mock 上游 E2E 12 项检查全过（同会话稳定、异会话隔离、显式透传、off 回退全局、reload 清空、`oc_session_map_size` 计数）。真实 7 轮 Claude Code agents 缓存复测两遍达标：总命中 78.7%/83.5%（基线 81.1-84.2%，遍 1 低 2.4pp 归因子代理冷启动 4 个 vs 基线 2-3 个），稳态 96-100%，`prompt_cached_tokens` 突破 40960 封顶随 prompt 增长（max 57344/59264），截断 0；pck 全程稳定且跨会话共享同一上游分片（两遍主对话 pck 同值，遍 2 冷启动仅 1 个）。回归测试 `oc_session_map_test.go`（稳定/隔离/透传/兜底/开关/淘汰/TTL/并发）。

## v0.14.25

- Strip volatile client injections and pin stable prefixes across both upstream body shapes (`fix(cache)`): Claude Code 逐请求注入易变内容（`<total_tokens>` 上下文计数、system-reminder 上下文、hand-back 通知），让上游请求前缀逐请求漂移、前缀缓存反复从漂移点截断。按两种上游 body 形状分别修复（`internal/app/instructions_stable.go`，接入点统一在 `buildOCRequestWithSubpathAndState` 的免费层指纹重做之后、`prompt_cache_key` 重算之前——指纹重做按客户端原文重建 body，此前的前置剥离都会被覆盖）：
  - 计数剥离扩展三种形状与三个 role：`stripVolatileCountersInMap` 剥离 instructions / input / messages 形状里 user、system、developer 消息的 `<total_tokens>` 计数块（regex 连同周围空白一起吃，防逐轮累积空行漂移；assistant 输出稳定不动；非文本 part 原样保留）。Responses 形状（muse-spark 系）计数走 instructions + user；chat 形状（ling 系）计数以 role=system join 进 messages[0] 尾部逐轮累积（实测 ling-3.1-flash-free 上累积 3 处、命中率 11.7%）。
  - chat 形状 system 钉首轮（`clipChatSystemStable` + `appendChatDeltaToLastUser`）：chat 协议把 system 提示 join 进 messages[0]，CLI 每轮往尾部追加 hand-back 通知（剥计数后仍逐轮增长 ~3K），上游前缀缓存从 system 增长点截断、tools 与会话历史全 miss（命中率封顶 ~17%）。首条 role=system 消息按会话注册首轮原文钉住、增量 TrimSpace 后并入末条 user 消息，注册表与回退行为同 Responses 形状的 `clipInstructionsToStablePrefix`（未命中/前缀不匹配时原样返回，不丢上下文）。
  - thinking 回放跳过（`claudeToResponsesBody`）：CLI 按 Anthropic 惯例只回放最后一轮 thinking，reasoning item 的有无/位置逐请求漂移，前缀缓存从首个 reasoning item 断；GLM 系上游推理自含，回放无增益。
  - pck 稳定段挑选：`prompt_cache_key` 的 tools 分支只哈希工具名集合（排序 join，异步 MCP/插件加载的 schema 漂移不再换缓存分片）；first_user 改取非 system-reminder 的稳定 part 并剥离计数（逐请求重写的 SR 注入块不再做会话标识）。
  - 调试设施：新增 `OPENCODE2API_DUMP_UPSTREAM=<dir>`——把发往上游的请求体逐个落盘（纳秒时间戳命名），用于缓存前缀字节级 diff 定位漂移段；默认关闭，仅限本地调试。
  - 验证（真实 4 轮 Claude Code agents 任务、同会话 resume、全程相同 `--allowedTools`）：muse-spark-1.3-contributor 总命中率 92.9%、主对话稳态 96-100%、计数残留 0、截断 0；ling-3.1-flash-free 主对话 7/8 请求 87-100%（唯一 low 是冷启动 probe）、计数残留 0/208 dumps、截断 0（28/28 `truncated=false`）。剩余低命中全为固有冷启动（每个新子代理 pck 按 first_user 隔离防串污染）与 CLI 2.1.293.45a 的 hand-back 中段插入（单次断点、断后即恢复——通知含子代理最终报告不可删，挪尾部实测更差，留原位为固有代价）。回归测试 `TestStripVolatileTokenCounters`、`TestStripVolatileTokenCountersSystemRole`、`TestClipChatSystemStable`、`TestStableUserTextFromParts`、`TestContentPromptCacheKey_ToolsNamesIgnoreSchemaDrift` 等；`docs/claude-cache-test-method.md` §6/§7 补两轮根因、验证结果与测试环境陷阱（8080 日常网关占用、CLI 429 后 fallback 桌面 3P OAuth 直连官方致流量绕网关、CLI 自动更新改变通知注入位置）。

## v0.14.22

- Fix muse-spark free-tier intermittent empty turns / truncation (`fix(responses)`, `fix(keypool)`, `fix(chat)`, `fix(httpclient)`): 免费档共享 Output-token 限速承压时，上游以 HTTP 200 + `response.created→response.incomplete`（零内容、usage 报 reasoning tokens）杀死请求；`PeekFirstFrame` 壳帧即 commit（`IsProductiveEvent` 是零调用死代码）+ `response.incomplete` 分支无零内容检查 → 客户端拿 200 空消息 `stop_reason=max_tokens`、零重试零报错，Claude Code 把空轮记入历史、agent 任务停摆（表现为「任务永远无法完成」）。六层修复（18-agent 排查对抗验证 0 驳回；mock 上游确定性复现闭环：empty_incomplete 从「200 空轮零重试」变为「pre-commit 换 key 重试 → 仍空 → 502 诚实失败」；真实网关 keypool 接管生效，`upstream_attempt` 出现 key_id 轮动）：
  - 产出感知 commit + 壳帧宽限窗（`fix(responses)`）：壳帧（created/in_progress/queued/output_item.added/content_part.added）与空 output 终态帧不再判产出 commit；首个壳帧后进入 `OPENCODE2API_SHELL_GRACE_MS`（默认 5000，`0` 恢复「任意帧即 commit」旧行为）宽限窗，到期仍只有壳帧按健康慢流放行（thinking 类上游首 delta 迟到不误重试）。限速杀死型空流在未向客户端写字节前经 `stream_empty_retry_max` + key_pool 换 key 重试；宽限窗 commit 后才到达的零内容 incomplete 补发 in-band error。
  - 终态 `output[]` 内容收割（`fix(responses)`）：`response.completed/incomplete` 分支补遍历 output、把未经 delta 通道流出的内容收割补发（reasoning summary 文本与 encrypted_content→signature、message output_text、function_call/shell_call arguments，与非流式路径对齐），`streamedItems`/`streamedOutIdx` 双键去重防重复补发；带内容的 incomplete 不再丢已产出内容。
  - 传输中断诚实失败（`fix(responses)`）：已 commit 的流中途死亡按错误类型分流——只有干净 EOF（`io.EOF`）才合成正常收尾（PartialEOF/ThinkingOnlyEOF 既有行为不变）；RST / unexpected EOF / 墙钟超时等传输中断补发 in-band `error` 事件，`stream_result` 记 `truncated=true`，Claude Code 得以感知失败并重试该回合，不再把半截文本/非法 JSON 的 tool_use 当完整回合记入历史。
  - 非流式残缺 SSE 兜底（`fix(responses)`）：免费层对所有请求强制 `stream:true`，非流客户端拿到被截断的 SSE（无终态帧）时——含 delta 内容则聚合为合成 response（`status:"incomplete"`）保留部分内容，连 delta 都没有则 502 诚实失败（Claude Code 对 5xx 自动重试整回合，优于 200 空消息被记入历史）；`"data:"` 无空格前缀与 PeekFirstFrame 对齐。
  - public auth 接入 key_pool（`fix(keypool)`）：免费模型 public 直连改为交由 key_pool round_robin 分摊每账号输出限额（kill-switch `OPENCODE2API_POOL_PUBLIC`，默认 on；池整体黑名单级失败自动回落 public 直连，绝不弄坏原本可用路径）。注意上线后 prompt 缓存亲和一次性重排（sticky 哈希基从 public-shared 变 token-based）。
  - 429 退避重试（`fix(keypool)`）：`callOpenCodeEndpoint` 的 429 重发加递增退避（Retry-After 头优先、缺省 1s/2s/3s 递增、cap 5s、尊重 ctx 取消），不再立即重发放大限速。
  - 上游墙钟超时可配置（`fix(httpclient)`）：`OPENCODE2API_UPSTREAM_TIMEOUT_SECS`（默认 900）。Go `http.Client.Timeout` 覆盖整个响应体流式读取——单出口 socks5 下任一 >5min 的 agent 长输出在恰好 300s 被掐且完全不可见；现为 900 可调，`<=0` 关闭整体墙钟。
  - chat 非流分支双重转换修复（`fix(chat)`）：`forwardChatViaResponses` 非流分支聚合结果已是 chat.completion 形时直接使用，不再走第二次 `convertResponsesToChat` 按原生 Responses 形读 `raw["output"]`（不存在）→ 正文被抹成空串而 usage 报 completion_tokens。
  - 回归测试 `muse_truncation_fix_test.go` 20 条（表驱动 + 端到端 mock 上游）；`docs/CONFIGURATION.md` 补三个环境变量与「壳帧宽限窗/产出感知 peek/传输中断诚实失败」行为说明；完整排查报告 `muse-truncation-report.md`（6 条根因、复现证据、验证缺口）。

- Fail loud on mid-stream transport death across anthropic upstream paths (`fix(anthropic)`, `fix(responses)`): claude→chat / chat→chat / responses→chat 翻译路径此前已各自 fail loud，最后三条伪造干净收尾的 anthropic 上游路径补齐（回归 `anthropic_truncation_fix_test.go` 10 条，含端到端空流换 key 重试）：
  - claude→anthropic 直通 `pipeAnthropicStream`（`fix(anthropic)`）：只有干净 EOF 才合成 `message_stop` 正常关流；RST / unexpected EOF / 墙钟超时先补发 in-band Anthropic `error` 事件再关流——此前任何读错误都伪造 `message_stop`，Claude Code 把半截 tool_use/文本当完整回合记入历史。
  - chat→anthropic `anthropicSSEToChatStream`（`fix(anthropic)`）：commit 后读错误按类型分流，干净 EOF 保持 finalize 合成 stop+[DONE]；传输中断补发 `upstream_truncated` 错误帧 + `[DONE]`（形状对照 chat.go emitError），不伪造 `finish_reason=stop`。
  - responses→anthropic `anthropicSSEToResponsesStream`（`fix(responses)`）：流式分支接入 `DriveStreamWithRetry`（与 chat→anthropic 同形）——此前整条路径无 peek，200 即占用、空流零重试；peek 首帧前置 + WriteHeader 延迟到首行，空流 EOF/读错/上游错误帧/首字节超时经 `stream_empty_retry_max` 换 key 重试；传输中断按 `response.failed` 收尾（response 带 error 字段、不置 SawFinish/DoneSeen，`stream_result` 记 `truncated=true`），不伪造 `status=completed`；重试全失败经 UpstreamErrorCapture 透传上游真实 status+body。

- Truncation-robustness knobs documented (`docs`): `OPENCODE2API_SHELL_GRACE_MS` / `OPENCODE2API_UPSTREAM_TIMEOUT_SECS` / `OPENCODE2API_POOL_PUBLIC` 三个环境变量与各路径截断标记形状（error 事件 / upstream_truncated 帧 / response.failed）说明。

## v0.14.21

- Fix free-tier 403 for Claude Code clients on tool-bearing requests (`fix(fingerprint)`): 上游免费层门禁按小写名**精确匹配**（大小写敏感），客户端（Claude Code）带 PascalCase `Bash`/`Glob`/`Grep`/`Read` 四件时，`ensureFreeTierTools` 的命中检查把 PascalCase 也算作已存在、跳过小写 stub 追加——上游门禁只认小写名，整档 403 `FreeTierError`（2026-10-07 public 实测）。`existing` 建键改为只记精确小写名，PascalCase 命中检查通过后仍追加小写 stub（终态 = 4 原工具位置保留 + 4 小写 stub，三个协议 subpath 同步）。回归测试 `TestEnsureFreeTierTools_CaseInsensitiveNoDup`（改为断言追加）+ `TestEnsureFreeTierTools_LowercaseNoDup`（小写幂等）。
- Fix upstream prefix-cache never hit by real Claude Code traffic (`fix(cache)`): `prompt_cache_key` 的内容键在 handler 层按免费层指纹/清洗**之前**的 tools 算出，而免费层指纹在 `buildOCRequestWithSubpathAndState` 追加 stub 后才发送——"哈希用的 tools" ≠ "实际发送的 tools"，key 分片与前缀错位。免费层且无 token 时按终态 bodyMap 重算 content key（`3cd1956`）。真实转发实测：稳定 system 多轮 `input_cached_tokens` 97% 命中（此前 0%）。
- Keep sticky egress on first 429/5xx retry (`fix(upstream)`): 重试分支此前首个 429/5xx 就 `invalidateUpstreamTarget` 换出口，每次都在新出口冷启动、缓存无法跨请求积累。现在首次失败保 sticky 同出口重试，仅连续失败才换出口（`attempt >= 1`）；transport_error 分支保持立即 invalidate。回归测试 `TestCallOpenCodeEndpoint429KeepsStickyFirstThenRotates`。
- Extend cache debug to the responses path (`feat(cache)`): `OPENCODE2API_CACHE_DEBUG=1` 时 claude-responses/responses 路径打出 `cache_debug_usage`（此前仅 chat 路径）；usage 白名单补 `input_tokens_details` 并原样打出未知键，避免误判零命中；content key 派生时打出分部件哈希（instr/tools/first_user + 长度），定位跨轮前缀漂移段（`cache_debug_key_parts`）。仅结构化计数与哈希，无请求内容。实测剩余瓶颈为 Claude Code 客户端 instructions 跨进程/跨轮注入（非网关改动）。
- Fix non-stream free-tier clients getting an empty body when upstream forces streaming (`fix(responses)`): 免费层上游强制 `stream:true`，客户端声明 `stream:false` 时上游仍返回 Responses SSE，非流分支直接 unmarshal 失败（`invalid character 'e'`）返回 189 字节空响应。新增 `extractResponsesJsonFromSse`：从 SSE 提取最后一个 `response.completed/incomplete/failed` 事件的完整 response JSON（自带 output/usage 全量），非 SSE 原样返回幂等；接入 `forwardClaudeViaResponses` 与 `probeClaudeViaResponses` 全部非流分支（首轮 + 同协议重试 + probe）。实测 `stream:false` 恢复完整 Claude JSON（含 usage/content）。回归测试 `TestExtractResponsesJsonFromSse` + `TestForwardClaudeViaResponses_NonStreamAggregatesForcedSSE`。
- Case-insensitive free-tier stub matching (`fix(fingerprint)`, superseded by the first fix above): `f4f9a1f` 统一 existing 建键为小写（`Bash==bash` 不重复追加）——实测该方向错误（见第一条），已由 `32dd2e9` 修正。

## v0.14.20

- Fix upstream 400 on over-length / illegal-char tool names at the anthropic upstream boundary (`fix(anthropic)`): 上游 Anthropic Messages API 强制 `tools[].name` / `tool_choice.name` / 历史 `tool_use.name` / `mcp_servers[].name` 满足 `^[a-zA-Z0-9_-]{1,64}$`，超长名（66 字符 MCP 风格名）或含非法字符名（点号、空格、Unicode）透传后被上游 400 拒绝。新增 `internal/app/name_compat.go` 确定性缩短/还原层，接入全部 5 条 anthropic 上游路径（claude 直通 / chat 入站 / responses 入站 / count_tokens / claude→responses）：缩短规则为纯函数（sha256 派生，合法名直通不变、含非法字符的短名按 rune 折叠为 `_`、超长名取折叠前缀 + 原名 sha256 后 8 字节十六进制恒 64 字符，同名输入恒同短名，tool_choice/历史 tool_use/tools[] 三处必然一致）；响应侧（流式 content_block 逐帧 / 非流式 content / 跨协议 Chat/Responses 转换）按相反映射还原，客户端可见名与所发逐字节一致，`mcp_servers[].name` 缩短时复合名双向映射；`taken` 集合 + prepare 两阶段遍历防折叠名抢注声明过的合法名、碰撞确定性消歧；全合法名快路径零改写。回归测试 `name_compat_test.go` 21 组 + 6 组真实 handler 集成回归。
- Remove the Responses native passthrough; `/v1/responses` uses the translation path only (`refactor(responses)`): 删除 `responses_passthrough.go`（1602 行：透传中继/探测/浮点归一化/echo 修复/透传清洗）及其测试；`/v1/responses` 入站不再透传到上游原生 `/responses`，翻译失败直接原样返回上游错误。模型注册表 + 探测判定搬到 `native_responses_registry.go`，名称重写机制并入 `name_compat.go`——claude/chat 入站行为不变（翻译失败回退探测 + 运行时记忆保留），显式 `protocol_rules` 与故障剔除语义不变。**行为变化**：`muse-spark-1.2/1.3-contributor` 系列经 `/v1/responses` 入站不再可用（其 chat/completions 通道上游整档 500，此前靠透传兜底）；claude/chat 入站不受影响。`e2e_launch_codex_test.go` 的 fake upstream 改为 chat/completions 协议，验证翻译路径端到端（真实 codex CLI）；`docs/API.md` / `docs/CONFIGURATION.md` 同步。

## v0.14.19

- Add zh/EN language toggle to the admin web UI (`feat(admin)`, issue #16):登录页与管理面板右上角新增「EN/中文」切换按钮，200+ 条 UI 文案（标签、toast、表格头、错误消息、占位符）全部走 i18n 字典。语言解析顺序 `?lang=` → `localStorage("admin_lang")` → `navigator.language`，默认中文，zh 字典值与旧界面逐字节一致（存量用户零感知）。新增 `internal/app/web/i18n.js`（210 对 key + `t(key, params)` 运行时，`{name}` 占位符），由 `GET /i18n.js` 免鉴权提供（登录页需要）；`auth.go` 服务端注入的登录错误消息改为传稳定 key、前端经 `t()` 本地化渲染；nav-tabs 在中间宽度改为收缩/横滚，不再把 header-actions 挤出卡片。新增回归测试 `TestI18N_KeyParity` / `TestI18N_NoChineseInHTML` / `TestI18N_ReferencesResolve`（web_i18n_test.go）。已知取舍：5 处原本 `<code>` 包裹的内联等宽样式改为纯文本；批量导入 textarea 占位符由多行变单行。

## v0.14.18

- Fix intermittent upstream 400 on Chat passthrough when the client sends only `max_completion_tokens` (`fix(chat)`, issue #35): the `max_tokens_cap` / `max_tokens_cap_per_model` budget injection unconditionally filled `max_tokens` whenever the client omitted it, and the resulting body then carried **both** `max_tokens` (=cap) and the client's `max_completion_tokens`. OpenAI deprecated `max_tokens` in favor of `max_completion_tokens` and requires them mutually exclusive; opencode zen 的部分后端实例严格校验并拒绝（`max_tokens and max_completion_tokens cannot both be set`），而另一些实例放行 —— 同一请求重放经常成功，表现为偶发失败且对 400 不做重试的客户端（如 ZCode）硬失败。`convertRequest` 现在在客户端已带 `max_completion_tokens` 时跳过 `max_tokens` 注入（双字段均未设置时仍注入 cap；显式 `max_tokens` 的收敛行为不变）。回归测试 `TestConvertRequest_NoMaxTokensInjectionWhenMaxCompletionTokensSet`（chat_bridge_test.go）。

## v0.14.17

- Fix streamed `tool_calls` landing outside `delta` on reasoning-heavy models (`fix(chat)`, issue #34): `muse-spark-*-contributor` intermittently emits stream `tool_calls` as a sibling of `delta` (`choices[0].tool_calls`) instead of inside it, so standard clients reading `delta.tool_calls` miss the call while `finish_reason` is still `tool_calls` and loop retries. New `hoistChoiceSiblingToolCalls` normalizes the shape at ingress (append after existing `delta.tool_calls`, idempotent) and is wired into every chat stream consumer: direct passthrough (`convertStreamChunkWithUsage`, before case-restore) + stats, `rawSSEReader` native-detect (reserialized), non-stream aggregator (`aggregateOpenAIStream`), `claudeStreamHandler`, `responsesStreamHandler`. Regression tests `TestChatStreamSiblingToolCallsHoistedIntoDelta` + end-to-end `TestChatStreamSiblingToolCalls_EndToEnd`. Live-verified on `muse-spark-1.3-contributor` (`stream:true` 7/7 clean, `delta.tool_calls` × 2 + `finish_reason=tool_calls`); note `stream:false` on the same model separately returns empty `content` with no `tool_calls` (independent issue, not covered).

- Protocol parity vs sub2api `apicompat` (`fix(chat)`, `fix(claude)`):对照 Wei-Shaw/sub2api `backend/internal/pkg/apicompat` 全量审计三条转换链路并补齐 9 项差异,官方 Responses 流事件文档确认事件语义,真实流量三协议 9/9 验证通过。
  - chat→responses 请求体:`parallel_tool_calls` / `service_tier` 透传上游(顶层键经 `preserveChatPassthroughKeys` 从原始 body 回填 ExtraBody,类型化结构装不下;`extra_body` 显式键优先);`response_format`(`json_schema` 展平 / `json_object` 透传)映射为 `text.format`(此前直接丢弃,structured-output 约束在该路径失效)。
  - responses→chat 流/聚合/非流式:`custom_tool_call`(custom/freeform 工具)纳入工具槽位,`custom_tool_call_input.delta/done` 与 function_call 同形累积(done 读 `input` 键);`incomplete` 按 `reason` 细分(`content_filter`→`content_filter`,其余→`length`,此前一律 `length`);上游 `service_tier` 回写 chat 顶层(流 chunk + 聚合 + 非流式);`response.done`(Realtime/WS 别名)与 `completed` 同等终结(此前聚合路径直接原样回吐上游体)。
  - chat→anthropic 请求体:thinking 生效即剥离 `temperature`/`top_p`(此前无条件透传,上游 400;与本项目 `convertClaudeRequest` 同口径)。
  - claude 响应:`content_filter` 的 `stop_reason` 由非法枚举 `refusal` 改为 `end_turn`(拒绝文本已进内容);`stop` 分支加 tool_use block 存在性回退(防止客户端不回传工具结果、对话卡死)。
  - usage/finish 闭集合:`anthropicUsageToChat` 补 `cache_creation_input_tokens`→`prompt_tokens_details.cache_creation_tokens` 归位;`normalizeFinishReason` 未知原因闭集合回退 `stop`(此前透传污染)。
  - 回归测试:`protocol_parity_sub2api_test.go` 新增 11 用例(custom 工具同 index/`input` 去重、`content_filter` 双路径、`response.done` 哨兵、`service_tier` 双路径、thinking 剥参、`cache_creation` 归位、未知 finish 闭集合、ExtraBody 回填等)。`docs/API.md` 同步。
  - 真实流量(新二进制,18358 direct + 18359 muse-spark responses-rule):direct chat 工具单 index、claude/responses 工具、chat 流 `[DONE]`、responses 流 `completed`、messages 流 `message_stop` 9/9 通过;thinking+temperature 同传 200。
- Fix muse-spark via Claude protocol never hitting upstream prefix-cache (`fix(cache)`): `claudeToResponsesBody` never injected `prompt_cache_key` / `prompt_cache_retention` — the chat→responses, native passthrough, and claude→anthropic paths all had the injection, only the claude→responses path missed it, so `/v1/messages` traffic on muse-spark models never warmed the upstream cache (`stats.json` showed ~3M prompt tokens with zero `cache_read_tokens` while chat-path `big-pickle` cached normally). The body now goes through `applyResponsesCacheHintsToRawBody` (responses-scoped: retention + session-derived key, no top-level `cache_control`), signature gains a `ctx` param at all 4 call sites. Also drops the non-spec top-level `stop` field the converter used to emit (chat path already dropped it). Regression tests in `claude_responses_cache_test.go`.
- Fix non-stream upstream error-in-200 swallowed as empty message (`fix(claude)`): some upstreams return HTTP 200 with an error-shaped body (`{"type":"error","error":{...}}`, e.g. model-side generation failure); `convertResponsesToClaude` failed to parse it as a success response and fell back to an empty text message with no usage, hiding the cause. The non-stream 2xx branches (forward first-try + same-protocol retry + probe) now detect the error shape via `isResponsesErrorBody` and relay it through the existing `convertResponsesErrorToClaude` as HTTP 502 with the upstream message preserved. Stream path already relayed `response.failed`/`error` events.

## v0.14.11

- Fix `input_tokens_details.cached_tokens` alias never reaching downstream cache stats (`fix(responses)`): muse-spark native responses upstreams report cache hits as `input_tokens_details.cached_tokens`, but `buildClaudeUsageCore` / `parseCacheUsage` only read the `prompt_tokens_details.cached_tokens` form — so client usage and gateway cache counters both dropped the read side. `responsesUsageToChat` now performs alias normalization (first-wins either direction, synced to the bridge converter's convention), restoring top-level `cache_read_input_tokens` + `readFromSplit` subtraction in Claude usage and cache-read recognition in stats. Regression tests in `chat_bridge_test.go` + `stats_test.go`.
- Fix native-responses models falling back to the chat translation path after a responses forward failure (`fix(claude)`): `claudeMessagesHandler` unconditionally dropped through to the chat translator when `forwardClaudeViaResponses` returned false, but muse-spark models memorized as native-responses are rejected by upstream on `/zen/v1/chat/completions` with `ModelProtocolUnsupported` — the "fallback" turned a retryable transport blip into a guaranteed 400. The handler now short-circuits with a structured 502 (`native-responses model cannot fall back to chat`) instead of touching chat; non-stream mid-body read errors additionally retry once on the same protocol before giving up. Regression tests in `claude_no_chat_fallback_test.go`.

## v0.14.10

- Fix Claude Code's `Error: No such tool available: glob/read` on free-tier models (`fix(opencode)`, PR #32). Root cause was self-inflicted, not the upstream rewriting names: `476314e` injected four **lowercase** placeholder tools (`bash`/`glob`/`grep`/`read`) to satisfy the 2026-09-18 free-tier fingerprint gate (requests missing any of them get 403). The model then called those lowercase names, but Claude Code registers `Bash`/`Glob`/`Grep`/`Read` **case-sensitively** and rejects the lowercase call. External twins: `router-for-me/CLIProxyAPI#1741`, `diegosouzapw/OmniRoute#11487`.
  - **Anti-invoke**: the four stub descriptions now prepend `Fingerprint only. Do NOT invoke this tool. ` (all three protocol shapes) to lower the chance the model even calls them (soft guard).
  - **Case restore**: new `freeTierStubCanonicalCase{bash→Bash, glob→Glob, grep→Grep, read→Read}` + `restoreToolNameCase(name)` — matches only the four exact lowercase stub names, idempotent, leaves other tool names (e.g. `weather`) untouched. Applied at every response touchpoint that writes a tool name back to the client: chat↔anthropic (non-stream + streaming `content_block_start`), claude→responses (input convert / non-stream / stream), claude→chat (`openAIToClaudeResponse` + `claudeStreamHandler`), the free-tier non-stream aggregator (`aggregateOpenAIStream`), chat→responses stream/aggregate, responses inbound stream, responses native passthrough (`restore` falls back to stub-case), and the claude→anthropic byte-relay (new `restoreAnthropicStreamLineCase` covering stream + peeked first frame, `restoreAnthropicBodyToolCase` for buffered non-stream). Adversarial verify caught 4 missed paths (notably the byte-relay) — all patched.
  - **Tests**: new `free_tier_case_restore_test.go` (10 cases — case table, anti-invoke prefix, aggregation restore, end-to-end `callOpenCodeAPI` delivering `Glob` to a PascalCase client, anthropic stream/buffered restore, claude→responses stream restore, non-stub passthrough, idempotency). Two prior tests corrected: `TestAggregate_ToolCallsAssembledByIndex` (now expects lowercase→PascalCase restore) and `TestResponsesSSEToChatStream_ToolArgumentsShareIndex` (its fixture used lowercase `bash` — the bug itself — now uses neutral `Bash`). `go build ./...` / `go vet` / `gofmt` clean; `go test ./...` green.

## v0.14.9

- Fix `400 Error from provider (Console): unknown parameter 'cache_control'` on `/v1/responses` for models routed to the native responses passthrough (`fix(responses)` / `fix(cache)`), regressing from v0.14.7's cache-hint patch. The passthrough previously called the shared `applyCacheHintsToRawBodyWithContext`, which injects a top-level `cache_control: {"type":"ephemeral","ttl":"1h"}` — but `cache_control` is only a legal **Anthropic Messages** field; on the OpenAI Responses surface it is an unknown top-level parameter, and strict-schema upstreams (Console) reject the whole request with `invalid_request_error`. The fix splits the injection: a new `applyResponsesCacheHintsToRawBody` is used on both native passthrough (`responses.go:1379`) and the chat→responses upstream bridge (`chat_to_responses_upstream.go`), keeping `prompt_cache_key` / `prompt_cache_retention` (zen prefix-cache hints remain effective — verified `cached_tokens:625` on `muse-spark-1.3-contributor-free` immediately after the fix) while no longer writing `cache_control`. The Anthropic-side passthrough keeps the top-level `cache_control` because the field **is** part of Anthropic Messages schema.
- Auto-recover on the chat translation path when the upstream still rejects top-level `cache_control` (`fix(cache)`): `responses.go` now detects a 400 whose body explicitly blames `cache_control` (`param="cache_control"`, or `message` containing `cache_control` + `unknown parameter` / `extra inputs` / `unrecognized|unsupported parameter` / `not permitted`), strips just the top-level key (per-message and tool-level `cache_control` blocks on Anthropic passthrough are preserved — those are legal), retries once, and **remembers** the model ID in-process so subsequent requests skip the injection entirely. `prompt_cache_key` / `prompt_cache_retention` stay on the retry, preserving cache-hit rate. Unrelated 400s (e.g. `Model is unavailable`) do not trigger the retry.
- New regression coverage (`protocol_regression_test.go`): passthrough skips top-level `cache_control`; rejection detector handles Console / `extra inputs` / `unsupported parameter` wordings and ignores message-text echoes; `stripTopLevelCacheControl` removes only the top-level key; marked models no longer get the field injected; unrelated 400s do not retry. `go test ./...` and `go vet ./...` clean.

## v0.14.8

- Docs (`docs(cache)`): clarify the v0.14.7 cache-hint patch's behavior — `cache_control` breakpoints are replayed on chat→claude translations, `prompt_cache_key` / `prompt_cache_retention` (default `24h`) are injected on responses passthrough + chat→responses translation, derived from the client `x-opencode-session` so per-session prefix caches survive across turns. Verified against `mimo-v2.6-flash` and `big-pickle` (turn-2 `prompt_cached_tokens` 28,672 → 32,448 within the ~32k cap).
- Fix (`fix(cache)`): preserve Claude `cache_control` breakpoints after Claude→Chat mapping and inject `prompt_cache` hints across all chat / responses paths; GLM/Zhipu skips the top-level injection (see v0.14.9 for the responses-side follow-up).

## v0.14.7

- Fix `/v1/models` hiding `-free` models from API key requests (`fix(models)`): the list endpoint ran every visible model through `publicFacingModelID`, which strips the upstream `-free` suffix before display — for every route, including authenticated ones. API key users therefore never saw the `xxx-free` variants in the catalog even though their key can call them, and Chat/Responses clients could not pick a free variant by its real ID. `replaceModelIDsWithAliases` now takes a `stripFreeSuffix` flag: only the public (keyless) route keeps the bare-name display (still paired with `resolveModel`'s bare→`xxx-free` reverse map, unchanged), while keyed requests (`auto` / `zen:` / `go:`) get the true upstream IDs verbatim. Explicitly configured aliases still win on both tiers.

## v0.14.6

- Fix `spawn_agent` (codex `multi_agent_v1`) unusable through the gateway (`fix(responses)` / `fix(launch)`), two cooperating root causes:
  - **Responses bridge silently dropped `namespace` tools** — codex 0.156 declares its multi-agent tools as `{"type":"namespace","name":"multi_agent_v1","tools":[... spawn_agent / wait_agent / send_input / ...]}` (sub-tools are plain `type:"function"` entries, per codex-rs `tools/src/responses_api.rs`). `convertResponsesTools` only recognised flat `function` / `apply_patch` / `shell`, so the whole namespace hit the default branch and was dropped before reaching the upstream — the model never saw `spawn_agent` and answered "tool unavailable". The bridge now flattens namespace sub-tools to Chat `function` tools by their **bare** sub-tool name (description prefixed with the namespace hint), and replays upstream calls as `function_call` with the name restored to the bare sub-tool name plus the `namespace` field set from kind metadata — matching the wire contract codex's router expects (`(namespace, name)` pair; a dotted `ns.name` would be rejected as `unsupported call`). `tool_choice` naming a namespaced tool (`{"type":"function","namespace":"ns","name":"ns.tool"}`) is normalized to the bare name, which is what the flattened upstream tools carry. `ResponsesTool` gains a `Tools []ResponsesTool` field to carry the namespace children.
  - **`launch codex` injected a temporary `model_catalog_json` that suppressed the client-side tool declaration** — codex only emits the `multi_agent_v1` namespace when the model's catalog advertises the corresponding capability, and the launch-time catalog (synthesized from the opencode modeldev list) left the fields unset, so codex never sent the namespace down the wire in the first place. `launch codex --no-model-catalog` skips the injection so codex falls back to the user's own `~/.codex/config.toml` catalog, whose ppio/cc-switch entries carry the right capabilities. Verified end-to-end via `opencode2api launch codex --no-model-catalog -- exec ... "spawn a subagent ..."`: `spawn_agent` is called, returns an `agent_id`, and `wait_agent` reports the subagent's completion.

## v0.14.5

- Request `reasoning.summary:"auto"` on **all** native responses upstream paths (`fix(reasoning)`): upstreams like muse-spark reason *silently* when only `reasoning.effort` is sent — `summary` stays an empty array, so clients see `reasoning_tokens` grow with no visible thinking text (Claude Code shows nothing; codex shows no thinking). A shared `responsesReasoningBody(effort, modelID)` helper now builds the outbound `reasoning` field and is used by both converters — chat→responses (`chatToResponsesBodyWithRaw`) and claude→responses (`claudeToResponsesBody`) — and the native responses passthrough (`sanitizeResponsesPassthroughBody`) injects `summary:"auto"` when the client omitted it (an explicitly supplied `summary` is respected untouched). The field only *requests* a summary: sensitive reasoning still travels the `include:["reasoning.encrypted_content"]` encrypted channel (surfacing as `thinking.signature` for roundtrip), while the visible summary is generated upstream, so sending `summary:auto` is safe on upstreams that ignore it. The passthrough also now deletes the whole `reasoning` object when effort normalizes to empty (e.g. `none` or an unwhitelistable value) instead of leaving a meaningless summary-only request that can 400, and correctly marks the body as changed when only the effort was normalized.
- Clamp `reasoning.effort` to the muse-spark whitelist inside the shared helper (`max`→`xhigh`, unnormalizable → omit `reasoning` entirely), keeping the chat and claude converters aligned with the passthrough path's `normalizeResponsesEffort` so no path can send an effort value the muse-spark native responses endpoint rejects with 400.
- Fix 401 "Model not supported" for `*-free` models with a `[1m]` context suffix (`fix(launch)`): free variants have no 1M tier, so upstream rejects `xxx-free[1m]`. `resolveLaunchModel` now strips the context suffix for free models first (walking the existing `switch`), keeping the standard free window and the `autoCompactWindow` (ctx × 0.9) instead of appending `[1m]`.

## v0.14.4

- Fix missing thinking/reasoning output for `mimo-v2.6-flash` (`fix(reasoning)`): mimo (via the OpenCode `/zen/v1/chat/completions` upstream) returns reasoning as OpenRouter-style fields — `delta.reasoning` (string) and `delta.reasoning_details[].text` — not the canonical `delta.reasoning_content` the gateway's converters read. Every reasoning consumer only looked at `reasoning_content`, so the thinking chain was silently dropped: chat passthrough surfaced a non-standard `reasoning` field that standard clients ignore, and the claude/responses converters emitted **no** thinking/reasoning blocks at all (so reasoning never displayed). A new `normalizeReasoningContent` hoists `reasoning` / `reasoning_details[].text` into `reasoning_content` (never clobbering an existing value), applied at the three places that consume upstream chat deltas — `cleanStreamDelta`/`promoteMisplacedReasoning` (chat), `claudeStreamHandler` (claude), the responses stream converter — and in the stream stats (`logging.ObserveDelta`) so `reasoning_chars` is counted correctly. Verified live against `mimo-v2.6-flash` on all three protocols (chat/messages/responses now emit reasoning, previously `reasoning_chars` was 0 on all), with no regression on models that already emit canonical `reasoning_content` (mimo-v2.5, nemotron-3-*, ling-3.0-flash-fin, big-pickle, space-bunny).

## v0.14.3

- Fix the misleading `stream disconnected before completion: Incomplete response returned, reason: max_output_tokens` errors codex hit against the native responses passthrough (e.g. `muse-spark-*-contributor`): two complementary fixes.
  - **Unify output-budget injection everywhere** (`fix(tokens)`): when `max_tokens_cap` / `max_tokens_cap_per_model` is configured and the client omits `max_tokens` / `max_output_tokens`, the gateway now injects the cap as the upstream budget on **all** paths — chat `convertRequest`, claude→responses `claudeToResponsesBody`, and the anthropic raw-body passthrough (now lifts sub-128 values to the 128 floor via `clampClaudeMaxTokens`, same as the responses passthrough contract from v0.14.2). Without an explicit cap the anthropic count_tokens passthrough stays lower-only, and claude→requests keep the 8192 max_tokens default. Previously only the responses passthrough injected; chat/claude paths still let the upstream fall back to its own much smaller default, so the model silently truncated long completions and the gateway surfaced that as a fake `max_output_tokens` incomplete.
  - **Auto-continue truncated streams** (`fix(responses)`): even with a large cap, upstream `response.incomplete` + `reason=max_output_tokens` can legitimately occur (model-side per-turn budget). The stream relay now withholds that terminal event from the client, issues a continuation request carrying the accumulated `output` items (reasoning/message/tool_call) plus a `Please continue.` instruction — up to 3 rounds — and, when a round completes, emits a single synthesized `response.completed` with the merged `output` and the last round's `usage`. Failed continuations fall back to relaying the original incomplete verbatim. Non-terminal SSE lines still stream one line at a time, so the typewriter effect is preserved; `completed` / `failed` / other incomplete reasons pass through byte-identically. Verified live against `muse-spark-1.3-contributor` via `launch codex exec` (long-form generations that previously died on the first incomplete now continue across rounds and land a single coherent completed response).

## v0.14.2

- Fix spurious `incomplete: reason=max_output_tokens` on Native `/v1/responses` passthrough when the client omits `max_output_tokens`: codex under native-responses routing does not emit `max_output_tokens`, so the upstream fell back to its own default output budget and aborted the SSE stream without `response.completed`; the gateway's EOF guard then synthesized `response.incomplete` with `reason=max_output_tokens`, which the codex client correctly bubbles up as "stream disconnected before completion". The native passthrough path now consults the same `max_tokens_cap` / `max_tokens_cap_per_model` rules as the chat→anthropic and chat→responses translation paths, and injects / clamps `max_output_tokens` into `[128, cap]` before forwarding. Adds observability: a structured log line records the effective `max_output_tokens` and `cap` per passthrough request.

## v0.14.1

- Fix muse-spark multi-turn failures on `/v1/responses` passthrough (`invalid_request_error` `` `arguments` `` must be valid JSON`): muse-spark occasionally emits `function_call` items with `arguments: ""` (typically hallucinated tool names during sub-agent dispatch). The Codex client replays the failed call verbatim into the next turn's `input`, and the upstream's strict server-side replay validation then rejects the entire request with 400 — so from the second turn the session is unrecoverable. `sanitizeResponsesPassthroughBody` now runs `coalesceReplayedToolCallArgs` on the passthrough path: history `function_call` / `custom_tool_call` / `local_shell_call` / `mcp_call` items with missing/empty/non-string `arguments` are coalesced to `"{}"` (object/array → JSON-serialized; valid JSON strings pass through unchanged, idempotent); `function_call_output` and visible text are never touched.
- Foreign reasoning `encrypted_content` replay repair is now preventive instead of reactive: the old flow waited for the upstream to 400 (`reasoning` `encrypted_content` was not issued to this caller) before stripping the caller-bound `id` + `encrypted_content` and re-sending — which cost every session one wasted 400 round-trip whenever the sticky egress rotated or the gateway restarted. `sanitizeResponsesPassthroughBody` now strips the replay echo fields on every outbound request (summary preserved); `callResponsesWithEchoRepair` remains as a fallback for non-muse-spark paths. Verified end-to-end against the live upstream: replayed empty-`arguments` + foreign reasoning echo now returns HTTP 200 with clean `response.completed` + `[DONE]` on `muse-spark-1.3-contributor-free` (the paid `muse-spark-1.3-contributor` tier stops at the unrelated workspace privacy wall, confirming the arguments fix is no longer the blocker).

## v0.14.0

- Fix `muse-spark-1.2/1.3-contributor-free` chat and streaming requests against the public free tier (upstream `500` on chat/completions, `403` on responses without tools fingerprint): the contributor tier is registered as native-responses-only by pattern (matching the opencode upstream's gating in `zen/util/handler.go`), and the free-tier tool injection now emits OpenAI `{"type":"function","name","parameters"}` shape on the `/responses` subpath instead of the Anthropic `input_schema` form. Non-stream chat clients on a forced-`stream:true` responses route get their SSE aggregated back into `chat.completion` JSON. Verified end-to-end via the live gateway on both `muse-spark-1.2-contributor-free` and `muse-spark-1.3-contributor-free`.
- Add `POST /v1/systemone` passthrough for TypeSafe System One models (`jev-1.13*`): these evaluate `state` + typed `questions` → structured `answers` and cannot be driven by Chat/Responses/Messages, so they get a dedicated inbound endpoint that relays verbatim to upstream `/zen/v1/systemone` (body preserved except `model`). Free-tier fingerprint rewrite stays scoped to the three text protocols and does not touch `systemone` (`internal/app/systemone.go`).

## v0.13.0

- Harden cross-protocol conversion across all three inbound × upstream directions per the official OpenAI/Anthropic specs (Chat Completions ↔ Responses ↔ Messages). Request side:
  - Unified max-token resolution: `max_completion_tokens` wins over `max_tokens`, clamped to `[128, cap]` (`max_tokens_cap` / `max_tokens_cap_per_model`), default 8192 on the Anthropic paths.
  - Chat→Responses drops the non-spec `stop` field and sets `store:false` + `include:["reasoning.encrypted_content"]`.
  - Responses→Anthropic normalizes `tool_use`/`tool_result` pairing (drops unanswered calls, orphan results, and illegal-JSON pairs), merges adjacent same-role messages, defaults empty schemas with `strict:false`, and downgrades `tool_choice` when its tool was dropped.
  - Claude→Responses filters `x-anthropic-billing-header` system blocks, lifts `tool_result` media into a separate user input item, emits `(empty)` for empty output, and maps `disable_parallel_tool_use` → `parallel_tool_calls:false`.
  - Enabling thinking/reasoning now strips the mutually exclusive `temperature`/`top_p`/`top_k` sampling parameters (avoiding upstream 400s) while keeping `output_config.effort` → `reasoning_effort`. Unknown server content blocks are serialized back instead of dropped, and assistant `reasoning_content` only piggybacks on tool-call turns.
  - `internal/domain` gains `MaxCompletionTokens` / `Stop` / penalties / `LogitBias` / `N` / `User` / `ResponseFormat` / `Seed`, `Message.Refusal`, `ToolFunction.Strict`, and `ClaudeRequest.ServiceTier`.
- Harden stream finalization on every converting path: idempotent EOF finalize (finish chunk + conditional usage chunk + `[DONE]`), stray upstream `[DONE]` consumed before terminal events on passthrough, `function_call_arguments.done` prefix-diff top-up and `[DONE]` guaranteed after failed/error events on Responses→Chat, `tool_use` initial-input fallback delta with `output_index`-keyed done events on Anthropic→Responses, and `signature_delta` emitted from `encrypted_content` before thinking blocks close on Responses→Claude (upstream `response.id` adopted as the message id).
- `/v1/messages/count_tokens` now forwards to the upstream `/zen/v1/messages/count_tokens` when a protocol rule resolves the model to the anthropic protocol, returning the exact count; it falls back to the local heuristic when no rule matches, when the rule resolves to chat/responses, or when the upstream call fails (Anthropic passthrough also relays non-2xx as `application/json` instead of forcing SSE).
- Fix Responses→Chat tool-call state being keyed by an unstable identifier (#17): `registerToolKeys` now allocates the chat index first and aliases both `item_id` (`fc_…`) and `output_index` onto it, so `response.output_item.added` (which names the call by `call_id`) and `response.function_call_arguments.delta` (which may name it only by `output_index`) can no longer open separate indices — previously a client merging deltas by index saw a named call with empty arguments followed by arguments with no name. Accumulated arguments and announcement flags move from `map[string]` to `map[int]`. Also removes an unchecked `item["id"].(string)` that panicked on an added event with no id.
- Fix Anthropic→Responses stream conversion losing blocks on EOF: content blocks are now tracked in a per-block struct (kind / item ID / output index / tool index / text builder) instead of parallel maps, `output_index` is allocated at `content_block_start` so done events always pair with their added event, and the stop and EOF paths share one `closeBlock` helper. `ensureTerminal` closes all leftover text/thinking/tool_use blocks sorted by `output_index` and backfills them into `response.completed.output`, so partial text, reasoning, and tool calls survive upstream disconnects. Duplicate `content_block_start` now fails the stream once instead of leaking events past `[DONE]`.
- Fix Claude↔Chat streaming losing the whole tool input when the upstream carries the full input on `content_block_start` and sends no `input_json_delta`: the start block's initial input is cached and emitted as a fallback at `content_block_stop` (defaulting to `{}` for zero-argument tools), so concatenated client-side arguments stay valid JSON.
- Fix Anthropic passthrough stream rewriting: `pipeAnthropicStream` now forwards bytes via `io.Copy` through an `io.MultiWriter` (client + `io.Pipe`), so CRLF/LF endings, blank-line event boundaries, and partial reads reach the client byte-identically while a sidecar reader still observes usage from `message_start`/`message_delta`. A `flushWriter` flushes every upstream chunk (plain `io.Copy` buffered SSE until EOF), and the EOF path joins the copy goroutine so nothing writes to the `ResponseWriter` after the handler returns.
- Log the upstream protocol resolution source (`rule` / `remembered` / `chat` default) in the passthrough `via` attribute and fallback warnings, so a route that could come from either an explicit rule or the native-responses memory is unambiguous in logs.
- Fix free-tier upstream fingerprinting for issue #19: since 2026-09-16/18 `opencode.ai/zen/v1` rejects free-tier requests unless `tools` contains bash/glob/grep/read **and** `stream:true` (else 403 `FreeTierError`); the gateway now appends only the missing required tools after whatever the client declared (preserving existing tool names/shape, on both OpenAI `tools[].function.name` and Anthropic `tools[].name` shapes) and forces upstream streaming, aggregating the SSE locally for non-streaming clients (`internal/app/opencode_fingerprint.go`). The fingerprint decision is keyed off `isFreeModel(resolvedModelID)` (doc § issue #19 ablation), **not** the client auth tier — a real `sk-` key on a free model is held to the same gate — and applies uniformly across the three upstream subpaths `chat/completions` / `messages` / `responses` (`messages` skips `stream_options` since that isn't Anthropic schema; `count_tokens`-style billing subpaths remain untouched). Session fingerprinting now sends `x-session-id`/`x-session-affinity` alongside the existing `x-opencode-session` (same stable value for sticky egress), and the UA template matches the real OpenCode client (`opencode/<ver> ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14`) with the minimum raised from 1.17.0 to **1.18.0** (live ablation 2026-09-18; fallback `1.18.31`). Note: `x-opencode-session` was **not** retired upstream — both header names pass — and `muse-spark-*-contributor-free` 500s are a tier rejection, not fingerprinting. Evidence: `docs/labs/2026-09-18-fingerprint-ablation.md`.

## v0.12.0

- Add configurable upstream protocol routing (`protocol_rules`), aligned with sub2api's `OpenCodeGoProtocolRule` approach: requests are routed to the OpenCode Zen **native upstream endpoint** matching the model, instead of always translating through Chat Completions:
  - `chat_completions` (default fallback, `/zen/v1/chat/completions`), `anthropic` (`/zen/v1/messages`), `responses` (`/zen/v1/responses`); the Go surface (`/zen/go/v1/*`) is selected by the existing catalog rules as before.
  - Rule syntax: exact model ID or a single trailing `*` wildcard (e.g. `claude-*`, `*`), case-insensitive, declared order = priority, first match wins. Validation: ≤64 entries, pattern ≤128 chars, no whitespace, protocol enum. `[1m]`-style context suffixes are stripped before matching, so an exact rule also covers suffixed variants.
  - Priority: explicit rule > native-responses runtime memory > default Chat Completions. **Default behavior is unchanged when no rule matches**; `/v1/chat/completions` applies only explicit rules so the existing translate→probe→passthrough fallback stays intact, while `/v1/messages` and `/v1/responses` apply the full priority (their memory dispatch already exists today).
- New cross-protocol conversions covering every inbound × upstream combination, with streaming state machines for both directions:
  - `/v1/messages` → anthropic: faithful body passthrough (only `model` rewritten); SSE relayed byte-for-byte with side-channel usage accounting.
  - `/v1/chat/completions` → anthropic: system extraction, `tool_calls`↔`tool_use`/`tool_result` (consecutive same-role merging), `max_tokens` default 8192 + per-model cap, `reasoning_effort`→`thinking.budget_tokens`, Anthropic SSE→Chat chunks (content / reasoning_content / tool_calls / finish / usage / `[DONE]`).
  - `/v1/chat/completions` → responses: `instructions`, function_call/function_call_output items, `max_output_tokens`, `reasoning.effort`; Responses SSE→Chat chunks.
  - `/v1/responses` → anthropic: reuses the handler's converted messages (multimodal + text-only degradation preserved), Anthropic SSE→Responses events (`response.created` / `output_text.delta` / `function_call_arguments.delta` / `output_item.done` / `completed`/`incomplete`), `storeResponseState` keeps `previous_response_id` chains working.
- Streaming, tool calls, reasoning, usage stats (incl. `cache_read`/`cache_created`) and error shapes are converted per inbound protocol; upstream HTTP status codes pass through faithfully (e.g. a Claude inbound sees Anthropic error bodies as-is, a Chat inbound gets them converted to Chat error shape).
- Configuration, three ways, with clear precedence:
  - `config.json` `protocol_rules` (lenient: invalid entries dropped with a warning).
  - Admin panel POST `/api/config` (strict: any invalid entry rejects the whole request with 400 and nothing is persisted) — hot-applies to the next request.
  - Admin UI "上游协议路由规则" table: ordered rules with move up/down, delete, and one-click presets (`claude-*`→anthropic, `gpt-*`→responses, `muse-spark-*`→responses, `qwen*`→anthropic).
- `buildOCRequestWithSubpath` injects `anthropic-version: 2023-06-01` and switches `Accept: text/event-stream` for streaming on the `messages` subpath; all upstream calls keep the shared retry / sticky-egress / multi-domain machinery and `x-opencode-*` headers.
- Docs: `CONFIGURATION.md` documents `protocol_rules` (syntax, priority, validation, caveats); `API.md` documents the routing behavior; `config.example.json` ships an empty `protocol_rules` example. `/v1/messages/count_tokens` remains a local heuristic and is unaffected by routing.
- Tests: rule matching/validation/priority (incl. rule > memory > chat), config-file lenient loading, admin GET/POST round-trip + 400 protection, per-protocol request/response conversion for buffered and streamed modes, and dispatch regression pinning the no-rule default path. Verified end-to-end against the real upstream with the public key (18 checks across three subagents: responses routing, default-path invariance incl. tool calls, anthropic routing + admin hot-reload).

## v0.11.2

- Fix free-tier (`Bearer public`) requests being rejected by the upstream with 403 `FreeTierError` "OpenCode's free tier can only be used from within OpenCode":
  - Live ablation against `opencode.ai/zen/v1` shows the free tier validates the `x-opencode-session` shape: it must match `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$` (12-hex timestamp prefix + 14 base62 chars, the OpenCode client's `descending()` identifier format). The gateway's old `ses_` + 24 random chars was rejected.
  - It also validates the client version: `User-Agent: opencode/<ver>` below 1.17.0 answers 426 `UpgradeRequired`. The npm fallback version is bumped from 1.15.3 to 1.18.31.
  - `x-opencode-request` now mirrors the client format (`msg_` + the same 26-char ID body); the upstream does not validate it, but requests now look identical to real OpenCode client traffic.
  - Verified end-to-end: direct upstream ablation matrix (403/426 → 200), plus gateway chat / streaming SSE / Anthropic messages / Responses all 200 with anonymous access.
- Fix `/v1/responses` native passthrough rejecting long-running conversations with the upstream 400 `reasoning \`encrypted_content\` was not issued to this caller` (reproduced with `muse-spark-*` + Codex):
  - The upstream binds replayed reasoning `encrypted_content` to the original caller (account + egress). When the gateway's sticky egress/domain re-binds mid-conversation (sticky TTL expiry, re-bind after a 429/5xx retry, process restart, key change), every later turn replays foreign reasoning content and fails — including Codex `exec resume` of an existing thread.
  - The gateway now recognizes that exact 400 (`isForeignReasoningEchoError`), strips the replayed reasoning echo (`id` + `encrypted_content`, both are caller-bound) from `input`, and re-sends the request once (`callResponsesWithEchoRepair`, shared by the probe and forward paths). Visible messages, tool calls and tool outputs are unchanged; only the stale reasoning payload is dropped, and the upstream re-issues reasoning for the current caller.
  - Other 400s keep byte-for-byte passthrough semantics, and requests without replayed reasoning echo still make exactly one upstream call.
  - Verified with `launch codex` on `muse-spark-1.3-contributor-free`: a thread built through a SOCKS5 egress then resumed over a different egress fails before the fix (Codex `Reconnecting... 5/5`) and completes normally after it, logged as `responses reasoning echo repair`.

## v0.11.1

- Internal refactor & hygiene (no user-facing protocol change):
  - Extract the duplicated SSE reader prelude (reader goroutine, keepalive ticker, close-wait cleanup) from the Claude / Responses / Claude-Responses stream handlers into a shared `streamReader`; add unit tests for Close-unblock and final-line-with-EOF races.
  - Deduplicate protocol converters: reuse the shared `thinkingBudgetToEffort` helper in `chat.go`, reuse `modelSelectionEntries` (now with a `freeOnly=false` mode) in the Codex model catalog, and merge the duplicated `extractModel*FromExtraArgs` pair into one flag-parameterized helper.
  - Synthesize `total_tokens` from `prompt_tokens + completion_tokens` when the upstream omits it, so chat-shape usage recording is no longer dropped.
  - Delete the dead `internal/ids` package, remove self-produced `logging.File/Level/Bodies` exported vars in favour of `Init(path, level, bodies)` params, and remove uncalled `stats` file-lock variants.
  - Serialize `config.Update`'s read-modify-store with a package mutex so concurrent admin writes cannot interleave.
  - Restore truncated doc comments in `claude.go` / `responses.go`, delete 5 unused sticky header constants, split version vars out of `main.go` into `version.go`, and slim `config.example.json` to non-default entries only.
- Fix `-debug` being silently ignored: `-debug` now bumps the slog level *before* logging init through a shared `applyLogLevel` helper, and an explicit `-log-level` still wins over `-debug`. Regression tests cover both directions.
- Docs: `API.md` documents `POST /v1/messages/count_tokens`; `CONFIGURATION.md` documents `native_responses_models` semantics; `DEPLOYMENT.md` compose section collapses to a link; archived the old design plan to `docs/labs/`.

## v0.11.0

- Text-only model detection is now driven by the models.dev catalog `modalities.input` data instead of a hardcoded `deepseek` prefix:
  - Known models reporting text-only input (e.g. `deepseek-v4-flash(-free)`, `glm-5.2`, `qwen3-coder*`, `grok-code`) now get the same `[image attached]` / `[document attached]` downgrade everywhere (Chat, Responses, Claude).
  - Multimodal models that used to be falsely caught by the `deepseek` prefix (e.g. `deepseek-v4-flash-vision-exp`) now keep their images.
  - `text_only_models` is still honored as an explicit case-insensitive prefix override on top of the catalog data; its default is now empty. Unknown models are never downgraded — with no catalog reachable, requests fail open and the upstream error remains truthful.
- Free-tier model detection now also honours the models.dev `cost` data instead of only the `-free` name suffix:
  - `big-pickle` (no `-free` suffix, but zero-cost upstream) now appears in `/v1/models`, the interactive launch selector, and the Codex `--model` catalog when running without an API key.
  - The check only consults the `opencode` provider entry in the models.dev catalog, so other vendors' promotional zero-cost pricing cannot leak into the public tier.

## v0.10.3

- Internal refactor (no user-facing behavior change): split the monolithic `internal/app` package into focused subpackages — `internal/config` (immutable config snapshot + accessors), `internal/logging` (logger init, request tracing, stream stats, redaction), `internal/stats` (token/cache usage accounting), `internal/modelsdev` (models.dev catalog fetch/cache with injectable proxy-aware HTTP client), and `internal/util` (shared helpers).
- Fix token usage accounting to correctly record integer-valued tokens, which a `float64` type assertion previously dropped.
- Fix a data race on the `socks5_sticky` flag when the config is reloaded concurrently (found via `go test -race`).

## v0.10.2

- Session-aware launch stub and upstream routing:
  - Preserve downstream client session identity for Claude Code (`X-Claude-Code-Session-Id`) and Codex (`Thread-Id` / `Session-Id`) headers.
  - Hash client session IDs for local sticky-routing keys and logs instead of echoing them verbatim.
  - Keep local sticky entries for downstream OpenCode sessions even when only one upstream domain is configured, so retries can rebind without losing the client session.
  - Propagate `x-opencode-session` downstream values to upstream OpenCode calls; generate a session fallback only when neither the client nor launch session supplies one.
  - Reset the sticky rebind sequence whenever the configured upstream base URL list changes.

## v0.10.1

- Fix `muse-spark-*` `/v1/responses` 400 `name` must be at most 64 characters for over-64-character connector/tool names sent by Codex:
  - Shorten outgoing `tools[].name`, `tools[].function.name`, `tool_choice.name`, and `input[].name` fields beyond 64 characters with a deterministic prefix + SHA-256 suffix.
  - Restore the original name in upstream response/SSE `function_call` events before relaying back, so clients (including Codex) see unchanged tool names.
  - Keep textual output untouched; non-`muse-spark` models remain byte-for-byte passthrough.


## v0.10.0

- Claude to native Responses passthrough:
  - `POST /v1/messages` now converts Claude Messages to the upstream native `/responses` endpoint when the chat translation path fails (404/500/501/502) or the model is remembered as native-only, reusing the passthrough memory registry shared with `/v1/responses`.
  - Lenient conversion throughout: unsupported blocks degrade to text annotations instead of HTTP 400; assistant message parts use `output_text` (upstream rejects `input_text` on assistant messages); tool schemas are normalized so `required` covers every property key; `reasoning.effort=max` maps to `xhigh`.
  - Native Responses output (message/reasoning/function_call/apply_patch/shell) converts back to Claude content blocks for both unary and SSE streaming.
- muse-spark strict-upstream hardening (gated to `muse-spark` models only):
  - Function-call arguments with integral floats (`1000.0`) are normalized to integers outside JSON strings in both unary and streaming relay, fixing strict clients (Codex Rust `usize` parsing) while leaving visible text like `echo 1.0` untouched.
  - Responses tool schemas and reasoning effort are sanitized before native passthrough.
- Verified end to end with `launch claude` and `launch codex` on `muse-spark-1.3-contributor`, including multi-turn tool-use and shell execution.


## v0.9.1

- Fix `SSE stream ended without [DONE]` on native Responses passthrough models (`muse-spark` etc.): the streaming relay now appends a `data: [DONE]` sentinel on clean EOF when the upstream ended its `/zen/v1/responses` stream without one (same upstream quirk already tolerated in the chat translation path). Streams that already carry a sentinel are relayed verbatim with no duplicate, and empty zero-event streams still pass through untouched so upstream failures are not masked.

## v0.9.0

- Native Responses Passthrough with Memory:
  - Requests whose chat translation path fails (404/501/502/500) are automatically probed against the upstream native `/responses` endpoint; on success the model is remembered and served directly on subsequent requests.
  - Confirmed models are served by a fidelity reverse proxy that preserves upstream 4xx/5xx status codes and error bodies instead of masking client errors as 502.
  - Unified all upstream calls (`chat/completions`, `responses`) on `callOpenCodeEndpoint` with retry/rotation, context propagation, and structured attempt logging.
- Streaming Relay Quality:
  - SSE is relayed line-by-line with per-event flush, fixing the 32KB `io.Copy` buffering stall that broke typewriter streaming.
  - Tail `usage` is extracted from `response.completed`/usage events for token accounting, upstream rate-limit headers are forwarded, and completed responses persist session state for `previous_response_id` chains.
- Safer Probing & Self-Healing Registry:
  - Probing never fires on 401/403/429/400, typed conversion errors, context cancellation, or transport errors, avoiding retry storms under rate limiting or credential failure.
  - Static preset models (`muse-spark-1.3-contributor`) skip translation from the first request; the new `native_responses_models` config merges additively without clearing learned models; dynamically learned models are evicted after consecutive failures while static models are immune.

## v0.8.0

- Redesigned Admin Dashboard & Workspace Layout:
  - Upgraded Web Admin UI to a modern top-tab layout with four dedicated workspaces: Overview & Telemetry, Models & Routing, Network & Proxy, and System Settings.
  - Added Live Match Tester and quick preset buttons for interactive model alias rule evaluation.
  - Fully exposed upstream behavior controls (`prompt_cache_retention`, `cache_control_breakpoints`, `text_only_models`) and network options (`socks5_sticky`) to prevent silent config overwrites when saving via the panel.
- Advanced Model Alias Matching Rules:
  - Expanded `model_alias` with multi-pattern matching rules (`exact`, `contains`, `prefix`, `suffix`, `regex`, `wildcard`).
  - Automatically preserves and propagates bracket suffixes (e.g. `claude-3-7-sonnet[1m]` maps to target model while retaining `[1m]`).
  - Full backward compatibility for legacy key-value dictionary formats.
- Cross-Process Advisory Locking for Token Usage Statistics:
  - Implemented atomic read-modify-write synchronization for `stats.json` using OS advisory locks (`flock` on Unix-like systems and `LockFileEx` on Windows).
  - Prevents token usage and request count overwrite/loss when running the daemon server concurrently with short-lived `opencode2api launch claude|codex` instances.
- models.dev Dual-Layer Cache & Proxy Reuse:
  - Added dual-layer caching (in-memory + disk cache) for models.dev catalog data.
  - Automatically reuses active SOCKS5 proxy configurations for catalog fetches with graceful fallback to cached catalog on network timeouts or failures.
  - Periodic background refresh every 6 hours.
- Shared Configuration, Statistics, and Log Path Resolution:
  - Unified path resolution across `server` and `launch` modes with user config directory fallback (`~/.config/opencode2api`).
  - Support for `OPENCODE2API_STATS` / `OPENCODE2API_STATS_FILE` and `OPENCODE2API_LOG` / `OPENCODE2API_LOG_FILE` environment variables and CLI overrides.
- Automated Build & Release Versioning:
  - Replaced hardcoded version constants with `runtime/debug.ReadBuildInfo()` dynamic module and VCS metadata resolution (commit revision, timestamp, dirty state).
  - Added `scripts/release.sh` and `make version` / `make release` for automated SemVer release derivation, pre-flight validation, and tag deployment.

## v0.7.0

- Redesign Web Admin Control Panel:
  - Modern aesthetic with refined dark palette, glassmorphism cards, and Lucide icons.
  - Responsive multi-section layout with sidebar navigation (Overview, Proxy & Upstream, Logs, Documentation, Configuration).
  - Live metric cards for active models, requests, cache hit rates, upstream health, and response latency.
  - Real-time log streaming viewer with auto-refresh, search filter, level tags, auto-scroll toggle, and pause/resume controls.
  - Visual upstream base URLs manager, model alias editor, and rate-limit cap configuration.
  - Clean authentication modal with seamless session management.
- Unified Config File Resolution Order:
  - Consistent config path resolution across server and launch modes:
    1. `OPENCODE2API_CONFIG` environment variable
    2. Explicit `-config` / `--config` CLI flags
    3. Existing `./config.json` in the working directory (preserves backward compatibility)
    4. Platform user configuration directory: `<UserConfigDir>/opencode2api/config.json` (`~/.config/opencode2api/config.json` on Linux, `~/Library/Application Support/opencode2api/config.json` on macOS)
  - When persisting configuration in server mode, automatically create parent user config directories if they do not exist.
  - Launch mode respects the same resolution order while remaining read-only.

## v0.6.0

- Add one-click cross-platform installers: `scripts/install.sh` for Linux/macOS/FreeBSD and `scripts/install.ps1` for Windows. Both resolve the latest GitHub Release (or a pinned tag), select the correct OS/arch tarball, verify its SHA256 from `checksums.txt`, install the binary under `~/.opencode2api/bin`, and print next-step `launch claude` / `launch codex` commands. Installation, platform support, version pinning, and release-asset naming are documented in `docs/INSTALL.md` and linked from both READMEs.
- Add `opencode2api launch codex`: starts the same localhost proxy as `launch claude`, then runs the installed Codex CLI with temporary `-c` provider overrides (`model_provider`, `model_providers.opencode2api.*`, `wire_api="responses"`, and `env_key="OPENCODE2API_OPENAI_API_KEY"`) so the child process uses opencode2api through its Responses API without writing `~/.codex/config.toml`. `--model` / `-m` after `--` are extracted, forwarded via Codex's `--model`, and the same `--key` / `OPENCODE_API_KEY` / `public` resolution is used. A temporary Codex model catalog is written from the current upstream model list and passed via `-c model_catalog_json=<temp path>`; it defaults to free-tier models for `public`, includes all models for paid/tier keys, and sets `context_window`, `max_context_window`, and `auto_compact_token_limit=<ctx*0.9>` so model switching and unknown upstream model IDs do not fall back to Codex's 258K default window.
- Make `launch claude` and `launch codex` read-only for the proxy config: `launch` now loads and applies `config.json` without calling `saveConfig`, so launching a child CLI can no longer rewrite the user's persistent proxy configuration.
- `opencode2api launch claude` now supports interactive TUI model selection, 1M context window mode, and automatic compaction:
  - When `--model` is omitted, an interactive TUI lists free-tier models from the upstream catalogs, sorted by context window (largest first), with `[1m]` markers for ≥1M-context models.
  - After model selection, the context window is looked up from models.dev: ≥1M gets a `[1m]` suffix on the model ID and `CLAUDE_CODE_AUTO_COMPACT_WINDOW=ctx×0.9`; <1M gets only the auto-compact; unknown gets neither.
  - Model ID is set via five `ANTHROPIC_*_MODEL` environment variables instead of `--model`, avoiding the `[claude-code:unrecognized_model]` warning.
  - `--model` can be placed after `--` (alongside claude passthrough flags like `--dangerously-skip-permissions`) and is extracted by opencode2api instead of being forwarded to claude.
  - The `[1m]` suffix flows through `resolveModel` / `resolveModelForAuth` / `mapPublicToFreeModel` transparently (stripped for catalog lookup, re-applied on the resolved ID).
  - models.dev catalog is fetched with a cache-busting query parameter and parsed from both the top-level `models` section and the nested `providers.*.models` section (where OpenCode-specific models like `x-preview-f-free` live).
  - TUI shows only models with a free variant; paid-only models are filtered out.
- Tolerate upstream streams that end with a usage-only chunk but no `finish_reason` and no `[DONE]` (observed on `muse-spark-1.2-contributor-free`): when the turn produced output and the terminal usage chunk carried output-token accounting, the Claude and Responses stream converters now synthesize `stop` / `response.completed` instead of failing with `stream ended without finish_reason`/`server_error`. Partial EOF with no output still fails instead of fabricating a reply.
- Fix a data race in the raw-SSE wrapper used by streaming Chat conversion: `Close()` and `Read()` on `rawSSEReader` are now synchronized.

## v0.5.0

- Add `POST /v1/messages/count_tokens` with local heuristic estimation: Claude Code polls this endpoint to manage its context window and trigger auto-compaction, and previously it 404'd. The new `claudeCountTokensHandler` answers synchronously with a local estimate (text at ~4 chars/token, per-message/system/tool structural overhead, and fixed `1600`/`3000` token estimates for image/document blocks), never calling the upstream and never incurring usage. Invalid JSON or missing `model` return a protocol-shaped 400; non-POST returns 405.
- Add multi-domain upstream load balancing with session stickiness: the new `upstream_base_urls` config spreads opencode zen traffic across multiple (reversed) domains. Sessions hash-stick to one (base URL, socks5 proxy) pair so per-egress prompt caches keep hitting, rebinding to a different target on 429/5xx/transport errors like the existing proxy stickiness. Catalog fetches round-robin over all domains, logs record `base_url` per attempt, and the admin UI gains an upstream domain editor. Defaults to `["https://opencode.ai"]` when unset.

## v0.4.9

- Silence multi-modal downgrade for text-only upstream models: models matching the new configurable `text_only_models` prefixes (default `["deepseek"]`, prefix-matched case-insensitively) have image and document parts replaced with `[image attached]` / `[document attached]` text annotations before the request leaves the proxy, so DeepSeek requests with screenshots or pasted images keep working instead of failing upstream with an image-unsupported error. Applied uniformly across the Chat, Responses, and Claude protocol surfaces (single choke point at `buildUpstreamBody`); text content and part order are preserved.

## v0.4.8

- Align Messages-API `input_tokens` with Anthropic semantics: when `cache_read_input_tokens` is derived from DeepSeek/OpenAI-style counters (`prompt_cache_hit_tokens` or `prompt_tokens_details.cached_tokens`), the hit portion is subtracted from `input_tokens`, since `prompt_tokens` includes it. Clients that price input and cache reads separately no longer bill hit tokens twice (289 prompt + 256 hit now reports `input_tokens: 33`, `cache_read_input_tokens: 256`). Anthropic-style `cache_read_input_tokens` sources are left untouched.

## v0.4.7

- Fix DeepSeek cache usage accounting: `prompt_cache_hit_tokens` is cached/read input, while `prompt_cache_miss_tokens` is ordinary uncached input and is no longer counted as `cache_creation_input_tokens` in Claude usage or `cache_created_tokens` in `stats.json`. The admin stats table now also displays `cache_read_tokens` / `cache_created_tokens`.
- Harden cache usage aggregation so canonical Anthropic cache fields take precedence over DeepSeek/`cached_tokens` fallbacks and are never double-counted when multiple usage shapes are present. Add regression tests for DeepSeek and Anthropic cache semantics.

## v0.4.6

- Add session-sticky egress (`socks5_sticky`, default `true`) for round-robin proxy mode. Each session/account (paid by API token, public by Claude metadata `session_id`, otherwise a shared fallback) pins one egress proxy, so upstream per-egress prompt caches keep building up: measured 99.8% cache hit on a pinned egress vs ~0% when rotation randomly switches egress between requests. Different sessions still rotate, keeping the multi-egress distribution.
- Release sticky bindings before retrying upstream errors: free-tier 429s are rate-limited per egress IP, so the retry rotates to the next proxy (verified live: 429 on egress A → automatic rebind to egress B → retry succeeds). Transport errors and 5xx release the binding too.
- Fix rebind-after-invalidate using a deterministic hash, which always landed the same session back on the same proxy (i.e. "switch IP on error" never actually happened). Rebinding now rotates egress via an incrementing sequence mixed into the hash.
- Clear all sticky bindings when the proxy configuration changes (`active_socks5` or the proxy list), so stale bindings never point at removed egresses.

## v0.4.5

- Improve prompt-cache hit rate on the OpenCode zen upstream. Requests now inject `prompt_cache_retention: "24h"` (upstream default is ~5 min) and an Anthropic-style `cache_control: {"type":"ephemeral","ttl":"1h"}` breakpoint for models that accept it. GLM/Zhipu models, which reject unknown fields, are always skipped, and client-supplied `extra_body` values win over the injected defaults. Both behaviors are configurable via `prompt_cache_retention` (`"24h"` default, `"in_memory"`, or `"off"`) and `cache_control_breakpoints` (default `true`).
- Map DeepSeek-style `prompt_cache_hit_tokens` into Claude usage as `cache_read_input_tokens` when the standard fields are absent, so cached tokens are visible on the Messages API. `prompt_cache_miss_tokens` is ordinary uncached input, not a cache write, so it is intentionally not reported as `cache_creation_input_tokens`.
- Aggregate per-model cache accounting in `stats.json` as `cache_read_tokens` / `cache_created_tokens` across Chat, Responses, and Messages (streaming and non-streaming), and add these columns to the admin panel. For DeepSeek-style upstreams the hit rate is `cache_read_tokens / prompt_tokens`.

## v0.4.4

- Fix DeepSeek/Qwen models that emit raw DSML/XML tool-call markup (for example `<｜DSML｜tool_calls>`, `<|DSML|tool_calls>`, `<tool_calls>`, and `<tool_call>`) by converting it to standard OpenAI `tool_calls` before it reaches Chat/Claude/Responses clients. Non-streaming responses are normalized in `callOpenCodeAPI`; streaming responses are wrapped by `callOpenCodeAPIStream` so all three downstream protocols share one conversion layer. Native `tool_calls`, `usage`, `reasoning_content`, `finish_reason`, and `[DONE]` semantics are preserved.

## v0.4.3

- Fix `cache_control` counting so signature fields are not treated as cacheable message content, and ensure Responses `cached_tokens` remains present in Claude usage translation.

## v0.4.2

- Refactor project layout: split the root `main.go` monolith into `internal/app` packages, add `cmd/opencode2api` as the executable entrypoint, extract protocol DTOs into `internal/domain`, random helpers into `internal/random`, response ID normalization into `internal/ids`, and sync Makefile/Dockerfile/release script/CI paths and ldflags. HTTP behavior and CLI flags are unchanged.

- Fix `/v1/responses` `text` parameter being forwarded verbatim as upstream `response_format`. The Responses API `text` nests `type` inside `format` (`{format:{type:...}, verbosity:...}`), but upstream providers require a top-level `type` in `response_format`, causing `400 response_format: missing field type`. Added `convertResponsesTextToResponseFormat` to translate `text.format` into a valid Chat Completions `response_format` (`text` / `json_object` / `json_schema`), and drop the field entirely when it cannot be represented (unknown type, missing required `json_schema` fields, non-object `text`, or verbosity-only) so the proxy never forwards a malformed `response_format` and never surfaces a 400 for this case. Verified against the real upstream and end-to-end through the proxy.

## v0.4.1

- Add `stop_sequence` field to non-streaming Claude responses, streaming `message_start`, and streaming `message_delta` so Claude Code sees `"stop_sequence": null` consistently, matching the Anthropic Messages API contract.
- Add `stop_details` field (omitempty) to `ClaudeResponse` for forward compatibility with `refusal` stop reason.
- Extend `claudeUnsupportedBlockTypes` with 7 new server-tool/MCP block types (`code_execution_tool_use`, `code_execution_tool_result`, `mcp_tool_use`, `mcp_tool_result`, `bash_code_execution_tool_result`, `web_fetch_tool_result`, `tool_reference`) for observability; requests with these blocks are still accepted (no 400).

## v0.4.0

- Fix 19 audited protocol compatibility issues across stream integrity, native Anthropic decoding, and request compatibility adapters.
- Stream integrity: emit protocol error events (`event: error` on Claude, `response.failed` on Responses) on upstream stream errors or abnormal EOF; support 15s keepalive ping before first token; adopt single-writer SSE architecture.
- Native Anthropic decoding: strict SSE lifecycle validation, numeric index ordering for content blocks, support typed deltas (`text_delta`, `thinking_delta`, `signature_delta`, `input_json_delta`, `redacted_thinking.data`), preserve ordered blocks across Claude roundtrips via `_opencode2api_anthropic_content`, deterministic response-ID prefix normalization, and return generic `upstream_error` without error detail leakage.
- Request compatibility: `/v1/responses` `tool_result` normalization and collection; Anthropic `document` and Responses `input_file` mapping to Chat `file` parts; structure-aware file/document validation; strict temperature boundary validation (0..1 for Messages, 0..2 for Chat/Responses); safe `cache_control` and signature counting.
- Public free-model routing: automatically downgrade keyless public-tier requests for paid model IDs to `-free` variants (e.g., `deepseek-v4-flash` → `deepseek-v4-flash-free`).
- Add configurable `max_tokens` cap (global `max_tokens_cap` and per-model `max_tokens_cap_per_model`), clamping upstream requests exceeding limits, with Admin UI controls and `/api/config` support.

## v0.3.10

- Add `socks5_paid_direct` (default `false`): when an `active_socks5` proxy is set, keyed/paid upstream traffic also uses SOCKS5 unless this flag is explicitly enabled for the old paid-direct bypass.

## v0.3.9

- Stop cross-model upstream fallback for both public and keyed auth; retry transient 401/429/5xx and transport errors on the same requested model only.
- Treat upstream `CreditsError` / insufficient balance as non-retryable so Anthropic and Chat requests return the original billing error instead of silently trying other catalog models.

## v0.3.8

- Fix Claude Code `/v1/messages` streaming: wait for the OpenAI usage-only chunk after `finish_reason` before emitting `message_delta`, so token usage (and cache fields when present) is no longer dropped.

## v0.3.7

- Fix Docker image build: copy `go.sum` so lumberjack dependency resolves during multi-arch image builds.

## v0.3.6

- Claude Code `/v1/messages` → Chat upstream conversion: accept `x-api-key` (reject `sk-ant-`), merge mid-conversation `system` into one leading system message, convert `tool_result` images to follow-up `image_url`, map `tool_choice.disable_parallel_tool_use` → `parallel_tool_calls=false`, narrow `metadata.user_id` JSON to `session_id`, skip server tools without `input_schema`, and log intentional drops (`context_management`, `cache_control`, betas) in `request_plan`.
- Map Claude Code `output_config.effort` (`--effort` / `CLAUDE_CODE_EFFORT_LEVEL`) onto upstream `reasoning_effort`, and treat `thinking.type=adaptive` as enabled.
- Add structured request logging with lumberjack file rotation (default `opencode2api.log` + stdout), `request_id` tracing, protocol/upstream/stream summaries, secret redaction, and runtime `log_level` / `log_bodies` via `/api/config`.
- Restore `/v1/messages` default CoT passthrough for Claude Code (thinking off only when force-disabled or `thinking.type=disabled`), while keeping empty-reply fallbacks from v0.3.5.
- Fix reasoning effort: stop stripping `reasoning_effort` before upstream calls; forward Claude `thinking` (including `budget_tokens`) and derive effort from budget when needed.
- Hide upstream `-free` suffix in `/v1/models` responses and resolve stripped names back to free upstream IDs.

## v0.3.5

- Fix empty Claude Code / OpenAI replies when the Go gateway puts the answer in `reasoning_content` (#37635): promote to `content`/`text` when thinking is not requested, and fall back to a text block if a thinking-only stream would otherwise end empty.
- Temporarily made `/v1/messages` thinking opt-in; reverted in Unreleased because Claude Code lost visible CoT.

## Prior

- Projectized the provided Go program.
- Added Go module metadata, local build targets, and release packaging script.
- Added CI and tag-driven multi-platform release automation.
- Changed release automation to parallel matrix builds with a final publish job.
- Added README, API, configuration, deployment, release, contribution, and security docs.
- Added build metadata and `-version` flag.
