package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// This file holds the pure Responses-protocol -> Chat Completions and
// Chat/Anthropic -> Claude Messages response converters moved out of
// internal/app/chat_to_responses_upstream.go, internal/app/claude.go, and
// internal/app/responses_to_anthropic.go. They are non-streaming / aggregate
// converters that return whole wire bodies. They perform no I/O, logging,
// metrics, or config reads; ID generation and timestamps are injected via an
// IDGen / NowFn. The app layer keeps same-name lowercase forwarding shells so
// existing *_test.go files compile unchanged.

// FormatOutputIndex 格式化 Responses output_index（number）为稳定 key。
// 与 item_id 一起作为 chat tool_calls index 的别名来源。缺省/非法值（0）返回
// 空串，不参与别名。
// (Moved from app/chat_to_responses_upstream.go formatOutputIndex.)
func FormatOutputIndex(v float64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%d", int(v))
}

// OutputIndexKey namespaces a Responses output_index inside the tool-index
// table, which is otherwise keyed by call_id / item id. It is the fallback key
// for argument events that carry no item_id, and it is the same key the
// Anthropic-SSE -> Responses stream pairs added/done events by.
// (Moved from app/chat_to_responses_upstream.go outputIndexKey.)
func OutputIndexKey(oi int) string {
	return fmt.Sprintf("#%d", oi)
}

// AggregateResponsesStreamToChat 把上游强制 stream:true 返回的 Responses SSE
// 流聚合为一个完整的 chat.completion JSON。body 不是 Responses SSE（如已是
// JSON、空体或流中带 error 事件）时原样返回，交给上层既有处理（含
// ConvertResponsesToChat 的 JSON 解析），因此幂等。nowFn 与 idGen 分别注入
// "created" 时间戳与 tool-call / fallback 响应 id。
// (Moved from app/chat_to_responses_upstream.go aggregateResponsesStreamToChat.)
func AggregateResponsesStreamToChat(nowFn NowFn, strGen IDGen, hexGen HexIDGen, body []byte, model string, wantReasoning bool) []byte {
	var id, outModel string
	var contentBuilder, reasoningBuilder strings.Builder
	var refusal string
	type toolAcc struct {
		callID, name, args string
	}
	tools := map[int]*toolAcc{}
	toolItemToIdx := map[string]int{}
	toolOrder := []int{}
	finishReason := "stop"
	var usage map[string]any
	sawChunk := false

	// toolIdxFor 解析 item id/call_id/output_index 对应的稳定 chat index。
	toolIdxFor := func(itemID, outputIndex string) int {
		if itemID != "" {
			if idx, ok := toolItemToIdx[itemID]; ok {
				return idx
			}
		}
		if outputIndex != "" {
			if idx, ok := toolItemToIdx["#"+outputIndex]; ok {
				return idx
			}
		}
		idx := len(toolOrder)
		toolOrder = append(toolOrder, idx)
		if itemID != "" {
			toolItemToIdx[itemID] = idx
		}
		if outputIndex != "" {
			toolItemToIdx["#"+outputIndex] = idx
		}
		return idx
	}
	ensureTool := func(itemID, outputIndex string) *toolAcc {
		idx := toolIdxFor(itemID, outputIndex)
		acc := tools[idx]
		if acc == nil {
			acc = &toolAcc{}
			tools[idx] = acc
		}
		return acc
	}

	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var evt map[string]any
		if json.Unmarshal([]byte(payload), &evt) != nil {
			continue
		}
		sawChunk = true
		if _, isErr := evt["error"]; isErr {
			return body
		}
		switch typ, _ := evt["type"].(string); typ {
		case "response.created", "response.in_progress", "response.queued":
			if resp, ok := evt["response"].(map[string]any); ok {
				if rid, _ := resp["id"].(string); rid != "" && id == "" {
					id = rid
				}
				if m, _ := resp["model"].(string); m != "" {
					outModel = m
				}
			}
		case "response.output_text.delta":
			if t, _ := evt["delta"].(string); t != "" {
				contentBuilder.WriteString(t)
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if wantReasoning {
				if t, _ := evt["delta"].(string); t != "" {
					reasoningBuilder.WriteString(t)
				}
			}
		case "response.refusal.delta":
			if t, _ := evt["delta"].(string); t != "" {
				refusal += t
			}
		case "response.output_item.added", "response.output_item.done":
			item, _ := evt["item"].(map[string]any)
			if item == nil {
				continue
			}
			it, _ := item["type"].(string)
			if it != "function_call" && it != "tool_call" {
				continue
			}
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			oi, _ := item["output_index"].(float64)
			acc := ensureTool(callID, FormatOutputIndex(oi))
			if callID != "" {
				acc.callID = callID
			}
			if n, _ := item["name"].(string); n != "" {
				acc.name = n
			}
			if args, _ := item["arguments"].(string); args != "" && acc.args == "" {
				acc.args = args
			}
		case "response.function_call_arguments.delta", "response.tool_call_arguments.delta":
			oi, _ := evt["output_index"].(float64)
			itemID, _ := evt["item_id"].(string)
			acc := ensureTool(itemID, FormatOutputIndex(oi))
			if pj, _ := evt["delta"].(string); pj != "" {
				acc.args += pj
			}
		case "response.function_call_arguments.done", "response.tool_call_arguments.done":
			oi, _ := evt["output_index"].(float64)
			itemID, _ := evt["item_id"].(string)
			acc := ensureTool(itemID, FormatOutputIndex(oi))
			if completed, _ := evt["arguments"].(string); completed != "" {
				acc.args = completed
			}
		case "response.completed", "response.incomplete":
			if resp, ok := evt["response"].(map[string]any); ok {
				if u, ok := resp["usage"].(map[string]any); ok {
					usage = u
				}
				if rid, _ := resp["id"].(string); rid != "" {
					id = rid
				}
				if m, _ := resp["model"].(string); m != "" {
					outModel = m
				}
				if status, _ := resp["status"].(string); status == "incomplete" {
					finishReason = "length"
				}
			}
		}
	}
	if !sawChunk {
		return body
	}
	if id == "" {
		id = "chatcmpl_" + strGen("chatcmpl_", 24)
	}
	if outModel == "" {
		outModel = model
	}
	if len(toolOrder) > 0 && finishReason == "stop" {
		finishReason = "tool_calls"
	}

	msg := map[string]any{"role": "assistant"}
	content := contentBuilder.String()
	if content != "" || len(toolOrder) == 0 {
		msg["content"] = content
	}
	if reasoning := reasoningBuilder.String(); reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if refusal != "" {
		msg["refusal"] = refusal
	}
	if len(toolOrder) > 0 {
		var toolCalls []map[string]any
		for _, idx := range toolOrder {
			acc := tools[idx]
			if acc == nil {
				continue
			}
			callID := acc.callID
			if callID == "" {
				callID = "call_" + hexGen("call_", 12)
			}
			args := acc.args
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      acc.name,
					"arguments": args,
				},
			})
		}
		msg["tool_calls"] = toolCalls
	}

	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": nowFn(),
		"model":   outModel,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finishReason,
		}},
	}
	if usage != nil {
		resp["usage"] = ResponsesUsageToChatBridge(usage)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}

