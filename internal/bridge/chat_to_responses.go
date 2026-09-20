package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file holds the pure Chat Completions -> Responses request-body
// construction moved out of internal/app/chat_to_responses_upstream.go. None of
// these functions perform I/O, logging, metrics, or config reads; the
// force-disable-thinking flag and max_tokens cap arrive via ConfigView, and the
// tool-call-ID generator is injected (production: internal/random hex). The app
// layer keeps same-name lowercase forwarding shells so existing *_test.go files
// compile unchanged.

// ChatMessagesToResponsesInput 把 Chat messages 转为 Responses instructions +
// input 数组。形状对齐 ClaudeMessagesToResponsesInput：system→instructions；
// user 文本→message/input_text；image_url→input_image；assistant.tool_calls→
// function_call；role=tool→function_call_output；assistant 文本→message/
// output_text。缺失的 tool_call_id / call_id 由 idGen 注入随机 hex 兜底
// （生产：internal/random，"call_"+12 hex）。
// (Moved from app/chat_to_responses_upstream.go chatMessagesToResponsesInput.)
func ChatMessagesToResponsesInput(idGen HexIDGen, messages []Message) (string, []any) {
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
						item["call_id"] = "call_" + idGen("call_", 12)
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
				callID = "call_" + idGen("call_", 12)
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

// ChatToResponsesBody 把 Chat Completions 请求转为 Responses 请求体。
// rawBody 为 nil。(Moved from app/chat_to_responses_upstream.go chatToResponsesBody.)
func ChatToResponsesBody(cv ConfigView, idGen HexIDGen, req *OpenAIRequest, modelID string, maxTokensCap int) []byte {
	return ChatToResponsesBodyWithRaw(cv, idGen, req, modelID, maxTokensCap, nil)
}

// ChatToResponsesBodyWithRaw 把 Chat Completions 请求转为 Responses 请求体。
// modelID 作为 body["model"]（原实现用形参而非 req.Model，二者在调用点相同）。
// rawBody 可选：同 ChatToAnthropicBodyWithRaw,供 ResolveMaxTokens 读
// max_completion_tokens 顶层字段。store 恒 false（对齐 sub2api 对无状态上游的
// 默认行为）；stream_options.include_usage 上游恒 true；include 合并
// reasoning.encrypted_content（去重）供未来 signature roundtrip。序列化失败
// 返回最小可用体（保持原 fallback 字节完全一致）。
// (Moved from app/chat_to_responses_upstream.go chatToResponsesBodyWithRaw.)
func ChatToResponsesBodyWithRaw(cv ConfigView, idGen HexIDGen, req *OpenAIRequest, modelID string, maxTokensCap int, rawBody map[string]any) []byte {
	instructions, input := ChatMessagesToResponsesInput(idGen, req.Messages)
	body := map[string]any{
		"model":  modelID,
		"input":  input,
		"stream": req.Stream,
		// 对零会话上游不写存储：对齐 sub2api 对无状态上游的默认行为。
		"store": false,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	if req.Stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	// max_output_tokens 统一经 ResolveMaxTokens（与 chat→anthropic 同口径）：
	// 未显式设置时不再"无 cap 就缺省",而是兜底 8192;且钳制下限 128。
	body["max_output_tokens"] = ResolveMaxTokens(rawBody, req, maxTokensCap)
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	// 注意：OpenAI Responses API 没有 stop 字段,Chat 侧入站的 stop 不向
	// 该请求体透传（透传既被上游忽略也是 spec 违例;Chat→Anthropic 方向的
	// stop_sequences 保留）。
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
		body["tool_choice"] = ClaudeToolChoiceCore(req.ToolChoice, false)
	}
	// reasoning：effort 映射；none/禁用省略。
	if !cv.ForceDisableThinking && !IsThinkingDisabled(req.Thinking) {
		effort := req.ReasoningEffort
		if effort == "" {
			effort = ReasoningEffortFromThinking(req.Thinking)
		}
		if effort != "" && effort != "none" {
			body["reasoning"] = map[string]any{"effort": MappedReasoningEffort(cv.ReasoningEffortMap, effort)}
		}
	}
	// 客户端 include（从 ExtraBody / rawBody 顶层）先落入 body,再与
	// reasoning.encrypted_content 合并去重;非法形状忽略。
	if inc, ok := rawBody["include"].([]any); ok {
		body["include"] = inc
	} else if inc, ok := ExtraBodyValue(req, "include").([]any); ok {
		body["include"] = inc
	}
	// include 合并 reasoning.encrypted_content（客户端已给 include 数组时
	// 去重追加,否则新建数组）——为未来 signature roundtrip 做准备。当前
	// 本方向响应侧还不读 encrypted_content（槽位由 Worker C 在响应侧加）。
	MergeResponsesIncludeKey(body, "reasoning.encrypted_content")
	b, err := json.Marshal(body)
	if err != nil {
		return []byte(fmt.Sprintf(`{"model":%q,"input":[],"stream":%t}`, modelID, req.Stream))
	}
	return b
}

// MergeResponsesIncludeKey 把 key 合并进 Responses 请求体的顶层 include
// 数组：存在则去重追加,不存在则新建。非法形状（非数组）视为不存在。
// (Moved from app/chat_to_responses_upstream.go mergeResponsesIncludeKey.)
func MergeResponsesIncludeKey(body map[string]any, key string) {
	if key == "" {
		return
	}
	existing, _ := body["include"].([]any)
	for _, e := range existing {
		if s, _ := e.(string); s == key {
			body["include"] = existing
			return
		}
	}
	body["include"] = append(existing, key)
}
