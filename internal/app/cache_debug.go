package app

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
)

// cacheDebugEnabled gates the optional cache-investigation logs. Off by
// default; set OPENCODE2API_CACHE_DEBUG=1 (or true/yes) to emit structure-only
// usage summaries (cache_read/hit counters). No request bodies or auth
// material are logged.
func cacheDebugEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("OPENCODE2API_CACHE_DEBUG")))
	return v == "1" || v == "true" || v == "yes"
}

// hashTextPrefix returns the first 16 hex chars of sha256(text) for log-only
// prefix comparison (no raw text leaked).
func hashTextPrefix(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// logCacheDebugUsage logs a compact view of the upstream usage map with the
// cache-related counters lifted out, if cache debugging is enabled.
func logCacheDebugUsage(protocol string, model string, upstreamUsage map[string]any) {
	if !cacheDebugEnabled() || upstreamUsage == nil {
		return
	}
	attrs := []any{
		"protocol", protocol,
		"model", model,
	}
	for _, k := range []string{
		"cache_read_input_tokens",
		"cache_creation_input_tokens",
		"prompt_cache_hit_tokens",
		"prompt_cache_miss_tokens",
		"input_tokens",
		"output_tokens",
		"prompt_tokens",
		"completion_tokens",
		"total_tokens",
	} {
		if v, ok := upstreamUsage[k]; ok {
			attrs = append(attrs, k, v)
		}
	}
	if pd, ok := upstreamUsage["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := pd["cached_tokens"]; ok {
			attrs = append(attrs, "prompt_cached_tokens", v)
		}
	}
	if itd, ok := upstreamUsage["input_tokens_details"].(map[string]any); ok {
		if v, ok := itd["cached_tokens"]; ok {
			attrs = append(attrs, "input_cached_tokens", v)
		}
	}
	// 上游偶发返回 Responses 原生以外的 usage 键:未知键原样打出(值已是
	// 数字计数,无隐私),避免"有缓存字段但白名单没列"时误判为零命中。
	known := map[string]bool{
		"cache_read_input_tokens": true, "cache_creation_input_tokens": true,
		"prompt_cache_hit_tokens": true, "prompt_cache_miss_tokens": true,
		"input_tokens": true, "output_tokens": true, "prompt_tokens": true,
		"completion_tokens": true, "total_tokens": true,
		"prompt_tokens_details": true, "input_tokens_details": true,
		"output_tokens_details": true,
	}
	for k, v := range upstreamUsage {
		if !known[k] {
			attrs = append(attrs, k, v)
		}
	}
	slog.Info("cache_debug_usage", attrs...)
}
