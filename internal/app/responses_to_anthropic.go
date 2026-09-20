package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== /v1/responses → Anthropic 上游 ========================

// forwardResponsesViaAnthropic 处理规则命中 anthropic 的 Responses 入站请求：
// 请求侧复用 handler 已构建的 chatReq（保留 responsesInputToMessages、
// fixToolCallGaps、多模态与 text-only 降级等既有行为）→ chatToAnthropicBody；
// 响应按客户端流式偏好转回 Responses 形状。
func forwardResponsesViaAnthropic(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, chatReq *OpenAIRequest, wantReasoning bool) {
	ctx := r.Context()
	upstreamBody := chatToAnthropicBody(chatReq, chatReq.Model)
	log := logging.FromContext(ctx)
	log.Info("responses via anthropic upstream",
		"model", chatReq.Model, "stream", chatReq.Stream, "keep_reasoning", wantReasoning)

	if chatReq.Stream {
		rc, status, _, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, chatReq.Model, auth)
		if err != nil || status < 200 || status >= 300 {
			writeCrossProtocolError(w, status, err, rc, "responses")
			return
		}
		defer rc.Close()
		anthropicSSEToResponsesStream(ctx, w, rc, chatReq.Model, wantReasoning)
		return
	}

	rc, status, _, err := callOpenCodeAnthropicEndpoint(ctx, upstreamBody, chatReq.Model, auth)
	if err != nil || status < 200 || status >= 300 {
		writeCrossProtocolError(w, status, err, rc, "responses")
		return
	}
	defer rc.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(rc, 32*1024*1024))
	if readErr != nil {
		writeUpstreamError(w, http.StatusBadGateway, fmt.Errorf("upstream read error"), "responses")
		return
	}
	responsesBody := convertAnthropicToResponses(respBody, chatReq.Model, wantReasoning)
	result := logging.SummarizeChatResult(responsesBody)
	logging.LogResult(ctx, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(responsesBody)
}

// writeCrossProtocolError 把上游错误统一写回：优先解析 Anthropic 错误体转
// 类型化错误，其次原样透传错误体，最后合成默认错误。
func writeCrossProtocolError(w http.ResponseWriter, status int, err error, rc io.ReadCloser, protocol string) {
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
			writeUpstreamError(w, status, ape, protocol)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(errBody)
		return
	}
	if err == nil {
		err = fmt.Errorf("upstream error")
	}
	writeUpstreamError(w, status, err, protocol)
}

// convertAnthropicToResponses 把 Anthropic message JSON 转为 Responses 对象：
// 链式复用 convertAnthropicToOpenAI（Anthropic→Chat）与 convertChatToResponses
// （Chat→Responses），随后应用请求回显与会话状态保存。
// 纯核已迁入 bridge.ConvertAnthropicToResponses；薄壳注入 now/idGen。
func convertAnthropicToResponses(anthropicBody []byte, model string, wantReasoning bool) []byte {
	return bridge.ConvertAnthropicToResponses(time.Now().Unix, randomIDGen, anthropicBody, model, wantReasoning)
}

// ======================== Anthropic SSE → Responses SSE 状态机 ========================

// anthropicSSEToResponsesStream 是状态机 #3（Anthropic Messages SSE → Responses
// SSE）的薄壳。纯核（named `event:`/`data:` 帧构造、按 output_index 升序的
// closeBlock 回填、seq 严格单调、ensureTerminal 幂等、重复 start/EOF 兜底）已迁入
// internal/bridge（anthropic_to_responses_stream.go，AnthropicToResponsesState）,
// 产物为 bridge.OutEvent(Raw 即原实现写出的 `event: ...\ndata: ...\n\n` 字节,逐字
// 节等价;终结 [DONE] 帧携带 Terminal 标记)。本壳负责全部副作用:HTTP 头/
// WriteHeader、streamReader 生命周期、select ctx/上游行、NoteChunk、w.Write+Flush,
// 以及流末从 bridge 快照装配 logging.StreamStats 再 deferred RecordChatUsage + Log。
func anthropicSSEToResponsesStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, wantReasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	st := bridge.NewAnthropicToResponsesState(randomHexIDGen, bridge.DefaultNowFn, model, wantReasoning)

	// stats 复刻原 StreamStats 装配:Start/FirstChunkAt 在 app 侧打点（每行
	// NoteChunk）,TextChars/ReasoningChars/ToolCallCount/终态标志在流末从 bridge
	// 快照回填（等价于旧实现 text_delta/thinking_delta 的即时累计）。
	stats := &logging.StreamStats{Start: time.Now()}
	defer func() {
		c := st.Counters()
		stats.TextChars = c.TextChars
		stats.ReasoningChars = c.ReasoningChars
		stats.ToolCallCount = c.ToolCallCount
		stats.FinishReason = c.FinishReason
		stats.SawFinish = c.SawFinish
		stats.DoneSeen = c.DoneSeen
		if fu := st.Usage(); len(fu) > 0 {
			statsx.RecordChatUsage(model, responsesUsageToChat(fu))
		}
		stats.Log(ctx, "responses")
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
				// Handle 发出终结事件（message_stop/error/重复 start 兜底）后直接
				// 退出,不再消费上游后续行。
				if st.Terminal() {
					return
				}
			}
			if pendingErr != nil {
				// 上游 EOF 未发 message_stop：补 response.completed 保证客户端终止
				// （幂等：已终结路径不受影响）。
				writeAll(st.Finalize())
				return
			}
		}
	}
}
