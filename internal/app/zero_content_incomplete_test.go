package app

// 零内容 incomplete（免费档限速把生成杀在不可见阶段）回归：delta 通道为
// 空、终态 output 无可交付内容时，chat→responses 翻译与 responses 原生透传
// 都必须发 error 而非正常收尾；peek 把“有 item 无内容”的终态判为非产出。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// 限速 kill 形态：created + 零内容 incomplete（output 空数组），无 delta。
const zeroContentKillStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_kill\",\"model\":\"test-resp\"}}\n\n" +
	"event: response.incomplete\n" +
	"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_kill\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"

// 另一 kill 形态：output 带空 reasoning item（有 item、无文本），旧 peek
// 按 len(output)>0 会误判产出而 commit。
const zeroContentKillReasoningStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_kill2\",\"model\":\"test-resp\"}}\n\n" +
	"event: response.incomplete\n" +
	"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_kill2\",\"status\":\"incomplete\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"

// 正常 tool_call 回合：output 只有 function_call，必须仍判产出（不得误杀）。
const toolCallOnlyTerminalPayload = `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","name":"Bash","arguments":"{}","call_id":"call_1"}]}}`

func TestZeroContentIncomplete_ProductiveEvent(t *testing.T) {
	cases := []struct {
		name       string
		payload    string
		productive bool
	}{
		{"empty output array", `{"type":"response.incomplete","response":{"status":"incomplete","output":[]}}`, false},
		{"reasoning without summary text", `{"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"reasoning","id":"rs_1","summary":[]}]}}`, false},
		{"reasoning with summary text", `{"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"hidden thought"}]}]}}`, true},
		{"message with empty content", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[]}]}}`, false},
		{"message with text", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}}`, true},
		{"tool call only", toolCallOnlyTerminalPayload, true},
		{"shell frame", `{"type":"response.created","response":{"id":"x"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesIsProductiveEvent([]byte(tc.payload)); got != tc.productive {
				t.Fatalf("responsesIsProductiveEvent = %v, want %v", got, tc.productive)
			}
		})
	}
}

// chat→responses 翻译：可提交但无正文（arguments 增量不算正文产出）后的
// 零内容 incomplete 不得按 length 正常收尾，必须发 upstream_truncated
// error 帧 + [DONE]（已 commit，重试无意义；客户端感知失败重试该回合）。
const committedZeroContentKillStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_kill3\",\"model\":\"test-resp\"}}\n\n" +
	"event: response.function_call_arguments.delta\n" +
	"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{}\"} \n\n" +
	"event: response.incomplete\n" +
	"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_kill3\",\"status\":\"incomplete\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"

// 纯零内容 kill（连壳帧后直接终态）：宽限 peek 应判非产出而换 key 重试，
// 客户端只看到重试后的正常流。
func TestChatViaResponses_ZeroContentKill_Retried(t *testing.T) {
	rec, transport := runChatViaResponsesStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: zeroContentKillStream},
		{status: http.StatusOK, body: chatViaResponsesSampleStream},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("expected retried healthy stream, got:\n%s", body)
	}
	if strings.Contains(body, "upstream_truncated") {
		t.Fatalf("retry should swallow the empty kill, got error: %s", body)
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (kill + retry); urls=%v", got, transport.requestedURLs)
	}
}

func TestChatViaResponses_ZeroContentIncomplete_EmitsError(t *testing.T) {
	rec, transport := runChatViaResponsesStream(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: committedZeroContentKillStream},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_truncated") {
		t.Fatalf("missing upstream_truncated error frame: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("must not synthesize length finish for zero-content kill: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
	_ = transport
}

// responses 原生透传：纯零内容 kill 走宽限 peek 重试（客户端只看到
// 重试后的正常流）；可提交后（有 delta 产出）的零内容 incomplete 才走
// error 事件。本用例覆盖后者：先给一段真实 delta 促成 commit，再给空终态。
const committedPassthroughKillStream = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_kill4\",\"model\":\"test-resp\"}}\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n" +
	"event: response.incomplete\n" +
	"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_kill4\",\"status\":\"incomplete\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"

func TestResponsesPassthrough_ZeroContentIncomplete_EmitsError(t *testing.T) {
	t.Helper()
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "test-pt-*", Protocol: "responses"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: committedPassthroughKillStream},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"test-pt-model","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_truncated") {
		t.Fatalf("missing upstream_truncated error event: %s", body)
	}
	if strings.Contains(body, `"status":"incomplete","incomplete_details"`) || strings.Contains(body, `"status": "incomplete"`) {
		t.Fatalf("must not write through empty incomplete: %s", body)
	}
	if got := len(transport.requestedURLs); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no continuation burn on empty kill); urls=%v", got, transport.requestedURLs)
	}
}

// responses 原生透传：空 reasoning item 的 kill 同样走 error（旧 peek 按
// item 数会误 commit）。
func TestResponsesPassthrough_ReasoningOnlyKill_EmitsError(t *testing.T) {
	t.Helper()
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "test-pt2-*", Protocol: "responses"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: zeroContentKillReasoningStream},
		{status: http.StatusOK, body: chatViaResponsesSampleStream},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"test-pt2-model","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	body := rec.Body.String()
	// peek 应判非产出而换 key 重试：客户端只看到第二轮正常流（透传原样
	// 写上游行，断言 delta 而非翻译后的 content），无 error。
	if !strings.Contains(body, `"delta":"hi"`) {
		t.Fatalf("expected retried healthy stream, got:\n%s", body)
	}
	if strings.Contains(body, "upstream_truncated") {
		t.Fatalf("retry should swallow the empty kill, got error: %s", body)
	}
	if got := len(transport.requestedURLs); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (kill + retry); urls=%v", got, transport.requestedURLs)
	}
}
