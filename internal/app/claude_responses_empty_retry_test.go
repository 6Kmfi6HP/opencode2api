package app

// claude→responses 流式链路的「空流兜底 + 首 token 前重试」端到端回归。
//
// 覆盖三档故障形态(对照 docs/CONFIGURATION.md 的 stream_empty_retry_max):
//   1. 空流 EOF(整流 EOF、零产出)→ 双层兜底:reasoningFallback 提升 /
//      claudeResponsesStreamWithRetry 重试。
//   2. 首字节超时(StreamFirstByteTimeoutMs 内无任何 SSE 行)。
//   3. 上游返回 200 后直接发 response.failed / error 帧(本应回 502 的活
//      失败的 2xx 形态)。
//
// 4. 对比基线:有部分产出后 EOF——按「已 commit,合成 stop」(ParalonCloud
//   Rule 2),不重试、不发 error。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

// stubRetryConfig 把 StreamEmptyRetryMax / StreamFirstByteTimeoutMs 覆盖为
// 测试所需值,t.Cleanup 里恢复。
func stubRetryConfig(t *testing.T, retryMax, firstByteTimeoutMs int) {
	t.Helper()
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.StreamEmptyRetryMax = retryMax
		s.StreamFirstByteTimeoutMs = firstByteTimeoutMs
	})
	t.Cleanup(func() {
		config.Update(func(s *config.Snapshot) {
			s.StreamEmptyRetryMax = old.StreamEmptyRetryMax
			s.StreamFirstByteTimeoutMs = old.StreamFirstByteTimeoutMs
		})
	})
}

// stubNativeModel 把 modelID 标记为「已知原生 responses 模型」,让
// claudeMessagesHandler 直接走 forwardClaudeViaResponses,而不是先跌进
// chat 翻译再 probe。
func stubNativeModel(t *testing.T, modelID string) {
	t.Helper()
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[modelID] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		applyConfig(AppConfig{ModelAlias: oldModelAlias})
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, modelID)
		nativeResponsesModels.Unlock()
	})
}

// 一份「正常完结」的最小 responses SSE 流(2 行 text delta + completed)。
const claudeResponsesHealthySSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_ok"}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant"}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"hello"}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

//  1. 首轮空流 → 重试拿到正常流:客户端只看到一条干净的成功流,不应出现
//     error 事件、不应看到前半截残流。这是真正的痛点场景(prefill 阶段
//     tunnel 被宰,直接干净 EOF)。
func TestClaudeResponsesStream_EmptyEOF_RetriesOnce(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubNativeModel(t, "claude-stream-model")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: claudeResponsesHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-stream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content_block_delta"`) || !strings.Contains(body, "hello") {
		t.Fatalf("expected text delta from retry, got:\n%s", body)
	}
	if !strings.Contains(body, `"message_stop"`) {
		t.Fatalf("expected clean message_stop, got:\n%s", body)
	}
	if strings.Contains(body, `"error"`) || strings.Contains(body, "upstream ended stream") {
		t.Fatalf("client must not see retry-induced error:\n%s", body)
	}
}

//  2. 两次都空流:重试额度用尽,客户端收到一条明确的 error 事件(不能
//     静默吞掉、也不能伪造正常收尾)。
func TestClaudeResponsesStream_EmptyEOF_ExhaustsRetryThenErrors(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubNativeModel(t, "claude-stream-model")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-stream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	// 一直未 commit,所以走 forwardClaudeViaResponses 的 fallback,落 502。
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (fallback after retries exhausted), body=%s", rec.Code, rec.Body.String())
	}
}

// 3) 重试关闭(retryMax=0):首轮空流直接落 502,不重试。
func TestClaudeResponsesStream_EmptyEOF_RetryDisabled(t *testing.T) {
	stubRetryConfig(t, 0, 5000)
	stubNativeModel(t, "claude-stream-model")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: ``, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-stream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no retry), body=%s", rec.Code, rec.Body.String())
	}
}

