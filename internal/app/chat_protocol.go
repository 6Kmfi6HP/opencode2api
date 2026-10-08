package app

import (
	"encoding/json"
	"fmt"
	"github.com/6Kmfi6HP/opencode2api/internal/util"
	"math"
	"net/http"
	"strings"
)

// normalizeFinishReason maps Anthropic stop reasons onto the closed set used
// by Chat Completions. Unknown reasons fall back to "stop".Chat
// finish_reason 是闭集合,透传上游新枚举会污染下游(对齐 sub2api 各映射器的
// 闭集合输出,无透传分支)。特例:
//   - model_context_window_exceeded 是截断,归 "length"(对齐 Bifrost
//     anthropicFinishReasonToBifrost 的 length 折叠),不再按 unknown 落 stop;
//   - pause_turn/compaction 表示未完结且无 chat 等价物,置空不报完成态
//     (对齐 Bifrost anthropicResponsesStatus:mapped=false → status unset)。
func normalizeFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "stop":
		return "stop"
	case "max_tokens", "length", "model_context_window_exceeded":
		return "length"
	case "tool_use", "tool_calls", "function_call":
		return "tool_calls"
	case "refusal", "content_filter":
		return "content_filter"
	case "pause_turn", "compaction":
		return ""
	default:
		return "stop"
	}
}

func anthropicUsageToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	out := make(map[string]any, len(usage)+3)
	for k, v := range usage {
		out[k] = v
	}
	if v, ok := usage["input_tokens"]; ok {
		out["prompt_tokens"] = v
	}
	if v, ok := usage["output_tokens"]; ok {
		out["completion_tokens"] = v
	}
	// 显式 total_tokens 优先（权威）;否则由 input/output 分量合成,避免下游
	// 与统计丢总量。顺序在字段重命名之后,保证 prompt/completion 任一来源
	// 的分量都能被计入。
	if _, has := out["total_tokens"]; !has {
		if p, pok := numberAsFloat(out["prompt_tokens"]); pok {
			if c, cok := numberAsFloat(out["completion_tokens"]); cok {
				out["total_tokens"] = p + c
			}
		}
	}
	// Anthropic 缓存读/写 token 顶层键透传,并同时归入 chat 约定位置
	// prompt_tokens_details.cached_tokens / .cache_creation_tokens（对齐 sub2api
	// 对 Chat Completions usage 的形状;原有顶层键透传保留,不改已有调用方行为）。
	if v, ok := numberAsFloat(usage["cache_read_input_tokens"]); ok {
		details, _ := out["prompt_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
		}
		if existing, eok := numberAsFloat(details["cached_tokens"]); !eok || existing == 0 {
			details["cached_tokens"] = v
		}
		out["prompt_tokens_details"] = details
	}
	// cache_creation_input_tokens 同样归位到 details(对齐 sub2api
	// promptDetailsFromResponses),否则按 OpenAI 形状计费/统计时写缓存 token 丢失。
	if v, ok := numberAsFloat(usage["cache_creation_input_tokens"]); ok {
		details, _ := out["prompt_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
		}
		if existing, eok := numberAsFloat(details["cache_creation_tokens"]); !eok || existing == 0 {
			details["cache_creation_tokens"] = v
		}
		out["prompt_tokens_details"] = details
	}
	// cache_creation object 的 5m/1h 明细归位到
	// prompt_tokens_details.cached_write_token_details（对齐 Bifrost
	// ChatCachedWriteTokenDetails）;扁平 cache_creation_input_tokens 缺失时以
	// 5m+1h 合成,保证计费/统计不丢写缓存分量（此前整段丢失）。
	if cc, ok := usage["cache_creation"].(map[string]any); ok {
		five, fiveOK := numberAsFloat(cc["ephemeral_5m_input_tokens"])
		one, oneOK := numberAsFloat(cc["ephemeral_1h_input_tokens"])
		if fiveOK || oneOK {
			details, _ := out["prompt_tokens_details"].(map[string]any)
			if details == nil {
				details = map[string]any{}
			}
			writeDetails, _ := details["cached_write_token_details"].(map[string]any)
			if writeDetails == nil {
				writeDetails = map[string]any{}
			}
			if fiveOK {
				if existing, eok := numberAsFloat(writeDetails["cached_write_tokens_5m"]); !eok || existing == 0 {
					writeDetails["cached_write_tokens_5m"] = five
				}
			}
			if oneOK {
				if existing, eok := numberAsFloat(writeDetails["cached_write_tokens_1h"]); !eok || existing == 0 {
					writeDetails["cached_write_tokens_1h"] = one
				}
			}
			details["cached_write_token_details"] = writeDetails
			out["prompt_tokens_details"] = details
			if _, hasFlat := numberAsFloat(usage["cache_creation_input_tokens"]); !hasFlat {
				total := 0.0
				if fiveOK {
					total += five
				}
				if oneOK {
					total += one
				}
				if total > 0 {
					// 扁平写缓存键缺失：以 5m+1h 合成,计费/统计
					// (parseCacheUsage) 只认扁平键。
					out["cache_creation_input_tokens"] = total
					details["cache_creation_tokens"] = total
				}
			}
		}
	}
	// 上游发 output_tokens_details.thinking_tokens 时归位到 chat 的
	// completion_tokens_details.reasoning_tokens。
	if outDetails, ok := usage["output_tokens_details"].(map[string]any); ok {
		if v, ok := numberAsFloat(outDetails["thinking_tokens"]); ok && v > 0 {
			details, _ := out["completion_tokens_details"].(map[string]any)
			if details == nil {
				details = map[string]any{}
			}
			if existing, eok := numberAsFloat(details["reasoning_tokens"]); !eok || existing == 0 {
				details["reasoning_tokens"] = v
			}
			out["completion_tokens_details"] = details
		}
	}
	// 上游 server_tool_use.web_search_requests（服务端 web 搜索计费）归位到
	// chat 的 tool_usage.web_search.num_requests 与
	// completion_tokens_details.num_search_queries（对齐 Bifrost
	// buildAnthropicPassthroughUsage——两处同值流出,原有顶层 server_tool_use
	// 透传保留）。
	if stu, ok := usage["server_tool_use"].(map[string]any); ok {
		if n, ok := numberAsFloat(stu["web_search_requests"]); ok && n > 0 {
			details, _ := out["completion_tokens_details"].(map[string]any)
			if details == nil {
				details = map[string]any{}
			}
			if existing, eok := numberAsFloat(details["num_search_queries"]); !eok || existing == 0 {
				details["num_search_queries"] = n
			}
			out["completion_tokens_details"] = details
			if _, has := out["tool_usage"]; !has {
				out["tool_usage"] = map[string]any{
					"web_search": map[string]any{"num_requests": n},
				}
			}
		}
	}
	delete(out, "input_tokens")
	delete(out, "output_tokens")
	return out
}

