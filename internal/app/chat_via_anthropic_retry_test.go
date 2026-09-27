package app

// chat→anthropic 流式链路的「空流兜底 + 首 token 前重试」端到端回归。
//
// 覆盖以下故障形态(对照 docs/CONFIGURATION.md 的 stream_empty_retry_max):
//   1. 空流 EOF(整流 EOF、零产出)→ 双层兜底,首 token 前换 key 重试。
//   2. 上游 200 后立刻发 error 帧(本应回 502 的活失败的 2xx 形态)→ 同上重试。
//   3. 首字节看门狗:连接接受后一字节不发,纯挂死→超过 stream_first_byte_timeout_ms
//      兜底换 key 重试。
//   4. 已有 message_start 之后 EOF(message_start 已 emit 出 role chunk,即
//      已 commit):按「合成 stop」正常关流,不再重试。
//   5. message_start 之前 EOF:返回未 commit,走重试。
//   6. 重试额度用尽:落 502,不伪装半截流。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// chatViaAnthropicHealthySSE 是一份「正常完结」的最小 anthropic SSE 流。
const chatViaAnthropicHealthySSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_ok","model":"go-anthropic-model","role":"assistant","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// stubAnthropicRoute 把 modelPattern(例如 "go-anthropic-*")绑定为 anthropic
// 协议,让 chat 入站走到 forwardChatViaAnthropic。返回清理。
func stubAnthropicRoute(t *testing.T, pattern string) {
	t.Helper()
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: pattern, Protocol: "anthropic"}})
}

// drainRecorder 提取 recorder 的所有 SSE 行(便于断言)。
func drainRecorder(rec *httptest.ResponseRecorder) string {
	return rec.Body.String()
}

//  1. 首轮空流 EOF,重试拿到正常流:客户端只看到一条干净的成功流,不应
//     出现 error 事件、不应看到前半截残流。
func TestChatViaAnthropicStream_EmptyEOF_RetriesOnce(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubAnthropicRoute(t, "go-anthropic-*")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: chatViaAnthropicHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"go-anthropic-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := drainRecorder(rec)
	// 首轮空流不应残留任何字节;客户端只看到重试后的干净流。
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("expected finish_reason=stop from retry, got:\n%s", body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("expected 'hello' text delta from retry, got:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected [DONE] terminator, got:\n%s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("client must not see retry-induced error:\n%s", body)
	}
	// 顶部 role chunk 只能来自重试后的首帧 message_start; 只有一个 role。
	if got := strings.Count(body, `"role":"assistant"`); got != 1 {
		t.Fatalf("expected exactly 1 role chunk (from retried stream), got %d:\n%s", got, body)
	}
}

// 2. 重试额度用尽,首轮 + 重试都空流:客户端收到 502,不伪装成功流。
func TestChatViaAnthropicStream_EmptyEOF_ExhaustsRetryThenErrors(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubAnthropicRoute(t, "go-anthropic-*")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"go-anthropic-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (retries exhausted, nothing committed), body=%s", rec.Code, rec.Body.String())
	}
	// 失败响应必须是 JSON 形状错误,而不是半截 SSE。
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("failure response must be JSON, got content-type %q; body=%s", ct, rec.Body.String())
	}
}

