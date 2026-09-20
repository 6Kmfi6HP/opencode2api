package app

import (
	"context"
	"encoding/json"
	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ======================== Claude -> Responses 原生转换（lenient） ========================
//
// 目标：Claude Messages 客户端（如 launch claude）在上游 chat 通道不可用、
// 但原生 responses 可用时，仍能工作。转换原则：尽可能支持，不支持的字段
// 做 best-effort 降级，绝不因为“不支持”返回 400。
//
// 降级规则：
//   - 未知 block（server_tool_use / web_search_* / mcp_* / code_execution_* 等）
//     序列化为文本保留上下文，不报错。
//   - image/document 无可用 source 时降级为 "[image attached]" /
//     "[document attached]" 文本，不报错。
//   - tool_use 缺 name 时跳过该 block；tool_result 缺 tool_use_id 时降级为
//     user 文本，不报错。
//   - redacted_thinking 无文本等价物，静默丢弃，不报错。
//   - top_k / cache_control / signature / context_management 等无对应物，
//     直接丢弃并记入 request_plan 日志，不报错。

// isAnthropicBillingHeader 报告 Claude Code 注入的计费头块（对齐 sub2api
// isAnthropicBillingHeaderText）：该块只对 Anthropic 计费链路有意义，转发到
// OpenAI Responses 上游只会浪费上下文，system/instructions 组装时滤掉。
// 薄壳转发到 bridge。
func isAnthropicBillingHeader(text string) bool {
	return bridge.IsAnthropicBillingHeader(text)
}

// extractClaudeSystemTextFiltered 与 extractClaudeSystemText 相同，但滤掉
// Claude Code 注入的 x-anthropic-billing-header 文本块。本转换保持
// system->instructions 的现状（不像 sub2api 那样转 developer item）。
// 薄壳转发到 bridge。
func extractClaudeSystemTextFiltered(system any) string {
	return bridge.ExtractClaudeSystemTextFiltered(system)
}

// claudeMessagesToResponsesInput 把 Claude messages + system 转为
// Responses 的 instructions + input 数组。永不返回错误。纯核已迁入
// bridge.ClaudeMessagesToResponsesInput；tool_use 缺 id 的随机 string 由
// randomIDGen 注入（生产：internal/random，与原 randomString 同字符集）。
// 薄壳转发到 bridge。
func claudeMessagesToResponsesInput(msgs []ClaudeMessage, system any) (string, []any) {
	return bridge.ClaudeMessagesToResponsesInput(randomIDGen, msgs, system)
}

// claudeToolResultMediaParts 提取 tool_result content 数组里的
// image/document part，转为 Responses input_image/input_file part。薄壳转发到 bridge。
func claudeToolResultMediaParts(block map[string]any) []any {
	return bridge.ClaudeToolResultMediaParts(block)
}

// claudeToolResultToText 提取 tool_result 的文本，图片/文档附件转为标注。
// 薄壳转发到 bridge。
func claudeToolResultToText(block map[string]any) string {
	return bridge.ClaudeToolResultToText(block)
}

// isMuseSparkModel 报告是否为 muse-spark 系模型（大小写不敏感子串匹配）。
// 薄壳转发到 bridge。
func isMuseSparkModel(modelID string) bool {
	return bridge.IsMuseSparkModel(modelID)
}

// responsesTextPart 按 role 返回合法的文本 part 类型：user/developer 用
// input_text，assistant 用 output_text。薄壳转发到 bridge。
func responsesTextPart(role, text string) map[string]any {
	return bridge.ResponsesTextPart(role, text)
}

// claudeToResponsesTools 把 Claude tools 转为 Responses function tools。
// Server tools（无 input_schema）静默跳过，不报错。薄壳转发到 bridge。
func claudeToResponsesTools(claudeTools []ClaudeTool, modelID string) []ResponsesTool {
	return bridge.ClaudeToResponsesTools(claudeTools, modelID)
}

// normalizeResponsesToolParameters 保证 function parameters 满足上游原生
// responses 的严格校验：required 必须存在且包含 properties 的每一个 key。
// 薄壳转发到 bridge。
func normalizeResponsesToolParameters(params map[string]any) map[string]any {
	return bridge.NormalizeResponsesToolParameters(params)
}

// claudeToolChoiceToResponses 把 Claude tool_choice 转为 Responses 形状的薄包装，
// 核心逻辑在 claudeToolChoiceCore（anthropic_protocol.go）。薄壳转发到 bridge。
func claudeToolChoiceToResponses(choice any) any {
	return bridge.ClaudeToolChoiceToResponses(choice)
}

// thinkingBudgetToEffort maps an Anthropic-style thinking budget_tokens value
// onto an OpenAI-compatible reasoning effort tier. Shared by
// reasoningEffortFromThinking (chat.go) and the claude->responses path.
// 薄壳转发到 bridge。
func thinkingBudgetToEffort(budget float64) string { return bridge.ThinkingBudgetToEffort(budget) }

// claudeThinkingToResponsesEffort 从 thinking / output_config 推导 effort。
// 禁用或推导不出时返回 ""（调用方省略 reasoning 字段，不报错）。纯核已迁入
// bridge.ClaudeThinkingToResponsesEffort；本壳注入 force-disable-thinking。
// 薄壳转发到 bridge。
func claudeThinkingToResponsesEffort(claudeReq ClaudeRequest, modelID string) string {
	return bridge.ClaudeThinkingToResponsesEffort(config.ForceDisableThinking(), claudeReq, modelID)
}

// normalizeResponsesEffort 把 effort 归一化到上游白名单。
// max->xhigh（最接近的高档），none/"" /未知->""（省略 reasoning 字段）。
// 薄壳转发到 bridge。
func normalizeResponsesEffort(effort string) string { return bridge.NormalizeResponsesEffort(effort) }

// claudeToResponsesBody 把 Claude 请求转为原生 Responses 请求体。永不报错，
// 失败时返回最小可用体（model + input），避免 400。纯核已迁入
// bridge.ClaudeToResponsesBody；本壳按 modelID 解析 ConfigView 与 cap 注入，
// tool_use id 生成经 randomIDGen 注入。薄壳转发到 bridge。
func claudeToResponsesBody(claudeReq ClaudeRequest, modelID string) []byte {
	return bridge.ClaudeToResponsesBody(bridgeConfigViewFor(modelID), randomIDGen, claudeReq, modelID)
}

// ======================== Responses -> Claude 转换（lenient） ========================

// responsesOutputToClaudeBlocks 把原生 Responses output 数组转为 Claude
// content blocks。薄壳转发到 bridge。
func responsesOutputToClaudeBlocks(output []any, wantReasoning bool) ([]ClaudeContent, string, bool) {
	return bridge.ResponsesOutputToClaudeBlocks(output, wantReasoning)
}

// convertResponsesToClaude 把原生 Responses 成功响应转为 Claude message。
// 解析失败时返回最小可用空文本消息，不报错。薄壳转发到 bridge（解析失败的
// warn 由本壳记录，保持原可观测性;bridge 纯层不打日志）。
func convertResponsesToClaude(respBody []byte, model string, wantReasoning bool) []byte {
	var probe struct {
		Output []any `json:"output"`
	}
	if err := json.Unmarshal(respBody, &probe); err != nil {
		slog.Warn("convertResponsesToClaude unmarshal failed", "error", err)
	}
	return bridge.ConvertResponsesToClaude(randomIDGen, respBody, model, wantReasoning)
}

// responsesUsageToChat 把 Responses usage 口径转为 chat 口径，复用
// buildClaudeMessageUsage 的缓存解析逻辑。薄壳转发到 bridge。
func responsesUsageToChat(usage map[string]any) map[string]any {
	return bridge.ResponsesUsageToChat(usage)
}

// convertResponsesErrorToClaude 把原生 Responses 错误体转为 Claude 错误形状。
// 上游 message 尽量保留，不暴露内部错误串。薄壳转发到 bridge。
func convertResponsesErrorToClaude(respBody []byte) []byte {
	return bridge.ConvertResponsesErrorToClaude(respBody)
}

// recordClaudeResponsesUsage 按 Responses usage 口径记录 token 与缓存统计。
func recordClaudeResponsesUsage(model string, usage map[string]any) {
	if usage == nil {
		return
	}
	chatUsage := responsesUsageToChat(usage)
	u := statsx.TokenUsage{}.FromMap(chatUsage)
	pt, ct, tt := u.PromptTokens, u.CompletionTokens, u.TotalTokens
	if tt > 0 {
		statsx.RecordTokenUsage(model, pt, ct, tt)
		statsx.RecordCacheUsage(model, chatUsage)
	}
}

// ======================== Claude 经原生 responses 的转发/探测 ========================

// probeClaudeViaResponses 专用于 chat 翻译路径失败后的投机探测：仅上游原生
// responses 返回 2xx 时才转换写回并记住该模型；任何失败都返回 false 且不写
// 任何响应，调用方保留原翻译路径错误原样返回。
func probeClaudeViaResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, claudeReq ClaudeRequest, stream bool, wantReasoning bool) bool {
	upstreamBody := claudeToResponsesBody(claudeReq, modelID)
	rc, status, _, err := callOpenCodeEndpoint(ctx, "responses", upstreamBody, modelID, auth)
	if err != nil || status < 200 || status >= 300 {
		if rc != nil {
			rc.Close()
		}
		return false
	}
	defer rc.Close()

	rememberNativeResponsesModel(modelID)
	logging.FromContext(ctx).Info("claude_responses_probe_succeeded", "model", modelID, "stream", stream)

	if stream {
		claudeResponsesStreamHandler(ctx, w, rc, modelID, wantReasoning)
		return true
	}
	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		return false
	}
	claudeBody := convertResponsesToClaude(respBody, modelID, wantReasoning)
	result := logging.SummarizeClaudeResult(claudeBody)
	logging.LogResult(ctx, result)
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			recordClaudeResponsesUsage(modelID, u)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	logging.MaybeBodySummary(ctx, "claude responses response body", claudeBody)
	_, _ = w.Write(claudeBody)
	return true
}