func numberAsFloat(v any) (float64, bool) { return util.NumberAsFloat(v) }

// toString converts a value to string, returning "" for non-strings.
func toString(v any) string { return util.ToString(v) }

// validateTemperature checks an optional temperature against an inclusive
// [min, max] range. nil (absent) and any in-range value (including the
// bounds and explicit zero) are accepted; negative, above-max, NaN and Inf
// values are rejected. It never clamps. An empty message means valid.
func validateTemperature(t *float64, min, max float64) string {
	if t == nil {
		return ""
	}
	v := *t
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Sprintf("temperature must be a finite number; got %v", v)
	}
	if v < min || v > max {
		return fmt.Sprintf("temperature must be between %g and %g; got %v", min, max, v)
	}
	return ""
}

// writeProtocolValidation400 writes a protocol-shaped HTTP 400 error for a
// request-side validation failure. protocol is "chat", "responses", or
// "claude". param is the offending field name (Chat/Responses only).
// Streaming requests also receive a plain JSON 400 before any SSE.
func writeProtocolValidation400(w http.ResponseWriter, protocol, param, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	switch protocol {
	case "claude":
		json.NewEncoder(w).Encode(map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "invalid_request_error",
				"message": message,
			},
		})
	default: // "chat", "responses"
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": message,
				"param":   param,
			},
		})
	}
}

// validateRequestTemperature is the shared entry point used by all three
// handlers. It returns true when the request is valid (nil or in-range) and
// writes a protocol-shaped 400 (returning false) otherwise.
func validateRequestTemperature(w http.ResponseWriter, t *float64, protocol string, min, max float64) bool {
	if msg := validateTemperature(t, min, max); msg != "" {
		writeProtocolValidation400(w, protocol, "temperature", msg)
		return false
	}
	return true
}

// applyErrorPrefix prepends the stable "Error: " marker used by the Responses
// request path when a tool_result carries is_error:true — the Responses
// function_call_output wire shape has no error field, so the text prefix is
// the only carrier there (OpenAI-compatible providers reject unknown input
// item parameters, so adding a nonstandard field risks a 400). The Anthropic
// Messages request path maps is_error onto the native tool_result.is_error
// instead of calling this (Message.IsError, see claudeToOpenAIMessages), and
// the Chat-bound tool_result content stays un-deformed. It avoids producing a
// duplicate prefix when the output text already starts with "Error:" (e.g. an
// upstream that echoes the error).
func applyErrorPrefix(text string) string {
	if strings.HasPrefix(text, "Error:") {
		return text
	}
	return "Error: " + text
}
