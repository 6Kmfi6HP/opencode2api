package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
func convertAnthropicToResponses(anthropicBody []byte, model string, wantReasoning bool) []byte {
	chatBody, err := convertAnthropicToOpenAI(anthropicBody, model)
	if err != nil {
		return []byte(`{"error":{"message":"failed to convert anthropic response","type":"upstream_error"}}`)
	}
	return convertChatToResponses(chatBody, model, wantReasoning, nil, nil, nil)
}

// ======================== Anthropic SSE → Responses SSE 状态机 ========================

// anthropicToResponsesState 把上游 Anthropic Messages SSE 事件流转换为
// Responses SSE 事件流。事件序列对齐 responsesStreamHandler 的输出形状。
type anthropicToResponsesState struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	stats         *logging.StreamStats
	id            string
	model         string
	wantReasoning bool
	seq           int
	outputIndex   int
	fullUsage     map[string]any
	// 当前打开的块：anthropic block index → 类型
	blocks      map[int]string
	itemIDs     map[int]string // anthropic block index → Responses item id
	toolIndices map[int]int    // anthropic block index → function_call output index
	toolCount   int
	// 文本/推理累计（用于 output_item.done 全量回填）
	fullText      strings.Builder
	fullReasoning strings.Builder
	// 已完成的 output 汇总（message_stop 时写入 response.completed）
	completedOutput []any
	terminalSent    bool
}

func anthropicSSEToResponsesStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, wantReasoning bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	st := &anthropicToResponsesState{
		w:             w,
		flusher:       flusher,
		stats:         &logging.StreamStats{Start: time.Now()},
		id:            "resp_" + randomHex(16),
		model:         model,
		wantReasoning: wantReasoning,
		blocks:        map[int]string{},
		itemIDs:       map[int]string{},
		toolIndices:   map[int]int{},
		fullUsage:     map[string]any{},
	}
	defer func() {
		st.stats.ToolCallCount = st.toolCount
		st.stats.TextChars = st.fullText.Len()
		st.stats.ReasoningChars = st.fullReasoning.Len()
		if len(st.fullUsage) > 0 {
			statsx.RecordChatUsage(model, responsesUsageToChat(st.fullUsage))
		}
		st.stats.Log(ctx, "responses")
	}()

	reader := newStreamReader(ctx, rc, 0)
	defer reader.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-reader.Read():
			pendingErr := result.err
			if result.line != "" {
				st.stats.NoteChunk()
				st.handleLine(result.line)
			}
			if pendingErr != nil {
				// 上游 EOF 未发 message_stop：补 response.completed 保证客户端终止。
				st.ensureTerminal("completed", "response.completed")
				return
			}
		}
	}
}