// ConvertResponsesToChat 把上游 Responses JSON 响应转为 Chat Completions 响应。
// 非 JSON 或带 error 字段的 body 原样返回，让客户端看到上游语义。nowFn 注入
// "created" 时间戳。
// (Moved from app/chat_to_responses_upstream.go convertResponsesToChat.)
func ConvertResponsesToChat(nowFn NowFn, respBody []byte, model string, wantReasoning bool) []byte {
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
		"created": nowFn(),
		"model":   outModel,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finishReason,
		}},
	}
	if u, ok := raw["usage"]; ok && u != nil {
		resp["usage"] = ResponsesUsageToChatBridge(u.(map[string]any))
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return respBody
	}
	return out
}

// OpenAIToClaudeResponse converts a Chat Completions response body into an
// Anthropic Messages response. It first consumes the private
// "_opencode2api_anthropic_content" ordered block list when present (preserving
// thinking / redacted_thinking / tool_use roundtrip and signatures), else falls
// back to string content + reasoning_content + tool_calls. The previous
// implementation logged (slog.Warn) and continued when the top-level unmarshal
// failed; the pure layer cannot log, so it proceeds identically on the empty
// struct and the warning is intentionally dropped. idGen mints the msg_ response
// ID. (Moved from app/claude.go openAIToClaudeResponse.)
func OpenAIToClaudeResponse(idGen IDGen, chatBody []byte, model string, wantReasoning bool) []byte {
	var chat struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	// 反序列化失败沿用旧的 slog.Warn-then-continue 语义（用空 struct 兜底），
	// 纯层改为静默继续；上层只见返回字节，不感知此软失败。
	_ = json.Unmarshal(chatBody, &chat)

	content := []ClaudeContent{}
	stopReason := "end_turn"

	if len(chat.Choices) > 0 {
		msg := chat.Choices[0].Message
		fr := chat.Choices[0].FinishReason

		// Try to read private ordered Anthropic content blocks first.
		var rawMsg map[string]any
		privateBlocks := []map[string]any(nil)
		if json.Unmarshal(chatBody, &rawMsg) == nil {
			if choices, ok := rawMsg["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if m, ok := choice["message"].(map[string]any); ok {
						if pb, ok := m["_opencode2api_anthropic_content"].([]any); ok {
							for _, item := range pb {
								if blk, ok := item.(map[string]any); ok {
									privateBlocks = append(privateBlocks, blk)
								}
							}
						}
					}
				}
			}
		}

		if len(privateBlocks) > 0 {
			// Consume private ordered blocks in array order.
			for _, blk := range privateBlocks {
				bt, _ := blk["type"].(string)
				switch bt {
				case "text":
					text, _ := blk["text"].(string)
					content = append(content, ClaudeContent{
						Type: "text",
						Text: text,
					})
				case "thinking":
					if wantReasoning {
						thinking, _ := blk["thinking"].(string)
						cc := ClaudeContent{
							Type:     "thinking",
							Thinking: thinking,
						}
						if sig, ok := blk["signature"].(string); ok && sig != "" {
							cc.Signature = sig
						}
						content = append(content, cc)
					}
				case "redacted_thinking":
					if wantReasoning {
						cc := ClaudeContent{
							Type: "redacted_thinking",
						}
						if d, ok := blk["data"].(string); ok && d != "" {
							cc.Data = d
						}
						content = append(content, cc)
					}
				case "tool_use":
					id, _ := blk["id"].(string)
					name, _ := blk["name"].(string)
					input := blk["input"]
					if input == nil {
						input = map[string]any{}
					}
					content = append(content, ClaudeContent{
						Type:  "tool_use",
						ID:    id,
						Name:  name,
						Input: input,
					})
				}
			}
		} else {
			// Fallback: string content + reasoning_content + tool_calls.
			if wantReasoning && msg.ReasoningContent != "" {
				content = append(content, ClaudeContent{
					Type:     "thinking",
					Thinking: msg.ReasoningContent,
				})
			}
			text := msg.Content
			// #37635: Go gateway often puts the whole answer in reasoning_content.
			// Promote to text when content is empty so Claude Code does not see an
			// empty end_turn and exit the agent loop.
			if text == "" && msg.ReasoningContent != "" && len(msg.ToolCalls) == 0 {
				text = msg.ReasoningContent
			}
			if text != "" {
				content = append(content, ClaudeContent{
					Type: "text",
					Text: text,
				})
			}
			for _, tc := range msg.ToolCalls {
				var input any
				json.Unmarshal([]byte(tc.Function.Arguments), &input)
				if input == nil {
					input = map[string]any{}
				}
				content = append(content, ClaudeContent{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
		}

		switch fr {
		case "stop":
			stopReason = "end_turn"
		case "length":
			stopReason = "max_tokens"
		case "tool_calls", "function_call":
			stopReason = "tool_use"
		case "content_filter":
			stopReason = "refusal"
		}
	}

	if len(content) == 0 {
		content = append(content, ClaudeContent{Type: "text", Text: ""})
	}

	// Response ID: keep upstream ID only if it is a valid msg_ ID;
	// otherwise generate a new msg_ ID. Never leak chatcmpl/resp IDs.
	respID := NormalizeClaudeMessageID(idGen, chat.ID)

	resp := ClaudeResponse{
		ID:           respID,
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        model,
		StopReason:   stopReason,
		StopSequence: nil,
	}
	if chat.Usage != nil {
		resp.Usage = BuildClaudeMessageUsage(chat.Usage)
	}
	result, _ := json.Marshal(resp)
	return result
}

// ConvertAnthropicToResponses 把 Anthropic message JSON 转为 Responses 对象：
// 链式复用 ConvertAnthropicToOpenAI（Anthropic→Chat）与 ConvertChatToResponses
// （Chat→Responses）。nowFn 与 idGen 贯穿两步注入。
// (Moved from app/responses_to_anthropic.go convertAnthropicToResponses.)
func ConvertAnthropicToResponses(nowFn NowFn, idGen IDGen, anthropicBody []byte, model string, wantReasoning bool) []byte {
	chatBody, err := ConvertAnthropicToOpenAI(nowFn, idGen, anthropicBody, model)
	if err != nil {
		return []byte(`{"error":{"message":"failed to convert anthropic response","type":"upstream_error"}}`)
	}
	return ConvertChatToResponses(idGen, chatBody, model, wantReasoning, nil, nil, nil)
}
