package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 调试端点默认关闭：未设置 OPENCODE2API_DEBUG_ADDR 时不应注册任何路由。
func TestDebugMux_ThreadsEndpoint(t *testing.T) {
	mux := newDebugMux()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/threads", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := out["goroutines"]; !ok {
		t.Fatalf("missing goroutines field: %v", out)
	}
	if _, ok := out["threads"]; !ok {
		t.Fatalf("missing threads field: %v", out)
	}
}

func TestDebugMux_PprofGoroutine(t *testing.T) {
	mux := newDebugMux()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "goroutine profile") && !strings.Contains(body, "goroutine") {
		t.Fatalf("unexpected pprof body: %q", truncateForDebugTest(body))
	}
}

func truncateForDebugTest(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
