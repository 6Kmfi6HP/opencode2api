package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== /v1/chat/completions → Anthropic 上游 ========================

// defaultAnthropicMaxTokens 是 Chat 入站缺省 max_tokens 时的兜底值。
// Anthropic Messages 上游把 max_tokens 视为必填。常量定义已迁入 bridge；
// 此处为同包别名，保持 app 内引用与测试不变。
const defaultAnthropicMaxTokens = bridge.DefaultAnthropicMaxTokens

// effortToThinkingBudget 把 reasoning_effort 映射为 Anthropic thinking 预算。
// 薄壳转发到 bridge。
func effortToThinkingBudget(effort string) int { return bridge.EffortToThinkingBudget(effort) }

// chatToolChoiceToAnthropic 把 Chat tool_choice 转为 Anthropic 形状。薄壳转发到 bridge。
func chatToolChoiceToAnthropic(choice any) any {
	return bridge.ChatToolChoiceToAnthropic(choice)
}

// logDroppedParts 把 bridge 纯核上报的被丢弃 part 事件转回原有的 slog.Debug
// 日志（保持可观测输出不变）。reason 已含转换器scope 语义；part_type /
// tool_use_id / prefix 作为结构化字段附加（与原日志字段一一对应）。
func logDroppedParts(scope string, dropped []bridge.DroppedPart) {
	for _, d := range dropped {
		args := make([]any, 0, 6)
		if d.PartType != "" {
			args = append(args, "part_type", d.PartType)
		}
		if d.ToolUseID != "" {
			args = append(args, "tool_use_id", d.ToolUseID)
		}
		if d.Prefix != "" {
			args = append(args, "prefix", d.Prefix)
		}
		slog.Debug(scope+d.Reason, args...)
	}
}

// chatTextToAnthropicContent 把字符串或 Chat 多模态 content 数组转为
// Anthropic content block 数组；无法解析的 part 丢弃（记 debug 日志），全部丢
// 光时回填 [image attached] 文本。薄壳转发到 bridge，dropped 事件经
// logDroppedParts 还原为原日志。
func chatTextToAnthropicContent(content any) ([]map[string]any, bool) {
	blocks, ok, dropped := bridge.ChatTextToAnthropicContent(content)
	logDroppedParts("chat→anthropic: ", dropped)
	return blocks, ok
}

// imageURLToAnthropicBlock 把 data URI 或 http(s) URL 转为 Anthropic image block。
// 无法解析时丢弃该 part（返回 nil）并日志。薄壳转发到 bridge。
func imageURLToAnthropicBlock(url string) map[string]any {
	block, d := bridge.ImageURLToAnthropicBlock(url)
	if d.Reason != "" {
		logDroppedParts("chat→anthropic: ", []bridge.DroppedPart{d})
	}
	return block
}

// chatMessagesToAnthropic 把 Chat messages 转为 Anthropic system + messages。
// 薄壳转发到 bridge；被丢弃 part 的 debug 日志在更上层（chatToAnthropicBody
// WithRaw / imageURLToAnthropicBlock 直接调用方）经 logDroppedParts 还原。
// 注：保持原签名 (string, []map[string]any) 以兼容 responses_to_anthropic.go
// 与测试。dropped 日志在本层还原（与原逐 part 即时记录等价——数量与字段一致，
// 仅发射时机聚合到本函数返回点）。
func chatMessagesToAnthropic(messages []Message) (string, []map[string]any) {
	system, out, dropped := bridge.ChatMessagesToAnthropic(messages)
	logDroppedParts("chat→anthropic: ", dropped)
	return system, out
}

// chatToolResultBlock 构造 role=tool 消息的 tool_result block。薄壳转发到
// bridge，dropped 事件经 logDroppedParts 还原为原日志。
func chatToolResultBlock(msg Message) map[string]any {
	block, dropped := bridge.ChatToolResultBlock(msg)
	logDroppedParts("chat→anthropic ", dropped)
	return block
}

// parseToolCallArguments 把 Chat 工具调用 arguments JSON 解析为 Anthropic
// input 对象；空补 {}，坏 JSON 兜底 {"_raw": ...} 不丢数据。薄壳转发到 bridge。
func parseToolCallArguments(args string) map[string]any {
	return bridge.ParseToolCallArguments(args)
}

