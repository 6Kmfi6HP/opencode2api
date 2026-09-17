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

// ======================== /v1/chat/completions → Anthropic 上游 ========================

// defaultAnthropicMaxTokens 是 Chat 入站缺省 max_tokens 时的兜底值。
// Anthropic Messages 上游把 max_tokens 视为必填。
const defaultAnthropicMaxTokens = 8192

// effortToThinkingBudget 把 reasoning_effort 映射为 Anthropic thinking 预算。
func effortToThinkingBudget(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 10240
	case "xhigh", "max":
		return 32768
	default:
		return 0
	}
}

// chatToolChoiceToAnthropic 把 Chat tool_choice 转为 Anthropic 形状。
func chatToolChoiceToAnthropic(choice any) any {
	switch v := choice.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "none"}
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

// chatTextToAnthropicContent 把字符串或 Chat 多模态 content 数组转为
// Anthropic content block 数组。图片按 data URI / URL 归类为 base64/url source。
func chatTextToAnthropicContent(content any) ([]map[string]any, bool) {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil, false
		}
		return []map[string]any{{"type": "text", "text": v}}, true
	case []any:
		var blocks []map[string]any
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				if t, _ := pm["text"].(string); t != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": t})
				}
			case "image_url":
				url, _ := pm["url"].(string)
				if url == "" {
					if iu, ok := pm["image_url"].(map[string]any); ok {
						url, _ = iu["url"].(string)
					}
				}
				if url == "" {
					continue
				}
				blocks = append(blocks, imageURLToAnthropicBlock(url))
			}
		}
		return blocks, len(blocks) > 0
	default:
		return nil, false
	}
}

// imageURLToAnthropicBlock 把 data URI 或 http(s) URL 转为 Anthropic image block。
func imageURLToAnthropicBlock(url string) map[string]any {
	if _, data, ok := strings.Cut(url, "data:"); ok {
		if meta, b64, ok2 := strings.Cut(data, ";base64,"); ok2 {
			media := strings.TrimPrefix(meta, "image/")
			if media == "" {
				media = "png"
			}
			return map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": "image/" + media, "data": b64,
				},
			}
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url", "url": url,
		},
	}
}

// chatMessagesToAnthropic 把 Chat messages 转为 Anthropic system + messages。
// system/developer 角色提取为 system（多条 "\n\n" 连接）；role=tool 转为
// tool_result user 消息；assistant tool_calls 内联为 tool_use block。
// 返回的 messages 已做连续同角色合并（Anthropic 要求交替）。
func chatMessagesToAnthropic(messages []Message) (string, []map[string]any) {
	var systemParts []string
	var out []map[string]any
	appendBlocks := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		// 连续同角色合并。
		if n := len(out); n > 0 {
			if prevRole, _ := out[n-1]["role"].(string); prevRole == role {
				prev, _ := out[n-1]["content"].([]map[string]any)
				out[n-1]["content"] = append(prev, blocks...)
				return
			}
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer":
			if blocks, ok := chatTextToAnthropicContent(msg.Content); ok {
				for _, b := range blocks {
					if t, _ := b["text"].(string); t != "" {
						systemParts = append(systemParts, t)
					}
				}
			}
		case "assistant":
			var blocks []map[string]any
			if bs, ok := chatTextToAnthropicContent(msg.Content); ok {
				blocks = append(blocks, bs...)
			}
			for _, tc := range msg.ToolCalls {
				input := parseToolCallArguments(tc.Function.Arguments)
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
				})
			}
			appendBlocks("assistant", blocks)
		case "tool":
			text := ""
			if s, ok := msg.Content.(string); ok {
				text = s
			} else if msg.Content != nil {
				if b, err := json.Marshal(msg.Content); err == nil {
					text = string(b)
				}
			}
			appendBlocks("user", []map[string]any{{
				"type": "tool_result", "tool_use_id": msg.ToolCallID,
				"content": []map[string]any{{"type": "text", "text": text}},
			}})
		case "user", "":
			if blocks, ok := chatTextToAnthropicContent(msg.Content); ok {
				appendBlocks("user", blocks)
			}
		}
	}
	return strings.Join(systemParts, "\n\n"), out
}