// emitEvent 写出一个 Responses SSE 事件，自动递增 sequence_number。
func (st *anthropicToResponsesState) emitEvent(event string, data map[string]any) {
	st.seq++
	data["type"] = event
	data["sequence_number"] = st.seq
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	st.w.Write([]byte("event: " + event + "\ndata: " + string(b) + "\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
}

// ensureTerminal 幂等写出终结事件（completed/incomplete/failed）。
func (st *anthropicToResponsesState) ensureTerminal(status, event string) {
	if st.terminalSent {
		return
	}
	st.terminalSent = true
	response := map[string]any{
		"id":      st.id,
		"object":  "response",
		"created": time.Now().Unix(),
		"status":  status,
		"model":   st.model,
		"output":  st.completedOutput,
		"usage":   st.fullUsage,
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	st.emitEvent(event, map[string]any{"response": response})
	st.w.Write([]byte("data: [DONE]\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
	st.stats.DoneSeen = true
	st.stats.SawFinish = true
}

func (st *anthropicToResponsesState) handleLine(line string) {
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return
	}
	var evt map[string]any
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return
	}
	switch typ, _ := evt["type"].(string); typ {
	case "message_start":
		if msg, ok := evt["message"].(map[string]any); ok {
			if m, _ := msg["model"].(string); m != "" {
				st.model = m
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
		}
		st.emitEvent("response.created", map[string]any{
			"response": map[string]any{
				"id": st.id, "object": "response", "created": time.Now().Unix(),
				"status": "in_progress", "model": st.model, "output": []any{},
			},
		})
	case "content_block_start":
		idx := numberToInt(evt["index"])
		cb, _ := evt["content_block"].(map[string]any)
		bt, _ := cb["type"].(string)
		st.blocks[idx] = bt
		switch bt {
		case "thinking":
			itemID := "rs_" + randomHex(12)
			st.itemIDs[idx] = itemID
			st.emitEvent("response.output_item.added", map[string]any{
				"output_index": st.outputIndex,
				"item": map[string]any{
					"type": "reasoning", "id": itemID, "summary": []any{},
				},
			})
		case "text":
			itemID := "msg_" + randomHex(12)
			st.itemIDs[idx] = itemID
			st.emitEvent("response.output_item.added", map[string]any{
				"output_index": st.outputIndex,
				"item": map[string]any{
					"type": "message", "id": itemID, "role": "assistant",
					"status": "in_progress", "content": []any{},
				},
			})
			st.emitEvent("response.content_part.added", map[string]any{
				"item_id": itemID, "output_index": st.outputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": ""},
			})
		case "tool_use":
			itemID := "fc_" + randomHex(12)
			st.itemIDs[idx] = itemID
			callID, _ := cb["id"].(string)
			name, _ := cb["name"].(string)
			st.toolIndices[idx] = st.toolCount
			st.toolCount++
			st.emitEvent("response.output_item.added", map[string]any{
				"output_index": st.outputIndex,
				"item": map[string]any{
					"type": "function_call", "id": itemID, "call_id": callID,
					"name": name, "arguments": "", "status": "in_progress",
				},
			})
		}
	case "content_block_delta":
		idx := numberToInt(evt["index"])
		d, _ := evt["delta"].(map[string]any)
		dt, _ := d["type"].(string)
		switch dt {
		case "text_delta":
			if t, _ := d["text"].(string); t != "" {
				st.fullText.WriteString(t)
				st.emitEvent("response.output_text.delta", map[string]any{
					"item_id": st.itemIDs[idx], "delta": t,
				})
			}
		case "thinking_delta":
			if st.wantReasoning {
				if t, _ := d["thinking"].(string); t != "" {
					st.fullReasoning.WriteString(t)
					st.emitEvent("response.reasoning_summary_text.delta", map[string]any{
						"item_id": st.itemIDs[idx], "delta": t,
					})
				}
			}
		case "input_json_delta":
			if toolIdx, ok := st.toolIndices[idx]; ok {
				if pj, _ := d["partial_json"].(string); pj != "" {
					st.emitEvent("response.function_call_arguments.delta", map[string]any{
						"item_id": st.itemIDs[idx], "output_index": toolIdx, "delta": pj,
					})
				}
			}
		}
	case "content_block_stop":
		idx := numberToInt(evt["index"])
		bt := st.blocks[idx]
		itemID := st.itemIDs[idx]
		switch bt {
		case "thinking":
			st.completedOutput = append(st.completedOutput, map[string]any{
				"type": "reasoning", "id": itemID, "summary": []any{},
			})
			st.emitEvent("response.output_item.done", map[string]any{
				"output_index": st.outputIndex,
				"item": map[string]any{
					"type": "reasoning", "id": itemID, "summary": []any{},
				},
			})
		case "text":
			text := st.fullText.String()
			st.completedOutput = append(st.completedOutput, map[string]any{
				"type": "message", "id": itemID, "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text}},
			})
			st.emitEvent("response.output_text.done", map[string]any{
				"item_id": itemID, "text": text,
			})
			st.emitEvent("response.output_item.done", map[string]any{
				"output_index": st.outputIndex,
				"item": map[string]any{
					"type": "message", "id": itemID, "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": text}},
				},
			})
		case "tool_use":
			toolIdx := st.toolIndices[idx]
			st.completedOutput = append(st.completedOutput, map[string]any{
				"type": "function_call", "id": itemID, "status": "completed",
			})
			st.emitEvent("response.function_call_arguments.done", map[string]any{
				"item_id": itemID, "output_index": toolIdx,
			})
			st.emitEvent("response.output_item.done", map[string]any{
				"output_index": st.outputIndex,
				"item": map[string]any{
					"type": "function_call", "id": itemID, "status": "completed",
				},
			})
		}
		st.outputIndex++
		delete(st.blocks, idx)
	case "message_delta":
		if u, ok := evt["usage"].(map[string]any); ok {
			mergeUsage(st.fullUsage, u)
		}
		if delta, ok := evt["delta"].(map[string]any); ok {
			if sr, _ := delta["stop_reason"].(string); sr != "" {
				st.stats.FinishReason = sr
				st.stats.SawFinish = true
			}
		}
	case "message_stop":
		if st.stats.FinishReason == "max_tokens" || st.stats.FinishReason == "max_tokens_cap" {
			st.ensureTerminal("incomplete", "response.incomplete")
		} else {
			st.ensureTerminal("completed", "response.completed")
		}
	case "error":
		em, _ := evt["error"].(map[string]any)
		message := "upstream error"
		if m, _ := em["message"].(string); m != "" {
			message = m
		}
		st.emitEvent("response.failed", map[string]any{
			"response": map[string]any{
				"id": st.id, "status": "failed",
				"error": map[string]any{"message": message},
			},
		})
		st.w.Write([]byte("data: [DONE]\n\n"))
		if st.flusher != nil {
			st.flusher.Flush()
		}
		st.terminalSent = true
	}
}
