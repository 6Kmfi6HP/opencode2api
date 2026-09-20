package bridge

import (
	"encoding/json"
	"strings"
)

// This file holds the pure Chat Completions response/stream-chunk cleaners
// moved out of internal/app/chat.go. They strip private roundtrip fields,
// nulls and empty strings, promote misplaced reasoning_content, and gate the
// client-facing usage chunk. None perform I/O, logging, metrics, or config
// reads; the only injected dependency is the IDGen used to normalize response
// IDs. The app layer keeps same-name lowercase forwarding shells so existing
// *_test.go files compile unchanged.

// CleanNulls deletes nil and empty-string values from a message map.
// (Moved from app/chat.go cleanNulls.)
func CleanNulls(m map[string]any) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			delete(m, k)
		}
	}
}

// PromoteMisplacedReasoning promotes a stray reasoning_content field to
// content when there is no content and no tool calls. Reasoning that precedes
// tool calls is left alone when keepReasoning is true. Reports whether the
// promotion happened.
// (Moved from app/chat.go promoteMisplacedReasoning.)
func PromoteMisplacedReasoning(fields map[string]any, keepReasoning bool) bool {
	rc, _ := fields["reasoning_content"].(string)
	if rc == "" {
		return false
	}
	if raw, ok := fields["tool_calls"]; ok && raw != nil {
		if arr, ok := raw.([]any); ok && len(arr) > 0 {
			return false
		}
	}
	content, _ := fields["content"].(string)
	if content != "" {
		return false
	}
	if keepReasoning {
		// Preserve CoT for thinking blocks / clients that read reasoning_content.
		return false
	}
	fields["content"] = rc
	delete(fields, "reasoning_content")
	return true
}

// CleanStreamDelta cleans a single streaming delta: promotes misplaced
// reasoning, drops nil/empty content, and gates reasoning_content on
// keepReasoning. (Moved from app/chat.go cleanStreamDelta.)
func CleanStreamDelta(delta map[string]any, keepReasoning bool) {
	_ = PromoteMisplacedReasoning(delta, keepReasoning)
	if v, ok := delta["content"]; ok && v == nil {
		delete(delta, "content")
	}
	if s, ok := delta["content"].(string); ok && s == "" {
		delete(delta, "content")
	}
	if !keepReasoning {
		delete(delta, "reasoning_content")
	} else {
		if v, ok := delta["reasoning_content"]; ok && v == nil {
			delete(delta, "reasoning_content")
		}
		if s, ok := delta["reasoning_content"].(string); ok && s == "" {
			delete(delta, "reasoning_content")
		}
	}
	if s, ok := delta["role"].(string); ok && s == "" {
		delete(delta, "role")
	}
}

// ClientStreamUsageWanted 解析客户端原始请求体中的
// stream_options.include_usage（顶层或 extra_body 扩展域），决定网关是否把
// 上游 include_usage=true 产出的 usage chunk 透传给客户端。
// (Moved from app/chat.go clientStreamUsageWanted.)
func ClientStreamUsageWanted(body []byte) bool {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return false
	}
	if so, ok := raw["stream_options"].(map[string]any); ok {
		if v, ok := so["include_usage"].(bool); ok && v {
			return true
		}
	}
	if eb, ok := raw["extra_body"].(map[string]any); ok {
		if so, ok := eb["stream_options"].(map[string]any); ok {
			if v, ok := so["include_usage"].(bool); ok && v {
				return true
			}
		}
	}
	return false
}

