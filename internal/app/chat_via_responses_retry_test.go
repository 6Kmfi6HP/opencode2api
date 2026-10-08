package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// chatViaResponsesSampleStream 一个常规原生 Responses SSE 流：created + 一段
// output_text.delta + completed(带 usage)。就绪探针 peek 看到 created 即
// commit;handler 收到 delta 写出正文,completed 触发 finalize。
const chatViaResponsesSampleStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\",\"model\":\"test-resp\"}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"

// runChatViaResponsesStream 触发一次 chat 入站 → 原生 responses 上游的流式
// 转发,返回 recorder 与 transport(便于断言上游调用次数)。
func runChatViaResponsesStream(t *testing.T, responses []fakeUpstreamResponse) (*httptest.ResponseRecorder, *fakeRetryTransport) {
	t.Helper()
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "test-resp-*", Protocol: "responses"}})
	transport := installFakeOpenCodeClient(t, responses)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-resp-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	return rec, transport
}

// 空流(200 响应 + 立即 EOF,未产出任何 SSE 帧)应被 DriveStreamWithRetry
// 识别为「未 commit」并换 key 重试一次,第二次产出正常后客户端收到内容。
func TestChatViaResponsesStream_EmptyEOF_RetriesOnce(t *testing.T) {
	rec, transport := runChatViaResponsesStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ""},                           // attempt 1: 空 body → EOF
		{status: http.StatusOK, body: chatViaResponsesSampleStream}, // attempt 2: 正常产出
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (1 empty + 1 retry); urls=%v", got, transport.requestedURLs)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("missing role chunk: %s", body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("missing content delta: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
}

// 重试额度耗尽（默认 1 次重试 = 共 2 次 attempt）后仍未 commit,网关
// 应在未向客户端写过任何 SSE 字节的前提下回落 502 JSON。
func TestChatViaResponsesStream_EmptyEOF_ExhaustsRetryThenErrors(t *testing.T) {
	rec, transport := runChatViaResponsesStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ""}, // attempt 1
		{status: http.StatusOK, body: ""}, // attempt 2 (重试 1 次)
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (initial + 1 retry exhausted); urls=%v", got, transport.requestedURLs)
	}
	// 必须没有向客户端写过任何 SSE 帧:不能出现部分 SSE body 后又写 JSON 的夹杂。
	body := rec.Body.String()
	if strings.Contains(body, "data: ") || strings.Contains(body, "[DONE]") {
		t.Fatalf("client must not receive any SSE bytes when commit never happened: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// 已 commit(role chunk 已写出)之后上游 EOF 且未完成 response.completed:
// 不触发重试,handler 用 finalize 合成 finish chunk + [DONE] 收尾。
func TestChatViaResponsesStream_PartialEOF_FinalizesNormallyNoRetry(t *testing.T) {
	partialStream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_partial\",\"model\":\"test-resp\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
		// 故意不发 response.completed / [DONE],直接 EOF。

	rec, transport := runChatViaResponsesStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: partialStream},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after commit)", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"partial"`) {
		t.Fatalf("missing content delta: %s", body)
	}
	// finalize 兜底:即便没等到 response.completed,也必须补 finish + [DONE],
	// 否则 OpenAI SDK 会挂在缺哨兵的流上。
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("missing synthesized finish_reason: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
}

// 上游首帧就是 response.failed:被 peek 识别为错误事件,返回未 commit,
// 触发重试。重试拿到正常产出后客户端应看到正常内容,看不到第一把 key
// 的错误事件。
func TestChatViaResponsesStream_ResponseFailed_Retries(t *testing.T) {
	failedStream := "event: response.failed\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_bad\",\"error\":{\"message\":\"upstream blew up\"}}}\n\n"

	rec, transport := runChatViaResponsesStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: failedStream},
		{status: http.StatusOK, body: chatViaResponsesSampleStream},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (failed + retry); urls=%v", got, transport.requestedURLs)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("missing content from retry attempt: %s", body)
	}
	// 客户端不应看到第一把 key 的错误事件——它在 peek 阶段被吞掉并整体重试。
	if strings.Contains(body, "upstream blew up") {
		t.Fatalf("first-attempt error leaked into client stream: %s", body)
	}
}

// role chunk 已写出(上游发过 response.created)之后才 EOF:说明客户端
// 已经收到首字节,不能再换 key 重试——必须用现有 partial 状态正常 finalize。
// 与 PartialEOF 区别:本用例连一个 delta 都没产出,仅 role,验证 sentRole
// 边界本身足以抑制重试。
// 宽限 peek 下纯壳帧（created）后 EOF 不再 commit：客户端尚未收到任何
// 字节，Drive 换 key 重试一次；重试成功后只看到干净的正常流。
func TestChatViaResponsesStream_RoleSentThenEOF_NoRetry(t *testing.T) {
	roleOnlyStream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_role\",\"model\":\"test-resp\"}}\n\n"
		// 没有 output_text.delta,也不发 completed,直接 EOF。

	rec, transport := runChatViaResponsesStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: roleOnlyStream},
		{status: http.StatusOK, body: chatViaResponsesSampleStream},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (shell-only → no commit → retry)", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("missing role chunk: %s", body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("missing retried content: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE] after finalize: %s", body)
	}
}
