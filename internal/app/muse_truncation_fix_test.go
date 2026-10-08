package app

// muse-spark-1.3-contributor-free 截断/中断修复的回归测试（对应
// muse-truncation-report.md 的 P1-P6）。
//
// 覆盖：
//   1. 零内容 response.incomplete 空轮（rank 1）：pre-commit 重试（P2）+
//      宽限窗 commit 后的 in-band error（P1）+ 终态 output 收割（P1）。
//   2. 流中途传输死亡（rank 2）：RST/超时不再合成为「正常结束」，补发
//      error 事件（P3）。
//   3. peek 产出感知与壳帧宽限窗（P2 单元级）。
//   4. keypool public+免费模型接管与死池护栏（P5a）。
//   5. 429 重试退避（P5b）。
//   6. chat 非流双重转换（P6）。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// 与用户事故日志 2026-10-07T21:01:36 (njbin81wzu2d) 同参数的零内容
// incomplete 流：created 壳帧 + incomplete(output:[], usage output_tokens=128)。
const museIncompleteEmptySSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_kill","status":"in_progress"}}` + "\n\n" +
	"event: response.incomplete\n" +
	`data: {"type":"response.incomplete","response":{"id":"resp_kill","status":"incomplete","output":[],"usage":{"input_tokens":1704,"output_tokens":128,"input_tokens_details":{"cached_tokens":1649}}}}` + "\n\n"

// 终态 output 里嵌 reasoning（summary + encrypted_content）的 incomplete 流
// ——上游把已产出内容嵌在终态 output、delta 通道为空的形态。
const museIncompleteWithReasoningSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_h","status":"in_progress"}}` + "\n\n" +
	"event: response.incomplete\n" +
	`data: {"type":"response.incomplete","response":{"id":"resp_h","status":"incomplete","output":[{"id":"r1","type":"reasoning","summary":[{"type":"summary_text","text":"hidden thought"}],"encrypted_content":"enc_sig_1"}],"usage":{"input_tokens":10,"output_tokens":128}}}` + "\n\n"

// 终态 output 里嵌 message 文本的 incomplete 流。
const museIncompleteWithMessageSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_m","status":"in_progress"}}` + "\n\n" +
	"event: response.incomplete\n" +
	`data: {"type":"response.incomplete","response":{"id":"resp_m","status":"incomplete","output":[{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered text"}]}],"usage":{"input_tokens":10,"output_tokens":64}}}` + "\n\n"

// stagedReader 按阶段回放数据块（每块前可延迟），最后阶段后返回 err（nil
// 则 EOF）。用于模拟「壳帧先到、终态帧迟到」的时序。
type stagedReader struct {
	chunks []stagedChunk
	pos    int
}

type stagedChunk struct {
	data  string
	delay time.Duration
	err   error
}

func (s *stagedReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.chunks) {
		return 0, io.EOF
	}
	c := s.chunks[s.pos]
	s.pos++
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if c.err != nil {
		return 0, c.err
	}
	n := copy(p, c.data)
	return n, nil
}

func (s *stagedReader) Close() error { return nil }

// timeoutErrReader 模拟 http.Client 墙钟超时的中途死亡：数据回放完后返回
// net.Error（Timeout()=true，非 io.EOF）。
type timeoutErrReader struct {
	data string
	pos  int
}

type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string {
	return "context deadline exceeded (Client.Timeout exceeded while reading body)"
}
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

func (r *timeoutErrReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, fakeNetTimeout{}
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *timeoutErrReader) Close() error { return nil }