// resolveMaxTokens 统一 Chat→Anthropic / Chat→Responses 两个方向的 max tokens
// 口径：max_completion_tokens 优先于 max_tokens，两个键都可以从 typed
// OpenAIRequest（MaxTokens 字段）顶层或 ExtraBody / 原始请求体 map 顶层拿到
// （OpenAIRequest 没有 max_completion_tokens 字段，调用方须先 json.Unmarshal
// 入站 body 传入 rawBody）。均未设置（或 <=0）时回退 MaxTokensCapFor(modelID)，
// 仍无 cap 则兜底 8192。最终结果钳制到 [128, cap]（cap>0 且 <128 时以 cap 为准，
// 不再强制下限 —— 配置者显式限满时须尊重）。
// 纯核已迁入 bridge.ResolveMaxTokens；本壳按 modelID 解析 cap（modelID==""
// 时传 0，与原 config.MaxTokensCapFor 守卫一致）注入。
func resolveMaxTokens(rawBody map[string]any, req *OpenAIRequest, modelID string) int {
	tokenCap := 0
	if modelID != "" {
		tokenCap = config.MaxTokensCapFor(modelID)
	}
	return bridge.ResolveMaxTokens(rawBody, req, tokenCap)
}

// intFromAny 宽松地把 JSON 数字 / int 值转为 int。薄壳转发到 bridge。
func intFromAny(v any) (int, bool) {
	return bridge.IntFromAny(v)
}

// chatToAnthropicBody 把 Chat Completions 请求转为 Anthropic Messages 请求体
// 字节。纯核已迁入 bridge.ChatToAnthropicBody；本壳按 modelID 解析 ConfigView
// 与 cap 注入，dropped 事件经 logDroppedParts 还原为原 debug 日志。
func chatToAnthropicBody(req *OpenAIRequest, modelID string) []byte {
	return chatToAnthropicBodyWithRaw(req, modelID, nil)
}

func chatToAnthropicBodyWithRaw(req *OpenAIRequest, modelID string, rawBody map[string]any) []byte {
	tokenCap := 0
	if modelID != "" {
		tokenCap = config.MaxTokensCapFor(modelID)
	}
	b, dropped := bridge.ChatToAnthropicBodyWithRaw(bridgeConfigViewFor(modelID), req, modelID, tokenCap, rawBody)
	logDroppedParts("chat→anthropic: ", dropped)
	return b
}

// extraBodyValue 取 ExtraBody 中的顶层字段。薄壳转发到 bridge。
func extraBodyValue(req *OpenAIRequest, key string) any {
	return bridge.ExtraBodyValue(req, key)
}

// rawRequestBodyMap 从已解析的 OpenAIRequest 重建入站 body 的顶层键视图，
// 供 resolveMaxTokens 读取 max_completion_tokens 等类型化结构装不下的字段。
// 薄壳转发到 bridge。
func rawRequestBodyMap(req *OpenAIRequest) map[string]any {
	return bridge.RawRequestBodyMap(req)
}

// forwardChatViaAnthropic 处理规则命中 anthropic 的 Chat 入站请求：请求转为
// Anthropic Messages，响应按客户端流式偏好转换回 Chat 形状。上游 4xx/5xx
// 经 parseAnthropicErrorBody 转为 chat 错误形状写回。
func forwardChatViaAnthropic(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, req *OpenAIRequest, keepReasoning bool) {
	ctx := r.Context()
	// 请求侧归一化前置：补全 assistant.tool_calls 的 tool 响应、text-only 模型
	// 降级多模态 content 为 "[image attached]" 文本。fixToolCallGaps /
	// modelIsTextOnly / downgradeMultimodalContent 定义在 chat.go（Worker B
	// 所有），这里只调用不修改。
	req.Messages = fixToolCallGaps(req.Messages)
	textOnly := modelIsTextOnly(req.Model)
	for i := range req.Messages {
		if parts, ok := req.Messages[i].Content.([]any); ok {
			req.Messages[i].Content = downgradeMultimodalContent(parts, textOnly)
		}
	}
	rawBody := rawRequestBodyMap(req)
	upstreamBody := chatToAnthropicBodyWithRaw(req, req.Model, rawBody)
	log := logging.FromContext(ctx)
	log.Info("chat via anthropic upstream",
		"model", req.Model, "stream", req.Stream, "keep_reasoning", keepReasoning)

	if req.Stream {
		rc, status, _, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, req.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			var errBody []byte
			if rc != nil {
				errBody, _ = io.ReadAll(io.LimitReader(rc, 64*1024))
				rc.Close()
			}
			if status < 100 || status >= 600 {
				status = http.StatusBadGateway
			}
			if len(errBody) > 0 {
				if ape, ok := parseAnthropicErrorBody(errBody); ok {
					writeUpstreamError(w, status, ape, "chat")
					return
				}
				// 上游错误体非 Anthropic 形状：原样透传，保真状态码。
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write(errBody)
				return
			}
			writeUpstreamError(w, status, fmt.Errorf("upstream error"), "chat")
			return
		}
		defer rc.Close()
		anthropicSSEToChatStream(ctx, w, rc, req.Model, keepReasoning, true)
		return
	}

	rc, status, _, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, req.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		var errBody []byte
		if rc != nil {
			errBody, _ = io.ReadAll(io.LimitReader(rc, 64*1024))
			rc.Close()
		}
		if status < 100 || status >= 600 {
			status = http.StatusBadGateway
		}
		if len(errBody) > 0 {
			if ape, ok := parseAnthropicErrorBody(errBody); ok {
				writeUpstreamError(w, status, ape, "chat")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write(errBody)
			return
		}
		writeUpstreamError(w, status, fmt.Errorf("upstream error"), "chat")
		return
	}
	defer rc.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		writeUpstreamError(w, http.StatusBadGateway, fmt.Errorf("upstream read error"), "chat")
		return
	}
	// Anthropic message（或 SSE 缓冲）→ Chat，复用既有回归转换器。
	outBody, convErr := convertAnthropicToOpenAI(respBody, req.Model)
	if convErr != nil {
		writeUpstreamError(w, http.StatusBadGateway, convErr, "chat")
		return
	}
	if cleaned, err := convertResponse(outBody, keepReasoning); err == nil {
		outBody = cleaned
	}
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(req.Model, anthropicUsageToChat(u))
		}
	}
	result := logging.SummarizeChatResult(outBody)
	logging.LogResult(ctx, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(outBody)
}

