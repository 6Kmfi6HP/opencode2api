package app

// anthropic 上游三条路径的「传输中断不伪造干净收尾」回归(对照
// claude_responses P3 的截断分流):
//
//   1. claude→anthropic 直通 pipeAnthropicStream:干净 EOF 合成 message_stop;
//      RST/unexpected EOF 等传输中断补发 in-band error 事件。
//   2. chat→anthropic anthropicSSEToChatStream:干净 EOF finalize 合成
//      stop+[DONE];传输中断补发 upstream_truncated 错误帧 + [DONE]。
//   3. responses→anthropic anthropicSSEToResponsesStream:peek 前置
//      (空流可重试);干净 EOF 合成 response.completed;传输中断按
//      response.failed 收尾。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// 一份「有 message_start + 部分文本,无 message_stop」的 anthropic SSE——
// 上游在文本中途死掉时客户端已收到的内容。
const anthropicPartialSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_t\",\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":2}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n"

// 一份「正常完结」的 anthropic SSE(message_delta 带 stop_reason + message_stop)。
const anthropicTruncHealthySSE = anthropicPartialSSE +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

//  1. 直通路径:传输中途死亡(io.ErrClosedPipe 非 EOF)→ in-band error 事件
//     必须出现,不能只发一条伪造的 message_stop。
func TestPipeAnthropicStream_TransportDeath_EmitsError(t *testing.T) {
	er := &errorReader{data: anthropicPartialSSE}
	rec := httptest.NewRecorder()
	committed, err := pipeAnthropicStream(context.Background(), rec, er, http.StatusOK, http.Header{}, "claude-pipe-test")
	if !committed || err != nil {
		t.Fatalf("committed=%v err=%v, want committed=true err=nil", committed, err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream stream interrupted before completion") {
		t.Fatalf("missing in-band error event, body:\n%s", body)
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("missing error frame, body:\n%s", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("stream must still close (message_stop) after error, body:\n%s", body)
	}
	// 半截正文仍应送达。
	if !strings.Contains(body, "par") {
		t.Fatalf("partial text lost, body:\n%s", body)
	}
}

//  2. 直通路径:干净 EOF(缺 message_stop)→ 旧行为不变,只合成
//     message_stop,不补 error 事件。
func TestPipeAnthropicStream_CleanEOF_SynthesizesStopOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	committed, err := pipeAnthropicStream(context.Background(), rec, strings.NewReader(anthropicPartialSSE), http.StatusOK, http.Header{}, "claude-pipe-test")
	if !committed || err != nil {
		t.Fatalf("committed=%v err=%v, want committed=true err=nil", committed, err)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("clean EOF must not emit error event, body:\n%s", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("expected synthesized message_stop, body:\n%s", body)
	}
}

//  3. chat→anthropic:commit 后传输死亡 → upstream_truncated 错误帧 +
//     [DONE],不得伪造 finish_reason=stop。
func TestAnthropicSSEToChatStream_TransportDeath_EmitsTruncatedError(t *testing.T) {
	er := &errorReader{data: anthropicPartialSSE}
	rec := httptest.NewRecorder()
	committed, err := anthropicSSEToChatStream(context.Background(), rec, er, "claude-chat-model", false, true, nil, nil)
	if !committed || err != nil {
		t.Fatalf("committed=%v err=%v, want committed=true err=nil", committed, err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_truncated") || !strings.Contains(body, "upstream stream interrupted before completion") {
		t.Fatalf("missing truncated error frame, body:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE] sentinel, body:\n%s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("transport death must not synthesize clean stop, body:\n%s", body)
	}
}

//  4. chat→anthropic:干净 EOF → 旧行为不变(finalize 合成 stop+[DONE],
//     无错误帧)。
func TestAnthropicSSEToChatStream_CleanEOF_FinalizesStop(t *testing.T) {
	rec := httptest.NewRecorder()
	committed, err := anthropicSSEToChatStream(context.Background(), rec, strings.NewReader(anthropicPartialSSE), "claude-chat-model", false, true, nil, nil)
	if !committed || err != nil {
		t.Fatalf("committed=%v err=%v, want committed=true err=nil", committed, err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "upstream_truncated") {
		t.Fatalf("clean EOF must not emit error frame, body:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected finalize stop+[DONE], body:\n%s", body)
	}
}

//  5. responses→anthropic:commit 后传输死亡 → response.failed 收尾,
//     不伪造 status=completed。
func TestAnthropicSSEToResponsesStream_TransportDeath_EmitsFailed(t *testing.T) {
	er := &errorReader{data: anthropicPartialSSE}
	rec := httptest.NewRecorder()
	committed, err := anthropicSSEToResponsesStream(context.Background(), rec, er, "claude-resp-model", true, nil, nil)
	if !committed || err != nil {
		t.Fatalf("committed=%v err=%v, want committed=true err=nil", committed, err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"response.failed"`) || !strings.Contains(body, "upstream stream interrupted before completion") {
		t.Fatalf("missing response.failed event, body:\n%s", body)
	}
	if strings.Contains(body, `"response.completed"`) {
		t.Fatalf("transport death must not synthesize completed, body:\n%s", body)
	}
	if !strings.Contains(body, `"status":"failed"`) {
		t.Fatalf("response object must carry status=failed, body:\n%s", body)
	}
}

// 6. responses→anthropic:干净 EOF → 旧行为不变,合成 response.completed。
func TestAnthropicSSEToResponsesStream_CleanEOF_SynthesizesCompleted(t *testing.T) {
	rec := httptest.NewRecorder()
	committed, err := anthropicSSEToResponsesStream(context.Background(), rec, strings.NewReader(anthropicPartialSSE), "claude-resp-model", true, nil, nil)
	if !committed || err != nil {
		t.Fatalf("committed=%v err=%v, want committed=true err=nil", committed, err)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"response.failed"`) {
		t.Fatalf("clean EOF must not emit failed, body:\n%s", body)
	}
	if !strings.Contains(body, `"response.completed"`) || !strings.Contains(body, "par") {
		t.Fatalf("expected synthesized completed with partial text, body:\n%s", body)
	}
}

//  7. responses→anthropic:空流(EOF 零帧)未 commit → 返回可重试错误,
//     不向客户端写 200。
func TestAnthropicSSEToResponsesStream_EmptyStream_NoCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	committed, err := anthropicSSEToResponsesStream(context.Background(), rec, strings.NewReader(""), "claude-resp-model", true, nil, nil)
	if committed || err == nil {
		t.Fatalf("committed=%v err=%v, want committed=false with retryable err", committed, err)
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("no bytes must be written before commit, got:\n%s", got)
	}
}

//  8. 端到端:responses→anthropic 空流首轮 → DriveStreamWithRetry 换 key
//     重试拿到正常流;客户端只看到一条干净成功流。
func TestForwardResponsesViaAnthropic_EmptyStream_RetriesThenSucceeds(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: anthropicTruncHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	chatReq := &OpenAIRequest{Model: "fallback-model-free", Stream: true,
		Messages: []Message{{Role: "user", Content: "hi"}}}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	rec := httptest.NewRecorder()
	forwardResponsesViaAnthropic(rec, req, UpstreamAuth{Mode: AuthRoutePublic}, chatReq, false)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retry, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "par") {
		t.Fatalf("expected text from retry, got:\n%s", body)
	}
	if !strings.Contains(body, `"response.completed"`) || strings.Contains(body, `"response.failed"`) {
		t.Fatalf("retry stream must complete cleanly, got:\n%s", body)
	}
	if len(transport.requestedModels) != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (empty stream then retry)", len(transport.requestedModels))
	}
}

//  9. 端到端:claude→anthropic 直通文本中途传输死亡 → 客户端收到 error
//     事件(200 已 commit),得以感知失败。
func TestClaudeViaAnthropic_TransportDeathMidStream_EmitsError(t *testing.T) {
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-death-*", Protocol: "anthropic"}})
	er := &errorReader{data: anthropicPartialSSE}
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, customBody: er, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-death-test","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed before death), body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream stream interrupted before completion") {
		t.Fatalf("missing in-band error event, body:\n%s", body)
	}
}

//  10. 端到端:chat→anthropic 空流首轮换 key 重试拿到正常流(既有
//     DriveStreamWithRetry 链路在 anthropicSSEToChatStream 上仍成立)。
func TestForwardChatViaAnthropic_EmptyStream_RetriesThenSucceeds(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: anthropicTruncHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := &OpenAIRequest{Model: "fallback-model-free", Stream: true,
		Messages: []Message{{Role: "user", Content: "hi"}}}
	rec := httptest.NewRecorder()
	forwardChatViaAnthropic(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), UpstreamAuth{Mode: AuthRoutePublic}, req, false)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retry, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "par") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected healthy retry stream, got:\n%s", body)
	}
}