// 1) rank 1 核心：零内容 incomplete 空流在 pre-commit 阶段被重试兜住——
// 客户端只看到重试后的成功流，不再收到 200 空消息 + stop_reason=max_tokens。
func TestClaudeResponsesStream_IncompleteEmptyOutput_RetriesBeforeCommit(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubNativeModel(t, "muse-truncation-fix-model")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: museIncompleteEmptySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: claudeResponsesHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"muse-spark-1.3-contributor-free","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hello") || !strings.Contains(body, `"message_stop"`) {
		t.Fatalf("expected retry stream to reach client:\n%s", body)
	}
	// 空轮不得出现：无 error 事件、无 max_tokens 空收尾。
	if strings.Contains(body, `"error"`) {
		t.Fatalf("retry must not leak error event:\n%s", body)
	}
	if strings.Contains(body, `"max_tokens"`) {
		t.Fatalf("empty max_tokens round must not reach client:\n%s", body)
	}
}

// 2) P1：终态 output 里的 reasoning（summary + encrypted_content）被收割——
// thinking_delta 携带 summary 文本、signature_delta 在关块前发出、
// stop_reason=max_tokens，不再丢内容。
func TestClaudeResponsesStream_IncompleteWithReasoning_HarvestsContent(t *testing.T) {
	stubRetryConfig(t, 0, 5000)
	stubNativeModel(t, "muse-truncation-fix-model")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: museIncompleteWithReasoningSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"muse-spark-1.3-contributor-free","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hidden thought") {
		t.Fatalf("harvested reasoning summary must reach client:\n%s", body)
	}
	if !strings.Contains(body, `"signature_delta"`) || !strings.Contains(body, "enc_sig_1") {
		t.Fatalf("encrypted_content must roundtrip via signature_delta:\n%s", body)
	}
	if !strings.Contains(body, `"max_tokens"`) || !strings.Contains(body, `"message_stop"`) {
		t.Fatalf("expected max_tokens stop + message_stop:\n%s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("harvested content means no error event:\n%s", body)
	}
}

// 3) P1：终态 output 里嵌 message 文本的 incomplete 被收割为 text_delta。
func TestClaudeResponsesStream_IncompleteWithMessage_HarvestsText(t *testing.T) {
	stubRetryConfig(t, 0, 5000)
	stubNativeModel(t, "muse-truncation-fix-model")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: museIncompleteWithMessageSSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"muse-spark-1.3-contributor-free","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "recovered text") {
		t.Fatalf("harvested message text must reach client:\n%s", body)
	}
	if !strings.Contains(body, `"max_tokens"`) || !strings.Contains(body, `"message_stop"`) {
		t.Fatalf("expected max_tokens stop + message_stop:\n%s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("harvested content means no error event:\n%s", body)
	}
}