// ======================== Anthropic SSE → Chat SSE 状态机 ========================
//
// 状态机纯核已迁入 internal/bridge（anthropic_to_chat_stream.go）：帧的构造、
// usage 合并、兜底与终止逻辑都是无 I/O 的纯转换，产物为 bridge.OutEvent（Raw
// 即原实现写出的 `data: ...\n\n` 字节，逐字节等价）。下方 anthropicSSEToChatStream
// 是保留同包同名的薄壳，负责全部副作用：HTTP 头/WriteHeader、streamReader 生命周期、
// select ctx/上游行、NoteChunk、w.Write+Flush，以及流末装配 logging.StreamStats
// 再走 deferred statsx.RecordChatUsage + Log。

func anthropicSSEToChatStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, keepReasoning bool, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	st := bridge.NewAnthropicToChatState(randomHexIDGen, bridge.DefaultNowFn, model, keepReasoning, includeUsage)

	// stats 复刻原 StreamStats 装配：Start/FirstChunkAt 时间在 app 侧打点（每行
	// NoteChunk），ToolCallCount/终态标志在流末从 bridge 快照回填。
	stats := &logging.StreamStats{Start: time.Now()}
	defer func() {
		c := st.Counters()
		stats.ToolCallCount = c.ToolCallCount
		stats.FinishReason = c.FinishReason
		stats.SawFinish = c.SawFinish
		stats.DoneSeen = c.DoneSeen
		if fu := st.Usage(); len(fu) > 0 {
			statsx.RecordChatUsage(model, anthropicUsageToChat(fu))
		}
		stats.Log(ctx, "chat")
	}()

	reader := newStreamReader(ctx, rc, 0)
	defer reader.Close()
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
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-reader.Read():
			pendingErr := result.err
			if result.line != "" {
				stats.NoteChunk()
				writeAll(st.Handle(bridge.LineEvent{RawLine: result.line}))
			}
			if pendingErr != nil {
				// EOF / 读错误兜底:message_stop 未到达时补 finish+usage 终块
				// +[DONE],保证 OpenAI SDK 不挂起（幂等）。
				writeAll(st.Finalize())
				c := st.Counters()
				if c.SkippedSignatures > 0 || c.SkippedRedacted > 0 {
					slog.Debug("chat stream: dropped unrepresentable anthropic deltas",
						"model", model, "signature_delta", c.SkippedSignatures, "redacted_thinking", c.SkippedRedacted)
				}
				return
			}
		}
	}
}

// finishReasonOr 返回 chat chunk 的 finish_reason 字段值（空串→nil，使字段
// 省略为 null，直到原因可知）。薄壳转发到 bridge；本壳仍被状态机 #2
// （chat_to_responses_upstream.go）复用，其迁移在后续单元进行。
func finishReasonOr(fr string) any {
	return bridge.FinishReasonOr(fr)
}

// numberToInt 宽松地把 any 数字转为 int（SSE JSON 解码后多为 float64）。
// 薄壳转发到 bridge；仍被 responses_to_anthropic.go 与状态机 #2 复用。
func numberToInt(v any) int {
	return bridge.NumberToInt(v)
}

// jsonString 由 count_tokens.go 提供（Worker D 所有），这里复用。