// parseToolCallArguments 把 Chat 工具调用 arguments JSON 解析为 Anthropic
// input 对象；空补 {}，坏 JSON 兜底 {"_raw": ...} 不丢数据。
func parseToolCallArguments(args string) map[string]any {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return map[string]any{}
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(trimmed), &input); err == nil {
		return input
	}
	return map[string]any{"_raw": args}
}

// chatToAnthropicBody 把 Chat Completions 请求转为 Anthropic Messages 请求体。
func chatToAnthropicBody(req *OpenAIRequest, modelID string) []byte {
	system, messages := chatMessagesToAnthropic(req.Messages)
	body := map[string]any{
		"model":    modelID,
		"messages": messages,
		"stream":   req.Stream,
	}
	if system != "" {
		body["system"] = system
	}
	maxTokens := defaultAnthropicMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	if cap := config.MaxTokensCapFor(modelID); cap > 0 && maxTokens > cap {
		maxTokens = cap
	}
	body["max_tokens"] = maxTokens
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if stop := extraBodyValue(req, "stop"); stop != nil {
		if arr, ok := stop.([]any); ok {
			var strs []string
			for _, v := range arr {
				if s, ok := v.(string); ok {
					strs = append(strs, s)
				}
			}
			if len(strs) > 0 {
				body["stop_sequences"] = strs
			}
		} else if s, ok := stop.(string); ok && s != "" {
			body["stop_sequences"] = []string{s}
		}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tool := map[string]any{
				"name":         t.Function.Name,
				"input_schema": schema,
			}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}
	if req.ToolChoice != nil {
		if choice := chatToolChoiceToAnthropic(req.ToolChoice); choice != nil {
			body["tool_choice"] = choice
		}
	}
	// thinking：effort 映射为预算；ForceDisableThinking 或客户端显式禁用则省略。
	if !config.ForceDisableThinking() && !isThinkingDisabled(req.Thinking) {
		effort := req.ReasoningEffort
		if effort == "" {
			effort = reasoningEffortFromThinking(req.Thinking)
		}
		if effort != "" && effort != "none" {
			effort = mappedReasoningEffort(effort)
			if budget := effortToThinkingBudget(effort); budget > 0 {
				body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
			}
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return []byte(fmt.Sprintf(`{"model":%q,"messages":[],"max_tokens":%d,"stream":%t}`, modelID, maxTokens, req.Stream))
	}
	return b
}

// extraBodyValue 取 ExtraBody 中的顶层字段。
func extraBodyValue(req *OpenAIRequest, key string) any {
	if req.ExtraBody == nil {
		return nil
	}
	return req.ExtraBody[key]
}

// forwardChatViaAnthropic 处理规则命中 anthropic 的 Chat 入站请求：请求转为
// Anthropic Messages，响应按客户端流式偏好转换回 Chat 形状。上游 4xx/5xx
// 经 parseAnthropicErrorBody 转为 chat 错误形状写回。
func forwardChatViaAnthropic(w http.ResponseWriter, r *http.Request, auth UpstreamAuth, req *OpenAIRequest, keepReasoning bool) {
	ctx := r.Context()
	upstreamBody := chatToAnthropicBody(req, req.Model)
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

type anthropicToChatState struct {
	w             http.ResponseWriter
	flusher       http.Flusher
	stats         *logging.StreamStats
	id            string
	model         string
	keepReasoning bool
	includeUsage  bool
	sentRole      bool
	blocks        map[int]string // anthropic block index → "text"|"thinking"|"tool_use"
	toolIndices   map[int]int    // anthropic block index → chat tool_calls index
	toolCount     int
	stopReason    string
	fullUsage     map[string]any
}

func anthropicSSEToChatStream(ctx context.Context, w http.ResponseWriter, rc io.Reader, model string, keepReasoning bool, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	st := &anthropicToChatState{
		w:             w,
		flusher:       flusher,
		stats:         &logging.StreamStats{Start: time.Now()},
		id:            "chatcmpl-" + randomHex(12),
		model:         model,
		keepReasoning: keepReasoning,
		includeUsage:  includeUsage,
		blocks:        map[int]string{},
		toolIndices:   map[int]int{},
		fullUsage:     map[string]any{},
	}
	defer func() {
		st.stats.ToolCallCount = st.toolCount
		if len(st.fullUsage) > 0 {
			statsx.RecordChatUsage(model, anthropicUsageToChat(st.fullUsage))
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

// emitChunk 写出一个 Chat SSE chunk。delta 为 nil 时用 {}；finishReason 非空时
// 附带 finish_reason；usage 非空时附带 usage（仅终块）。
func (st *anthropicToChatState) emitChunk(delta map[string]any, finishReason string, usage map[string]any) {
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

func finishReasonOr(fr string) any {
	if fr == "" {
		return nil
	}
	return fr
}

// handleLine 处理一行上游 Anthropic SSE。
func (st *anthropicToChatState) handleLine(line string) {
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
			if u, ok := msg["usage"].(map[string]any); ok {
				mergeUsage(st.fullUsage, u)
			}
			if m, _ := msg["model"].(string); m != "" {
				st.model = m
			}
		}
		if !st.sentRole {
			st.sentRole = true
			st.emitChunk(map[string]any{"role": "assistant", "content": ""}, "", nil)
		}
	case "content_block_start":
		idx := numberToInt(evt["index"])
		cb, _ := evt["content_block"].(map[string]any)
		bt, _ := cb["type"].(string)
		st.blocks[idx] = bt
		if bt == "tool_use" {
			toolIdx := st.toolCount
			st.toolCount++
			st.toolIndices[idx] = toolIdx
			name, _ := cb["name"].(string)
			id, _ := cb["id"].(string)
			st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": toolIdx, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}}}, "", nil)
		}
	case "content_block_delta":
		idx := numberToInt(evt["index"])
		d, _ := evt["delta"].(map[string]any)
		dt, _ := d["type"].(string)
		switch dt {
		case "text_delta":
			if t, _ := d["text"].(string); t != "" {
				st.emitChunk(map[string]any{"content": t}, "", nil)
			}
		case "thinking_delta":
			if st.keepReasoning {
				if t, _ := d["thinking"].(string); t != "" {
					st.emitChunk(map[string]any{"reasoning_content": t}, "", nil)
				}
			}
		case "input_json_delta":
			if toolIdx, ok := st.toolIndices[idx]; ok {
				if pj, _ := d["partial_json"].(string); pj != "" {
					st.emitChunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIdx, "id": nil, "type": "function",
						"function": map[string]any{"name": "", "arguments": pj},
					}}}, "", nil)
				}
			}
		}
	case "message_delta":
		if delta, ok := evt["delta"].(map[string]any); ok {
			if sr, _ := delta["stop_reason"].(string); sr != "" {
				st.stopReason = normalizeFinishReason(sr)
			}
		}
		if u, ok := evt["usage"].(map[string]any); ok {
			mergeUsage(st.fullUsage, u)
		}
	case "message_stop":
		if st.stopReason == "" {
			st.stopReason = "stop"
		}
		st.emitChunk(map[string]any{}, st.stopReason, nil)
		if st.includeUsage && len(st.fullUsage) > 0 {
			st.emitChunk(map[string]any{}, "", anthropicUsageToChat(st.fullUsage))
		}
		st.w.Write([]byte("data: [DONE]\n\n"))
		if st.flusher != nil {
			st.flusher.Flush()
		}
		st.stats.DoneSeen = true
		st.stats.SawFinish = true
		st.stats.FinishReason = st.stopReason
	case "error":
		em, _ := evt["error"].(map[string]any)
		msg := "upstream error"
		if m, _ := em["message"].(string); m != "" {
			msg = m
		}
		st.w.Write([]byte("data: " + `{"error":{"message":` + jsonString(msg) + `}}` + "\n\n"))
		if st.flusher != nil {
			st.flusher.Flush()
		}
	}
}

// numberToInt 宽松地把 any 数字转为 int（SSE JSON 解码后多为 float64）。
func numberToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// jsonString 由 count_tokens.go 提供，这里复用。