// 4) P1：宽限窗 commit 后才到达的零内容 incomplete 不再静默合成
// 「max_tokens 空轮」——补发 in-band error 让客户端感知并重试该回合。
func TestClaudeResponsesStream_ZeroContentIncompleteAfterGrace_EmitsError(t *testing.T) {
	t.Setenv("OPENCODE2API_SHELL_GRACE_MS", "100")
	stubRetryConfig(t, 0, 5000)
	stubNativeModel(t, "muse-truncation-fix-model")

	// created 立即到，incomplete 延迟 500ms——宽限窗（100ms）先到期 commit，
	// 终态帧在主循环里才被判定为零内容。
	staged := &stagedReader{chunks: []stagedChunk{
		{data: "event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_g","status":"in_progress"}}` + "\n\n"},
		{data: "event: response.incomplete\n" + `data: {"type":"response.incomplete","response":{"id":"resp_g","status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":128}}}` + "\n\n", delay: 500 * time.Millisecond},
	}}
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, customBody: staged, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"muse-spark-1.3-contributor-free","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "generation killed") {
		t.Fatalf("post-commit zero-content incomplete must emit in-band error:\n%s", body)
	}
	if !strings.Contains(body, `"message_stop"`) {
		t.Fatalf("stream must still close protocol-legally:\n%s", body)
	}
}

// 5) P2：壳帧-only 干净 EOF（零内容终态都没有）在 pre-commit 阶段重试。
func TestClaudeResponsesStream_ShellOnlyEOF_Retries(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubNativeModel(t, "muse-truncation-fix-model")

	shellOnlySSE := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_s","status":"in_progress"}}` + "\n\n" +
		"event: response.in_progress\n" +
		`data: {"type":"response.in_progress","response":{"id":"resp_s","status":"in_progress"}}` + "\n\n"

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: shellOnlySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		{status: http.StatusOK, body: claudeResponsesHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"muse-spark-1.3-contributor-free","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hello") || !strings.Contains(body, `"message_stop"`) {
		t.Fatalf("shell-only EOF must retry into healthy stream:\n%s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("retry must not leak error event:\n%s", body)
	}
}

// 6) P3 核心：已 commit 的流中途 RST（非 EOF 传输死亡）不再合成为「正常
// 结束」——截断的 tool_use 后必须补发 error 事件，重试槽位不被消费。
func TestClaudeResponsesStream_RSTMidToolCall_EmitsErrorNotStop(t *testing.T) {
	stubRetryConfig(t, 1, 5000)
	stubNativeModel(t, "muse-truncation-fix-model")

	// created + function_call item + 半截 arguments delta，然后
	// io.ErrClosedPipe（非 EOF 传输死亡）。
	rstBody := &errorReader{
		data: "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_t","status":"in_progress"}}` + "\n\n" +
			"event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc1","type":"function_call","call_id":"call_1","name":"Bash"}}` + "\n\n" +
			"event: response.function_call_arguments.delta\n" +
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc1","delta":"{\"path\": \"/Users/gy` + "\n\n",
	}

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, customBody: rstBody, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		// 已 commit 不可重试：该槽位绝不应被消费。
		{status: http.StatusOK, body: claudeResponsesHealthySSE, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"muse-spark-1.3-contributor-free","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "upstream stream interrupted") {
		t.Fatalf("mid-stream RST must emit error event, not fake end_turn:\n%s", body)
	}
	if strings.Contains(body, "hello") {
		t.Fatalf("committed stream must not consume retry slot:\n%s", body)
	}
}

// 7) P3：http.Client 墙钟超时的中途死亡同样补发 error 事件（errors.Is
// 命中 net.Error 的 Timeout、不命中 io.EOF）。
func TestClaudeResponsesStream_TimeoutKillMidStream_EmitsError(t *testing.T) {
	stubRetryConfig(t, 0, 5000)
	stubNativeModel(t, "muse-truncation-fix-model")

	timeoutBody := &timeoutErrReader{data: "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_to","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"item_id":"m1","delta":"half way"}` + "\n\n"}

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, customBody: timeoutBody, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"muse-spark-1.3-contributor-free","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) || !strings.Contains(body, "upstream stream interrupted") {
		t.Fatalf("timeout kill mid-stream must emit error event:\n%s", body)
	}
}

