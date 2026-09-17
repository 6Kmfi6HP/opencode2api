package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== /v1/chat/completions → Responses 上游 ========================

// chatMessagesToResponsesInput 把 Chat messages 转为 Responses instructions +
// input 数组。形状对齐 claudeMessagesToResponsesInput：system→instructions；
// user 文本→message/input_text；image_url→input_image；assistant.tool_calls→
// function_call；role=tool→function_call_output；assistant 文本→message/output_text。
func chatMessagesToResponsesInput(messages []Message) (string, []any) {
	var instructions string
	var input []any
	var systemParts []string
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			if s, ok := msg.Content.(string); ok && s != "" {
				systemParts = append(systemParts, s)
			}
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				for _, tc := range msg.ToolCalls {
					item := map[string]any{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					}
					if tc.ID == "" {
						item["call_id"] = "call_" + randomHex(12)
					}
					if item["arguments"] == "" {
						item["arguments"] = "{}"
					}
					input = append(input, item)
				}
			}
			if s, ok := msg.Content.(string); ok && s != "" {
				input = append(input, map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": s}},
				})
			}
		case "tool":
			output := ""
			if s, ok := msg.Content.(string); ok {
				output = s
			} else if msg.Content != nil {
				if b, err := json.Marshal(msg.Content); err == nil {
					output = string(b)
				}
			}
			callID := msg.ToolCallID
			if callID == "" {
				callID = "call_" + randomHex(12)
			}
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": callID, "output": output,
			})
		default: // user / ""
			switch c := msg.Content.(type) {
			case string:
				if c != "" {
					input = append(input, map[string]any{
						"type": "message", "role": "user",
						"content": []any{map[string]any{"type": "input_text", "text": c}},
					})
				}
			case []any:
				var parts []any
				for _, p := range c {
					pm, ok := p.(map[string]any)
					if !ok {
						continue
					}
					switch pm["type"] {
					case "text":
						if t, _ := pm["text"].(string); t != "" {
							parts = append(parts, map[string]any{"type": "input_text", "text": t})
						}
					case "image_url":
						url, _ := pm["url"].(string)
						if url == "" {
							if iu, ok := pm["image_url"].(map[string]any); ok {
								url, _ = iu["url"].(string)
							}
						}
						if url != "" {
							parts = append(parts, map[string]any{
								"type": "input_image", "image_url": url,
							})
						}
					}
				}
				if len(parts) > 0 {
					input = append(input, map[string]any{
						"type": "message", "role": "user", "content": parts,
					})
				}
			}
		}
	}
	instructions = strings.Join(systemParts, "\n\n")
	return instructions, input
}