// ConvertStreamChunkWithUsage 转换流式 chunk，并在同一次解析中顺带返回 usage。
// 注意：流循环仍会为流统计单独解析一次 chunk；这里的 "顺带提取" 只是免去了
// usage 的第三次解析。clientWantsUsage=false 时丢弃只含 usage 且 choices 为空
// 的 chunk：那是网关为流统计向上游强制 include_usage=true 产出的，客户端未
// 请求就不该收到。idGen 注入用于归一 chunk 里的响应 id。
// (Moved from app/chat.go convertStreamChunkWithUsage.)
func ConvertStreamChunkWithUsage(idGen IDGen, line string, keepReasoning, clientWantsUsage bool) (string, map[string]any) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "data: [DONE]" || trimmed == "[DONE]" {
		return line, nil
	}
	if !strings.HasPrefix(line, "data: ") {
		return line, nil
	}
	data := line[6:]
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return line, nil
	}

	// 提取 usage
	var usage map[string]any
	if u, ok := raw["usage"].(map[string]any); ok {
		usage = u
	}

	choices, ok := raw["choices"].([]any)
	if !ok || len(choices) == 0 {
		// Chat Completions deliberately uses an empty choices array for the
		// terminal usage chunk. 客户端未请求 include_usage 时抑制它。
		if usage != nil && !clientWantsUsage {
			return "", usage
		}
		if id, ok := raw["id"].(string); ok && id != "" {
			raw["id"] = NormalizeChatResponseID(idGen, id)
		}
		delete(raw, "cost")
		converted, err := json.Marshal(raw)
		if err != nil {
			return line, usage
		}
		return "data: " + string(converted), usage
	}
	for i, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			CleanStreamDelta(delta, keepReasoning)
			choice["delta"] = delta
		}
		if msg, ok := choice["message"].(map[string]any); ok {
			CleanNulls(msg)
			PromoteMisplacedReasoning(msg, keepReasoning)
			if !keepReasoning {
				delete(msg, "reasoning_content")
			}
			delete(msg, "_opencode2api_anthropic_content")
			choice["message"] = msg
		}
		if v, ok := choice["logprobs"]; ok && v == nil {
			delete(choice, "logprobs")
		}
		if v, ok := choice["finish_reason"]; ok && v == nil {
			delete(choice, "finish_reason")
		}
		if s, ok := choice["finish_reason"].(string); ok && s == "" {
			delete(choice, "finish_reason")
		}
		choices[i] = choice
	}
	raw["choices"] = choices
	if v, ok := raw["usage"]; ok && v == nil {
		delete(raw, "usage")
	}
	if id, ok := raw["id"].(string); ok && id != "" {
		raw["id"] = NormalizeChatResponseID(idGen, id)
	}
	delete(raw, "cost")
	converted, err := json.Marshal(raw)
	if err != nil {
		return line, usage
	}
	return "data: " + string(converted), usage
}

// ConvertResponse cleans a complete (non-streaming) Chat Completions body:
// normalizes the id, cleans each choice's message (nulls, misplaced reasoning,
// reasoning gate, private roundtrip field strip), drops nil logprobs and the
// cost field. Un-marshalable input is returned unchanged with a nil error so
// the gateway passes upstream error bodies through untouched — the pure layer
// cannot log, so the previous slog.Warn is intentionally dropped here (the
// app shell observes the unchanged-bytes pass-through as before).
// (Moved from app/chat.go convertResponse.)
func ConvertResponse(idGen IDGen, data []byte, keepReasoning bool) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return data, nil
	}
	if id, ok := raw["id"].(string); ok && id != "" {
		raw["id"] = NormalizeChatResponseID(idGen, id)
	}
	if choices, ok := raw["choices"].([]any); ok {
		for i, c := range choices {
			if choice, ok := c.(map[string]any); ok {
				if msg, ok := choice["message"].(map[string]any); ok {
					CleanNulls(msg)
					PromoteMisplacedReasoning(msg, keepReasoning)
					if !keepReasoning {
						delete(msg, "reasoning_content")
					}
					// Strip private Anthropic roundtrip field so it never
					// leaks to Chat Completions consumers.
					delete(msg, "_opencode2api_anthropic_content")
					choice["message"] = msg
				}
				if v, ok := choice["logprobs"]; ok && v == nil {
					delete(choice, "logprobs")
				}
				choices[i] = choice
			}
		}
		raw["choices"] = choices
	}
	delete(raw, "cost")
	return json.Marshal(raw)
}
