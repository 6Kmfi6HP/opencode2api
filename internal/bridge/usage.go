package bridge

import (
	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// ClaudeUsage is the Anthropic Messages usage shape. The bridge re-exports the
// domain type so usage builders share one definition with the app layer.
type ClaudeUsage = domain.ClaudeUsage

// NormalizeFinishReason maps Anthropic stop reasons onto the closed set used
// by Chat Completions. (Moved from chat_protocol.go.)
func NormalizeFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "stop":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "tool_use", "tool_calls", "function_call":
		return "tool_calls"
	case "refusal", "content_filter":
		return "content_filter"
	default:
		return reason
	}
}

// AnthropicUsageToChat converts an Anthropic usage map to the Chat
// Completions usage shape. (Moved from chat_protocol.go.)
func AnthropicUsageToChat(usage map[string]any) map[string]any {
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
		if p, pok := NumberAsFloat(out["prompt_tokens"]); pok {
			if c, cok := NumberAsFloat(out["completion_tokens"]); cok {
				out["total_tokens"] = p + c
			}
		}
	}
	// Anthropic 缓存读/写 token 顶层键透传,并同时归入 chat 约定位置
	// prompt_tokens_details.cached_tokens（对齐 sub2api 对 Chat Completions
	// usage 的形状;原有顶层键透传保留,不改已有调用方行为）。
	if v, ok := NumberAsFloat(usage["cache_read_input_tokens"]); ok {
		details, _ := out["prompt_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
		}
		if existing, eok := NumberAsFloat(details["cached_tokens"]); !eok || existing == 0 {
			details["cached_tokens"] = v
		}
		out["prompt_tokens_details"] = details
	}
	// 上游发 output_tokens_details.thinking_tokens 时归位到 chat 的
	// completion_tokens_details.reasoning_tokens。
	if outDetails, ok := usage["output_tokens_details"].(map[string]any); ok {
		if v, ok := NumberAsFloat(outDetails["thinking_tokens"]); ok && v > 0 {
			details, _ := out["completion_tokens_details"].(map[string]any)
			if details == nil {
				details = map[string]any{}
			}
			if existing, eok := NumberAsFloat(details["reasoning_tokens"]); !eok || existing == 0 {
				details["reasoning_tokens"] = v
			}
			out["completion_tokens_details"] = details
		}
	}
	delete(out, "input_tokens")
	delete(out, "output_tokens")
	return out
}

// MergeUsage merges an incremental usage map into the accumulated table (new
// values overwrite old, unknown keys are preserved). (Moved from
// anthropic_upstream.go.)
func MergeUsage(full map[string]any, delta map[string]any) {
	for k, v := range delta {
		full[k] = v
	}
}

// MergeUsageMaps deep-merges src over dst: src fields overwrite dst fields
// (including 0). Nested maps are recursively merged. Fields absent from src
// are retained. (Moved from claude.go.)
func MergeUsageMaps(dst any, src map[string]any) map[string]any {
	if src == nil {
		if dm, ok := dst.(map[string]any); ok {
			return dm
		}
		return nil
	}
	var result map[string]any
	if dm, ok := dst.(map[string]any); ok {
		result = make(map[string]any, len(dm))
		for k, v := range dm {
			result[k] = v
		}
	} else {
		result = map[string]any{}
	}
	for k, v := range src {
		if existing, ok := result[k]; ok {
			if srcMap, ok := v.(map[string]any); ok {
				if existing != nil {
					result[k] = MergeUsageMaps(existing, srcMap)
					continue
				}
			}
		}
		result[k] = v
	}
	return result
}

// usageIntField reads an integer usage field, reporting presence. (Moved from
// claude.go.)
func usageIntField(fields map[string]any, key string) (int, bool) {
	if fields == nil {
		return 0, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return 0, false
	}
	return int(ToFloat64(value)), true
}

// usageMapField reads a nested-map usage field, reporting presence and type.
// (Moved from claude.go.)
func usageMapField(fields map[string]any, key string) (map[string]any, bool) {
	if fields == nil {
		return nil, false
	}
	value, ok := fields[key]
	if !ok || value == nil {
		return nil, false
	}
	mapped, ok := value.(map[string]any)
	return mapped, ok
}

