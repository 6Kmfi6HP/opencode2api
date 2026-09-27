package app

// responses 入站 → 上游 chat-completions 翻译路径的「空流兜底 + 首 token
// 前重试」端到端回归。镜像 chat_stream_retry_test.go 与
// claude_responses_empty_retry_test.go,针对的是 responses.go 中
// responsesStreamHandler 接上 DriveStreamWithRetry 后的行为。
//
// 覆盖五档故障形态(对照 docs/CONFIGURATION.md 的 stream_empty_retry_max /
// stream_first_byte_timeout_ms):
//  1. 空流 EOF(整流 EOF、零产出)→ 重试一次,客户端只看到正常流。
//  2. 重试额度耗尽——未 commit,客户端收到一个干净的 502 JSON,而不是
//     半截 SSE 后再切到错误响应。
//  3. 已 commit(role/created 帧已写出)后 EOF——不再重试,handler 合成
//     response.failed(reason=upstream_truncated)收尾。
//  4. 上游首帧就是顶层 error 帧——peek 吞掉并触发重试,客户端只看到
//     重试后的正常流。
//  5. 上游挂死,首字节超时——Drive 切换到重试,客户端恢复正常产出。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// responsesHandlerHealthySSE 是一条「正常完结」的 chat-completion SSE 流(这
// 条入站是 /v1/responses,上游是 OpenAI chat completions,经
// responsesStreamHandler 翻译为 responses SSE)。heartbeat + 两个 delta +
// stop finish_reason + [DONE]。
const responsesHandlerHealthySSE = ": heartbeat\n\n" +
	`data: {"id":"chatcmpl_resp_ok","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl_resp_ok","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl_resp_ok","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// responsesHandlerPartialSSE 是「半截」流:role+一个 delta 后 EOF,没有
// finish_reason 也没有 [DONE]。已 commit(response.created + output_item
// 都已写)后,handler 应合成 response.failed 兜底,而不是走外层重试。
const responsesHandlerPartialSSE = `data: {"id":"chatcmpl_resp_partial","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}` + "\n\n"

// responsesHandlerErrorSSE 是「首帧就顶层 error」的伪 200 流(应当触发
// 重试而不是把错误帧甩给客户端)。
const responsesHandlerErrorSSE = `data: {"error":{"message":"upstream internal","type":"server_error"}}` + "\n\n"

// responsesHandlerSilentBody 仅含 SSE 注释行,peek 在 EOF 处判「无产出」,
// 应触发首字节超时/EOF 兜底重试。
const responsesHandlerSilentBody = ": keepalive\n: keepalive\n: keepalive\n"

// runResponsesHandlerStream 用给定上游响应队列触发一次 /v1/responses 的流式
// 请求,返回 recorder 与 transport(便于断言上游调用次数与请求模型)。
func runResponsesHandlerStream(t *testing.T, responses []fakeUpstreamResponse) (*httptest.ResponseRecorder, *fakeRetryTransport) {
	t.Helper()
	transport := installFakeOpenCodeClient(t, responses)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"fallback-model-free","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	return rec, transport
}

// 空流(200 + 立即 EOF,零产出)应被 DriveStreamWithRetry 识别为「未
// commit」并换 key 重试一次;第二次产出正常后客户端只看到干净的成功流。
func TestResponsesHandler_StreamEmptyEOF_RetriesOnce(t *testing.T) {
	stubRetryConfig(t, 1, 5000)

	rec, transport := runResponsesHandlerStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: responsesHandlerHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (1 empty + 1 retry); urls=%v", got, transport.requestedURLs)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"response.created"`) {
		t.Fatalf("expected response.created from retry, got:\n%s", body)
	}
	if !strings.Contains(body, "hel") || !strings.Contains(body, "lo") {
		t.Fatalf("expected content delta from retry, got:\n%s", body)
	}
	// 客户端不应看到「upstream_truncated」等重试引发的错误事件——首轮空流
	// 应被默默吞掉。
	if strings.Contains(body, `"response.failed"`) || strings.Contains(body, "upstream_truncated") {
		t.Fatalf("client must not see retry-induced failure event:\n%s", body)
	}
}