//  3. 部分产出后 EOF(已有 role chunk + text delta,但没有 message_stop):
//     按 ParalonCloud Rule 2 不重试,按现有「合成 stop」逻辑正常关流——既
//     不让 agent 拿半截,也不多达一次重算 input。
func TestChatViaAnthropicStream_PartialEOF_FinalizesNormallyNoRetry(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubAnthropicRoute(t, "go-anthropic-*")

	partialSSE := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_partial","model":"go-anthropic-model","role":"assistant","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial answer"}}` + "\n\n"
	// 没有 message_stop / message_delta ——上游在 text delta 后死掉。

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: partialSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		// 即便是当 retry 候选位准备的一条正常流,也绝不应被发出。
		{status: http.StatusOK, body: chatViaAnthropicHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"go-anthropic-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := drainRecorder(rec)
	// 半截正文必须送达。
	if !strings.Contains(body, "partial answer") {
		t.Fatalf("expected partial text to reach client:\n%s", body)
	}
	// 合成正常收尾,不发 error。
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("expected synthesized finish_reason=stop, got:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected [DONE] after synthesized stop, got:\n%s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("must not emit error on partial EOF (already committed):\n%s", body)
	}
	// 重试不应被触发:fallback 槽位里的正常流不应出现在响应里。
	if strings.Contains(body, "hello") {
		t.Fatalf("retry slot must not have been consumed (already-committed stream):\n%s", body)
	}
}

//  4. 上游 200 但首帧就是 type=error:按未 commit 处理,走重试,而不
//     是把半截 SSE 流甩给客户端。
func TestChatViaAnthropicStream_ErrorEvent_Retries(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubAnthropicRoute(t, "go-anthropic-*")

	errorOnlySSE := "event: error\n" +
		`data: {"type":"error","error":{"type":"api_error","message":"upstream internal"}}` + "\n\n"

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: errorOnlySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: chatViaAnthropicHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"go-anthropic-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := drainRecorder(rec)
	if !strings.Contains(body, "hello") {
		t.Fatalf("expected text from retry, got:\n%s", body)
	}
	// 上游 error 帧必须被 peek 吞掉并触发重试,不应直接翻译成 client-visible error。
	if strings.Contains(body, "upstream internal") {
		t.Fatalf("upstream error must not leak to client when retry succeeded:\n%s", body)
	}
}

//  5. 上游发了 message_start 但随后立刻 EOF,没有任何 content_block。按
//     「已 commit」处理(已经向客户端写过 role chunk),合成 stop 关闭,
//     不重试。这相当于 topic_1 「Thinking-Only or Empty Post-Start」的
//     message_start-only 边界场景。
func TestChatViaAnthropicStream_MessageStartThenEOF_NoRetry(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubAnthropicRoute(t, "go-anthropic-*")

	startOnlySSE := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_only","model":"go-anthropic-model","role":"assistant","usage":{"input_tokens":2}}}` + "\n\n"
	// 没有任何 content_block / message_stop,直接 EOF。

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: startOnlySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: chatViaAnthropicHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"go-anthropic-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (committed by role chunk), body=%s", rec.Code, rec.Body.String())
	}
	body := drainRecorder(rec)
	// 已 commit:message_start 已翻译为 role chunk;synthesize stop+[DONE]。
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("expected role chunk from message_start:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("expected synthesized finish_reason=stop after EOF, got:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected [DONE] after synthesized stop:\n%s", body)
	}
	// 重试不应被触发。
	if strings.Contains(body, "hello") {
		t.Fatalf("retry slot must not have been consumed (already-committed by message_start):\n%s", body)
	}
}

//  6. 完全死锁(连接接受后一字节都不发,纯挂在 socket 上)——由
//     stream_first_byte_timeout_ms 兜出,不应傻等到外层 ctx 超时。
func TestChatViaAnthropicStream_HungUpstream_TimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hung-upstream test in -short mode")
	}
	stubRetryConfig(t, 1, 200) // 200ms 首字节超时
	stubAnthropicRoute(t, "go-anthropic-*")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		// 用 slowReader 的 Close-only 行为模拟挂死:Read 永远阻塞,直
		// 到 Close 才返回 EOF。peek 的 timeout 必须先于它触发。
		{status: http.StatusOK, body: "", header: http.Header{"Content-Type": []string{"text/event-stream"}}, customBody: newSlowReader()},
		{status: http.StatusOK, body: chatViaAnthropicHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"go-anthropic-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	start := time.Now()
	chatCompletionsHandler(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (retry succeeded after watchdog), body=%s (took %s)", rec.Code, rec.Body.String(), elapsed)
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("expected retry to recover from hung upstream (took %s):\n%s", elapsed, rec.Body.String())
	}
	// 看门狗应当在远小于默认值的时间内触发(默认 5s,我们设了 200ms)。
	if elapsed > 5*time.Second {
		t.Fatalf("watchdog should have fired well within 5s, took %s", elapsed)
	}
}

//  7. 单元级:anthropicSSEToChatStream 在 sentRole=false 时 EOF,应返回未
//     commit + errStreamIncompleteNoCommit(供 DriveStreamWithRetry 重试)。
func TestAnthropicSSEToChatStream_NoRoleEOF_ReturnsNoCommit(t *testing.T) {
	// 一个空 data 帧都没有的上游流(连 message_start 都没发)。
	sse := ""
	rec := httptest.NewRecorder()
	committed, err := anthropicSSEToChatStream(context.Background(), rec, io.NopCloser(strings.NewReader(sse)), "go-anthropic-model", false, true, nil, nil)
	if committed {
		t.Fatalf("expected not-committed on empty upstream before any message_start")
	}
	if err == nil {
		t.Fatalf("expected errStreamIncompleteNoCommit on empty upstream before any message_start")
	}
	// 客户端不应有任何字节。
	if rec.Body.Len() != 0 {
		t.Fatalf("no bytes should have been written to client, got %q", rec.Body.String())
	}
}
