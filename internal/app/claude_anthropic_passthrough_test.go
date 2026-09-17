package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
	statsx "github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// setProtocolRulesForTest 设置规则并在测试结束后恢复。
func setProtocolRulesForTest(t *testing.T, rules []domain.ProtocolRule) {
	t.Helper()
	old := getProtocolRules()
	t.Cleanup(func() { setProtocolRules(old) })
	setProtocolRules(rules)
}

// TestClaudeAnthropicPassthrough_Buffered 非流式：body 直通仅改 model、
// 未知字段保留、响应原样写回、usage 记账。
func TestClaudeAnthropicPassthrough_Buffered(t *testing.T) {
	const model = "claude-x-anthropic-test"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x-anthropic-test","content":[{"type":"text","text":"hello from anthropic"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":128,"metadata":{"user_id":"u-9"},"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from anthropic") {
		t.Fatalf("body = %s, want passthrough text", rec.Body.String())
	}

	// 请求应打到 /zen/v1/messages。
	if len(transport.requestedURLs) != 1 {
		t.Fatalf("URLs = %#v, want single messages call", transport.requestedURLs)
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/messages") {
		t.Fatalf("URL = %s, want /zen/v1/messages", transport.requestedURLs[0])
	}
	if got := transport.requestHeaders[0].Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
	}

	// body 直通：model 已改写；metadata 等未知字段保留。
	payload := transport.requestPayloads[0]
	if payload["model"] != model {
		t.Fatalf("payload model = %#v", payload["model"])
	}
	md, _ := payload["metadata"].(map[string]any)
	if md == nil || md["user_id"] != "u-9" {
		t.Fatalf("metadata passthrough lost: %#v", payload["metadata"])
	}
	if payload["max_tokens"] != float64(128) {
		t.Fatalf("max_tokens = %#v", payload["max_tokens"])
	}

	// usage 记账（上游重试 3 次成功，统计为 3 倍）。断言用内存态 Snapshot
	// 而非 ReadTokenStatsSnapshot（后者读文件，写入原子替换存在落盘延迟）。
	ms := statsx.Snapshot().Models[model]
	if ms == nil || ms.PromptTokens < 5 || ms.CompletionTokens < 7 {
		t.Fatalf("usage = %#v, want prompt >=5 completion >=7", ms)
	}
	if ms.CacheReadTokens < 2 || ms.CacheCreatedTokens < 3 {
		t.Fatalf("cache usage = %#v, want cache_read >=2 cache_created >=3", ms)
	}
}

// TestClaudeAnthropicPassthrough_Stream 流式：SSE 事件序列保真。
func TestClaudeAnthropicPassthrough_Stream(t *testing.T) {
	const model = "claude-x-stream-test"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":4}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"message_start", "content_block_delta", "text_delta", "message_stop", "end_turn"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body missing %q; body=%s", want, body)
		}
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/messages") {
		t.Fatalf("URL = %s", transport.requestedURLs[0])
	}
	// 上游流式请求 Accept 应为 text/event-stream。
	if got := transport.requestHeaders[0].Get("Accept"); got != "text/event-stream" {
		t.Fatalf("stream Accept = %q, want text/event-stream", got)
	}
}

// TestClaudeAnthropicPassthrough_ErrorFidelity 上游 4xx 错误体保真透传。
func TestClaudeAnthropicPassthrough_ErrorFidelity(t *testing.T) {
	const model = "claude-x-err-test"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusBadRequest, body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (fidelity); body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") || !strings.Contains(rec.Body.String(), "bad field") {
		t.Fatalf("error body not passed through: %s", rec.Body.String())
	}
}

// TestClaudeAnthropicPassthrough_TransportErrorFallsBackToChat 仅传输层错误
// 返回 false → 回落 chat 翻译路径。
func TestClaudeAnthropicPassthrough_TransportErrorFallsBackToChat(t *testing.T) {
	const model = "claude-x-transport-test"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{err: errors.New("transport boom")},
		{status: http.StatusNotFound, body: `{"type":"error","error":{"type":"not_found_error","message":"no such model"}}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	// 第一次 anthropic 直通（传输错误）+ 回落 chat 翻译 = 两个请求。
	if len(transport.requestedURLs) < 2 {
		t.Fatalf("expected fallback request, URLs = %#v", transport.requestedURLs)
	}
	if rec.Code == 0 {
		t.Fatal("handler produced no response")
	}
}

// TestBuildOCRequestWithSubpath_Messages 验证 messages subpath 的头注入与 URL。
func TestBuildOCRequestWithSubpath_Messages(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{})

	req, err := buildOCRequestWithSubpath("claude-x", map[string]any{"stream": true}, UpstreamAuth{Mode: AuthRoutePublic}, false, "https://upstream.test", "messages", "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "https://upstream.test/zen/v1/messages" {
		t.Fatalf("URL = %s", req.URL)
	}
	if req.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", req.Header.Get("anthropic-version"))
	}
	if req.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("Accept = %q", req.Header.Get("Accept"))
	}

	req2, err := buildOCRequestWithSubpath("claude-x", map[string]any{}, UpstreamAuth{Mode: AuthRoutePublic}, true, "https://upstream.test", "messages", "ses_test")
	if err != nil {
		t.Fatal(err)
	}
	if req2.URL.String() != "https://opencode.ai/zen/go/v1/messages" {
		t.Fatalf("go surface URL = %s", req2.URL)
	}
	if req2.Header.Get("Accept") != "application/json" {
		t.Fatalf("non-stream Accept = %q", req2.Header.Get("Accept"))
	}
}
