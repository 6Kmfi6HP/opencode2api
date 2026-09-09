package config

import (
	"strings"
	"sync/atomic"
)

// Snapshot is an immutable snapshot of the read-only package config fields. It
// is exchanged atomically via snapshot, so readers never touch the RWMutex-
// guarded mutable state in the app package.
type Snapshot struct {
	MaxTokensCap         int
	MaxTokensCapPerModel map[string]int
	ReasoningEffortMap   map[string]string
	ForceDisableThinking bool
	PromptCacheRetention string
	CacheBreakpoints     bool
	TextOnlyModels       []string
}

// snapshot holds the current Snapshot. All reads go through Get, which lazily
// seeds the default snapshot on first use so startup never observes a zero
// value.
var snapshot atomic.Value

// Default returns the read-only snapshot with default field values.
func Default() Snapshot {
	return Snapshot{
		MaxTokensCapPerModel: map[string]int{},
		ReasoningEffortMap:   map[string]string{},
		PromptCacheRetention: "", // "" -> runtime default "24h"; "off" disables injection
		CacheBreakpoints:     true,
		TextOnlyModels:       []string{}, // models.dev modality data drives the default
	}
}

// Get returns the current immutable config snapshot, seeding the default
// snapshot on first use.
func Get() Snapshot {
	if s, ok := snapshot.Load().(Snapshot); ok {
		return s
	}
	s := Default()
	snapshot.Store(s)
	return s
}

// Update applies fn to a copy of the current snapshot and stores the result.
// fn should replace maps/slices rather than mutate their contents. It returns
// the newly stored snapshot.
func Update(fn func(*Snapshot)) Snapshot {
	snap := Get()
	fn(&snap)
	snapshot.Store(snap)
	return snap
}

// MaxTokensCapFor returns the effective max_tokens cap for the given model:
// the per-model value if set, otherwise the global default. A return value of
// 0 means no cap (max_tokens is forwarded as-is).
func MaxTokensCapFor(model string) int {
	snap := Get()
	if cap, ok := snap.MaxTokensCapPerModel[model]; ok {
		return cap
	}
	return snap.MaxTokensCap
}

// PromptCacheRetention returns the retention value injected into upstream
// cache requests. "" (unset) yields the runtime default "24h" which pulls the
// zen gateway's prefix cache TTL from ~5 minutes to a day; "off" disables
// injection entirely.
func PromptCacheRetention() string {
	if v := Get().PromptCacheRetention; v == "" {
		return "24h"
	} else {
		return v
	}
}

// CacheBreakpoints reports whether cache_control breakpoints should be
// injected for supported models.
func CacheBreakpoints() bool {
	return Get().CacheBreakpoints
}

// ForceDisableThinking reports whether reasoning is disabled globally.
func ForceDisableThinking() bool {
	return Get().ForceDisableThinking
}

// ReasoningEffortMap returns a copy of the reasoning-effort mapping so callers
// cannot mutate the shared snapshot map.
func ReasoningEffortMap() map[string]string {
	m := Get().ReasoningEffortMap
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// IsTextOnlyModel reports whether the configured text_only_models prefixes
// mark the resolved upstream model ID as text-only. Matching is
// case-insensitive prefix matching, so one configured prefix covers every
// variant (e.g. "deepseek" matches both "deepseek-v4-flash" and
// "deepseek-v4-flash-free"). This list is a manual escape hatch on top of the
// models.dev modality data, which drives the default decision. Models judged
// text-only have their multimodal image/document parts downgraded to text
// annotations instead of being forwarded upstream.
func IsTextOnlyModel(modelID string) bool {
	name := strings.ToLower(strings.TrimSpace(modelID))
	if name == "" {
		return false
	}
	for _, prefix := range Get().TextOnlyModels {
		if strings.HasPrefix(name, strings.ToLower(strings.TrimSpace(prefix))) {
			return true
		}
	}
	return false
}
