package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

// ---------- claude→responses 缓存 hint 注入 ----------

// 缺省时注入 prompt_cache_retention（默认 24h），且不写非法顶层字段
// cache_control / stop。
func TestClaudeToResponsesBody_InjectsCacheHints(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.PromptCacheRetention = "24h"
	})
	defer config.Update(func(s *config.Snapshot) { *s = old })

	body := claudeToResponsesBody(context.Background(), ClaudeRequest{
		Model:     "muse-spark-1.3-contributor",
		MaxTokens: ptr(64),
		Messages:  []ClaudeMessage{{Role: "user", Content: "hi"}},
	}, "muse-spark-1.3-contributor")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if m["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt_cache_retention = %v, want 24h (body=%s)", m["prompt_cache_retention"], body)
	}
	if _, ok := m["cache_control"]; ok {
		t.Fatalf("cache_control must not be injected into Responses body (body=%s)", body)
	}
	if _, ok := m["stop"]; ok {
		t.Fatalf("stop is not a valid Responses field, must not be sent (body=%s)", body)
	}
}

// retention=off 时不注入 retention。
func TestClaudeToResponsesBody_RetentionOffSkips(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.PromptCacheRetention = "off"
	})
	defer config.Update(func(s *config.Snapshot) { *s = old })

	body := claudeToResponsesBody(context.Background(), ClaudeRequest{
		Model:     "muse-spark-1.3-contributor",
		MaxTokens: ptr(64),
		Messages:  []ClaudeMessage{{Role: "user", Content: "hi"}},
	}, "muse-spark-1.3-contributor")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if _, ok := m["prompt_cache_retention"]; ok {
		t.Fatalf("prompt_cache_retention must be absent when off (body=%s)", body)
	}
}

// StopSequences 不再透传为顶层 stop（非法 Responses 字段）。
func TestClaudeToResponsesBody_DropsStopSequences(t *testing.T) {
	body := claudeToResponsesBody(context.Background(), ClaudeRequest{
		Model:         "muse-spark-1.3-contributor",
		MaxTokens:     ptr(64),
		Messages:      []ClaudeMessage{{Role: "user", Content: "hi"}},
		StopSequences: []string{"<stop>"},
	}, "muse-spark-1.3-contributor")
	if strings.Contains(string(body), `"stop"`) {
		t.Fatalf("stop must not appear in Responses body (body=%s)", body)
	}
}

// ---------- 非流式 error 形包体透传 ----------

func TestIsResponsesErrorBody(t *testing.T) {
	errShape := []byte(`{"type":"error","error":{"type":"api_error","message":"The model failed to generate a response."}}`)
	if !isResponsesErrorBody(errShape) {
		t.Fatalf("error shape not detected")
	}
	okShape := []byte(`{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`)
	if isResponsesErrorBody(okShape) {
		t.Fatalf("success shape misdetected as error")
	}
	if isResponsesErrorBody([]byte(`not json`)) {
		t.Fatalf("invalid json misdetected as error")
	}
}

// error 形包体经 writeClaudeResponsesUpstreamError 写回 502 + 原文。
func TestWriteClaudeResponsesUpstreamError_Relays502(t *testing.T) {
	errBody := []byte(`{"type":"error","error":{"type":"api_error","message":"The model failed to generate a response."}}`)
	w := httptest.NewRecorder()
	if !writeClaudeResponsesUpstreamError(context.Background(), w, "m", errBody) {
		t.Fatalf("error shape not relayed")
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal relay: %v", err)
	}
	em, _ := out["error"].(map[string]any)
	if em["message"] != "The model failed to generate a response." {
		t.Fatalf("message lost: %s", w.Body.String())
	}
}

// 正常包体不触发 error 透传。
func TestWriteClaudeResponsesUpstreamError_PassthroughSuccess(t *testing.T) {
	okBody := []byte(`{"id":"resp_1","status":"completed","output":[]}`)
	w := httptest.NewRecorder()
	if writeClaudeResponsesUpstreamError(context.Background(), w, "m", okBody) {
		t.Fatalf("success shape must not be relayed as error")
	}
}