// chatToResponsesBody 把 Chat Completions 请求转为 Responses 请求体。
func chatToResponsesBody(req *OpenAIRequest, modelID string) []byte {
	instructions, input := chatMessagesToResponsesInput(req.Messages)
	body := map[string]any{
		"model":  modelID,
		"input":  input,
		"stream": req.Stream,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if req.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		v := *req.MaxTokens
		if cap := config.MaxTokensCapFor(modelID); cap > 0 && v > cap {
			v = cap
		}
		body["max_output_tokens"] = v
	} else if cap := config.MaxTokensCapFor(modelID); cap > 0 {
		body["max_output_tokens"] = cap
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if stop := extraBodyValue(req, "stop"); stop != nil {
		body["stop"] = stop
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{
				"type": "function", "name": t.Function.Name,
			}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			if t.Function.Parameters != nil {
				tool["parameters"] = t.Function.Parameters
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = claudeToolChoiceCore(req.ToolChoice, false)
	}
	// reasoning：effort 映射；none/禁用省略。
	if !config.ForceDisableThinking() && !isThinkingDisabled(req.Thinking) {
		effort := req.ReasoningEffort
		if effort == "" {
			effort = reasoningEffortFromThinking(req.Thinking)
		}
		if effort != "" && effort != "none" {
			body["reasoning"] = map[string]any{"effort": mappedReasoningEffort(effort)}
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return []byte(fmt.Sprintf(`{"model":%q,"input":[],"stream":%t}`, modelID, req.Stream))
	}
	return b
}

// forwardChatViaResponses 处理规则命中 responses 的 Chat 入站请求：请求转
// Responses，响应转回 Chat 形状（流式 SSE→SSE / 非流式 JSON→JSON）。
func forwardChatViaResponses(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, req *OpenAIRequest, keepReasoning bool) {
	ctx := r.Context()
	upstreamBody := chatToResponsesBody(req, req.Model)
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
	outBody := convertResponsesToChat(respBody, req.Model, keepReasoning)
	var usageResp map[string]any
	if json.Unmarshal(respBody, &usageResp) == nil {
		if u, ok := usageResp["usage"].(map[string]any); ok {
			statsx.RecordChatUsage(req.Model, responsesUsageToChat(u))
		}
	}
	result := logging.SummarizeChatResult(outBody)
	logging.LogResult(ctx, result)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(outBody)
}

// convertResponsesToChat 把上游 Responses JSON 响应转为 Chat Completions 响应。
func convertResponsesToChat(respBody []byte, model string, wantReasoning bool) []byte {
	var raw map[string]any
	if err := json.Unmarshal(respBody, &raw); err != nil {
		// 非 JSON（例如上游错误体）：原样返回，让客户端看到上游语义。
		return respBody
	}
	if em, ok := raw["error"]; ok && em != nil {
		return respBody
	}

	id, _ := raw["id"].(string)
	outModel, _ := raw["model"].(string)
	if outModel == "" {
		outModel = model
	}
	var textParts []string
	var reasoningParts []string
	var refusal string
	var toolCalls []map[string]any
	finishReason := "stop"
	if status, _ := raw["status"].(string); status == "incomplete" {
		finishReason = "length"
	}
	output, _ := raw["output"].([]any)
	for _, itemRaw := range output {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "message":
			content, _ := item["content"].([]any)
			for _, c := range content {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				switch cm["type"] {
				case "output_text":
					if t, _ := cm["text"].(string); t != "" {
						textParts = append(textParts, t)
					}
				case "refusal":
					if t, _ := cm["refusal"].(string); t != "" {
						refusal += t
					}
				}
			}
		case "reasoning":
			if wantReasoning {
				if summary, ok := item["summary"].([]any); ok {
					for _, s := range summary {
						if sm, ok := s.(map[string]any); ok {
							if t, _ := sm["text"].(string); t != "" {
								reasoningParts = append(reasoningParts, t)
							}
						}
					}
				}
			}
		case "function_call", "tool_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			})
		}
	}
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	msg := map[string]any{"role": "assistant"}
	content := strings.Join(textParts, "")
	if content != "" || len(toolCalls) == 0 {
		msg["content"] = content
	}
	if reasoning := strings.Join(reasoningParts, "\n"); reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if refusal != "" {
		msg["refusal"] = refusal
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}

	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   outModel,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finishReason,
		}},
	}
	if u, ok := raw["usage"]; ok && u != nil {
		resp["usage"] = responsesUsageToChat(u.(map[string]any))
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return respBody
	}
	return out
}

// ======================== Responses SSE → Chat SSE 状态机 ========================

type responsesToChatState struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	stats         *logging.StreamStats
	id            string
	model         string
	keepReasoning bool
	includeUsage  bool
	sentRole      bool
	toolIndices   map[string]int // Responses item id/call_id → chat tool_calls index
	toolCount     int
	sawTool       bool
	fullUsage     map[string]any
}

func responsesSSEToChatStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, keepReasoning bool, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	st := &responsesToChatState{
		w:             w,
		flusher:       flusher,
		stats:         &logging.StreamStats{Start: time.Now()},
		id:            "chatcmpl-" + randomHex(12),
		model:         model,
		keepReasoning: keepReasoning,
		includeUsage:  includeUsage,
		toolIndices:   map[string]int{},
		fullUsage:     map[string]any{},
	}
	defer func() {
		st.stats.ToolCallCount = st.toolCount
		if len(st.fullUsage) > 0 {
			statsx.RecordChatUsage(model, responsesUsageToChat(st.fullUsage))
		}
		st.stats.Log(ctx, "chat")
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
				return
			}
		}
	}
}

