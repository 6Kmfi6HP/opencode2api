package app

import (
	"encoding/json"
	"log/slog"
	"os"
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
	return os.WriteFile(path, data, 0644)
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
	}
	socks5PaidDirect = cfg.Socks5PaidDirect
	socks5Mu.Unlock()

	if cfg.PromptCacheRetention != "" {
		promptCacheRetention = cfg.PromptCacheRetention
	}
	if cfg.CacheControlBreakpoints != nil {
		cacheBreakpoints = *cfg.CacheControlBreakpoints
	}

}

func resolveModel(model string) string {
	m := strings.TrimSpace(model)
	configMu.RLock()
	alias, ok := modelAlias[m]
	configMu.RUnlock()
	if ok {
		return alias
	}
	// Clients see free models without the "-free" suffix from /v1/models.
	// Map the display name back to the upstream free ID when that is the only match.
	if m != "" && !isFreeModel(m) {
		freeID := m + "-free"
		if !modelExistsInCaches(m) && modelExistsInCaches(freeID) {
			return freeID
		}
	}
	return m
}

// Explicit catalog routing wins over a legacy same-name alias to the free variant.
func resolveModelForAuth(auth UpstreamAuth, model string) string {
	m := strings.TrimSpace(model)
	exactModelAvailable := false
	switch auth.Mode {
	case AuthRouteGo:
		exactModelAvailable = isModelInGoCatalog(m)
	case AuthRouteZen:
		exactModelAvailable = isModelInZenCatalog(m)
	}
	if m != "" && exactModelAvailable {
		configMu.RLock()
		alias, ok := modelAlias[m]
		configMu.RUnlock()
		if ok && strings.TrimSpace(alias) == m+"-free" {
			return m
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

// rejectsCacheControl reports whether a resolved upstream model is known to
// reject the Anthropic-style cache_control field (GLM/Zhipu refuse unknown
// top-level fields with "Extra inputs are not permitted").
func rejectsCacheControl(modelID string) bool {
	name := strings.ToLower(strings.TrimSpace(modelID))
	return strings.HasPrefix(name, "glm") || strings.HasPrefix(name, "zhipu") || strings.HasPrefix(name, "z-ai") || strings.HasPrefix(name, "zai")
}
