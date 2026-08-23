package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildClaudeUsageCoreDeepSeekMissIsNotCreation(t *testing.T) {
	usage := buildClaudeUsageCore(map[string]any{
		"prompt_tokens":            float64(200),
		"prompt_cache_hit_tokens":  float64(160),
		"prompt_cache_miss_tokens": float64(40),
		"completion_tokens":        float64(35),
	})
	// input_tokens excludes the cache-hit portion (Anthropic semantics:
	// input, read and creation are mutually exclusive).
	if got := usage["input_tokens"]; got != 40 {
		t.Fatalf("input_tokens = %v, want 40", got)
	}
	if got := usage["output_tokens"]; got != 35 {
		t.Fatalf("output_tokens = %v, want 35", got)
	}
	if got := usage["cache_read_input_tokens"]; got != 160 {
		t.Fatalf("cache_read_input_tokens = %v, want 160", got)
	}
	if _, ok := usage["cache_creation_input_tokens"]; ok {
		t.Fatalf("cache_creation_input_tokens should be absent for DeepSeek miss-only usage, got %v", usage["cache_creation_input_tokens"])
	}
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

func setTokenStatsForTest(t *testing.T, stats *TokenStatsData) *TokenStatsData {
	t.Helper()
	resetStatsAndLogTestState(t)

	tokenStatsMu.Lock()
	previous := tokenStats
	tokenStats = stats
	tokenStatsMu.Unlock()
	return previous
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
	setTokenStatsPath(statsPath)

	if err := saveTokenStats(); err != nil {
		t.Fatalf("saveTokenStats() error = %v", err)
	}

	tokenStatsMu.Lock()
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu.Unlock()

	loadTokenStats()

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
	setTokenStatsPath(filepath.Join(t.TempDir(), "blocked"))

	if err := os.MkdirAll(getTokenStatsPath(), 0o755); err != nil {
		t.Fatal(err)
	}

	err := saveTokenStats()
	if err == nil {
		t.Fatal("saveTokenStats() error = nil, want write error")
	}
	if !strings.Contains(err.Error(), getTokenStatsPath()) {
		t.Fatalf("error = %v, want target path %q", err, getTokenStatsPath())
	}
}

func TestAdminStatsDeleteReturnsSaveError(t *testing.T) {
	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	setTokenStatsForTest(t, &TokenStatsData{TotalRequests: 3})
	setTokenStatsPath(filepath.Join(t.TempDir(), "blocked"))

	if err := os.MkdirAll(getTokenStatsPath(), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	adminStatsHandler(rec, httptest.NewRequest(http.MethodDelete, "/api/stats", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body %q, want %d", rec.Code, rec.Body.String(), http.StatusInternalServerError)
	}
	contentType := rec.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	if !strings.Contains(rec.Body.String(), `"error":"Failed to save token stats"`) {
		t.Fatalf("body = %q, want JSON save error", rec.Body.String())
	}
	if !strings.Contains(buf.String(), `msg="failed to save cleared token stats"`) {
		t.Fatalf("log = %q, want failed-save warning/error", buf.String())
	}
}
