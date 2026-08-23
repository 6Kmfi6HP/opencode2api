package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// ======================== 配置 ========================

var (
	port                 string
	configPath           = "config.json"
	modelAlias           = map[string]string{}
	reasoningEffortMap   = map[string]string{}
	forceDisableThinking bool
	maxTokensCap         int
	maxTokensCapPerModel = map[string]int{}
	promptCacheRetention string // "" -> runtime default "24h"; "off" disables injection
	cacheBreakpoints     = true
	textOnlyModels       = []string{"deepseek"} // default: text-only upstreams
	debugMode            bool
	configMu             sync.RWMutex
	storedResponses      = map[string]StoredResponseState{}
	storedResponsesMu    sync.RWMutex
)

// ======================== 配置管理 ========================

func loadConfig(path string) AppConfig {
	var cfg AppConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Warn("config parse failed", "error", err)
	}
	return cfg
}

func saveConfig(path string, cfg AppConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// The default fallback lives in a per-user directory that may not exist
	// yet; create it only when persisting configuration.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func applyConfig(cfg AppConfig) {
	configMu.Lock()
	defer configMu.Unlock()
	if cfg.ModelAlias != nil {
		modelAlias = cfg.ModelAlias
	}
	if cfg.ReasoningEffortMap != nil {
		reasoningEffortMap = cfg.ReasoningEffortMap
	}
	forceDisableThinking = cfg.ForceDisableThinking
	maxTokensCap = cfg.MaxTokensCap
	if cfg.MaxTokensCapPerModel != nil {
		maxTokensCapPerModel = cfg.MaxTokensCapPerModel
	}

	socks5Mu.Lock()
	if cfg.Socks5Proxies != nil {
		socks5Proxies = cfg.Socks5Proxies
	}
	if activeSocks5 != cfg.ActiveSocks5 {
		activeSocks5 = cfg.ActiveSocks5
		socks5Client = nil
		socks5ClientAddr = ""
		atomic.StoreUint32(&socks5RRIndex, 0)
		// 代理配置变化后旧 sticky 绑定可能指向已不存在的出口,全部清空重建。
		stickyMu.Lock()
		stickyEntries = map[string]*stickyProxyEntry{}
		stickyMu.Unlock()
	}
	socks5PaidDirect = cfg.Socks5PaidDirect
	socks5Mu.Unlock()

	setUpstreamBaseURLs(cfg.UpstreamBaseURLs)

	socks5Sticky = true
	if cfg.Socks5Sticky != nil {
		socks5Sticky = *cfg.Socks5Sticky
	}

	if cfg.PromptCacheRetention != "" {
		promptCacheRetention = cfg.PromptCacheRetention
	}
	if cfg.CacheControlBreakpoints != nil {
		cacheBreakpoints = *cfg.CacheControlBreakpoints
	}
	if cfg.TextOnlyModels != nil {
		textOnlyModels = cfg.TextOnlyModels
	}

}

// stripContextSuffix splits a model ID into its base and context suffix.
// A model ID like "deepseek-v4-flash[1m]" yields base="deepseek-v4-flash"
// and suffix="[1m]". If the ID does not end with a "[...]" bracket suffix,
// the returned suffix is empty and base is the trimmed input.
func stripContextSuffix(modelID string) (base, suffix string) {
	s := strings.TrimSpace(modelID)
	if idx := strings.LastIndex(s, "["); idx > 0 && strings.HasSuffix(s, "]") {
		return s[:idx], s[idx:]
	}
	return s, ""
}

func resolveModel(model string) string {
	m := strings.TrimSpace(model)
	base, suffix := stripContextSuffix(m)
	configMu.RLock()
	alias, ok := modelAlias[base]
	configMu.RUnlock()
	if ok {
		return alias + suffix
	}
	// Clients see free models without the "-free" suffix from /v1/models.
	// Map the display name back to the upstream free ID when that is the only match.
	if base != "" && !isFreeModel(base) {
		freeID := base + "-free"
		if !modelExistsInCaches(base) && modelExistsInCaches(freeID) {
			return freeID + suffix
		}
	}
	return m
}

// Explicit catalog routing wins over a legacy same-name alias to the free variant.
func resolveModelForAuth(auth UpstreamAuth, model string) string {
	m := strings.TrimSpace(model)
	base, suffix := stripContextSuffix(m)
	exactModelAvailable := false
	switch auth.Mode {
	case AuthRouteGo:
		exactModelAvailable = isModelInGoCatalog(base)
	case AuthRouteZen:
		exactModelAvailable = isModelInZenCatalog(base)
	}
	if base != "" && exactModelAvailable {
		configMu.RLock()
		alias, ok := modelAlias[base]
		configMu.RUnlock()
		if ok && strings.TrimSpace(alias) == base+"-free" {
			return base + suffix
		}
	}
	return resolveModel(m)
}

