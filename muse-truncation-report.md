# muse-spark-1.3-contributor-free 截断/中断排查报告

日期：2026-10-07 · 基线 commit b323621 · 18 个 agent / 904 次工具调用 / 97 分钟（4 路读码 + 日志挖掘 + 真实网关复现 + 对抗验证）

## 结论

截断不是单一 bug，而是一条故障链。**主因**：免费档共享 Output-token 配额承压时，上游以 HTTP 200 + `response.created → response.incomplete`（零内容、usage 报 128 reasoning token）杀死请求；网关 `PeekFirstFrame` 在首个壳帧就 commit（`IsProductiveEvent` 是注册了但全仓库零调用的死代码），`response.incomplete` 分支不做零内容检查，于是把空流当成**成功**转发：客户端收到 200 + 空消息 + `stop_reason=max_tokens`，零重试零报错。Claude Code 把空轮记入历史，agent 循环拿不到产出而停摆——表现为"任务永远无法完成"。**"间歇性"是外生的**：共享配额随其他用户流量波动（复现 232 轮 / ~50 万 output tokens 在空闲窗 0 次截断 0 次 429；用户事故窗 21:01）。

## 复现

| 手段 | 结果 |
|---|---|
| 真实网关 + 真实上游 opencode.ai（8001 端口，用户 config 副本）232 轮 / 6 组配置 / ~50 万 output tokens | 0 客户端可见截断、0 次 429（测试时段 22:00-22:30 配额未承压，阴性结果如实报告） |
| **mock 上游确定性复现（8002 端口）** | 发 `response.created + response.incomplete(output:[], usage output_tokens=128 全 reasoning、零内容)` → 网关当成功流转发：客户端收到 HTTP 200 + `message_start + ping + message_delta(stop_reason=max_tokens) + message_stop`、**零内容块、只打 1 次上游请求（零重试）**。与用户日志 21:01:36 `request_id=njbin81wzu2d` 的 `chunks=0 empty_reply=true finish_reason=length` 签名逐字段一致。证据：`/tmp/muse-repro/evidence.json`、`mock_turn_ei.txt` |

## 确认根因（6 条，对抗验证 0 驳回）

### #1（rank 1）空流在壳帧即 commit，零内容 incomplete 被当成功转发
- `PeekFirstFrame.flushFrame`（`internal/app/stream_retry.go:268-280`）对任何非 `[DONE]`/非空 data 行无条件 `hasFrame=true`，只查 `hooks.IsErrorEvent`；`IsProductiveEvent`（`stream_retry.go:103/111/118` 注册）是死代码——`response.created` 壳帧即 commit（`stream_retry.go:321-327`）。
- handler 对 `response.incomplete`（`internal/app/claude_responses.go:2027-2040`）只合并 usage、置 `stopReason=max_tokens`，无零内容检查；`finalizeClaudeResponsesStream`（`claude_responses.go:2078-2140`）无条件发收尾；handler 在 `claude_responses.go:1714` 无条件 `return (true,nil)`，`DriveStreamWithRetry` 见 committed 立即 return（`stream_retry.go:368-370`）——**空流重试（StreamEmptyRetryMax=1）+ keypool 换 key 对本协议在 commit 后完全失效**。
- 日志侧 `truncated := !DoneSeen && !SawFinish`（`internal/logging/logging.go:449`），合成收尾后真实截断在 `stream_result` 里不可见。

### #2（rank 2）已 commit 的流中途死亡被合成为「正常结束」
传输 EOF/RST/读错误/300s 墙钟超时在 `claude_responses.go:1669-1703` 不区分错误类型：`producedText||toolOrder>0` 时合成 `SawFinish=true/FinishReason=stop`，**无 error 事件、不重试**。截在 `function_call arguments` 流中途 → 客户端拿到 input 为非法 JSON 的 tool_use + 干净 `end_turn`（工具调用解析失败、回合报错）；截在文本中途 → 丢尾但带 `end_turn`（静默截断）。对照：`chat.go:873` 发 `upstream_truncated`、`responses.go:2402` 发 `response.failed`——claude-responses 是唯一不标记的翻译路径。

