package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/bridge"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== /v1/chat/completions → Responses 上游 ========================

// chatMessagesToResponsesInput 把 Chat messages 转为 Responses instructions +
// input 数组。纯核已迁入 bridge.ChatMessagesToResponsesInput；call_id 兜底随
// 机 hex 由 randomHexIDGen 注入（生产：internal/random，与原 randomHex 同
// 字符集）。薄壳转发到 bridge。
func chatMessagesToResponsesInput(messages []Message) (string, []any) {
	return bridge.ChatMessagesToResponsesInput(randomHexIDGen, messages)
}

// chatToResponsesBody 把 Chat Completions 请求转为 Responses 请求体。
// rawBody 为 nil。纯核已迁入 bridge。
func chatToResponsesBody(req *OpenAIRequest, modelID string) []byte {
	return chatToResponsesBodyWithRaw(req, modelID, nil)
}

func chatToResponsesBodyWithRaw(req *OpenAIRequest, modelID string, rawBody map[string]any) []byte {
	tokenCap := 0
	if modelID != "" {
		tokenCap = config.MaxTokensCapFor(modelID)
	}
	return bridge.ChatToResponsesBodyWithRaw(bridgeConfigViewFor(modelID), randomHexIDGen, req, modelID, tokenCap, rawBody)
}

// mergeResponsesIncludeKey 把 key 合并进 Responses 请求体的顶层 include
// 数组：存在则去重追加,不存在则新建。非法形状（非数组）视为不存在。
// 薄壳转发到 bridge。
func mergeResponsesIncludeKey(body map[string]any, key string) {
	bridge.MergeResponsesIncludeKey(body, key)
}

// forwardChatViaResponses 处理规则命中 responses 的 Chat 入站请求：请求转
// Responses，响应转回 Chat 形状（流式 SSE→SSE / 非流式 JSON→JSON）。
func forwardChatViaResponses(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, req *OpenAIRequest, keepReasoning bool) {
	ctx := r.Context()
	// 请求侧归一化前置：补全 assistant.tool_calls 的 tool 响应（fixToolCallGaps
	// 定义在 chat.go,Worker B 所有;这里只调用不修改）。
	req.Messages = fixToolCallGaps(req.Messages)
	upstreamBody := chatToResponsesBodyWithRaw(req, req.Model, rawRequestBodyMap(req))
	log := logging.FromContext(ctx)
	log.Info("chat via responses upstream", "model", req.Model, "stream", req.Stream, "keep_reasoning", keepReasoning)

	if req.Stream {
		rc, status, _, err := callOpenCodeEndpoint(ctx, "responses", upstreamBody, req.Model, auth)
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
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write(errBody)
				return
			}
			writeUpstreamError(w, status, fmt.Errorf("upstream error"), "chat")
			return
		}
		defer rc.Close()
		responsesSSEToChatStream(ctx, w, rc, req.Model, keepReasoning, true)
		return
	}

	rc, status, _, err := callOpenCodeEndpoint(ctx, "responses", upstreamBody, req.Model, auth)
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
	// 免费层强制 stream:true 后，Responses 上游回的是 SSE;非流式 chat 客户端
	// 需要单个 chat.completion JSON，先聚合（幂等：已是 JSON 时原样返回）。
	respBody = aggregateResponsesStreamToChat(respBody, req.Model, keepReasoning)
	outBody := convertResponsesToChat(respBody, req.Model, keepReasoning)
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(req.Model, responsesUsageToChatBridge(u))
		}
	}
	result := logging.SummarizeChatResult(outBody)
	logging.LogResult(ctx, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(outBody)
}

// aggregateResponsesStreamToChat 把上游强制 stream:true 返回的 Responses SSE
// 流聚合为一个完整的 chat.completion JSON。与 responsesSSEToChatStream 共用
// 同一套事件解析（output_text.delta / reasoning delta / output_item 与
// function_call_arguments、response.completed/incomplete 的 usage），只是终点
// 是合并后的 JSON 而不是逐块转发的 chat SSE。body 不是 Responses SSE（如已
// 是 JSON、空体或流中带 error 事件）时原样返回，交给上层既有处理（含
// convertResponsesToChat 的 JSON 解析），因此幂等。纯核已迁入
// bridge.AggregateResponsesStreamToChat；薄壳注入 now/strGen/hexGen。
func aggregateResponsesStreamToChat(body []byte, model string, wantReasoning bool) []byte {
	return bridge.AggregateResponsesStreamToChat(time.Now().Unix, randomIDGen, randomHexIDGen, body, model, wantReasoning)
}

