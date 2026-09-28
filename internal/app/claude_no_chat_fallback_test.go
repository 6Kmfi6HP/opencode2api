package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errFakeTransportDisconnect = errors.New("fake upstream: transport disconnect")

// TestClaudeResponsesPassthrough_NoChatFallbackOnFailure 修复 A 的回归:
// 已记忆 native-responses 模型 forward 失败时不得落到 chat 翻译路径——
// 否则会被上游以 ModelProtocolUnsupported 拒绝(实测 muse-spark-1.3-contributor-free
// 在 chat/completions 端点即 400)。断言:handler 直接回 502,不发生
// chat 二次调用。
func TestClaudeResponsesPassthrough_NoChatFallbackOnFailure(t *testing.T) {
	const remembered = "claude-no-chat-fallback"
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[remembered] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, remembered)
		nativeResponsesModels.Unlock()
	})

	// 模拟 forwardClaudeViaResponses 走传输层错误分支 (err != nil) → markNativeResponsesFailure
	// + return false。fakeTransport 拿到 err 时 callOpenCodeEndpoint 把它当 transport_error
	// 处理并 retry最多 maxUpstreamRetries 次; 把 maxUpstreamRetries 内的所有 attempt 都填为
	// error,fake 队列将被消耗尽,最后一次返回 errlast → forward 返回 false → 修复 A 短路逻辑
	// 被触发(不能走 chat 翻译路径)。
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{err: errFakeTransportDisconnect},
		{err: errFakeTransportDisconnect},
		{err: errFakeTransportDisconnect},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+remembered+`","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no chat fallback), body=%s", rec.Code, rec.Body.String())
	}
	if len(transport.requestedURLs) == 0 {
		t.Fatalf("expected at least one upstream call, got none")
	}
	for _, u := range transport.requestedURLs {
		if strings.HasSuffix(u, "/zen/v1/chat/completions") {
			t.Fatalf("found chat fallback to %s; native-responses model must not fall back", u)
		}
		if !strings.HasSuffix(u, "/zen/v1/responses") {
			t.Fatalf("URL = %s, want /zen/v1/responses", u)
		}
	}
	if !strings.Contains(rec.Body.String(), "native-responses model cannot fall back to chat") {
		t.Fatalf("body = %s, want explicit fallback refusal message", rec.Body.String())
	}
}

// TestClaudeResponsesForward_NonStreamReadErrRetrySameProtocol 修复 B 的回归:
// responses 非流式包体读取中断时不得直接落 chat(那会撞 ModelProtocolUnsupported);
// 应同协议(responses)重试一次,第二次 200 + 完整 body 应正常写回。
func TestClaudeResponsesForward_NonStreamReadErrRetrySameProtocol(t *testing.T) {
	const remembered = "claude-retry-readerr"
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[remembered] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, remembered)
		nativeResponsesModels.Unlock()
	})

	// 第一次 200 但 body 读到一半被切 (io.ErrClosedPipe),触发 readErr 同协议重试。
	// errorReader 在 stream_integrity_test.go 中定义,模拟 partial-read + 读错误。
	truncatedReader := &errorReader{
		data: `{"id":"resp_first","object":"response",`, // 读取一半被切
	}

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, customBody: truncatedReader},
		{status: http.StatusOK, body: `{"id":"resp_recovered","status":"completed","output":[{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered after same-protocol retry"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+remembered+`","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after same-protocol retry, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recovered after same-protocol retry") {
		t.Fatalf("body = %s, want body from same-protocol retry", rec.Body.String())
	}
	if len(transport.requestedURLs) != 2 {
		t.Fatalf("requested URLs = %#v, want exactly two responses calls (retry on same protocol)", transport.requestedURLs)
	}
	for i, u := range transport.requestedURLs {
		if !strings.HasSuffix(u, "/zen/v1/responses") {
			t.Fatalf("URL[%d] = %s, want /zen/v1/responses (same-protocol retry)", i, u)
		}
	}
}
