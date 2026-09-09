package app

import (
	_ "embed"
	"encoding/json"
	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	"github.com/6Kmfi6HP/opencode2api/internal/stats"
	"log/slog"
	"net/http"
)

// ======================== Admin 管理页面 ========================

func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	refreshOCSession()
	fetched, err := fetchModels()
	if err == nil && len(fetched) > 0 {
		modelMu.Lock()
		modelsCache = fetched
		modelsLoaded = true
		modelMu.Unlock()
		slog.Info("free models refreshed", "count", len(fetched))
	}
	goFetched, goErr := fetchGoModels()
	if goErr == nil && len(goFetched) > 0 {
		modelMu.Lock()
		goModelsCache = goFetched
		modelMu.Unlock()
		slog.Info("go catalog refreshed", "count", len(goFetched))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"session": ocSessionID,
		"free":    len(modelsCache),
		"go":      len(goModelsCache),
	})

}

func adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snap := config.Get()
		configMu.RLock()
		cfg := AppConfig{ModelAlias: modelAliasRules}
		configMu.RUnlock()
		cfg.ReasoningEffortMap = snap.ReasoningEffortMap
		cfg.ForceDisableThinking = snap.ForceDisableThinking
		cfg.MaxTokensCap = snap.MaxTokensCap
		cfg.MaxTokensCapPerModel = snap.MaxTokensCapPerModel
		promptCacheRetentionRT := snap.PromptCacheRetention
		cacheBreakpointsRT := snap.CacheBreakpoints
		textOnlyModelsRT := append([]string(nil), snap.TextOnlyModels...)
		socks5Mu.RLock()
		cfg.Socks5Proxies = socks5Proxies
		cfg.ActiveSocks5 = activeSocks5
		cfg.Socks5PaidDirect = socks5PaidDirect
		cfg.UpstreamBaseURLs = upstreamBaseURLs
		socks5StickyRT := socks5Sticky
		socks5Mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model_alias":               cfg.ModelAlias,
			"reasoning_effort_map":      cfg.ReasoningEffortMap,
			"force_disable_thinking":    cfg.ForceDisableThinking,
			"max_tokens_cap":            cfg.MaxTokensCap,
			"max_tokens_cap_per_model":  cfg.MaxTokensCapPerModel,
			"socks5_proxies":            cfg.Socks5Proxies,
			"active_socks5":             cfg.ActiveSocks5,
			"socks5_paid_direct":        cfg.Socks5PaidDirect,
			"upstream_base_urls":        cfg.UpstreamBaseURLs,
			"prompt_cache_retention":    promptCacheRetentionRT,
			"cache_control_breakpoints": cacheBreakpointsRT,
			"socks5_sticky":             socks5StickyRT,
			"text_only_models":          textOnlyModelsRT,
			"log_level":                 logging.LevelString(),
			"log_bodies":                logging.BodiesEnabled(),
		})
	case http.MethodPost:
		var payload struct {
			AppConfig
			LogLevel  *string `json:"log_level,omitempty"`
			LogBodies *bool   `json:"log_bodies,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := saveConfig(configPath, payload.AppConfig); err != nil {
			http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(payload.AppConfig)
		if payload.LogLevel != nil {
			logging.SetLevelString(*payload.LogLevel)
		}
		if payload.LogBodies != nil {
			logging.SetBodies(*payload.LogBodies)
		}
		if debugMode {
			slog.Info("config updated",
				"aliases", len(payload.ModelAlias),
				"effort_map", len(payload.ReasoningEffortMap),
				"force_disable", payload.ForceDisableThinking,
				"max_tokens_cap", payload.MaxTokensCap,
				"max_tokens_cap_per_model", len(payload.MaxTokensCapPerModel),
				"log_level", logging.LevelString(),
				"log_bodies", logging.BodiesEnabled(),
			)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func adminStatsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// /api/stats is the canonical observation point for the user: read the
		// latest stats file directly so it reflects writes from all binary
		// instances that share this file (long-running server plus short-lived
		// `opencode2api launch claude|codex` proxies). The in-memory snapshot
		// is used only when the file is unreadable.
		snap, err := stats.ReadTokenStatsSnapshot()
		if err != nil {
			http.Error(w, `{"error":"read stats failed"}`, http.StatusInternalServerError)
			return
		}
		data, err := json.Marshal(snap)
		if err != nil {
			http.Error(w, `{"error":"marshal error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case http.MethodDelete:
		if err := stats.ResetTokenStats(); err != nil {
			slog.Error("failed to save cleared token stats", "path", getTokenStatsPath(), "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save token stats"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func adminPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}

func renderLoginPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminLoginHTML))
	if msg != "" {
		w.Write([]byte("<script>document.addEventListener('DOMContentLoaded',function(){var m=document.getElementById('login-msg');if(m){m.textContent='" + msg + "';m.style.display='block'}})</script>"))
	}
}

//go:embed web/login.html
var adminLoginHTML string

//go:embed web/admin.html
var adminHTML string
