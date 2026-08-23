package app

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// ======================== Token 统计 ========================

type ModelStats struct {
	RequestCount     int64 `json:"request_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// CacheTokens aggregates upstream prompt-cache accounting: read = tokens
	// served from cache, created = tokens written into the cache. Both are
	// optional and only present when the upstream reports them.
	CacheReadTokens    int64 `json:"cache_read_tokens,omitempty"`
	CacheCreatedTokens int64 `json:"cache_created_tokens,omitempty"`
}

type TokenStatsData struct {
	TotalRequests int64                  `json:"total_requests"`
	Models        map[string]*ModelStats `json:"models"`
}

var (
	tokenStats     = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu   sync.Mutex
	tokenStatsPath = "stats.json"
)

// setTokenStatsPath updates the file path used to persist token usage statistics.
func setTokenStatsPath(path string) {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	if path != "" {
		tokenStatsPath = path
	}
}

// getTokenStatsPath returns the currently configured token stats file path.
func getTokenStatsPath() string {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	return tokenStatsPath
}

// ======================== Token 统计 ========================

func loadTokenStats() {
	tokenStatsMu.Lock()
	path := tokenStatsPath
	tokenStatsMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var st TokenStatsData
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	tokenStatsMu.Lock()
	if st.Models == nil {
		st.Models = map[string]*ModelStats{}
	}
	tokenStats = &st
	tokenStatsMu.Unlock()
}

func saveTokenStats() error {
	tokenStatsMu.Lock()
	data, err := json.MarshalIndent(tokenStats, "", "  ")
	path := tokenStatsPath
	tokenStatsMu.Unlock()
	if err != nil {
		return fmt.Errorf("marshal token stats: %w", err)
	}

	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create stats directory %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write stats file %s: %w", path, err)
	}
	return nil
}

func recordTokenUsage(model string, promptTokens, completionTokens, totalTokens int64) {
	tokenStatsMu.Lock()
	tokenStats.TotalRequests++
	ms, ok := tokenStats.Models[model]
	if !ok {
		ms = &ModelStats{}
		tokenStats.Models[model] = ms
	}
	ms.RequestCount++
	ms.PromptTokens += promptTokens
	ms.CompletionTokens += completionTokens
	ms.TotalTokens += totalTokens
	tokenStatsMu.Unlock()
	go func() {
		if err := saveTokenStats(); err != nil {
			slog.Warn("failed to save token stats", "path", getTokenStatsPath(), "error", err)
		}
	}()
}

// recordCacheUsage aggregates upstream prompt-cache accounting per model.
// Call it with the raw upstream usage map; zero/nil inputs are no-ops so
// call sites don't need extra branching.
func recordCacheUsage(model string, usage map[string]any) {
	if model == "" || len(usage) == 0 {
		return
	}
	read, created := parseCacheUsage(usage)
	if read == 0 && created == 0 {
		return
	}
	tokenStatsMu.Lock()
	ms, ok := tokenStats.Models[model]
	if !ok {
		ms = &ModelStats{}
		tokenStats.Models[model] = ms
	}
	ms.CacheReadTokens += read
	ms.CacheCreatedTokens += created
	tokenStatsMu.Unlock()
	go func() {
		if err := saveTokenStats(); err != nil {
			slog.Warn("failed to save token stats", "path", getTokenStatsPath(), "error", err)
		}
	}()
}

// parseCacheUsage extracts cache token counts from the various usage shapes
// seen across upstreams:
//
//	OpenAI/Anthropic: prompt_tokens_details.cached_tokens, cache_creation_input_tokens
//	DeepSeek:         prompt_cache_hit_tokens, prompt_cache_miss_tokens
//
// Returns (read, created).
func parseCacheUsage(usage map[string]any) (int64, int64) {
	var read, created int64
	// Prefer the canonical Anthropic cache field. The fallbacks use the same
	// semantic category (already-cached prompt tokens), so any combination is
	// read once instead of double-counted.
	if v, ok := usageIntField(usage, "cache_read_input_tokens"); ok {
		read += int64(v)
	} else if v, ok := usageIntField(usage, "prompt_cache_hit_tokens"); ok {
		read += int64(v)
	} else if details, ok := usageMapField(usage, "prompt_tokens_details"); ok {
		if v, ok := usageIntField(details, "cached_tokens"); ok {
			read += int64(v)
		}
	}
	if v, ok := usageIntField(usage, "cache_creation_input_tokens"); ok {
		created += int64(v)
	}
	return read, created
}