### #3（rank 3）public auth 绕过 keypool + 429 重试无 backoff
- `selectPoolKey` 对 `AuthRoutePublic` 直接 return（`internal/app/keypool.go:194-196`）——**配置的 5 把上游 key 在 public 档形同虚设**（事故日志所有 `upstream_attempt` 行均无 `key_id` 字段，逐一核实），401/429/输出限速全部砸回同一匿名身份 + 单一 socks5 出口（`100.89.104.82:1090`）。
- `callOpenCodeEndpoint`（`internal/app/opencode.go:727-771`）429 立即重发，无任何 backoff（grep 证实三文件无 `time.Sleep`、无 Retry-After 处理），上游明确要求 "Please retry after a brief wait"。

### #4（rank 4）非流式辅助调用收到 SSE 被当 JSON 解析 → 200 空文本消息
免费档 fingerprint 对所有请求强制 `stream:true`（上游仅接受流式）→ 非流客户端拿到 Responses SSE → `extractResponsesJsonFromSse`（`claude_responses.go:984-1018`）只在有终态事件时提取（c9167eb 已修）；**SSE 被截断/无终态帧时 `last==nil` → 原样返回 SSE body** → unmarshal 报 `invalid character 'e'`（'e' 即 "event:" 开头）→ 空 text + `end_turn` + HTTP 200。另：该函数只认 `"data: "`（带空格，992 行），PeekFirstFrame 兼容 `"data:"`（`stream_retry.go:301`）——前缀不一致。

### #5（rank 5）终态事件 output[] 内容收割缺口（路径不对称）
`response.incomplete` 分支不遍历 output；`response.completed` 只取 tool stopReason + reasoning signature、不取 message 文本；`*.done` 事件整包丢弃。非流式路径 `responsesOutputToClaudeBlocks`（740-860）却全量遍历——同一上游形态流式丢内容、非流式能恢复。

### #6（rank 6）chat 非流分支双重转换抹掉正文
`forwardChatViaResponses` 非流分支先聚合为 chat.completion 形，再 `convertResponsesToChat` 按原生 Responses 形读 `raw["output"]`（`chat_to_responses_upstream.go:710`，不存在）→ `content:""` 而 usage 报 completion_tokens。不在 Claude Code 主路径（/v1/messages）上，但是复现中确认的活跃 bug。

## 修复方案（评审 winner：P1-P6 六层，每层独立可回滚、全部向后兼容）

实施顺序：**P1+P2 同批**（rank 1）→ P3 → P4 → P5 → P6；每个 P 独立 commit。

### P1 终态 output[] 内容收割（`internal/app/claude_responses.go`）
`response.incomplete` 分支（2027-2040）补遍历 `resp.output` 并补发未经 delta 通道流出的内容（镜像非流式 `responsesOutputToClaudeBlocks` 的遍历面）；`response.completed` 分支（1973-2017）同款收割；零内容 incomplete 补发 in-band error 使其客户端可见。emitter 增 `streamedItems map[string]bool` / `streamedOutIdx map[int]bool`（双键去重防重复补发）与 `wantReasoning bool`（防 signature_delta 发到被 promote 成 text 的块上）。

### P2 产出感知 commit + 壳帧宽限窗（`internal/app/stream_retry.go`）
修活 `IsProductiveEvent`：`flushFrame`（268-280）改查 `hooks.IsProductiveEvent`（nil 时保持现状，chat/anthropic 两条 peek 路径行为逐字节不变）；壳帧（`created/in_progress/queued/output_item.added/content_part.added`）与空 output 终态帧不再 commit；壳帧后进入 **5s 宽限窗**（env `OPENCODE2API_SHELL_GRACE_MS`），限速杀死（实测 291ms 即发 incomplete）在未写任何字节前经 `StreamEmptyRetryMax` + keypool 换 key 重试；宽限到期仍只有壳帧按健康慢流放行（避免朴素"等产出"实现把 thinking>30s 的健康流误判超时重试）。

