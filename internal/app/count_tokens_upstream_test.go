package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// 命中 protocol_rules 的 anthropic 规则时，count_tokens 应直连
// /zen/v1/messages/count_tokens，且 2xx 响应透传 input_tokens。
func TestCountTokensHandler_RouteHitAnthropicForwardsUpstream(t *testing.T) {
	const model = "claude-x-ct-hit"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"input_tokens":321}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"`+model+`","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if resp["input_tokens"] != float64(321) {
		t.Fatalf("input_tokens = %#v, want 321 (upstream passthrough)", resp["input_tokens"])
	}
	if len(transport.requestedURLs) != 1 || !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/messages/count_tokens") {
		t.Fatalf("URL = %#v, want single /zen/v1/messages/count_tokens call", transport.requestedURLs)
	}
	// upstream body 应只改写 model 且 max_tokens 按要求收敛到 [128, cap]。
	payload := transport.requestPayloads[0]
	if payload["model"] != model {
		t.Fatalf("payload model = %#v, want %q", payload["model"], model)
	}
	if payload["max_tokens"] != float64(128) {
		t.Fatalf("max_tokens = %#v, want 128 (already at min)", payload["max_tokens"])
	}
	if _, hasStream := payload["stream"]; hasStream {
		t.Fatalf("count_tokens forward must not send stream: %v", payload["stream"])
	}
}

// 命中规则但上游传输层错误时回落本地启发式（input_tokens>0，body 仍是
// 启发式 JSON，不透传上游错误）。
func TestCountTokensHandler_UpstreamTransportErrorFallsBackToHeuristic(t *testing.T) {
	const model = "claude-x-ct-transport"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	installFakeOpenCodeClient(t, nil)
	// 走 stub 而非真实 fake transport（callOpenCodeEndpoint 会自动重试，
	// 假 transport 只备一个槽位会被重试消耗崩盘）。
	old := callCountTokensUpstream
	t.Cleanup(func() { callCountTokensUpstream = old })
	callCountTokensUpstream = func(_ context.Context, _ []byte, _ string, _ UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
		return nil, 0, nil, errors.New("websocket-ish dial fail")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (heuristic fallback); body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if tok := resp["input_tokens"]; tok == nil {
		t.Fatalf("input_tokens missing; body=%s", rec.Body.String())
	}
}

// 命中规则但上游返回非 2xx 时回落本地启发式。
func TestCountTokensHandler_UpstreamNon2xxFallsBackToHeuristic(t *testing.T) {
	const model = "claude-x-ct-non2xx"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	installFakeOpenCodeClient(t, nil)
	old := callCountTokensUpstream
	t.Cleanup(func() { callCountTokensUpstream = old })
	callCountTokensUpstream = func(_ context.Context, _ []byte, _ string, _ UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
		body := `{"type":"error","error":{"type":"api_error","message":"down"}}`
		return io.NopCloser(strings.NewReader(body)), http.StatusServiceUnavailable, http.Header{}, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 heuristic; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "api_error") {
		t.Fatalf("must not relay upstream error to client: %s", rec.Body.String())
	}
}

// 未命中 anthropic 规则时保持本地启发式，不发上游请求。
func TestCountTokensHandler_RuleMissStaysLocal(t *testing.T) {
	const model = "some-chat-model"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "some-chat-*", Protocol: "chat_completions"}})
	transport := installFakeOpenCodeClient(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeCountTokensHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(transport.requestedURLs) != 0 {
		t.Fatalf("expected no upstream call, got %#v", transport.requestedURLs)
	}
}

// callCountTokensUpstream 直接注入，校验 forwardCountTokensViaAnthropic 对
// max_tokens 收敛与非 2xx 的判定，不依赖真实 HTTP。
func TestForwardCountTokensViaAnthropic_ClampAndRelay(t *testing.T) {
	old := callCountTokensUpstream
	t.Cleanup(func() { callCountTokensUpstream = old })
	callCountTokensUpstream = func(_ context.Context, body []byte, _ string, _ UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, 0, nil, err
		}
		if m["max_tokens"] != float64(100000) {
			t.Fatalf("upstream max_tokens = %#v, want 100000 (unclamped, no cap configured)", m["max_tokens"])
		}
		return io.NopCloser(strings.NewReader(`{"input_tokens":7}`)), http.StatusOK, nil, nil
	}
	claudeReq := ClaudeRequest{Model: "m", MaxTokens: ptr(100000), Messages: []ClaudeMessage{{Role: "user", Content: "hi"}}}
	status, body, ok := forwardCountTokensViaAnthropic(context.Background(), claudeReq, UpstreamAuth{Mode: AuthRoutePublic}, "m")
	if !ok || status != http.StatusOK {
		t.Fatalf("forward failed: status=%d ok=%v body=%s", status, ok, body)
	}
	if !strings.Contains(string(body), "input_tokens") {
		t.Fatalf("relay body = %s, want upstream input_tokens", body)
	}
}
