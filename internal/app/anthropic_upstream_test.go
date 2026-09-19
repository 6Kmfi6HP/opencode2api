package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// forwardClaudeViaAnthropic：上游 2xx + stream 请求保持 SSE 管道透传
// （Content-Type=text/event-stream、字节保真）—— tee 路径不被非 2xx 分流
// 改动波及。
func TestForwardClaudeViaAnthropic_Stream2xxPipesRawEvents(t *testing.T) {
	const model = "claude-x-passthrough-stream"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	sse := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_p\",\"usage\":{\"input_tokens\":4}}}\r\n\r\n" +
		"event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if rec.Body.String() != sse {
		t.Fatalf("stream body must be byte-identical to upstream:\nup:   %q\ngot:  %q", sse, rec.Body.String())
	}
	if got := transport.requestHeaders[0].Get("Accept"); got != "text/event-stream" {
		t.Fatalf("stream Accept = %q, want text/event-stream", got)
	}
}

// forwardClaudeViaAnthropic：流式请求撞上上游非 2xx 时已把错误以 application/json
// 保真直转（客户端侧 SSE 未建立，发 JSON 是 Claude Code 期望的形状），handler
// 看到 true 后不应再走回落；非流式请求的非 2xx 同样 buffered 直转。
func TestForwardClaudeViaAnthropic_StreamErrorRelaysJSONNotSSE(t *testing.T) {
	const model = "claude-x-passthrough-stream-err"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-x-*", Protocol: "anthropic"}})
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusTooManyRequests, body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
		{status: http.StatusTooManyRequests, body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
		{status: http.StatusTooManyRequests, body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passthrough; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", ct)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("stream-request error must not be wrapped in SSE frame: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Fatalf("error body not passed through: %s", rec.Body.String())
	}
}

// forwardClaudeViaAnthropic：非流式 + 非 2xx 同样以 application/json +
// 原状态码直转（读侧限 32MiB 后整体回写）。
func TestRelayAnthropicBuffered_Non2xxJSONPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	hdr := http.Header{"Content-Type": []string{"application/json"}}
	relayAnthropicBuffered(context.Background(), rec,
		io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`)),
		http.StatusBadRequest, hdr, "claude-x")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want application/json; charset=utf-8", ct)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Fatalf("error body not echoed: %s", rec.Body.String())
	}
}

// 单位层空 header 的 2xx 流管道（与 chat_protocol_routing_test.go 的组合/
// 分片用例呼应）保留在那一侧，这里聚焦 forward 的分流决策与 header 语义。
func TestPipeAnthropicStream_PreservesHeaderAndBytes(t *testing.T) {
	upstreamBody := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rec := httptest.NewRecorder()
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	pipeAnthropicStream(context.Background(), rec, io.NopCloser(strings.NewReader(upstreamBody)), http.StatusOK, header, "m")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("body diverged:\nup:  %q\ngot: %q", upstreamBody, rec.Body.String())
	}
}

// clampAnthropicProtocolMaxTokens：显式小值抬到 128 下限（以前只降不抬）。
func TestClampAnthropicProtocolMaxTokens_LiftsBelowFloor(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 128000
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	bodyMap := map[string]any{"max_tokens": float64(50)}
	if got := clampAnthropicProtocolMaxTokens(bodyMap, "any-model"); got != 128 {
		t.Fatalf("低于 128 应抬到 128, got %d", got)
	}
}