// 8) P2 单元级：responsesIsProductiveEvent 的壳帧/终态分类矩阵，及
// chat/anthropic hooks 行为不变。
func TestResponsesIsProductiveEvent_Matrix(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"created shell", `{"type":"response.created","response":{"id":"r"}}`, false},
		{"in_progress shell", `{"type":"response.in_progress","response":{"id":"r"}}`, false},
		{"queued shell", `{"type":"response.queued"}`, false},
		{"output_item.added shell", `{"type":"response.output_item.added","output_index":0,"item":{"id":"i","type":"message"}}`, false},
		{"content_part.added shell", `{"type":"response.content_part.added","part":{"type":"output_text"}}`, false},
		{"completed with text output", `{"type":"response.completed","response":{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}}`, true},
		{"completed with bare message", `{"type":"response.completed","response":{"id":"r","output":[{"type":"message"}]}}`, false},
		{"completed empty", `{"type":"response.completed","response":{"id":"r","output":[]}}`, false},
		// 零内容 kill 形态：空 reasoning item 不算产出（usage 照报
		// output_tokens 但无文本；有 summary 文本的 reasoning 算产出，
		// 对齐 harvestTerminalOutput 可收割口径）。
		{"incomplete with bare reasoning", `{"type":"response.incomplete","response":{"id":"r","output":[{"type":"reasoning"}]}}`, false},
		{"incomplete with reasoning summary", `{"type":"response.incomplete","response":{"id":"r","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}]}]}}`, true},
		{"incomplete empty", `{"type":"response.incomplete","response":{"id":"r","output":[]}}`, false},
		{"text delta", `{"type":"response.output_text.delta","delta":"x"}`, true},
		{"arguments delta", `{"type":"response.function_call_arguments.delta","delta":"x"}`, true},
		{"error frame", `{"type":"error","error":{"message":"x"}}`, false},
		{"response.failed", `{"type":"response.failed","response":{"error":{"message":"x"}}}`, false},
		{"[DONE]", "[DONE]", false},
		{"empty", "  ", false},
		{"non-JSON conservative", `oops not json`, true},
	}
	for _, tc := range cases {
		if got := responsesIsProductiveEvent([]byte(tc.payload)); got != tc.want {
			t.Errorf("%s: productive = %v, want %v", tc.name, got, tc.want)
		}
	}
	// chat/anthropic hooks 对任意非 error 非 [DONE] 帧判产出：行为不变
	// （壳帧语义只对 Responses hooks 生效）。
	shell := []byte(`{"type":"response.created","response":{"id":"r"}}`)
	if !chatIsProductiveEvent(shell) {
		t.Errorf("chat hook must keep judging shell frames productive")
	}
	if !anthropicIsProductiveEvent([]byte(`{"type":"ping"}`)) {
		t.Errorf("anthropic hook must keep judging ping productive")
	}
}

// 9) P2 单元级：壳帧宽限窗——壳帧后上游挂住，宽限到期按健康慢流放行
// （commit 而非重试）。
func TestPeekFirstFrameWithGrace_ShellThenHang_CommitsOnGrace(t *testing.T) {
	hang := &stagedReader{chunks: []stagedChunk{
		{data: "event: response.created\n" + `data: {"type":"response.created","response":{"id":"r"}}` + "\n\n"},
		// 下一块永不到达（time.Sleep 超过宽限窗）。
		{data: "", delay: 2 * time.Second},
	}}
	out := PeekFirstFrameWithGrace(context.Background(), hang, 5*time.Second, 50*time.Millisecond, ResponsesProtocolHooks)
	if out.Err != nil {
		t.Fatalf("grace expiry must commit (healthy slow stream), got err=%v", out.Err)
	}
	if out.Reader == nil || len(out.Consumed) == 0 {
		t.Fatalf("expected Reader + Consumed on grace commit, got Reader=%v Consumed=%d", out.Reader, len(out.Consumed))
	}
	out.Reader.Close()
}

// 10) P2 单元级：created + 空终态 + EOF → 未 commit（可重试）。
func TestPeekFirstFrameWithGrace_EmptyIncompleteEOF_NotCommitted(t *testing.T) {
	body := museIncompleteEmptySSE
	out := PeekFirstFrameWithGrace(context.Background(), strings.NewReader(body), 5*time.Second, 50*time.Millisecond, ResponsesProtocolHooks)
	if !errors.Is(out.Err, errStreamIncompleteNoCommit) {
		t.Fatalf("empty incomplete EOF must stay uncommitted (retryable), got err=%v", out.Err)
	}
}