func getForceDisableThinking() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return forceDisableThinking
}

func getReasoningEffortMap() map[string]string {
	configMu.RLock()
	defer configMu.RUnlock()
	cp := make(map[string]string, len(reasoningEffortMap))
	for k, v := range reasoningEffortMap {
		cp[k] = v
	}
	return cp
}

// getMaxTokensCapForModel returns the effective max_tokens cap for the given
// model: the per-model value if set, otherwise the global default. A return
// value of 0 means no cap (max_tokens is forwarded as-is).
func getMaxTokensCapForModel(model string) int {
	configMu.RLock()
	defer configMu.RUnlock()
	if cap, ok := maxTokensCapPerModel[model]; ok {
		return cap
	}
	return maxTokensCap
}

// getPromptCacheRetention returns the retention value injected into upstream
// cache requests. "" (unset) yields the runtime default "24h" which pulls the
// zen gateway's prefix cache TTL from ~5 minutes to a day; "off" disables
// injection entirely.
func getPromptCacheRetention() string {
	configMu.RLock()
	defer configMu.RUnlock()
	if promptCacheRetention == "" {
		return "24h"
	}
	return promptCacheRetention
}

func getCacheBreakpoints() bool {
	configMu.RLock()
	defer configMu.RUnlock()
	return cacheBreakpoints
}

// isTextOnlyModel reports whether the resolved upstream model ID only accepts
// text input. Matching is case-insensitive prefix matching, so one configured
// prefix covers every variant (e.g. "deepseek" matches both
// "deepseek-v4-flash" and "deepseek-v4-flash-free"). When a request resolves
// to a text-only model, multimodal image/document parts are downgraded to text
// annotations instead of being forwarded upstream.
func isTextOnlyModel(modelID string) bool {
	name := strings.ToLower(strings.TrimSpace(modelID))
	if name == "" {
		return false
	}
	configMu.RLock()
	defer configMu.RUnlock()
	for _, prefix := range textOnlyModels {
		if strings.HasPrefix(name, strings.ToLower(strings.TrimSpace(prefix))) {
			return true
		}
	}
	return false
}

// rejectsCacheControl reports whether a resolved upstream model is known to
// reject the Anthropic-style cache_control field (GLM/Zhipu refuse unknown
// top-level fields with "Extra inputs are not permitted").
func rejectsCacheControl(modelID string) bool {
	name := strings.ToLower(strings.TrimSpace(modelID))
	return strings.HasPrefix(name, "glm") || strings.HasPrefix(name, "zhipu") || strings.HasPrefix(name, "z-ai") || strings.HasPrefix(name, "zai")
}

// setUpstreamBaseURLs stores the normalized upstream base URL list. The list
// lives in httpsclient.go's socks5Mu-guarded state; here we only normalize
// and push it in, clearing sticky bindings when the set actually changed.
func setUpstreamBaseURLs(raw []string) {
	socks5Mu.Lock()
	cur := upstreamBaseURLs
	socks5Mu.Unlock()

	normalized := normalizeBaseURLs(raw)
	changed := len(normalized) != len(cur)
	if !changed {
		for i, u := range normalized {
			if u != cur[i] {
				changed = true
				break
			}
		}
	}
	socks5Mu.Lock()
	upstreamBaseURLs = normalized
	atomic.StoreUint32(&baseURLRRIndex, 0)
	socks5Mu.Unlock()
	if changed {
		stickyMu.Lock()
		stickyEntries = map[string]*stickyProxyEntry{}
		stickyMu.Unlock()
	}
}

// normalizeBaseURLs trims trailing slashes, drops blanks and duplicates.
// An empty result falls back to the default https://opencode.ai.
func normalizeBaseURLs(raw []string) []string {
	if len(raw) == 0 {
		return defaultBaseURLs
	}
	var out []string
	seen := map[string]bool{}
	for _, u := range raw {
		u = strings.TrimSpace(u)
		if u == "" || strings.HasPrefix(u, "//") {
			continue
		}
		u = strings.TrimSuffix(u, "/")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	if len(out) == 0 {
		return defaultBaseURLs
	}
	return out
}

var defaultBaseURLs = []string{"https://opencode.ai"}
