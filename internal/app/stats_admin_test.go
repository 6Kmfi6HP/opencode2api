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

	"github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// setTokenStatsForTest replaces the in-memory stats snapshot and restores the
// previous snapshot when the test completes.
func setTokenStatsForTest(t *testing.T, s *stats.TokenStatsData) *stats.TokenStatsData {
	t.Helper()
	resetStatsAndLogTestState(t)
	return stats.SetSnapshot(s)
}

func TestAdminStatsDeleteReturnsSaveError(t *testing.T) {
	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	setTokenStatsForTest(t, &stats.TokenStatsData{TotalRequests: 3})
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