// 11) P2 单元级：产出帧（delta）照常立即 commit，不被宽限窗拖延。
func TestPeekFirstFrameWithGrace_ProductiveDelta_CommitsImmediately(t *testing.T) {
	body := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"x"}` + "\n\n"
	start := time.Now()
	out := PeekFirstFrameWithGrace(context.Background(), strings.NewReader(body), 5*time.Second, 5*time.Second, ResponsesProtocolHooks)
	if out.Err != nil || out.Reader == nil {
		t.Fatalf("productive delta must commit, got err=%v reader=%v", out.Err, out.Reader)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("productive commit must not wait for grace window, took %v", time.Since(start))
	}
	out.Reader.Close()
}

// 12) P2 单元级：grace=0（默认 PeekFirstFrame 路径）保持「任意帧即
// commit」的旧行为——壳帧即 commit。
func TestPeekFirstFrame_NoGrace_KeepsOldShellCommit(t *testing.T) {
	body := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r"}}` + "\n\n"
	out := PeekFirstFrame(context.Background(), strings.NewReader(body), 5*time.Second, ResponsesProtocolHooks)
	if out.Err != nil || out.Reader == nil {
		t.Fatalf("grace=0 must keep old shell-frame commit, got err=%v reader=%v", out.Err, out.Reader)
	}
	out.Reader.Close()
}

// 13) P5a：public + 免费模型交由 keypool 接管（round_robin 轮动）。
func TestKeyPool_PublicFreeModel_PooledTakesOver(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, Strategy: "round_robin", Keys: []UpstreamKey{{Key: "ka"}, {Key: "kb"}}})
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	var got []string
	for range 4 {
		out, keyID, ok := selectPoolKey(auth, "muse-spark-1.3-contributor-free", nil, nil, "")
		if !ok || keyID == "" || out.Token == "" {
			t.Fatalf("public + free model must take over by pool, got ok=%v keyID=%q", ok, keyID)
		}
		got = append(got, keyID)
	}
	if len(got) != 4 {
		t.Fatalf("unexpected picks: %v", got)
	}
}

// 14) P5a：kill-switch（OPENCODE2API_POOL_PUBLIC=off）回退 public 直连。
func TestKeyPool_PublicFreeModel_KillSwitchOff(t *testing.T) {
	t.Setenv("OPENCODE2API_POOL_PUBLIC", "off")
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, Keys: []UpstreamKey{{Key: "ka"}}})
	if _, _, ok := selectPoolKey(UpstreamAuth{Mode: AuthRoutePublic}, "muse-spark-1.3-contributor-free", nil, nil, ""); ok {
		t.Fatal("kill-switch off must restore public direct")
	}
}

// 15) P5a：池整体黑名单级失败（401 长冷却）时回落 public 直连，不用死
// key 硬撞。
func TestKeyPool_PublicFreeModel_HardDeadPoolFallsBack(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, Keys: []UpstreamKey{{Key: "ka"}, {Key: "kb"}}})
	// 每把 key 连续 3 次 401 → 长冷却（blacklistAfter 默认 3）。
	for range 3 {
		reportKeyResult("k1", http.StatusUnauthorized, nil)
		reportKeyResult("k2", http.StatusUnauthorized, nil)
	}
	if _, _, ok := selectPoolKey(UpstreamAuth{Mode: AuthRoutePublic}, "muse-spark-1.3-contributor-free", nil, nil, ""); ok {
		t.Fatal("hard-dead pool must fall back to public direct, not pick a dead key")
	}
}

// 16) P5b：Retry-After 解析与 429 退避计算。
func TestParseRetryAfter_And_Backoff(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"2", 2 * time.Second},
		{"  3 ", 3 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"bogus", 0},
		{"", 0},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// HTTP 日期形态：未来时间取差值。
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 {
		t.Errorf("parseRetryAfter(http-date) = %v, want > 0", got)
	}
	// 退避：Retry-After 优先、递增缺省、cap 5s。
	if got := upstream429Backoff(0, "1"); got != time.Second {
		t.Errorf("retry-after priority: got %v", got)
	}
	if got := upstream429Backoff(2, ""); got != 3*time.Second {
		t.Errorf("incremental default: got %v", got)
	}
	if got := upstream429Backoff(0, "99"); got != 5*time.Second {
		t.Errorf("cap 5s: got %v", got)
	}
}

