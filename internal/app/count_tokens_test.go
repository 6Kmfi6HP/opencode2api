package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// =====================================================================
// count_tokens: estimateClaudeInputTokens
// =====================================================================

func TestEstimateClaudeInputTokens_StringContents(t *testing.T) {
	// "Hello, world" = 13 chars, 13/4 = 3; + messageOverhead(4) => 7
	req := ClaudeRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "Hello, world"},
		},
	}
	got := estimateClaudeInputTokens(req)
	// 每个 message 计入 overhead；断言一个稳定下限，精确值由实现定义。
	if got <= 0 {
		t.Fatalf("estimateClaudeInputTokens = %d, want > 0", got)
	}
}

func TestEstimateClaudeInputTokens_BlockContents(t *testing.T) {
	req := ClaudeRequest{
		Messages: []ClaudeMessage{
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"},
				},
				map[string]any{
					"type":   "document",
					"source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "AAAA"},
				},
			}},
		},
	}
	// 文本 5/4=1 + 图像块固定 1600 + 文档块固定 3000 + overhead
	got := estimateClaudeInputTokens(req)
	if got < 4600 {
		t.Fatalf("estimateClaudeInputTokens = %d, want >= 4600 (text+image+document)", got)
	}
}

func TestEstimateClaudeInputTokens_SystemAndTools(t *testing.T) {
	req := ClaudeRequest{
		Model: "claude-sonnet-4-5",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
		},
		Tools: []ClaudeTool{
			{
				Name:        "search",
				Description: "Search the web",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"q": map[string]any{"type": "string"}},
				},
			},
		},
	}
	base := estimateClaudeInputTokens(ClaudeRequest{
		Model:    req.Model,
		Messages: req.Messages,
	})
	withSystem := estimateClaudeInputTokens(ClaudeRequest{
		Model:    req.Model,
		System:   req.System,
		Messages: req.Messages,
	})
	withTools := estimateClaudeInputTokens(req)
	if withSystem <= base {
		t.Fatalf("with system = %d, want > base %d", withSystem, base)
	}
	if withTools <= withSystem {
		t.Fatalf("with tools = %d, want > with system %d", withTools, withSystem)
	}
}

// =====================================================================
// count_tokens handler
// =====================================================================

func TestCountTokensHandler_PostOK(t *testing.T) {
	transport := installFakeOpenCodeClient(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"Hello"}]}`))
	rec := httptest.NewRecorder()

	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("input_tokens = %d, want > 0", resp.InputTokens)
	}
	if len(transport.requestedURLs) != 0 {
		t.Fatalf("upstream calls = %d, want 0 (handler must not hit upstream)", len(transport.requestedURLs))
	}
}

func TestCountTokensHandler_MissingModel400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"messages":[{"role":"user","content":"Hello"}]}`))
	rec := httptest.NewRecorder()

	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if typ, _ := resp["type"].(string); typ != "error" {
		t.Fatalf("response type = %q, want error", typ)
	}
}

func TestCountTokensHandler_InvalidJSON400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":`))
	rec := httptest.NewRecorder()

	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCountTokensHandler_MethodNotAllowed405(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil)
	rec := httptest.NewRecorder()

	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestCountTokensHandler_NoUpstreamCall(t *testing.T) {
	// Without any fake client installed, a call to the upstream would panic on
	// nil httpClient. The handler must answer synchronously and never touch it.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	rec := httptest.NewRecorder()

	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}