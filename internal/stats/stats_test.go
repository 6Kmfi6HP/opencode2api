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