// BuildClaudeUsageCore builds the core Claude usage map from an upstream
// (Chat/Responses) usage map. (Moved from claude.go.)
func BuildClaudeUsageCore(upstreamUsage map[string]any) ClaudeUsage {
	if len(upstreamUsage) == 0 {
		return nil
	}

	usage := ClaudeUsage{}
	// readFromSplit marks cache_read sourced from DeepSeek/OpenAI-style
	// counters (prompt_cache_hit_tokens / prompt_tokens_details.cached_tokens),
	// whose prompt_tokens includes the hit portion. An Anthropic-style
	// cache_read_input_tokens is already exclusive of input_tokens and must
	// not be subtracted.
	readFromSplit := false
	if value, ok := usageIntField(upstreamUsage, "prompt_tokens"); ok {
		usage["input_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "input_tokens"); ok {
		if _, exists := usage["input_tokens"]; !exists {
			usage["input_tokens"] = value
		}
	}
	if value, ok := usageIntField(upstreamUsage, "completion_tokens"); ok {
		usage["output_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "output_tokens"); ok {
		if _, exists := usage["output_tokens"]; !exists {
			usage["output_tokens"] = value
		}
	}
	if value, ok := usageIntField(upstreamUsage, "cache_creation_input_tokens"); ok {
		usage["cache_creation_input_tokens"] = value
	}
	if value, ok := usageIntField(upstreamUsage, "cache_read_input_tokens"); ok {
		usage["cache_read_input_tokens"] = value
	} else if promptDetails, ok := usageMapField(upstreamUsage, "prompt_tokens_details"); ok {
		if value, ok := usageIntField(promptDetails, "cached_tokens"); ok {
			usage["cache_read_input_tokens"] = value
			readFromSplit = true
		}
	}
	// DeepSeek-style counters split the prompt into hit (read) and miss
	// (ordinary input). Miss is not a cache write, so it is intentionally
	// left out of the Claude cache fields.
	if _, exists := usage["cache_read_input_tokens"]; !exists {
		if value, ok := usageIntField(upstreamUsage, "prompt_cache_hit_tokens"); ok {
			usage["cache_read_input_tokens"] = value
			readFromSplit = true
		}
	}
	// Anthropic semantics: input_tokens excludes cache reads (input, read and
	// creation are mutually exclusive). prompt_tokens from DeepSeek/OpenAI
	// includes the hit portion, so subtract it here; otherwise a client that
	// prices input and cache reads separately would bill the hit tokens twice.
	if readFromSplit {
		if read, ok := usage["cache_read_input_tokens"].(int); ok && read > 0 {
			if input, ok := usage["input_tokens"].(int); ok {
				if read >= input {
					usage["input_tokens"] = 0
				} else {
					usage["input_tokens"] = input - read
				}
			}
		}
	}
	if outputDetails, ok := usageMapField(upstreamUsage, "output_tokens_details"); ok {
		usage["output_tokens_details"] = outputDetails
	} else if outputDetails, ok := usageMapField(upstreamUsage, "completion_tokens_details"); ok {
		usage["output_tokens_details"] = outputDetails
	}
	if serverToolUse, ok := usageMapField(upstreamUsage, "server_tool_use"); ok {
		usage["server_tool_use"] = serverToolUse
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

// BuildClaudeMessageUsage builds the full (non-streaming) Claude usage map.
// (Moved from claude.go.)
func BuildClaudeMessageUsage(upstreamUsage map[string]any) ClaudeUsage {
	usage := BuildClaudeUsageCore(upstreamUsage)
	if usage == nil {
		usage = ClaudeUsage{}
	}
	if cacheCreation, ok := usageMapField(upstreamUsage, "cache_creation"); ok {
		usage["cache_creation"] = cacheCreation
	}
	if serviceTier, ok := upstreamUsage["service_tier"].(string); ok && serviceTier != "" {
		usage["service_tier"] = serviceTier
	}
	if inferenceGeo, ok := upstreamUsage["inference_geo"].(string); ok && inferenceGeo != "" {
		usage["inference_geo"] = inferenceGeo
	}
	if _, exists := usage["input_tokens"]; !exists {
		usage["input_tokens"] = 0
	}
	if _, exists := usage["output_tokens"]; !exists {
		usage["output_tokens"] = 0
	}
	return usage
}

// BuildClaudeDeltaUsage builds the streaming-delta Claude usage map. (Moved
// from claude.go.)
func BuildClaudeDeltaUsage(upstreamUsage map[string]any) ClaudeUsage {
	usage := BuildClaudeUsageCore(upstreamUsage)
	if usage == nil {
		usage = ClaudeUsage{}
	}
	if _, exists := usage["output_tokens"]; !exists {
		usage["output_tokens"] = 0
	}
	return usage
}

// ResponsesUsageToChat converts a Responses usage map to the chat usage
// shape, reusing the cache parsing of buildClaudeMessageUsage. (Moved from
// claude_responses.go.)
func ResponsesUsageToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	out := map[string]any{}
	if v, ok := usage["input_tokens"]; ok {
		out["prompt_tokens"] = v
	} else if v, ok := usage["prompt_tokens"]; ok {
		out["prompt_tokens"] = v
	}
	if v, ok := usage["output_tokens"]; ok {
		out["completion_tokens"] = v
	} else if v, ok := usage["completion_tokens"]; ok {
		out["completion_tokens"] = v
	}
	if v, ok := usage["total_tokens"]; ok {
		out["total_tokens"] = v
	} else if pt, ok := NumberAsFloat(out["prompt_tokens"]); ok {
		if ct, ok := NumberAsFloat(out["completion_tokens"]); ok {
			// 上游缺 total_tokens 时由分量合成，避免下游/统计丢总量。
			out["total_tokens"] = pt + ct
		}
	}
	// 透传缓存与细节字段，BuildClaudeUsageCore 会识别标准键。
	for _, k := range []string{"prompt_tokens_details", "completion_tokens_details", "input_tokens_details", "output_tokens_details", "cache_creation_input_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens", "server_tool_use", "service_tier"} {
		if v, ok := usage[k]; ok {
			out[k] = v
		}
	}
	return out
}

// ResponsesUsageToChatBridge layers the chat-bridge usage shape on top of
// ResponsesUsageToChat: DeepSeek prompt_cache_hit/miss_tokens passthrough,
// input_tokens_details.cached_tokens → prompt_tokens_details.cached_tokens
// aliasing, and output_tokens_details.reasoning_tokens/thinking_tokens
// relocation. (Moved from chat_to_responses_upstream.go.)
func ResponsesUsageToChatBridge(usage map[string]any) map[string]any {
	out := ResponsesUsageToChat(usage)
	if out == nil {
		return nil
	}
	// total 兜底合成（ResponsesUsageToChat 也合成;这里防调用方绕过）。
	if _, has := out["total_tokens"]; !has {
		if p, pok := NumberAsFloat(out["prompt_tokens"]); pok {
			if c, cok := NumberAsFloat(out["completion_tokens"]); cok {
				out["total_tokens"] = p + c
			}
		}
	}
	// DeepSeek 缓存命中/未命中键透传。
	for _, k := range []string{"prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
		if v, ok := usage[k]; ok {
			out[k] = v
		}
	}
	// cached_tokens 别名:input_tokens_details.cached_tokens ↔
	// prompt_tokens_details.cached_tokens（先有谁用谁,另一形态同步出来）。
	cached := 0.0
	hasCached := false
	if d, ok := usage["input_tokens_details"].(map[string]any); ok {
		if v, ok := NumberAsFloat(d["cached_tokens"]); ok {
			cached = v
			hasCached = true
		}
	}
	if !hasCached {
		if d, ok := out["prompt_tokens_details"].(map[string]any); ok {
			if v, ok := NumberAsFloat(d["cached_tokens"]); ok {
				cached = v
				hasCached = true
			}
		}
	}
	if hasCached {
		details, _ := out["prompt_tokens_details"].(map[string]any)
		if details == nil {
			details = map[string]any{}
		}
		details["cached_tokens"] = cached
		out["prompt_tokens_details"] = details
	}
	if od, ok := usage["output_tokens_details"].(map[string]any); ok {
		var reasoning float64
		var hasReasoning bool
		if v, ok := NumberAsFloat(od["reasoning_tokens"]); ok {
			reasoning, hasReasoning = v, true
		} else if v, ok := NumberAsFloat(od["thinking_tokens"]); ok {
			reasoning, hasReasoning = v, true
		}
		if hasReasoning {
			details, _ := out["completion_tokens_details"].(map[string]any)
			if details == nil {
				details = map[string]any{}
			}
			if existing, eok := NumberAsFloat(details["reasoning_tokens"]); !eok || existing == 0 {
				details["reasoning_tokens"] = reasoning
			}
			out["completion_tokens_details"] = details
		}
	}
	return out
}