// forwardClaudeViaResponses 处理已确认原生模型的转发：上游 2xx 转换成功后
// 写回；上游 4xx/5xx 转为 Claude 错误形状保真透传状态码；仅传输层错误
// （拿不到上游响应）返回 false，调用方兜底。
func forwardClaudeViaResponses(ctx context.Context, w http.ResponseWriter, auth UpstreamAuth, modelID string, claudeReq ClaudeRequest, stream bool, wantReasoning bool) bool {
	upstreamBody := claudeToResponsesBody(claudeReq, modelID)
	rc, status, _, err := callOpenCodeEndpoint(ctx, "responses", upstreamBody, modelID, auth)
	if err != nil {
		markNativeResponsesFailure(modelID)
		if rc != nil {
			rc.Close()
		}
		return false
	}
	defer rc.Close()

	if status >= 500 {
		markNativeResponsesFailure(modelID)
	}

	if stream && status >= 200 && status < 300 {
		claudeResponsesStreamHandler(ctx, w, rc, modelID, wantReasoning)
		return true
	}

	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		markNativeResponsesFailure(modelID)
		return false
	}

	if status >= 200 && status < 300 {
		claudeBody := convertResponsesToClaude(respBody, modelID, wantReasoning)
		result := logging.SummarizeClaudeResult(claudeBody)
		logging.LogResult(ctx, result)
		var usageResp map[string]any
		if json.Unmarshal(respBody, &usageResp) == nil {
			if u, ok := usageResp["usage"].(map[string]any); ok {
				recordClaudeResponsesUsage(modelID, u)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		logging.MaybeBodySummary(ctx, "claude responses response body", claudeBody)
		_, _ = w.Write(claudeBody)
		return true
	}

	// 上游错误：转为 Claude 错误形状，状态码保真透传。
	claudeErr := convertResponsesErrorToClaude(respBody)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(claudeErr)
	return true
}

// ======================== Claude 经原生 responses 的流式转换 ========================

// claudeResponsesBlock 转发到 bridge.ClaudeResponsesBlock（纯 struct，已迁移；
// 字段名导出，详见 bridge/responses_convert.go）。流式状态机整体已迁入
// bridge.ResponsesToClaudeStream，本类型别名仅为兼容保留。
type claudeResponsesBlock = bridge.ClaudeResponsesBlock

// claudeResponsesStreamHandler 是状态机 #4（原生 Responses SSE → Claude
// Messages SSE）的薄壳。纯核（handleEvent 事件翻译、getOrCreate/ensureStart/
// adoptResponseID/emitText/emitThinking/emitTool/emitError 闭包、按 claudeIndex
// 升序关块的 finalize、空回复 reasoningFallback 提升、encrypted_content→
// signature_delta roundtrip、adoptResponseID 归一化 msg_ id、finalize 幂等）已
// 迁入 internal/bridge（responses_to_claude_stream.go，ResponsesToClaudeStream），
// 产物为 bridge.OutEvent(Raw 即原 writeSSEEvent 写出的 `event: ...\ndata: ...\n\n`
// 字节,逐字节等价;终结 message_stop 帧携带 Terminal 标记)。本壳负责全部副作用:
// HTTP 头/WriteHeader、streamReader 生命周期与 15s keepalive、SSE 帧空行聚合
// （event:/data: 行拆分、[DONE] 哨兵、裸 JSON 行）、ctx、w.Write+Flush、
// 按 chunkCount 差量补 stats.NoteChunk,以及流末从 bridge 快照装配
// logging.StreamStats 再 deferred RecordTokenUsage/RecordCacheUsage + Log。
func claudeResponsesStreamHandler(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, wantReasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	stats := &logging.StreamStats{Start: time.Now()}

	st := bridge.NewResponsesToClaudeStream(randomIDGen, model, wantReasoning)

	defer func() {
		c := st.Counters()
		stats.TextChars = c.TextChars
		stats.ReasoningChars = c.ReasoningChars
		stats.PromotedReasoning = c.PromotedReasoning
		stats.ToolCallCount = c.ToolCallCount
		stats.FinishReason = c.FinishReason
		stats.SawFinish = c.SawFinish
		stats.DoneSeen = c.DoneSeen
		stats.Log(ctx, "claude-responses")
		// deferred usageFromResponsesMap + RecordTokenUsage/RecordCacheUsage：
		// 与原实现一致地按 Responses usage 口径记录 token 与缓存统计。
		if fu := st.Usage(); len(fu) > 0 {
			chatUsage := responsesUsageToChat(fu)
			pt, ct, tt := usageFromResponsesMap(chatUsage)
			if tt > 0 {
				statsx.RecordTokenUsage(model, pt, ct, tt)
				statsx.RecordCacheUsage(model, chatUsage)
			}
		}
	}()

	writeAll := func(evs []bridge.OutEvent) {
		for _, ev := range evs {
			if len(ev.Raw) > 0 {
				w.Write(ev.Raw)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	// syncNoteChunk 把 bridge 自记的 delta 计数差量补到 stats（保持
	// Chunks/FirstChunkAt 打点时机与原 handleEvent 内 NoteChunk 一致）。
	noted := 0
	syncNoteChunk := func() {
		for noted < st.ChunkCount() {
			stats.NoteChunk()
			noted++
		}
	}

	// SSE 解析：按空行分帧，聚合 event: + data: 行。
	var frameEvent string
	var frameData []string
	doFinalize := func() {
		writeAll(st.Finalize())
	}
	flushFrame := func() {
		defer func() {
			frameEvent = ""
			frameData = nil
		}()
		if len(frameData) == 0 {
			return
		}
		for _, dataLine := range frameData {
			trimmed := strings.TrimSpace(dataLine)
			if trimmed == "[DONE]" {
				st.MarkDoneSeen()
				if st.Finished() {
					doFinalize()
				}
				return
			}
			if !strings.HasPrefix(trimmed, "{") {
				continue
			}
			var evt map[string]any
			if err := json.Unmarshal([]byte(trimmed), &evt); err != nil {
				continue
			}
			writeAll(st.Handle(bridge.ResponsesEvent{Event: evt, FrameEvent: frameEvent}))
			syncNoteChunk()
			if st.Finished() {
				doFinalize()
				return
			}
		}
	}

	// keepalive：首 token 前客户端仅能收到 ping。
	reader := newStreamReader(ctx, rc, 15*time.Second)
	defer reader.Close()

loop:
	for {
		select {
		case <-ctx.Done():
			return
		case <-reader.Keepalive():
			writeAll([]bridge.OutEvent{st.PingEvent()})
		case res := <-reader.Read():
			line := res.line
			trimmedRight := strings.TrimRight(line, "\r\n")
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				flushFrame()
				if st.Finished() {
					break loop
				}
			} else if strings.HasPrefix(trimmed, ":") {
				// SSE 注释，心跳，忽略。
				continue
			} else if strings.HasPrefix(trimmed, "event:") {
				frameEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			} else if strings.HasPrefix(trimmed, "data:") {
				frameData = append(frameData, strings.TrimSpace(strings.TrimPrefix(trimmedRight, "data:")))
				// 非标准单行 data（无空行分隔）也尝试及时处理：仅当该行自身是完整 JSON
				// 且下一行未知时，flush 会在空行到达时执行；这里不提前 flush，
				// 避免半帧误解析。
			} else if strings.HasPrefix(trimmed, "{") {
				// 非标准裸 JSON 行。
				frameData = append(frameData, trimmed)
			}
			if res.err != nil {
				// EOF 前可能还有未以空行结尾的最后一帧，补 flush（completed 的
				// doFinalize 已在 flushFrame 内执行，此处不再重复）。
				if len(frameData) > 0 {
					flushFrame()
				}
				if !st.Finalized() {
					if st.ProducedOutput() {
						// 上游干净 EOF 但缺 completed（如 muse-spark 系只发事件不发 DONE）：
						// 合成正常结束，不报错。
						st.MarkEOFStop()
						doFinalize()
					} else {
						writeAll([]bridge.OutEvent{st.EOFErrorEvent()})
					}
				}
				break loop
			}
			if st.Finished() && len(frameData) == 0 {
				// completed 已处理，等待 EOF/DONE 后退出；继续消费避免 goroutine 泄漏。
				continue
			}
		}
	}
}

// usageFromResponsesMap 从 Responses usage 提取 (prompt, completion, total)。
// total_tokens 缺失时由分量合成——与 responsesUsageToChat 同一兜底规则。
// 纯核已迁入 bridge.UsageFromResponsesMap（去掉 statsx.FromMap 依赖，bridge
// 自解析）；薄壳转发以保留同包同名符号。
func usageFromResponsesMap(usage map[string]any) (int64, int64, int64) {
	return bridge.UsageFromResponsesMap(usage)
}
