package app

// chat 直连(/v1/chat/completions)链路的「空流兜底 + 首 token 前重试」端到
// 端回归。镜像 claude_responses_empty_retry_test.go,针对的是 chat.go 走
// DriveStreamWithRetry 的重构。
//
// 覆盖三档故障形态(对照 docs/CONFIGURATION.md 的 stream_empty_retry_max):
//  1. 空流 EOF(整流 EOF、零产出)→ 重试。
//  2. 首字节超时(StreamFirstByteTimeoutMs 内无任何 SSE 行)。
//  3. 上游返回 200 后直接发顶层 error 帧(本应回 502 的伪 2xx)。
//  4. 已有部分产出后 EOF——已 commit,向客户端发一个 error 帧并收尾,
//     不再重试。
//  5. 重试关闭(retryMax=0)时首轮空流直接落 502。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatHealthySSE 是一条完整正常结束的 chat-completion SSE 流(2 行 delta
// + [DONE])。
const chatHealthySSE = `data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n" +
	`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// chatPartialSSE 是「半截」流:送出一个 delta 后 EOF,没有 finish_reason
// 也没有 [DONE]。
const chatPartialSSE = `data: {"id":"chatcmpl_x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"partial answer"}}]}` + "\n\n"

// chatErrorSSE 是「首帧即顶层错误」的伪 200 流(应当触发重试而不是把错
// 误帧甩给客户端)。
const chatErrorSSE = `data: {"error":{"message":"upstream internal","type":"server_error"}}` + "\n\n"

// chatSilentHeartbeatBody 仅包含 SSE 注释行(心跳)。peek 会消费完这些注
// 释后在 EOF 判定「无产出」,应触发重试。
const chatSilentHeartbeatBody = ": keepalive\n: keepalive\n: keepalive\n"

func TestChatStream_EmptyEOF_RetriesOnce(t *testing.T) {
	stubRetryConfig(t, 1, 5000)

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: chatHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fallback-model-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hel") || !strings.Contains(body, "lo") {
		t.Fatalf("expected text delta from retry, got:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected [DONE] sentinel, got:\n%s", body)
	}
	// 客户端不应看到任何 error 帧——首轮的空流应当被默默重试。
	if strings.Contains(body, `"error"`) {
		t.Fatalf("client must not see retry-induced error frame:\n%s", body)
	}
}

func TestChatStream_EmptyEOF_ExhaustsRetryThenErrors(t *testing.T) {
	stubRetryConfig(t, 1, 5000)

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fallback-model-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	// 一直未 commit,handler 走「drive 失败」分支,应回 502 JSON。
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (retries exhausted), body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream") {
		t.Fatalf("expected upstream error JSON, got:\n%s", rec.Body.String())
	}
}

func TestChatStream_EmptyEOF_RetryDisabled(t *testing.T) {
	stubRetryConfig(t, 0, 5000)

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fallback-model-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no retry), body=%s", rec.Code, rec.Body.String())
	}
}

func TestChatStream_PartialEOF_EmitsErrorFrameNoRetry(t *testing.T) {
	stubRetryConfig(t, 1, 5000)

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: chatPartialSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		// 即便留了「如果重试就用这条」的槽位,也绝不应被发出——上游已经
		// 写过一个 delta,handler 已 commit。
		{status: http.StatusOK, body: chatHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fallback-model-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// 半截正文必须送达。
	if !strings.Contains(body, "partial answer") {
		t.Fatalf("expected partial text to reach client:\n%s", body)
	}
	// 上游提前 EOF:必须兜底发 [DONE](不能让客户端悬挂)。
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected [DONE] after partial EOF:\n%s", body)
	}
	// 上游提前 EOF:按 spec 应附加 upstream_truncated 错误帧。
	if !strings.Contains(body, "upstream_truncated") {
		t.Fatalf("expected upstream_truncated error frame after partial EOF:\n%s", body)
	}
	// 不应触发重试——重试槽位的 hello 不应出现。
	if strings.Contains(body, "hello") {
		t.Fatalf("retry slot must not have been consumed (already-committed stream):\n%s", body)
	}
}

func TestChatStream_ErrorFrame_Retries(t *testing.T) {
	stubRetryConfig(t, 1, 5000)

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: chatErrorSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: chatHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fallback-model-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hel") {
		t.Fatalf("expected text from retry, got:\n%s", body)
	}
	// 上游首帧的顶层错误不应直接漏给客户端(已被 peek 吞掉并触发重试)。
	if strings.Contains(body, "upstream internal") {
		t.Fatalf("upstream error must not leak to client when retry succeeded:\n%s", body)
	}
}

func TestChatStream_SilentUpstream_TimesOut(t *testing.T) {
	stubRetryConfig(t, 1, 200) // 200ms 首字节超时

	// 上游 body 里只有 SSE 注释行(心跳),没有任何 data: 帧,然后 EOF。
	// peek 会消费完这些注释,在 EOF 处判定「无产出」并触发重试。
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: chatSilentHeartbeatBody, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: chatHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fallback-model-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hel") {
		t.Fatalf("expected retry to recover stream after silent-upstream:\n%s", rec.Body.String())
	}
}

func TestChatStream_HungUpstream_FirstByteWatchdogFires(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hung-upstream test in -short mode")
	}
	stubRetryConfig(t, 1, 200) // 200ms 首字节超时

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		// 用 slowReader 的 Close-only 行为模拟挂死:Read 永远阻塞,直到
		// Close 才返回 EOF。peek 的 timeout 必须先于它触发。
		{status: http.StatusOK, body: "", header: http.Header{"Content-Type": []string{"text/event-stream"}}, customBody: newSlowReader()},
		{status: http.StatusOK, body: chatHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fallback-model-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hel") {
		t.Fatalf("expected retry to recover from hung upstream:\n%s", rec.Body.String())
	}
}
