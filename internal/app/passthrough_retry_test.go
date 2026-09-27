package app

// passthrough_retry_test.go 覆盖 byte-passthrough 路径(relayResponsesStream
// + pipeAnthropicStream)在 peek 失败时的「空流重试」与在 EOF 时的「半截合成
// 收尾」两条行为契约。
//
// 不同于 claude_responses_empty_retry_test.go 的协议翻译路径,这里被测函
// 数自己就是「写字节到客户端」的最后一公里——peek 拿到的原始字节必须原
// 样写回 w,不能 unmarshal→remarshal(避免 key 顺序、tag 数字、空格被改
// 写)。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// responsesPassthroughHealthySSE 一份「正常完结」的最小原生 Responses SSE 流。
const responsesPassthroughHealthySSE = "event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"hello"}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

// TestRelayResponsesStream_EmptyEOF_RetriesOnce：上游首轮 200 但 body 立刻
// EOF(prefill 阶段 tunnel 被宰,干净 EOF)。peek 应判为未 commit,Drive
// StreamWithRetry 切下一把 key 重试拿到正常流,客户端只见干净成功流。
func TestRelayResponsesStream_EmptyEOF_RetriesOnce(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubNativeModel(t, "gpt-resp-retry-a")

	callCount := 0
	upstreamCall := func(c context.Context, body []byte) (io.ReadCloser, int, http.Header, error) {
		callCount++
		hdr := http.Header{"Content-Type": []string{"text/event-stream"}}
		// 首轮 rc 由 relayResponsesToClient 调用方提供(空流);callOnce 后续
		// 重试才走这里。所以本闭包首次调用就该返回健康 SSE。
		return io.NopCloser(strings.NewReader(responsesPassthroughHealthySSE)), http.StatusOK, hdr, nil
	}

	rec := httptest.NewRecorder()
	firstRC := io.NopCloser(strings.NewReader(``)) // 由 relayResponsesToClient 拿到
	hdr := http.Header{"Content-Type": []string{"text/event-stream"}}
	// 首轮 rc 已建立(空流),走带 DriveStreamWithRetry 的外层包装:首轮进入
	// relayResponsesStream 会 peek 出 EOF,返回 errStreamIncompleteNoCommit,
	// driver 用 upstreamCall 拿到第二份(健康)流再跑一遍。
	relayResponsesToClient(context.Background(), rec, firstRC, http.StatusOK, hdr, "gpt-resp-retry-a", true, ResponsesAPIRequest{}, newResponsesNameRewrites(), UpstreamAuth{}, []byte(`{"model":"gpt-resp-retry-a","stream":true,"input":"hi"}`), upstreamCall)

	if callCount != 1 {
		t.Fatalf("expected exactly one retry upstream call, got callCount=%d", callCount)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"response.output_text.delta"`) || !strings.Contains(body, "hello") {
		t.Fatalf("expected retry to deliver healthy SSE, got:\n%s", body)
	}
}

// TestRelayResponsesStream_PartialEOF_NoRetry：上游已发过 data 帧后 EOF,无
// completed——已 commit,不再触发空流重试;末尾合成 incomplete + [DONE]。
func TestRelayResponsesStream_PartialEOF_NoRetry(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	partial := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"partial answer"}` + "\n\n"
	// 故意没有 response.completed / [DONE] —— EOF 兜底走的是「合成 incom
	// plete + [DONE]」,不应触发空流重试(那个分支只在首帧之前)。
	callCount := 0
	upstreamCall := func(c context.Context, body []byte) (io.ReadCloser, int, http.Header, error) {
		callCount++
		// retry 槽放一条健康 SSE——不应被消费。
		return io.NopCloser(strings.NewReader(responsesPassthroughHealthySSE)), http.StatusOK, http.Header{"Content-Type": []string{"text/event-stream"}}, nil
	}

	rec := httptest.NewRecorder()
	hdr := http.Header{"Content-Type": []string{"text/event-stream"}}
	relayResponsesToClient(context.Background(), rec,
		io.NopCloser(strings.NewReader(partial)), http.StatusOK, hdr, "gpt-resp-retry-b", true,
		ResponsesAPIRequest{}, newResponsesNameRewrites(), UpstreamAuth{}, []byte(`{"model":"gpt-resp-retry-b","stream":true,"input":"hi"}`), upstreamCall)

	if callCount != 0 {
		t.Fatalf("retry must not be consumed on partial EOF (already committed), got callCount=%d", callCount)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "partial answer") {
		t.Fatalf("expected partial body delivered:\n%s", body)
	}
	if !strings.Contains(body, "response.incomplete") {
		t.Fatalf("expected synthesized response.incomplete on EOF:\n%s", body)
	}
	if strings.Contains(body, "hello") {
		t.Fatalf("must not consume retry slot when already committed:\n%s", body)
	}
}

// anthropicHealthySSE 一份「正常完结」的最小 Anthropic SSE 流。
const anthropicHealthySSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_a","role":"assistant","usage":{"input_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// TestPipeAnthropicStream_EmptyEOF_RetriesOnce：/messages → Anthropic 直通,
// 上游首轮 200 但 body 立刻 EOF,经 DriveStreamWithRetry 重试拿到正常流。
func TestPipeAnthropicStream_EmptyEOF_RetriesOnce(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-ant-retry-*", Protocol: "anthropic"}})
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: anthropicHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-ant-retry-a","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"message_start"`) || !strings.Contains(body, "hi") {
		t.Fatalf("expected retry to deliver healthy SSE, got:\n%s", body)
	}
	if !strings.Contains(body, `"type":"message_stop"`) {
		t.Fatalf("expected message_stop from retry, got:\n%s", body)
	}
}

// TestPipeAnthropicStream_EOFAfterMessageStart_SynthesizesMessageStop：已见过
// message_start 但 EOF 前没有 message_stop——已 commit,合成一条 message_stop
// 让客户端正常关流;不重试。
func TestPipeAnthropicStream_EOFAfterMessageStart_SynthesizesMessageStop(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-ant-partial-*", Protocol: "anthropic"}})
	partialSSE := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_p","role":"assistant","usage":{"input_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n"
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: partialSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		// 重试候选位的健康流不应被消费(已 commit)。
		{status: http.StatusOK, body: anthropicHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-ant-partial-a","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "partial") {
		t.Fatalf("expected partial body delivered, got:\n%s", body)
	}
	if !strings.Contains(body, `"type":"message_stop"`) {
		t.Fatalf("expected synthesized message_stop on EOF, got:\n%s", body)
	}
	// 重试不应被触发(retry 槽位的 msg_a 来自健康流,partial SSE 用的是
	// msg_p)。
	if strings.Contains(body, "msg_a") {
		t.Fatalf("retry slot must not have been consumed (already committed):\n%s", body)
	}
}

// TestPipeAnthropicStream_ErrorEvent_Retries：上游 200 但首帧就是 type=error
// 的 SSE 事件,peek 视为未 commit,DriveStreamWithRetry 切 key 重试;最终的
// retry 槽返回正常流。
func TestPipeAnthropicStream_ErrorEvent_Retries(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-ant-error-*", Protocol: "anthropic"}})
	errSSE := "event: error\n" +
		`data: {"type":"error","error":{"type":"overloaded_error","message":"boom"}}` + "\n\n"
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: errSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: anthropicHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-ant-error-a","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"message_start"`) {
		t.Fatalf("expected retry message_start, got:\n%s", body)
	}
	if strings.Contains(body, "overloaded_error") {
		t.Fatalf("upstream error frame must not leak to client when retry succeeded:\n%s", body)
	}
}