//  4. 已有部分产出后 EOF(话题做了一半被宰):按 ParalonCloud Rule 2 不
//     重试,按现有「合成 stop」逻辑正常关流——既不让 agent 拿半截,也不
//     多达一次重算 input。
func TestClaudeResponsesStream_PartialEOF_SynthesizesStopNoRetry(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubNativeModel(t, "claude-stream-model")

	partialSSE := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_partial"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"partial answer"}` + "\n\n"
	// 故意没有 response.completed ——上游在 text delta 后死掉。

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: partialSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		// 即便是当 retry 候选位准备的一条正常流,也绝不应被发出。
		{status: http.StatusOK, body: claudeResponsesHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-stream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// 半截正文必须送达。
	if !strings.Contains(body, "partial answer") {
		t.Fatalf("expected partial text to reach client:\n%s", body)
	}
	// 合成正常收尾,不发 error。
	if !strings.Contains(body, `"message_stop"`) {
		t.Fatalf("expected synthesized message_stop:\n%s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("must not emit error on partial EOF (already committed):\n%s", body)
	}
	// 重试不应被触发:fallback 槽位里的正常流不应出现在响应里。
	if strings.Contains(body, "hello") {
		t.Fatalf("retry slot must not have been consumed (already-committed stream):\n%s", body)
	}
}

//  5. 仅有 thinking 死流:agent 至少拿到思考内容(reasoningFallback 提升
//     为 text),不发 error。这是「disconnect during thinking」的兜底。
func TestClaudeResponsesStream_ThinkingOnlyEOF_PromotesReasoning(t *testing.T) {
	stubRetryConfig(t, 0, 5000) // 关重试,才能直接观察兜底行为(开着重试会先重试一次)
	stubNativeModel(t, "claude-stream-model")

	thinkingOnlySSE := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_thinking"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"r1","type":"reasoning"}}` + "\n\n" +
		"event: response.reasoning_text.delta\n" +
		`data: {"type":"response.reasoning_text.delta","output_index":0,"item_id":"r1","delta":"let me think"}` + "\n\n"
	// EOF：上游在思考阶段被杀。

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: thinkingOnlySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-stream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// 思考内容必须以 text_delta 形式送达(空回复保护)。
	if !strings.Contains(body, "let me think") {
		t.Fatalf("expected reasoning promoted to text, got:\n%s", body)
	}
	if !strings.Contains(body, `"message_stop"`) {
		t.Fatalf("expected synthesized message_stop:\n%s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("must not emit error when reasoning was promoted:\n%s", body)
	}
}

//  6. 上游返回 200 但首帧就是 response.failed:按未 commit 处理,走重试,
//     而不是把半截 SSE 流甩给客户端。
func TestClaudeResponsesStream_ErrorFrame_Retries(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubNativeModel(t, "claude-stream-model")

	errorOnlySSE := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_bad","status":"failed","error":{"message":"upstream internal"}}}` + "\n\n"

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: errorOnlySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: claudeResponsesHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-stream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hello") {
		t.Fatalf("expected text from retry, got:\n%s", body)
	}
	// 上游的 response.failed 不应直接翻译成 client-visible 的 error 帧
	// (它已被 peek 吞掉并触发重试)。
	if strings.Contains(body, "upstream internal") {
		t.Fatalf("upstream error must not leak to client when retry succeeded:\n%s", body)
	}
}

//  7. 首字节看门狗:上游接受连接后只在窗口内发心跳注释、不发任何 data
//     帧——超过 stream_first_byte_timeout_ms 应触发空流重试。
func TestClaudeResponsesStream_SilentUpstream_TimesOut(t *testing.T) {
	stubRetryConfig(t, 1, 200) // 200ms 首字节超时
	stubNativeModel(t, "claude-stream-model")

	// 上游 body 里只有 SSE 注释行(心跳),没有任何 data: 帧,然后 EOF。
	// peek 会消费完这些注释,在 EOF 处判定「无产出」并触发重试。
	silentBody := ": keepalive\n: keepalive\n: keepalive\n"

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: silentBody, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: claudeResponsesHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-stream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("expected retry to recover stream after silent-upstream:\n%s", rec.Body.String())
	}
}

//  8. 完全死锁(连接接受后一字节都不发,纯挂在 socket 上)——由
//     stream_first_byte_timeout_ms 兜出,不应傻等到外层 ctx 超时。
func TestClaudeResponsesStream_HungUpstream_FirstByteWatchdogFires(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping hung-upstream test in -short mode")
	}
	stubRetryConfig(t, 1, 200) // 200ms 首字节超时
	stubNativeModel(t, "claude-stream-model")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		// 用 slowReader 的 Close-only 行为模拟挂死:Read 永远阻塞,直
		// 到 Close 才返回 EOF。peek 的 timeout 必须先于它触发。
		{status: http.StatusOK, body: "", header: http.Header{"Content-Type": []string{"text/event-stream"}}, customBody: newSlowReader()},
		{status: http.StatusOK, body: claudeResponsesHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-stream-model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("expected retry to recover from hung upstream:\n%s", rec.Body.String())
	}
}