// formatOutputIndex 格式化 Responses output_index（number）为稳定 key。
// 与 item_id 一起作为 chat tool_calls index 的别名来源。
// 缺省/非法值返回空串，不参与别名。
// 纯核已迁入 bridge.FormatOutputIndex；薄壳转发。
func formatOutputIndex(v float64) string {
	return bridge.FormatOutputIndex(v)
}

// convertResponsesToChat 把上游 Responses JSON 响应转为 Chat Completions 响应。
// 纯核已迁入 bridge.ConvertResponsesToChat；薄壳注入 now。
func convertResponsesToChat(respBody []byte, model string, wantReasoning bool) []byte {
	return bridge.ConvertResponsesToChat(time.Now().Unix, respBody, model, wantReasoning)
}

// ======================== Responses SSE → Chat SSE 状态机 ========================

// responsesUsageToChatBridge 在 claude_responses.go 的 responsesUsageToChat
// 之上补齐 chat 桥接需要的 usage 口径:DeepSeek prompt_cache_hit/miss_tokens
// 透传、input_tokens_details.cached_tokens → prompt_tokens_details.cached_tokens
// 别名、output_tokens_details.reasoning_tokens/thinking_tokens 归位。
// 薄壳转发到 bridge。
func responsesUsageToChatBridge(usage map[string]any) map[string]any {
	return bridge.ResponsesUsageToChatBridge(usage)
}

// responsesSSEToChatStream 是状态机 #2 的薄壳。纯核（帧构造、tool-call 三键索引、
// done 差量补发、兜底与终止）已迁入 internal/bridge（responses_to_chat_stream.go，
// ResponsesToChatState），产物为 bridge.OutEvent（Raw 即原实现写出的 `data: ...\n\n`
// 字节，逐字节等价;[DONE] 帧携带 Terminal 标记）。本壳负责全部副作用:HTTP 头/
// WriteHeader、streamReader 生命周期、select ctx/上游行、NoteChunk、w.Write+Flush,
// 以及流末从 bridge 快照装配 logging.StreamStats 再 deferred RecordChatUsage + Log。
func responsesSSEToChatStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, keepReasoning bool, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	st := bridge.NewResponsesToChatState(randomHexIDGen, bridge.DefaultNowFn, model, keepReasoning, includeUsage)

	// stats 复刻原 StreamStats 装配:Start/FirstChunkAt 时间在 app 侧打点（每行
	// NoteChunk）,ToolCallCount/终态标志在流末从 bridge 快照回填。
	// 注:原实现的 responses→chat 路径从不写 stats.FinishReason（原 finalize
	// 只置 DoneSeen/SawFinish),故这里不回填 FinishReason,保持日志字段逐字节
	// 等价（bridge 的 finishReason 仅用于 SSE 终态帧,不进 StreamStats）。
	stats := &logging.StreamStats{Start: time.Now()}
	defer func() {
		c := st.Counters()
		stats.ToolCallCount = c.ToolCallCount
		stats.SawFinish = c.SawFinish
		stats.DoneSeen = c.DoneSeen
		if fu := st.Usage(); len(fu) > 0 {
			statsx.RecordChatUsage(model, responsesUsageToChatBridge(fu))
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
				// EOF / 读错误兜底:response.completed 未到达时补终态
				// finish chunk(+usage)+[DONE],保证 OpenAI SDK 不挂起
				// （幂等:已完成路径不受影响）。
				writeAll(st.Finalize())
				return
			}
		}
	}
}

// outputIndexKey namespaces a Responses output_index inside the tool-index
// table, which is otherwise keyed by call_id / item id. It is the fallback key
// for argument events that carry no item_id, and it is the same key
// responses_to_anthropic.go pairs added/done events by.
// 纯核已迁入 bridge.OutputIndexKey；薄壳转发。
func outputIndexKey(oi int) string {
	return bridge.OutputIndexKey(oi)
}
