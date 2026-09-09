package app

import (
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
)

// installLoggingHooks wires the app-level body-summary helpers (thinking-state
// classification and cache_control block counting) into the logging package.
// They live in the app package, so they are injected here to avoid an import
// cycle between app and logging.
func installLoggingHooks() {
	logging.SetSummaryExtras(logging.SummaryExtras{
		ThinkingState:     thinkingState,
		CacheControlCount: countCacheControlInValue,
	})
}

// authModeString maps an auth route mode to its log label.
func authModeString(mode AuthRouteMode) string {
	switch mode {
	case AuthRouteGo:
		return "go"
	case AuthRouteZen:
		return "zen"
	case AuthRouteAuto:
		return "auto"
	default:
		return "public"
	}
}

// thinkingState classifies a request's "thinking" field for the request_plan
// log summary.
func thinkingState(value any) string {
	if value == nil {
		return "absent"
	}
	if isThinkingDisabled(value) {
		return "disabled"
	}
	if m, ok := value.(map[string]any); ok {
		if t, _ := m["type"].(string); t == "adaptive" {
			return "adaptive"
		}
	}
	if isThinkingEnabled(value) {
		return "enabled"
	}
	return "present"
}

// mappedReasoningEffort returns the configured reasoning-effort mapping for
// in, or in unchanged when no mapping is configured.
func mappedReasoningEffort(in string) string {
	if in == "" {
		return ""
	}
	effortMap := config.ReasoningEffortMap()
	if mapped, ok := effortMap[in]; ok {
		return mapped
	}
	return in
}
