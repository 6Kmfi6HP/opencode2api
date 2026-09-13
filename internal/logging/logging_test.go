package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// testThinkingEnabled/Disabled mirror the app package's thinking helpers so
// the body-summary tests stay self-contained. The app owns the canonical
// implementation (which is the only way to avoid an app<->logging import
// cycle); these local stand-ins exist purely to exercise summarizing.
func testIsThinkingEnabled(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		return t == "enabled" || t == "adaptive"
	case bool:
		return v
	default:
		return false
	}
}

func testIsThinkingDisabled(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		return t == "disabled"
	case bool:
		return !v
	default:
		return false
	}
}

func testThinkingState(value any) string {
	if value == nil {
		return "absent"
	}
	if testIsThinkingDisabled(value) {
		return "disabled"
	}
	if m, ok := value.(map[string]any); ok {
		if t, _ := m["type"].(string); t == "adaptive" {
			return "adaptive"
		}
	}
	if testIsThinkingEnabled(value) {
		return "enabled"
	}
	return "present"
}

func testCacheControlCount(v any) int {
	switch x := v.(type) {
	case map[string]any:
		n := 0
		if _, ok := x["cache_control"]; ok {
			n++
		}
		for key, child := range x {
			if key == "input_schema" || key == "input" {
				continue
			}
			n += testCacheControlCount(child)
		}
		return n
	case []any:
		n := 0
		for _, child := range x {
			n += testCacheControlCount(child)
		}
		return n
	}
	return 0
}

func installTestSummaryExtras() {
	SetSummaryExtras(SummaryExtras{
		ThinkingState:     testThinkingState,
		CacheControlCount: testCacheControlCount,
	})
}

func TestRedactSecret(t *testing.T) {
	if got := redactSecret(""); got != "" {
		t.Fatalf("empty secret = %q", got)
	}
	got := redactSecret("sk-abcdefghijklmnopqrstuvwxyz0123456789")
	if !strings.HasPrefix(got, "sk-abc") {
		t.Fatalf("prefix missing: %q", got)
	}
	if !strings.Contains(got, "len=37") && !strings.Contains(got, "len=") {
		t.Fatalf("length missing: %q", got)
	}
	if strings.Contains(got, "uvwxyz") {
		t.Fatalf("full secret leaked: %q", got)
	}
}

func TestRedactLogAttrScrubsSecrets(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: redactLogAttr,
	})
	logger := slog.New(handler)
	logger.Info("auth check",
		"authorization", "Bearer sk-abcdefghijklmnopqrstuvwxyz0123456789",
		"token", "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"ok", true,
	)
	out := buf.String()
	if strings.Contains(out, "sk-abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("authorization secret leaked: %s", out)
	}
	if strings.Contains(out, "sk-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzz") {
		t.Fatalf("token secret leaked: %s", out)
	}
	if !strings.Contains(out, "authorization=") || !strings.Contains(out, "…(len=") {
		t.Fatalf("expected redacted fields, got: %s", out)
	}
}

func TestSummarizeJSONBodyOmitsRawText(t *testing.T) {
	installTestSummaryExtras()
	raw := []byte(`{
		"model":"m1",
		"stream":true,
		"reasoning_effort":"high",
		"thinking":{"type":"enabled","budget_tokens":4096},
		"messages":[
			{"role":"user","content":"SECRET_USER_PROMPT_SHOULD_NOT_APPEAR"},
			{"role":"assistant","content":[{"type":"text","text":"SECRET_ASSISTANT"}],"reasoning_content":"cot","tool_calls":[{"id":"1"}]}
		],
		"tools":[{"type":"function"}]
	}`)
	summary := summarizeJSONBody(raw, 4096)
	encoded := mustJSON(summary)
	if strings.Contains(encoded, "SECRET_") {
		t.Fatalf("raw body leaked into summary: %s", encoded)
	}
	if summary["model"] != "m1" {
		t.Fatalf("model = %v", summary["model"])
	}
	if summary["messages_count"] != 2 {
		t.Fatalf("messages_count = %v", summary["messages_count"])
	}
	if summary["has_reasoning_content"] != true {
		t.Fatalf("has_reasoning_content = %v", summary["has_reasoning_content"])
	}
	if summary["has_tool_calls"] != true {
		t.Fatalf("has_tool_calls = %v", summary["has_tool_calls"])
	}
	if summary["thinking"] != "enabled" {
		t.Fatalf("thinking = %v", summary["thinking"])
	}
}

