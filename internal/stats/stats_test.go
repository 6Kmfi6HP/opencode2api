package stats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetStatsForTest saves the current in-memory snapshot and path, restoring
// them when the test completes.
func resetStatsForTest(t *testing.T) {
	t.Helper()
	tokenStatsMu.Lock()
	orig := tokenStats
	origPath := tokenStatsPath
	tokenStatsMu.Unlock()
	t.Cleanup(func() {
		tokenStatsMu.Lock()
		tokenStats = orig
		tokenStatsMu.Unlock()
		SetPath(origPath)
	})
}

func setTokenStatsForTest(t *testing.T, stats *TokenStatsData) *TokenStatsData {
	t.Helper()
	resetStatsForTest(t)

	tokenStatsMu.Lock()
	previous := tokenStats
	tokenStats = stats
	tokenStatsMu.Unlock()
	return previous
}

func TestParseCacheUsageCanonicalFields(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"cache_read_input_tokens":     float64(64),
		"cache_creation_input_tokens": float64(8),
	})
	if read != 64 || created != 8 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (64, 8)", read, created)
	}
}

func TestParseCacheUsageDeepSeekMissIsNotCreation(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"prompt_tokens":            float64(200),
		"prompt_cache_hit_tokens":  float64(160),
		"prompt_cache_miss_tokens": float64(40),
	})
	if read != 160 || created != 0 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (160, 0)", read, created)
	}
}

func TestParseCacheUsagePrefersCanonicalRead(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"cache_read_input_tokens": float64(80),
		"prompt_cache_hit_tokens": float64(160),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(200),
		},
	})
	if read != 80 || created != 0 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (80, 0)", read, created)
	}
}

// TestParseCacheUsageResponsesAliasShape 锁定 muse-spark 回归的下游口径：
// responsesUsageToChat 把 input_tokens_details.cached_tokens 归位到
// prompt_tokens_details 后，parseCacheUsage 必须认出 read。
func TestParseCacheUsageResponsesAliasShape(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"prompt_tokens": float64(749),
		"total_tokens":  float64(992),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(625),
		},
		"input_tokens_details": map[string]any{
			"cached_tokens": float64(625),
		},
	})
	if read != 625 || created != 0 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (625, 0)", read, created)
	}
}

// TestParseCacheUsageMuseSparkRawShape 锁定原生透传路径（recordResponsesUsage
// 直接把上游 responses usage 传给 RecordCacheUsage，不经过归一）：只有
// input_tokens_details.cached_tokens 时也必须认出 read，否则 muse-spark 系
// 经透传的缓存命中在 stats.json 里永远是 0。
func TestParseCacheUsageMuseSparkRawShape(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"input_tokens": float64(749),
		"input_tokens_details": map[string]any{
			"cached_tokens": float64(625),
		},
		"output_tokens": float64(133),
		"output_tokens_details": map[string]any{
			"reasoning_tokens": float64(120),
		},
		"total_tokens": float64(882),
	})
	if read != 625 || created != 0 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (625, 0)", read, created)
	}
}

func TestSaveAndLoadTokenStats(t *testing.T) {
	setTokenStatsForTest(t, &TokenStatsData{
		TotalRequests: 1,
		Models: map[string]*ModelStats{
			"test-model": {RequestCount: 1, PromptTokens: 10},
		},
	})

	dir := t.TempDir()
	statsPath := filepath.Join(dir, "nested", "stats_test.json")
	SetPath(statsPath)

	if err := saveTokenStats(); err != nil {
		t.Fatalf("saveTokenStats() error = %v", err)
	}

	tokenStatsMu.Lock()
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu.Unlock()

	LoadTokenStats()

	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	if tokenStats.TotalRequests != 1 {
		t.Fatalf("TotalRequests = %d, want 1", tokenStats.TotalRequests)
	}
	modelStats := tokenStats.Models["test-model"]
	if modelStats == nil || modelStats.PromptTokens != 10 {
		t.Fatalf("modelStats = %+v, want PromptTokens = 10", modelStats)
	}
}

func TestSaveTokenStatsReturnsWriteError(t *testing.T) {
	setTokenStatsForTest(t, &TokenStatsData{Models: map[string]*ModelStats{}})
	SetPath(filepath.Join(t.TempDir(), "blocked"))

	if err := os.MkdirAll(GetPath(), 0o755); err != nil {
		t.Fatal(err)
	}

	err := saveTokenStats()
	if err == nil {
		t.Fatal("saveTokenStats() error = nil, want write error")
	}
	if !strings.Contains(err.Error(), GetPath()) {
		t.Fatalf("error = %v, want target path %q", err, GetPath())
	}
}
