package bridge

// ConfigView is the snapshot of operator configuration that the bridge's pure
// conversion logic depends on. The app layer resolves the global config under
// configMu and delivers a value snapshot; the bridge never reads the global
// config itself, keeping it free of any internal/config import.
//
// All fields are value-semantic: ReasoningEffortMap is treated as read-only by
// the bridge and is captured by the app once per request so concurrent
// reloads cannot mutate a map the bridge is mid-read on.
type ConfigView struct {
	// MaxTokensCap caps any client-supplied max_tokens / max_output_tokens.
	// Zero or negative means uncapped.
	MaxTokensCap int
	// ForceDisableThinking strips/clamps reasoning regardless of the client's
	// thinking / reasoning_effort request.
	ForceDisableThinking bool
	// ReasoningEffortMap rewrites a client reasoning_effort value (key) to the
	// operator-preferred value (value). Empty/nil map = identity.
	ReasoningEffortMap map[string]string
	// PromptCacheRetention selects the upstream prompt-cache retention policy
	// ("" = upstream default).
	PromptCacheRetention string
	// CacheBreakpoints enables Anthropic-style cache_control breakpoints on the
	// request body.
	CacheBreakpoints bool
	// RejectsCacheControl reports whether the resolved upstream model is known to
	// reject the Anthropic-style cache_control field (GLM/Zhipu refuse unknown
	// top-level fields). The app resolves this from the model ID
	// (rejectsCacheControl) and injects the verdict so the bridge never reads the
	// model-classification table itself.
	RejectsCacheControl bool
	// TextOnly restricts models to text-only modality (strip multimodal parts).
	// The app resolves this from the model ID (config.IsTextOnlyModel ||
	// modelsdev.IsTextOnly) and injects the verdict.
	TextOnly bool
}