func TestSummarizeJSONBodyClaudeCodePayload(t *testing.T) {
	installTestSummaryExtras()
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":32000,
		"stream":true,
		"system":[{"type":"text","text":"You are Claude Code","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"system","content":[{"type":"text","text":"mid system","cache_control":{"type":"ephemeral"}}]}
		],
		"tools":[{"name":"web_search","type":"web_search_20250305"}],
		"thinking":{"type":"adaptive"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015"}]},
		"output_config":{"effort":"high"}
	}`)
	summary := summarizeJSONBody(raw, 0)
	if summary["context_management"] != true {
		t.Fatalf("summary missing context_management: %#v", summary)
	}
	if summary["cache_control_blocks"] == nil {
		t.Fatalf("summary missing cache_control_blocks: %#v", summary)
	}
}

func mustJSON(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestLoggingMiddlewareRequestID(t *testing.T) {
	handler := Middleware(func(w http.ResponseWriter, r *http.Request) {
		id := requestID(r.Context())
		if id == "" {
			t.Fatal("missing request id in context")
		}
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Request-Id", "client-req-123")
	rr := httptest.NewRecorder()
	handler(rr, req)
	if got := rr.Header().Get("X-Request-Id"); got != "client-req-123" {
		t.Fatalf("X-Request-Id = %q, want client-req-123", got)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr2 := httptest.NewRecorder()
	handler(rr2, req2)
	if got := rr2.Header().Get("X-Request-Id"); got == "" {
		t.Fatal("expected generated X-Request-Id")
	}
}

func TestInitLoggerCreatesRotatingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")

	prevStdout := Stdout
	prevSize, prevBackups, prevAge := MaxSize, MaxBackups, MaxAge
	prevCompress := Compress
	prevRotator := rotator
	t.Cleanup(func() {
		CloseRotator()
		Stdout = prevStdout
		MaxSize, MaxBackups, MaxAge = prevSize, prevBackups, prevAge
		Compress = prevCompress
		rotator = prevRotator
		Init("", "info", false)
	})

	Stdout = false
	MaxSize = 1
	MaxBackups = 3
	MaxAge = 1
	Compress = false
	CloseRotator()
	Init(path, "info", false)

	slog.Info("hello logging", "n", 1)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file missing: %v", err)
	}

	// Force rotation by writing through lumberjack in chunks under MaxSize.
	if rotator == nil {
		t.Fatal("expected lumberjack rotator")
	}
	chunk := bytes.Repeat([]byte("x"), 256*1024)
	for i := 0; i < 6; i++ {
		if _, err := rotator.Write(chunk); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
	}
	if _, err := rotator.Write([]byte("\nafter-rotate\n")); err != nil {
		t.Fatalf("write after rotate: %v", err)
	}
	_ = rotator.Close()
	rotator = nil

	deadline := time.Now().Add(2 * time.Second)
	var foundBackup bool
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		for _, e := range entries {
			name := e.Name()
			if name != "test.log" && strings.HasPrefix(name, "test") {
				foundBackup = true
				break
			}
		}
		if foundBackup {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !foundBackup {
		// lumberjack may keep rotated name as test-<timestamp>.log
		entries, _ := os.ReadDir(dir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected rotated backup in %v", names)
	}
}

func TestSetLogLevelRuntime(t *testing.T) {
	prev := LevelString()
	t.Cleanup(func() { SetLevelString(prev) })
	SetLevelString("debug")
	if LevelString() != "debug" {
		t.Fatalf("level = %s", LevelString())
	}
	SetLevelString("warn")
	if levelVar.Level() != slog.LevelWarn {
		t.Fatalf("LevelVar = %v", levelVar.Level())
	}
}

func TestReqLoggerIncludesRequestID(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: redactLogAttr})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.WithValue(context.Background(), reqIDKey, "rid-42")
	FromContext(ctx).Info("ping")
	if !strings.Contains(buf.String(), "request_id=rid-42") {
		t.Fatalf("missing request_id: %s", buf.String())
	}
}

// Ensure lumberjack type stays imported for compile-time sanity in older toolchains.
var _ = lumberjack.Logger{}