func (st *responsesToChatState) emitChunk(delta map[string]any, finishReason string, usage map[string]any) {
	chunk := map[string]any{
		"id":      st.id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   st.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReasonOr(finishReason),
		}},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	st.w.Write([]byte("data: " + string(b) + "\n\n"))
	if st.flusher != nil {
		st.flusher.Flush()
	}
}

func (st *responsesToChatState) ensureRole() {
	if st.sentRole {
		return
	}
	st.sentRole = true
	st.emitChunk(map[string]any{"role": "assistant", "content": ""}, "", nil)
}

func (st *responsesToChatState) handleLine(line string) {
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return
	}
	var evt map[string]any
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return
	}
	switch typ, _ := evt["type"].(string); typ {
	case "response.created", "response.in_progress", "response.queued":
		if resp, ok := evt["response"].(map[string]any); ok {
			if id, _ := resp["id"].(string); id != "" {
				st.id = id
			}
			if m, _ := resp["model"].(string); m != "" {
				st.model = m
			}
			if u, ok := resp["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
		}
		st.ensureRole()
	case "response.output_text.delta":
		st.ensureRole()
		if t, _ := evt["delta"].(string); t != "" {
			st.emitChunk(map[string]any{"content": t}, "", nil)
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if st.keepReasoning {
			st.ensureRole()
			if t, _ := evt["delta"].(string); t != "" {
				st.emitChunk(map[string]any{"reasoning_content": t}, "", nil)
			}
		}
	case "response.refusal.delta":
		// refusal 增量并入正文，保持与 convertResponsesToChat 的降级一致。
		st.ensureRole()
		if t, _ := evt["delta"].(string); t != "" {
			st.emitChunk(map[string]any{"content": t}, "", nil)
		}
	case "response.output_item.added":
		item, _ := evt["item"].(map[string]any)
		if item == nil {
			return
		}
		switch item["type"] {
		case "function_call", "tool_call":
			st.sawTool = true
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			name, _ := item["name"].(string)
			toolIdx := st.toolCount
			st.toolCount++
			if callID != "" {
				st.toolIndices[callID] = toolIdx
			}
			st.ensureRole()
			st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}}}, "", nil)
		}
	case "response.function_call_arguments.delta", "response.tool_call_arguments.delta":
		st.ensureRole()
		itemID, _ := evt["item_id"].(string)
		toolIdx, ok := st.toolIndices[itemID]
		if !ok {
			toolIdx = st.toolCount
			st.toolCount++
			if itemID != "" {
				st.toolIndices[itemID] = toolIdx
			}
		}
		if pj, _ := evt["delta"].(string); pj != "" {
			st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": nil, "type": "function",
				"function": map[string]any{"name": "", "arguments": pj},
			}}}, "", nil)
		}
	case "response.completed", "response.incomplete":
		if resp, ok := evt["response"].(map[string]any); ok {
			if u, ok := resp["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
			if status, _ := resp["status"].(string); status == "incomplete" {
				st.emitChunk(map[string]any{}, "length", nil)
			} else {
				fr := "stop"
				if st.sawTool {
					fr = "tool_calls"
				}
				st.emitChunk(map[string]any{}, fr, nil)
			}
		}
		if st.includeUsage && len(st.fullUsage) > 0 {
			st.emitChunk(map[string]any{}, "", responsesUsageToChat(st.fullUsage))
		}
		st.w.Write([]byte("data: [DONE]\n\n"))
		if st.flusher != nil {
			st.flusher.Flush()
		}
		st.stats.DoneSeen = true
		st.stats.SawFinish = true
	case "response.failed", "error":
		em, _ := evt["response"].(map[string]any)
		message := "upstream error"
		if em != nil {
			if errObj, ok := em["error"].(map[string]any); ok {
				if m, _ := errObj["message"].(string); m != "" {
					message = m
				}
			}
		}
		if m, ok := evt["message"].(string); ok && m != "" {
			message = m
		}
		st.w.Write([]byte("data: " + `{"error":{"message":` + jsonString(message) + `}}` + "\n\n"))
		if st.flusher != nil {
			st.flusher.Flush()
		}
	}
}