// 17) P5b：上游 429 后退避再重试（注入 5ms 退避观测），最终拿到 200。
func TestCallOpenCodeEndpoint_Rate429_BackoffBeforeRetry(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusTooManyRequests, body: `{"error":{"code":"rate_limit_exceeded","message":"Output token rate limit exceeded. Please retry after a brief wait."}}`},
		{status: http.StatusOK, body: `{"ok":true}`},
	})
	old := upstream429Backoff
	slept := false
	upstream429Backoff = func(attempt int, retryAfter string) time.Duration {
		slept = true
		return 5 * time.Millisecond
	}
	t.Cleanup(func() { upstream429Backoff = old })

	rc, status, _, err := callOpenCodeEndpoint(context.Background(), "responses",
		[]byte(`{"model":"test-model","input":[]}`), "test-model", UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-x"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("429 backoff retry must succeed, got status=%d err=%v", status, err)
	}
	rc.Close()
	if !slept {
		t.Fatal("429 must back off before retry")
	}
}

// 18) P3b：上游墙钟超时可配置（env 覆盖、0 关闭、非法回退默认）。
func TestUpstreamTimeoutConfig(t *testing.T) {
	t.Setenv("OPENCODE2API_UPSTREAM_TIMEOUT_SECS", "5")
	if got := upstreamTimeout(); got != 5*time.Second {
		t.Errorf("env override: got %v, want 5s", got)
	}
	t.Setenv("OPENCODE2API_UPSTREAM_TIMEOUT_SECS", "0")
	if got := upstreamTimeout(); got != 0 {
		t.Errorf("env 0 disables wall clock: got %v", got)
	}
	t.Setenv("OPENCODE2API_UPSTREAM_TIMEOUT_SECS", "bogus")
	if got := upstreamTimeout(); got != 900*time.Second {
		t.Errorf("invalid falls back to default 900s: got %v", got)
	}
	t.Setenv("OPENCODE2API_UPSTREAM_TIMEOUT_SECS", "")
	if got := upstreamTimeout(); got != 900*time.Second {
		t.Errorf("unset default 900s: got %v", got)
	}
	if httpClient.Timeout != upstreamTimeout() {
		t.Errorf("httpClient.Timeout %v must track upstreamTimeout()", httpClient.Timeout)
	}
}

// 19) P6：chat 非流分支聚合已产出 chat.completion 时不做第二次转换——
// 正文不再被抹空。
func TestForwardChatViaResponses_NonStream_NoDoubleConversion(t *testing.T) {
	sse := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_c","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"HELLO-FROM-UPSTREAM"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_c","status":"completed","usage":{"input_tokens":5,"output_tokens":185}}}` + "\n\n" +
		"data: [DONE]\n\n"
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})

	w := httptest.NewRecorder()
	req := &domain.OpenAIRequest{
		Model:    "muse-spark-1.3-contributor-free",
		Messages: []domain.Message{{Role: "user", Content: "hi"}},
	}
	forwardChatViaResponses(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), UpstreamAuth{Mode: AuthRoutePublic}, req, false)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "HELLO-FROM-UPSTREAM") {
		t.Fatalf("aggregated content must survive double-conversion guard:\n%s", body)
	}
	if !strings.Contains(body, `"completion_tokens":185`) {
		t.Fatalf("usage must be relayed:\n%s", body)
	}
}

// 20) P4 单元级：无终态帧残缺 SSE 的聚合与壳帧-only error 兜底已在
// TestExtractResponsesJsonFromSse 覆盖；这里补 chat.completion 形判定。
func TestIsChatCompletionBody(t *testing.T) {
	if !isChatCompletionBody([]byte(`{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant"}}]}`)) {
		t.Fatal("aggregator product must be detected")
	}
	if isChatCompletionBody([]byte(`{"id":"x","status":"completed","output":[]}`)) {
		t.Fatal("native responses shape must not be detected as chat.completion")
	}
	if isChatCompletionBody([]byte(`not json`)) {
		t.Fatal("non-JSON must not be detected")
	}
}
