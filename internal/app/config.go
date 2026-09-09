package app

import (
	"encoding/json"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/domain"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// ======================== 配置 ========================

type compiledKeywordRule struct {
	rule          domain.ModelKeywordRule
	compiledRegex *regexp.Regexp
}

var (
	port              string
	configPath        = "config.json"
	modelAliasRules   = []domain.ModelKeywordRule{}
	compiledRules     = []compiledKeywordRule{}
	debugMode         bool
	configMu          sync.RWMutex
	storedResponses   = map[string]StoredResponseState{}
	storedResponsesMu sync.RWMutex
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

func compileKeywordRules(rules []domain.ModelKeywordRule) ([]domain.ModelKeywordRule, []compiledKeywordRule) {
	cleanRules := make([]domain.ModelKeywordRule, 0, len(rules))
	compiled := make([]compiledKeywordRule, 0, len(rules))
	for _, r := range rules {
		k := strings.TrimSpace(r.Keyword)
		t := strings.TrimSpace(r.Target)
		if k == "" || t == "" {
			continue
		}
		r.Keyword = k
		r.Target = t
		if r.MatchType == "" {
			r.MatchType = domain.MatchContains
		}
		cr := compiledKeywordRule{rule: r}
		if r.MatchType == domain.MatchRegex {
			pattern := r.Keyword
			if r.CaseInsensitive && !strings.HasPrefix(pattern, "(?i)") {
				pattern = "(?i)" + pattern
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				slog.Warn("invalid keyword regex rule skipped", "pattern", r.Keyword, "error", err)
				continue
			}
			cr.compiledRegex = re
		}
		cleanRules = append(cleanRules, r)
		compiled = append(compiled, cr)
	}
	return cleanRules, compiled
}

func matchKeywordRule(base string) (string, bool) {
	configMu.RLock()
	defer configMu.RUnlock()

	baseLower := strings.ToLower(base)

	for _, cr := range compiledRules {
		if !cr.rule.Enabled {
			continue
		}

		targetBase := base
		if cr.rule.CaseInsensitive {
			targetBase = baseLower
		}
		kw := cr.rule.Keyword
		if cr.rule.CaseInsensitive {
			kw = strings.ToLower(kw)
		}

		switch cr.rule.MatchType {
		case domain.MatchExact:
			if targetBase == kw {
				return cr.rule.Target, true
			}
		case domain.MatchPrefix:
			if strings.HasPrefix(targetBase, kw) {
				return cr.rule.Target, true
			}
		case domain.MatchRegex:
			if cr.compiledRegex != nil && cr.compiledRegex.MatchString(base) {
				return cr.rule.Target, true
			}
		case domain.MatchContains:
			fallthrough
		default:
			if strings.Contains(targetBase, kw) {
				return cr.rule.Target, true
			}
		}
	}
	return "", false
}

func applyConfig(cfg AppConfig) {
	configMu.Lock()
	modelAliasRules, compiledRules = compileKeywordRules(cfg.ModelAlias)
	configMu.Unlock()

	// Build a fresh snapshot, overlaying cfg onto the current value so fields
	// the config omits keep their prior value (matching the previous global
	// behavior). Maps/slices are deep-copied so callers mutating cfg later
	// cannot race readers.
	config.Update(func(s *config.Snapshot) {
		if cfg.ReasoningEffortMap != nil {
			m := make(map[string]string, len(cfg.ReasoningEffortMap))
			for k, v := range cfg.ReasoningEffortMap {
				m[k] = v
			}
			s.ReasoningEffortMap = m
		}
		s.ForceDisableThinking = cfg.ForceDisableThinking
		s.MaxTokensCap = cfg.MaxTokensCap
		if cfg.MaxTokensCapPerModel != nil {
			m := make(map[string]int, len(cfg.MaxTokensCapPerModel))
			for k, v := range cfg.MaxTokensCapPerModel {
				m[k] = v
			}
			s.MaxTokensCapPerModel = m
		}
		if cfg.PromptCacheRetention != "" {
			s.PromptCacheRetention = cfg.PromptCacheRetention
		}
		if cfg.CacheControlBreakpoints != nil {
			s.CacheBreakpoints = *cfg.CacheControlBreakpoints
		}
		if cfg.TextOnlyModels != nil {
			s.TextOnlyModels = append([]string(nil), cfg.TextOnlyModels...)
		}
	})

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
	socks5Sticky = true
	if cfg.Socks5Sticky != nil {
		socks5Sticky = *cfg.Socks5Sticky
	}
	socks5Mu.Unlock()

	setUpstreamBaseURLs(cfg.UpstreamBaseURLs)

	if cfg.NativeResponsesModels != nil {
		setNativeResponsesModels(cfg.NativeResponsesModels)
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
	if target, matched := matchKeywordRule(base); matched {
		return target + suffix
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
		if target, matched := matchKeywordRule(base); matched {
			if strings.TrimSpace(target) == base+"-free" {
				return base + suffix
			}
		}
	}
	return resolveModel(m)
}

func getModelKeywordRules() []domain.ModelKeywordRule {
	configMu.RLock()
	defer configMu.RUnlock()
	cp := make([]domain.ModelKeywordRule, len(modelAliasRules))
	copy(cp, modelAliasRules)
	return cp
}

func getModelAliasMap() map[string]string {
	configMu.RLock()
	defer configMu.RUnlock()
	m := make(map[string]string)
	for _, r := range modelAliasRules {
		if r.Enabled && r.MatchType == domain.MatchExact {
			m[r.Keyword] = r.Target
		}
	}
	return m
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
		stickyRebindSeq = 0
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