### P3 EOF 分支按错误类型分流（`internal/app/claude_responses.go:1669-1703`）
`errors.Is(res.err, io.EOF)` 才允许合成「正常结束」（保留 muse-spark 干净 EOF 缺 `[DONE]` 的既有行为，PartialEOF/ThinkingOnlyEOF 测试不回归）；其余（RST/unexpected EOF/http.Client 墙钟超时）走中断分支：`SawFinish=false`（日志自动记 `truncated=true`）、Warn、`emitError("upstream stream interrupted")`、关未闭合块——截在 tool arguments 流中时客户端拿到 error 事件而非假 `end_turn`，**Claude Code 得以感知失败并重试该回合**。

### P3b 300s 墙钟超时可配置（`internal/app/httpclient.go`）
`httpClient`（18-25）与 `buildProxyClient`（331-341）的 `Timeout: 300s` 改读 env `OPENCODE2API_UPSTREAM_TIMEOUT_SECS`（默认 900）。Go `http.Client.Timeout` 覆盖整个响应体流式读取——单出口 socks5 下任一 >5min 的 agent 长输出在恰好 300s 被掐（slow-drip 实测 `duration_ms=300002 finish_reason=stop truncated=false`，完全不可见）。
**评审移植建议（更优形态）**：`Timeout=0 + Transport.ResponseHeaderTimeout=300s` —— TTFB 边界保留、mid-body kill 整体消除；若保守保留整体超时，至少移植 httpclient 回归断言。

### P4 非流式 SSE 残缺兜底（`claude_responses.go:984-1018`）
`last==nil` 时：SSE 含 delta 内容 → 聚合为合成 response（status:"incomplete"，部分内容保留）；连 delta 都没有 → 合成 error body → 502（Claude Code 对 5xx 自动重试整回合，优于 200 空消息被记入历史）。顺带修 `"data: "` 前缀：`TrimPrefix "data:"` + `TrimSpace`，与 PeekFirstFrame 对齐。

### P5 身份分摊（间歇性的放大器）
- **P5a**（`internal/app/keypool.go:194-196`）：public + 免费模型允许 keypool 接管——5 把 key round_robin 分摊每账号输出限额。env kill-switch `OPENCODE2API_POOL_PUBLIC`（默认 on）；**死池护栏**：全部候选 key 长冷却（≥blacklistAfter）时回落 public 直连，绝不弄坏原本可用路径。注意上线后 prompt 缓存亲和一次性重排（sticky 哈希基从 public-shared 变 token-based）。
- **P5b**（`internal/app/opencode.go:727-771`）：429 重试加递增 backoff（Retry-After 优先、缺省 (attempt+1)s、cap 5s、尊重 ctx）。包级 var 便于测试注入。仅 HTTP 层 429 生效；流内杀死由 P2 承接。

### P6 chat 非流双重转换（`chat_to_responses_upstream.go:409-410`）
聚合结果已是 chat.completion 形时直接使用（新 helper `isChatCompletionBody` 按 `object=="chat.completion"` 判定），仅聚合未生效时才走第二次转换。

### 文档同步
`docs/CONFIGURATION.md` 补 `OPENCODE2API_POOL_PUBLIC` / `OPENCODE2API_SHELL_GRACE_MS` / `OPENCODE2API_UPSTREAM_TIMEOUT_SECS` 三个 knob 与「免费档限速杀死时网关的 pre-commit 重试与 output 收割」行为说明。

## 已知行为代价（评审确认可接受）

- **P2 最大代价**：thinking>5s 的健康请求 `message_start` 延后 ~5s（宽限到期才 commit），可用 `OPENCODE2API_SHELL_GRACE_MS` 调小；峰值延迟 ≈ 上游 TTFB + 5s，Claude Code 10min 超时内安全。朴素"等产出"实现会把 thinking>30s 的健康流误判重试（StreamFirstByteTimeoutMs=30000），故用宽限期而非无限等待。
- **P2 影响面**：改 `PeekFirstFrame` 涉及 6 个调用点，但 chat/anthropic hooks 对任意帧判产出 → 行为不变；只有 Responses 两条 hooks 语义变化——需全量 `make test` 确认无既有用例依赖「壳帧即 commit」。
- **P5a 上线须知**：匿名 public 流量开始消耗真实 key 配额。
- **P5b 最坏延迟**：连续 429 时请求总时长 +9s。

