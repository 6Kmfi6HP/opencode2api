package bridge

import (
	"encoding/json"
	"strings"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// ======================== Thinking/Reasoning 判断 ========================
//
// Pure thinking/reasoning decision functions, moved out of internal/app. None
// of these read the global config; every externally-derived input arrives as a
// value parameter so the bridge layer stays pure. The strings/json imports back
// the trivially-pure helpers; OpenAIRequest comes from internal/domain.

// IsThinkingEnabled reports whether an Anthropic-style `thinking` field enables
// reasoning. (Moved from app/chat.go.)
func IsThinkingEnabled(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		// Claude Code sends adaptive thinking with --effort / CLAUDE_CODE_EFFORT_LEVEL.
		return t == "enabled" || t == "adaptive"
	case bool:
		return v
	default:
		return false
	}
}

// EffortFromOutputConfig reads Claude Code's output_config.effort
// (set by --effort / CLAUDE_CODE_EFFORT_LEVEL). (Moved from app/chat.go.)
func EffortFromOutputConfig(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	effort, _ := m["effort"].(string)
	return strings.TrimSpace(effort)
}

// IsThinkingDisabled reports whether an Anthropic-style `thinking` field
// disables reasoning. (Moved from app/chat.go.)
func IsThinkingDisabled(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		return t == "disabled"
	case bool:
		return !v
	default:
		return false
	}
}

// BuildUpstreamThinking preserves budget_tokens / effort fields when present.
// (Moved from app/chat.go.)
func BuildUpstreamThinking(value any) map[string]any {
	out := map[string]any{"type": "enabled"}
	m, ok := value.(map[string]any)
	if !ok {
		return out
	}
	for _, key := range []string{"budget_tokens", "effort"} {
		if v, exists := m[key]; exists && v != nil {
			out[key] = v
		}
	}
	return out
}

// ThinkingBudgetToEffort maps an Anthropic-style thinking budget_tokens value
// onto an OpenAI-compatible reasoning effort tier. Shared by
// ReasoningEffortFromThinking and the claude->responses path. (Moved from
// app/claude_responses.go.)
func ThinkingBudgetToEffort(budget float64) string {
	switch {
	case budget <= 0:
		return ""
	case budget < 2048:
		return "low"
	case budget < 8192:
		return "medium"
	case budget < 16384:
		return "high"
	default:
		return "xhigh"
	}
}

// ReasoningEffortFromThinking maps an Anthropic-style thinking object onto an
// OpenAI-compatible reasoning_effort when the client did not set one
// explicitly. An explicit "effort" string wins; otherwise the shared
// ThinkingBudgetToEffort tiers budget_tokens. (Moved from app/chat.go.)
func ReasoningEffortFromThinking(value any) string {
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if effort, ok := m["effort"].(string); ok && effort != "" {
		return effort
	}
	var budget float64
	switch v := m["budget_tokens"].(type) {
	case float64:
		budget = v
	case int:
		budget = float64(v)
	case int64:
		budget = float64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return ""
		}
		budget = f
	default:
		return ""
	}
	return ThinkingBudgetToEffort(budget)
}

// WantsReasoning reports whether the request should carry reasoning upstream.
// forceDisableThreads the operator's ForceDisableThinking in as a value so the
// bridge never reads config. (Moved from app/chat.go.)
func WantsReasoning(forceDisable bool, req *domain.OpenAIRequest) bool {
	if forceDisable {
		return false
	}
	if IsThinkingDisabled(req.Thinking) {
		return false
	}
	if IsThinkingEnabled(req.Thinking) {
		return true
	}
	if req.ExtraBody != nil {
		if IsThinkingDisabled(req.ExtraBody["thinking"]) {
			return false
		}
		if IsThinkingEnabled(req.ExtraBody["thinking"]) {
			return true
		}
	}
	return true
}

// MappedReasoningEffort returns the configured reasoning-effort mapping for
// in, or in unchanged when no mapping is configured. effortMap threads the
// operator's ReasoningEffortMap in as a value. (Moved from app/log_fields.go.)
func MappedReasoningEffort(effortMap map[string]string, in string) string {
	if in == "" {
		return ""
	}
	if mapped, ok := effortMap[in]; ok {
		return mapped
	}
	return in
}

// EffortToThinkingBudget 把 reasoning_effort 映射为 Anthropic thinking 预算。
// (Moved from app/chat_to_anthropic.go.)
func EffortToThinkingBudget(effort string) int {
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

// NormalizeResponsesEffort 把 effort 归一化到上游白名单。
// max->xhigh（最接近的高档），none/"" /未知->""（省略 reasoning 字段）。
// (Moved from app/claude_responses.go.)
func NormalizeResponsesEffort(effort string) string {
	effort = strings.TrimSpace(effort)
	switch effort {
	case "":
		return ""
	case "none":
		return ""
	case "minimal", "low", "medium", "high", "xhigh":
		return effort
	case "max":
		return "xhigh"
	default:
		return ""
	}
}