// 重试额度耗尽(retryMax=1,共 2 次 attempt)仍未 commit 时,网关应在未
// 向客户端写过任何 SSE 字节的前提下回落 502 JSON。
func TestResponsesHandler_StreamEmptyEOF_ExhaustsRetry(t *testing.T) {
	stubRetryConfig(t, 1, 5000)

	rec, transport := runResponsesHandlerStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (retries exhausted), body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (initial + 1 retry exhausted); urls=%v", got, transport.requestedURLs)
	}
	body := rec.Body.String()
	// 必须没有向客户端写过任何 SSE 帧:不能出现部分 SSE body 后又写 JSON 的夹杂。
	if strings.Contains(body, "event: ") || strings.Contains(body, `data:.+"response"`) {
		t.Fatalf("client must not receive any SSE bytes when commit never happened: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// 已 commit(response.created + 一段 output_text.delta 已写出)后 EOF 且
// 未完成 finish:不触发重试,handler 必须合成 response.failed(reason=
// upstream_truncated)收尾——responses API 没有 [DONE] 哨兵。
func TestResponsesHandler_PartialEOF_SynthesizesFailedNoRetry(t *testing.T) {
	stubRetryConfig(t, 1, 5000)

	rec, transport := runResponsesHandlerStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: responsesHandlerPartialSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		// 即便留了「如果重试就用这条」的槽位,也绝不应被发出——已经 commit。
		{status: http.StatusOK, body: responsesHandlerHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after commit)", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"response.created"`) {
		t.Fatalf("missing response.created (must have committed before EOF):\n%s", body)
	}
	if !strings.Contains(body, "partial") {
		t.Fatalf("missing partial content delta:\n%s", body)
	}
	// 收尾必须是 response.failed 而不是静默收尾——客户端才能感知半截流。
	if !strings.Contains(body, `"response.failed"`) {
		t.Fatalf("missing synthesized response.failed:\n%s", body)
	}
	if !strings.Contains(body, "upstream_truncated") {
		t.Fatalf("expected upstream_truncated reason in response.failed:\n%s", body)
	}
}

// 上游首帧就发顶层 error 帧:被 peek 识别为错误,返回未 commit,触发换
// key 重试。重试拿到正常产出后客户端只看到正常流,看不到第一把 key 的错
// 误帧。
func TestResponsesHandler_ErrorChunk_Retries(t *testing.T) {
	stubRetryConfig(t, 1, 5000)

	rec, transport := runResponsesHandlerStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: responsesHandlerErrorSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: responsesHandlerHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (failed + retry); urls=%v", got, transport.requestedURLs)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hel") {
		t.Fatalf("expected retry content, got:\n%s", body)
	}
	// 客户端不应看到第一把 key 的错误信息——它已被 peek 吞掉并整体重试。
	if strings.Contains(body, "upstream internal") {
		t.Fatalf("first-attempt upstream error leaked into client stream:\n%s", body)
	}
}

// 上游挂死(body 永远阻塞 Read):首字节看门狗必须在 timeout 内放弃等
// 待并触发重试,客户端从第二把 key 拿到正常流。
func TestResponsesHandler_HungUpstream_TimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hung-upstream test in -short mode")
	}
	stubRetryConfig(t, 1, 200) // 200ms 首字节超时

	rec, transport := runResponsesHandlerStream(t, []fakeUpstreamResponse{
		// slowReader 的 Read 永远阻塞,直到 Close 才返回 EOF,模拟挂死上游。
		{status: http.StatusOK, body: "", header: http.Header{"Content-Type": []string{"text/event-stream"}}, customBody: newSlowReader()},
		{status: http.StatusOK, body: responsesHandlerHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (hung + retry); urls=%v", got, transport.requestedURLs)
	}
	if !strings.Contains(rec.Body.String(), "hel") {
		t.Fatalf("expected retry to recover from hung upstream:\n%s", rec.Body.String())
	}
}