## 回归测试（13 条，遵循 AGENTS.md：表驱动、Test<Area>_<Case>、SSE 事件顺序断言）

1. `TestClaudeResponsesStream_IncompleteEmptyOutput_RetriesBeforeCommit`（rank1 核心，mock 与 mock_turn_ei.txt 同参）：空 incomplete → 重试槽位放健康 SSE，断言客户端收到重试流内容与 `message_stop`、无空轮、无 error。
2. `TestClaudeResponsesStream_IncompleteWithOutput_HarvestsContent`（P1+P2）：incomplete output 带 reasoning summary+encrypted_content → `thinking_delta` 携带文本、`signature_delta` 在 `content_block_stop` 前、无 error。
3. `TestClaudeResponsesStream_IncompleteWithMessage_HarvestsText`：output 内嵌 message/output_text → text_delta 送达。
4. `TestClaudeResponsesStream_ShellOnlyEOF_Retries`（P2 EOF 分支）：壳帧后干净 EOF → 重试生效。
5. `TestClaudeResponsesStream_RSTMidToolCall_EmitsErrorNotStop`（P3 核心）：半截 arguments.delta + 非 EOF 错误 → error 事件、无假 end_turn。
6. `TestClaudeResponsesStream_TimeoutKillMidStream_EmitsError`：net.Error Timeout 中途死亡 → error 而非 stop 合成。
7. `TestPeekFirstFrame_ProductiveMatrix`（表驱动）：壳帧→非产出；delta→产出；completed-with-output→产出；空终态→非产出；chat/anthropic hooks 行为不变。
8. `TestPeekFirstFrame_ShellGraceCommitsOnTimeout`：只发 created 后挂住，grace 50ms → 到期 commit 而非重试。
9. `TestExtractResponsesJsonFromSse_NoTerminal`（表驱动）：带 delta 无终态 → 合成 response；created-only → error body；`"data:"` 无空格可解析；已有终态 → c9167eb 行为不变。
10. `TestForwardChatViaResponses_NonStream_NoDoubleConversion`（P6）：content=="HELLO-FROM-UPSTREAM"、usage 正确。
11. `TestSelectPoolKey_PublicFreeModel_TakesOver / _KillSwitchOff / _HardDeadPoolFallsBack / _PaidModelPassthrough`。
12. `TestCallOpenCodeEndpoint_Rate429_BackoffBeforeRetry`：429→backoff→200；ctx 取消立即退出。
13. 回归确认既有 8 条 `claude_responses_empty_retry_test.go` 用例（PartialEOF_SynthesizesStopNoRetry、ErrorFrame_Retries）行为不变。

运行：`make fmt && make vet && go test ./...`；部署后（`scripts/update-remote.sh`）在真实 muse-spark 流量上观察 `empty_reply`/`truncated` 指标归零。

## 验证缺口（完备性批评家的提醒，实现前值得补）

- **真实 Claude Code CLI 从未参与复现**（复现客户端全是 python requests）：建议实现前用 `ANTHROPIC_BASE_URL` 指向本地网关 + mock 上游跑一次带 agent 子任务的多轮会话，验证「200+空消息+max_tokens → 记入历史/停摆」与修复后的「error 事件 → 重试推进」。
- **Retry-After 头从未被观测**（日志 0 次，opencode.go 只记 errBody 不记响应头）：实现 P5b 前先临时落一次 `resp.Header` 取证。
- **rank 5 收割的真实价值未验证**：真实 muse-spark incomplete 帧是否在 output[] 携带可恢复内容未知（128 token 全 reasoning）；可在 responses 路径加限频的原始上游 SSE debug 采样（env 开关）抓一次真实帧全文，避免为不存在的形态写代码。
- **P2 重试放大回路未测**：持续承压窗（mock 交替 empty/429 共 50+ 轮）下验证重试有界、不放大配额消耗。
- **根因 #6 的 evidence 在交接中被截断**：HEAD 上重跑 chat 路径 stream:false 测试再并入。
- **anthropic 原生路径未审读**：`anthropic_upstream.go`/`anthropic_protocol.go` 共享同一 300s 超时与截断家族（muse-spark 不走此路径），建议低优先级同族扫描。
