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
	slog.Info("cache_debug_usage", attrs...)
}
