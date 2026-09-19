package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

// ======================== 请求转换 ========================

func TestChatToAnthropicBody_BasicMapping(t *testing.T) {
	mt := 512
	req := &OpenAIRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: "system", Content: "be nice"},
			{Role: "user", Content: "hello"},
		},
		MaxTokens:   &mt,
		Temperature: ptr(0.5),
		Tools: []Tool{{
			Type: "function",
			Function: ToolFunction{
				Name:        "weather",
				Description: "get weather",
				Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			},
		}},
		ToolChoice: "auto",
	}
	body := chatToAnthropicBody(req, "claude-x")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["system"] != "be nice" {
		t.Fatalf("system = %#v", got["system"])
	}
	if got["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens = %#v", got["max_tokens"])
	}
	if got["temperature"] != 0.5 {
		t.Fatalf("temperature = %#v", got["temperature"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %#v, want 1 (system extracted)", msgs)
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("role = %#v", msg["role"])
	}
	blocks := msg["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("content blocks = %#v", blocks)
	}
	tools := got["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["name"] != "weather" || tl["input_schema"] == nil {
		t.Fatalf("tool = %#v", tl)
	}
	tc := got["tool_choice"].(map[string]any)
	if tc["type"] != "auto" {
		t.Fatalf("tool_choice = %#v", tc)
	}
}

func TestChatToAnthropicBody_ToolCallsAndResults(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
				ID: "call_1", Type: "function",
				Function: FunctionCall{Name: "weather", Arguments: `{"city":"sf"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
			{Role: "tool", ToolCallID: "call_1", Content: "windy"},
		},
	}
	body := chatToAnthropicBody(req, "claude-x")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %#v, want 3 (tool results merged)", msgs)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("assistant role = %#v", asst["role"])
	}
	asstBlocks := asst["content"].([]any)
	toolUse := asstBlocks[0].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "call_1" || toolUse["name"] != "weather" {
		t.Fatalf("tool_use = %#v", toolUse)
	}
	input := toolUse["input"].(map[string]any)
	if input["city"] != "sf" {
		t.Fatalf("input = %#v", input)
	}
	user := msgs[2].(map[string]any)
	userBlocks := user["content"].([]any)
	if len(userBlocks) != 2 {
		t.Fatalf("tool results should merge into one user message: %#v", userBlocks)
	}
	tr0 := userBlocks[0].(map[string]any)
	if tr0["type"] != "tool_result" || tr0["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result = %#v", tr0)
	}
}

func TestChatToAnthropicBody_BadArgumentsFallback(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "c1", Type: "function",
				Function: FunctionCall{Name: "f", Arguments: `not json`},
			}}},
		},
	}
	body := chatToAnthropicBody(req, "claude-x")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	blocks := msgs[0].(map[string]any)["content"].([]any)
	tu := blocks[0].(map[string]any)
	input := tu["input"].(map[string]any)
	if input["_raw"] != "not json" {
		t.Fatalf("bad arguments fallback = %#v", input)
	}
}

func TestChatToAnthropicBody_MaxTokensDefaultAndCap(t *testing.T) {
	// 缺省兜底 8192。
	req := &OpenAIRequest{Model: "claude-x", Messages: []Message{{Role: "user", Content: "hi"}}}
	body := chatToAnthropicBody(req, "claude-x")
	var got map[string]any
	json.Unmarshal(body, &got)
	if got["max_tokens"] != float64(defaultAnthropicMaxTokens) {
		t.Fatalf("default max_tokens = %#v", got["max_tokens"])
	}

	// per-model cap 生效。
	oldSnap := config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = oldSnap }) })
	config.Update(func(s *config.Snapshot) { s.MaxTokensCapPerModel = map[string]int{"claude-capped": 1000} })
	mt := 5000
	req2 := &OpenAIRequest{Model: "claude-capped", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: &mt}
	body2 := chatToAnthropicBody(req2, "claude-capped")
	var got2 map[string]any
	json.Unmarshal(body2, &got2)
	if got2["max_tokens"] != float64(1000) {
		t.Fatalf("capped max_tokens = %#v, want 1000", got2["max_tokens"])
	}
}

func TestChatToAnthropicBody_EffortToThinking(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-x", Messages: []Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "high",
	}
	body := chatToAnthropicBody(req, "claude-x")
	var got map[string]any
	json.Unmarshal(body, &got)
	thinking := got["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(10240) {
		t.Fatalf("thinking = %#v, want enabled/10240", thinking)
	}

	// ForceDisableThinking → 不注入。
	oldSnap := config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = oldSnap }) })
	config.Update(func(s *config.Snapshot) { s.ForceDisableThinking = true })
	body2 := chatToAnthropicBody(req, "claude-x")
	var got2 map[string]any
	json.Unmarshal(body2, &got2)
	if _, exists := got2["thinking"]; exists {
		t.Fatalf("thinking should be dropped when force-disabled: %#v", got2["thinking"])
	}
}

func TestChatToResponsesBody_BasicMapping(t *testing.T) {
	mt := 256
	req := &OpenAIRequest{
		Model: "gpt-x",
		Messages: []Message{
			{Role: "system", Content: "sys prompt"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call_9", Type: "function",
				Function: FunctionCall{Name: "f", Arguments: `{"a":1}`},
			}}},
			{Role: "tool", ToolCallID: "call_9", Content: "out"},
		},
		MaxTokens:       &mt,
		ReasoningEffort: "medium",
		Tools: []Tool{{
			Type: "function",
			Function: ToolFunction{Name: "f", Description: "d",
				Parameters: map[string]any{"type": "object"}},
		}},
	}
	body := chatToResponsesBody(req, "gpt-x")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["instructions"] != "sys prompt" {
		t.Fatalf("instructions = %#v", got["instructions"])
	}
	if got["max_output_tokens"] != float64(256) {
		t.Fatalf("max_output_tokens = %#v", got["max_output_tokens"])
	}
	if got["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatalf("reasoning = %#v", got["reasoning"])
	}
	tools := got["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["type"] != "function" || tl["name"] != "f" || tl["description"] != "d" {
		t.Fatalf("tool = %#v", tl)
	}
	input := got["input"].([]any)
	// user message + function_call + function_call_output
	var sawCall, sawOutput bool
	for _, item := range input {
		im := item.(map[string]any)
		if im["type"] == "function_call" && im["call_id"] == "call_9" && im["name"] == "f" {
			sawCall = true
		}
		if im["type"] == "function_call_output" && im["call_id"] == "call_9" && im["output"] == "out" {
			sawOutput = true
		}
	}
	if !sawCall || !sawOutput {
		t.Fatalf("input items = %#v (call=%v output=%v)", input, sawCall, sawOutput)
	}
}

// ======================== 非流式响应转换 ========================

func TestConvertResponsesToChat(t *testing.T) {
	resp := `{"id":"resp_1","object":"response","model":"gpt-x","status":"completed",
		"output":[
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking..."}]},
			{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"answer"}]},
			{"type":"function_call","id":"fc1","call_id":"call_5","name":"lookup","arguments":"{\"q\":\"x\"}"}
		],
		"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`
	out := convertResponsesToChat([]byte(resp), "gpt-x", true)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	choices := got["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "answer" {
		t.Fatalf("content = %#v", msg["content"])
	}
	if msg["reasoning_content"] != "thinking..." {
		t.Fatalf("reasoning_content = %#v", msg["reasoning_content"])
	}
	tcs := msg["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_5" {
		t.Fatalf("tool_call id = %#v", tc["id"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", choice["finish_reason"])
	}
	usage := got["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(20) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestConvertResponsesToChat_Incomplete(t *testing.T) {
	resp := `{"id":"r","status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"part"}]}]}`
	out := convertResponsesToChat([]byte(resp), "m", false)
	var got map[string]any
	json.Unmarshal(out, &got)
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish_reason = %#v, want length", choice["finish_reason"])
	}
}

// ======================== 流式转换 ========================

// drainSSEFromHandler 在内存中执行 handler 并返回 SSE 文本。
func drainSSEFromHandler(h func(w http.ResponseWriter)) string {
	rec := httptest.NewRecorder()
	h(rec)
	return rec.Body.String()
}

func TestAnthropicSSEToChatStream(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"ponder\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", true, true)
	})

	// role 首块。
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("missing role chunk: %s", body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("missing text delta: %s", body)
	}
	if !strings.Contains(body, `"reasoning_content":"ponder"`) {
		t.Fatalf("missing reasoning delta: %s", body)
	}
	if !strings.Contains(body, `"name":"f"`) || !strings.Contains(body, `{\"a\":`) || !strings.Contains(body, `1}`) {
		t.Fatalf("missing tool call deltas: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing finish: %s", body)
	}
	// usage 终块（prompt 3 / completion 9）。
	if !strings.Contains(body, `"prompt_tokens":3`) || !strings.Contains(body, `"completion_tokens":9`) {
		t.Fatalf("missing usage block: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
}

// tool_use start 块携带 initial input 且全程无 input_json_delta 时,stop 时
// 必须把 initial input 作为 arguments 兜底 emit 一次,否则 tool call input
// 会整体丢失(用户在 chat 端收到的 tool_calls.function.arguments 是空)。
func TestAnthropicSSEToChatStream_ToolUseInitialInputFallback(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{\"k\":\"v\"}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, false)
	})
	// 首块:arguments 应为空(不等 initial,避免与后续 delta 拼接)。
	// JSON 字段序由 map 序列化决定,这里只断言关键 token 同时存在。
	if !strings.Contains(body, `"name":"f"`) || !strings.Contains(body, `"arguments":""`) {
		t.Fatalf("first chunk should have name and empty arguments: %s", body)
	}
	// stop 兜底:initial input emit一次。
	if !strings.Contains(body, `"arguments":"{\"k\":\"v\"}"`) {
		t.Fatalf("stop fallback should emit initial input as arguments: %s", body)
	}
}

// 同一 tool_use 若 start 带 initial 但 delta 也到达,不能 double-emit
// (OpenAI chat 客户端会 concat 所有 arguments 片段)。
func TestAnthropicSSEToChatStream_ToolUseInitialInputNotDoubledWhenDeltaArrives(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{\"k\":\"v\"}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"other\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, false)
	})
	// delta 到达 → initial 必须被抑制;只能看到 delta 碎片,不能出现 initial 内容。
	if strings.Contains(body, `{\"k\":\"v\"}`) {
		t.Fatalf("initial input leaked into arguments stream when delta was present: %s", body)
	}
	if !strings.Contains(body, `{\"other\":`) {
		t.Fatalf("missing delta chunk: %s", body)
	}
}

// 无参工具(start 块 input={} 且全程无 input_json_delta)在 stop 时必须兜底
// 补 "{}",否则客户端 concat 出的 arguments 是空串 —— 非法 JSON,严格
// OpenAI 客户端 json.Unmarshal 会失败;非流式 buildOpenAIResponse 语义
// 也是 "{}"。
func TestAnthropicSSEToChatStream_ToolUseEmptyInputFallback(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, false)
	})
	// start 块仍先流 "arguments":""(不为空 initial 单独 emit),stop 兜底补 "{}"。
	if !strings.Contains(body, `"arguments":""`) {
		t.Fatalf("first chunk should carry empty arguments: %s", body)
	}
	if !strings.Contains(body, `"arguments":"{}"`) {
		t.Fatalf("stop fallback must emit \"{}\" for zero-argument tool: %s", body)
	}
}

// pipeAnthropicStream 应字节级透传:CRLF 行尾不加额外 \n,事件/空行边界
// 原样保留,客户端收到的 body 与上游 body 完全一致。
func TestPipeAnthropicStream_ByteIdentityPassthrough(t *testing.T) {
	upstreamBody := "event: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\n" +
		"event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\r\n\r\n" +
		"event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"

	// 直通上游响应需要 header,这里绕开 forwardClaudeViaAnthropic 直接调
	// pipeAnthropicStream,用一个空 header 即可。
	rec := httptest.NewRecorder()
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	pipeAnthropicStream(context.Background(), rec, io.NopCloser(strings.NewReader(upstreamBody)), http.StatusOK, header, "m")

	// 字节完全一致。
	if rec.Body.String() != upstreamBody {
		t.Fatalf("response body diverged from upstream:\nupstream: %q\ngot:      %q", upstreamBody, rec.Body.String())
	}
	// header 透传 + WriteHeader 状态。
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Content-Type 应保留 text/event-stream(直通优先级最高)。
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}

// flushCountingRecorder 记录 Flush 次数:httptest.ResponseRecorder.Flush 是
// 空操作,无法暴露"只攒到 EOF 才 Flush"的流式回归,需要真实计数。
type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCountingRecorder) Flush() { f.flushes++ }

// chunkedReader 每次 Read 只吐出一段预设 chunk,模拟上游 SSE 分片到达,
// 让 io.Copy 产生与 chunk 一一对应的 Write。
type chunkedReader struct {
	chunks []string
	i      int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

// 每个上游 chunk 都必须触发一次 Flush,保证事件粒度实时下发 —— 若回到
// 只在 EOF Flush 一次,http.ResponseWriter 的缓冲会把 SSE 攒批,破坏
// 打字机效果(responses_passthrough.go 逐行 Flush 同一约定)。
func TestPipeAnthropicStream_FlushesEachUpstreamChunk(t *testing.T) {
	chunk1 := "event: message_start\r\ndata: {\"type\":\"message_start\"}\r\n\r\n"
	chunk2 := "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	fcr := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	header := http.Header{}
	pipeAnthropicStream(context.Background(), fcr, &chunkedReader{chunks: []string{chunk1, chunk2}}, http.StatusOK, header, "m")

	if fcr.flushes != 2 {
		t.Fatalf("expected one flush per upstream chunk (2), got %d", fcr.flushes)
	}
	if got := fcr.Body.String(); got != chunk1+chunk2 {
		t.Fatalf("byte identity broken: %q", got)
	}
}

// interleaved thinking/thinking/text/text 应在 Responses 侧产生 3 个 output_index,
// 每个 output_item.done 与先前 output_item.added 的 index 完全一致(这是 #3 的
// 回归保障 —— 旧代码按 content_block_stop 递增会让 stop 时 index 与 start 时错位)。
func TestAnthropicSSEToResponsesStream_OutputIndexPerItem(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, io.NopCloser(strings.NewReader(sse)), "claude-x", true)

	// emitEvent 是 map marshal,这里直接解析 data 行做结构化断言;item.id
	// 是随机生成的不写死,依赖下面对 added/done 配对与 index 单调性的检查。
	body := rec.Body.String()
	// added/done 的 output_index 必须与对应 itemID 匹配。
	// 通过解析每个 data JSON 断言 item.id 与 output_index 一一对应。
	type keyedEvent struct {
		OutputIndex int `json:"output_index"`
		Item        struct {
			ID string `json:"id"`
		} `json:"item"`
		Type string `json:"type"`
	}
	addedByItemID := map[string]int{}
	doneByItemID := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e keyedEvent
		if err := json.Unmarshal([]byte(line[6:]), &e); err != nil {
			continue
		}
		switch e.Type {
		case "response.output_item.added":
			addedByItemID[e.Item.ID] = e.OutputIndex
		case "response.output_item.done":
			doneByItemID[e.Item.ID] = e.OutputIndex
		}
	}
	if len(addedByItemID) != 2 || len(doneByItemID) != 2 {
		t.Fatalf("expected 2 added and 2 done, got added=%d done=%d; body:\n%s",
			len(addedByItemID), len(doneByItemID), body)
	}
	// thinking 是 rs_, text 是 msg_;两个都要出现。
	var sawR, sawM bool
	for id := range addedByItemID {
		if strings.HasPrefix(id, "rs_") {
			sawR = true
		}
		if strings.HasPrefix(id, "msg_") {
			sawM = true
		}
	}
	if !sawR || !sawM {
		t.Fatalf("expected one rs_ and one msg_ item, got added: %+v; body:\n%s", addedByItemID, body)
	}
	for id, addedIdx := range addedByItemID {
		doneIdx, ok := doneByItemID[id]
		if !ok {
			t.Fatalf("item %q added with index %d but never done", id, addedIdx)
		}
		if doneIdx != addedIdx {
			t.Fatalf("item %q added with output_index=%d but done with output_index=%d", id, addedIdx, doneIdx)
		}
	}
}

// EOF 断流时 ensureTerminal 应把已流出但未关闭的 text block 内容回填进
// completedOutput 并补 output_text.done / output_item.done。
func TestAnthropicSSEToResponsesStream_EOFPreservesPartialText(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"abc\"}}\n\n"
	// 故意不写 content_block_stop / message_stop,直接 EOF。

	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, io.NopCloser(strings.NewReader(sse)), "claude-x", false)
	body := rec.Body.String()
	// 终结事件必须带着 partial text;否则客户端按 response.completed.output 重组会丢尾。
	if !strings.Contains(body, `"text":"abc"`) {
		t.Fatalf("partial text was not preserved in terminal output: %s", body)
	}
	if !strings.Contains(body, `"type":"response.output_text.done"`) {
		t.Fatalf("missing output_text.done: %s", body)
	}
	if !strings.Contains(body, `"type":"response.output_item.done"`) {
		t.Fatalf("missing output_item.done: %s", body)
	}
}

// EOF 断流时 thinking 块未关闭:ensureTerminal 必须补 reasoning 的
// output_item.done 并把 item 回填进 response.completed.output,否则客户端
// 会永久看到一个 in_progress 的 reasoning item。
func TestAnthropicSSEToResponsesStream_EOFClosesOpenThinking(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"abc\"}}\n\n"
	// 故意不写 content_block_stop / message_stop,直接 EOF。

	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, io.NopCloser(strings.NewReader(sse)), "claude-x", true)
	body := rec.Body.String()

	var addedReasoningID, doneReasoningID string
	completedHasReasoning := false
	for _, e := range parseSSEEvents(t, body) {
		switch e.Name {
		case "response.output_item.added", "response.output_item.done":
			item, _ := e.Data["item"].(map[string]any)
			if typ, _ := item["type"].(string); typ != "reasoning" {
				continue
			}
			if e.Name == "response.output_item.added" {
				addedReasoningID, _ = item["id"].(string)
			} else {
				doneReasoningID, _ = item["id"].(string)
			}
		case "response.completed":
			resp, _ := e.Data["response"].(map[string]any)
			out, _ := resp["output"].([]any)
			for _, item := range out {
				if m, ok := item.(map[string]any); ok && m["type"] == "reasoning" {
					completedHasReasoning = true
				}
			}
		}
	}
	if addedReasoningID == "" {
		t.Fatalf("thinking block was never added: %s", body)
	}
	if doneReasoningID != addedReasoningID {
		t.Fatalf("reasoning item %q added but done %q: EOF close missing: %s", addedReasoningID, doneReasoningID, body)
	}
	if !completedHasReasoning {
		t.Fatalf("response.completed output missing reasoning item: %s", body)
	}
}

// EOF 断流时 tool_use 块未关闭:已流出的 arguments 增量必须用
// function_call_arguments.done / output_item.done 收尾,且 function_call
// item 要进入 response.completed.output,否则部分参数凭空消失。
func TestAnthropicSSEToResponsesStream_EOFClosesOpenToolUse(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n"
	// 故意不写 content_block_stop / message_stop,直接 EOF(arguments 半成品)。

	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, io.NopCloser(strings.NewReader(sse)), "claude-x", false)
	body := rec.Body.String()

	var addedCallID, doneCallID, argsDoneItemID string
	completedHasCall := false
	for _, e := range parseSSEEvents(t, body) {
		switch e.Name {
		case "response.output_item.added", "response.output_item.done":
			item, _ := e.Data["item"].(map[string]any)
			if typ, _ := item["type"].(string); typ != "function_call" {
				continue
			}
			if e.Name == "response.output_item.added" {
				addedCallID, _ = item["id"].(string)
			} else {
				doneCallID, _ = item["id"].(string)
			}
		case "response.function_call_arguments.done":
			argsDoneItemID, _ = e.Data["item_id"].(string)
		case "response.completed":
			resp, _ := e.Data["response"].(map[string]any)
			out, _ := resp["output"].([]any)
			for _, item := range out {
				if m, ok := item.(map[string]any); ok && m["type"] == "function_call" {
					completedHasCall = true
				}
			}
		}
	}
	if addedCallID == "" {
		t.Fatalf("tool_use block was never added: %s", body)
	}
	if argsDoneItemID != addedCallID {
		t.Fatalf("arguments.done item_id %q, want added call %q: %s", argsDoneItemID, addedCallID, body)
	}
	if doneCallID != addedCallID {
		t.Fatalf("function_call %q added but done %q: EOF close missing: %s", addedCallID, doneCallID, body)
	}
	if !completedHasCall {
		t.Fatalf("response.completed output missing function_call item: %s", body)
	}
}

// 同一 index 重复 content_block_start(中间无 stop):立刻 response.failed +
// [DONE] 终止,之后的行一律忽略 —— 任何事件(包括另一个终结帧)都不得写
// 到 [DONE] 之后。
func TestAnthropicSSEToResponsesStream_DuplicateBlockStartTerminates(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"leaked\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, io.NopCloser(strings.NewReader(sse)), "claude-x", false)
	body := rec.Body.String()

	if !strings.Contains(body, `"type":"response.failed"`) {
		t.Fatalf("duplicate content_block_start should fail the stream: %s", body)
	}
	if !strings.Contains(body, `"delta":"first"`) {
		t.Fatalf("pre-failure content missing: %s", body)
	}
	if strings.Contains(body, "leaked") {
		t.Fatalf("events after terminal frame must be suppressed: %s", body)
	}
	parts := strings.SplitAfter(body, "data: [DONE]\n\n")
	if len(parts) < 2 || strings.TrimSpace(parts[1]) != "" {
		t.Fatalf("stream emitted frames after [DONE]: %s", body)
	}
}

func TestAnthropicSSEToChatStream_DropsReasoningWhenDisabled(t *testing.T) {
	sse := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"secret\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, false)
	})
	if strings.Contains(body, "secret") {
		t.Fatalf("reasoning leaked: %s", body)
	}
	if !strings.Contains(body, `"content":"ok"`) {
		t.Fatalf("text missing: %s", body)
	}
}

func TestResponsesSSEToChatStream(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_9\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc1\",\"call_id\":\"call_2\",\"name\":\"find\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc1\",\"delta\":\"{\\\"x\\\":1}\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_9\",\"usage\":{\"input_tokens\":8,\"output_tokens\":5,\"total_tokens\":13}}}\n\n"

	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", true, true)
	})

	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("missing role chunk: %s", body)
	}
	if !strings.Contains(body, `"content":"hel"`) || !strings.Contains(body, `"content":"lo"`) {
		t.Fatalf("missing text deltas: %s", body)
	}
	if !strings.Contains(body, `"name":"find"`) || !strings.Contains(body, `{\"x\":1}`) {
		t.Fatalf("missing tool deltas: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing finish: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":8`) || !strings.Contains(body, `"completion_tokens":5`) {
		t.Fatalf("missing usage block: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
}

// ======================== 分派回归 ========================

// TestDispatch_NoRules_DefaultBehaviorUnchanged 无规则时三个 handler 走既有
// chat 翻译路径。
func TestDispatch_NoRules_DefaultBehaviorUnchanged(t *testing.T) {
	setProtocolRulesForTest(t, nil)
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"c1","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"fallback-model-free","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/chat/completions") {
		t.Fatalf("URL = %s, want chat/completions", transport.requestedURLs[0])
	}
}

// TestDispatch_ChatToAnthropicRule 规则命中 anthropic 时 chat 入站打到
// /zen/v1/messages。
func TestDispatch_ChatToAnthropicRule(t *testing.T) {
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "go-anthropic-*", Protocol: "anthropic"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"via anthropic"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"go-anthropic-model","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "via anthropic") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/messages") {
		t.Fatalf("URL = %s, want /zen/v1/messages", transport.requestedURLs[0])
	}
}

// TestDispatch_ChatToResponsesRule 规则命中 responses 时 chat 入站打到
// /zen/v1/responses。
func TestDispatch_ChatToResponsesRule(t *testing.T) {
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "go-responses-*", Protocol: "responses"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"via responses"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"go-responses-model","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "via responses") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/responses") {
		t.Fatalf("URL = %s, want /zen/v1/responses", transport.requestedURLs[0])
	}
}

// TestDispatch_ClaudeToAnthropicRule 规则命中 anthropic 时 claude 入站直通。
func TestDispatch_ClaudeToAnthropicRule(t *testing.T) {
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "go-anthropic-*", Protocol: "anthropic"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"direct"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"go-anthropic-model","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/messages") {
		t.Fatalf("URL = %s, want /zen/v1/messages", transport.requestedURLs[0])
	}
}

// TestDispatch_ResponsesToAnthropicRule 规则命中 anthropic 时 responses 入站
// 走 /zen/v1/messages（请求为转换后的 Anthropic 形状）。
func TestDispatch_ResponsesToAnthropicRule(t *testing.T) {
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "go-anthropic-*", Protocol: "anthropic"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"resp via anthropic"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"go-anthropic-model","input":"hello there"}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resp via anthropic") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/messages") {
		t.Fatalf("URL = %s, want /zen/v1/messages", transport.requestedURLs[0])
	}
	// 请求体应为 Anthropic 形状（max_tokens 必填兜底）。
	payload := transport.requestPayloads[0]
	if payload["max_tokens"] == nil {
		t.Fatalf("anthropic payload missing max_tokens: %#v", payload)
	}
	if payload["input"] != nil {
		t.Fatalf("anthropic payload should not contain input: %#v", payload)
	}
}

// TestAggregateResponsesStreamToChat_NonStream 免费层强制 stream:true 后，
// responses 上游回 SSE（response.created/output_text.delta/.../completed）；
// 非流式 chat 路径必须先聚合为单个 chat.completion JSON 再透回头端，
// 否则客户端收到的是 SSE 而非 JSON。
func TestAggregateResponsesStreamToChat_NonStream(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_agg\",\"model\":\"muse-spark-1.3-contributor-free\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" world\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_agg\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"

	body := aggregateResponsesStreamToChat([]byte(sse), "muse-spark-1.3-contributor-free", false)

	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("aggregate output must be valid JSON, got: %s", string(body))
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("object = %#v, want chat.completion", out["object"])
	}
	if out["id"] != "resp_agg" {
		t.Fatalf("id = %#v, want resp_agg", out["id"])
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %#v", out["choices"])
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello world" {
		t.Fatalf("content = %#v", msg["content"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %#v", choices[0].(map[string]any)["finish_reason"])
	}
	if usage, ok := out["usage"].(map[string]any); !ok || usage["total_tokens"] != float64(5) {
		t.Fatalf("usage = %#v", out["usage"])
	}

	// 已是 JSON 时幂等（不二次聚合）
	jsonBody := []byte(`{"id":"r1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2}}`)
	if string(aggregateResponsesStreamToChat(jsonBody, "m", false)) != string(jsonBody) {
		t.Fatal("JSON input must pass through unchanged")
	}
}

// TestDispatch_ChatToResponsesMemory 记忆层命中（native-responses registry）
// 时 chat 入站直接打 /zen/v1/responses，不再走 chat/completions + probe 回退。
// 这是 muse-spark-*-contributor[-free] 的修复关键：chat 通道 500 整档拒，
// 只能靠 memory 层预路由绕开。
func TestDispatch_ChatToResponsesMemory(t *testing.T) {
	const model = "muse-spark-1.3-contributor-free"
	setProtocolRulesForTest(t, nil) // 无显式规则，纯记忆层命中
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"via memory"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`},
	})

	// forwardChatViaResponses 仅在客户端非流式时才有可能不强制 stream:true;
	// 但当前实现对 chat→responses 上游仍强制 stream:true（免费层门禁），非流式
	// 客户端靠 aggregateResponsesStreamToChat 把 SSE 还原成 JSON。流式客户端
	// 走 responsesSSEToChatStream，不经过这里。为非流式 sit 起聚合路径的桩数据。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/responses") {
		t.Fatalf("URL = %s, want /zen/v1/responses (memory-routed), got %#v", transport.requestedURLs[0], transport.requestedURLs)
	}
	// 非流式客户端应收到聚合后的 chat.completion JSON（非 SSE）
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("non-stream client must receive JSON, got: %s", rec.Body.String())
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("object = %#v, want chat.completion", out["object"])
	}
	if !strings.Contains(string(rec.Body.Bytes()), "via memory") {
		t.Fatalf("body missing upstream text: %s", rec.Body.String())
	}
}

func TestResponsesSSEToChatStream_ToolArgumentsShareIndex(t *testing.T) {
	sse := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_7\",\"model\":\"gpt-x\"}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_a\",\"call_id\":\"call_a\",\"name\":\"bash\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"fc_a\",\"delta\":\"{\\\"cmd\\\":\\\"free -h\\\"}\"}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":2,\"item\":{\"type\":\"function_call\",\"id\":\"fc_b\",\"call_id\":\"call_b\",\"name\":\"bash\"}}\n\n" +
		// no item_id here: matched by output_index
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":2,\"delta\":\"{\\\"cmd\\\":\\\"uptime\\\"}\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_7\"}}\n\n"

	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		responsesSSEToChatStream(context.Background(), w, strings.NewReader(sse), "gpt-x", false, false)
	})

	names := map[int]string{}
	args := map[int]string{}
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %q", payload)
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				names[tc.Index] += tc.Function.Name
				args[tc.Index] += tc.Function.Arguments
			}
		}
	}

	if len(names) != 2 {
		t.Fatalf("want 2 tool calls, got indices %v (arguments %v): %s", names, args, body)
	}
	if names[0] != "bash" || args[0] != `{"cmd":"free -h"}` {
		t.Fatalf("tool call 0 = %q %q", names[0], args[0])
	}
	if names[1] != "bash" || args[1] != `{"cmd":"uptime"}` {
		t.Fatalf("tool call 1 = %q %q", names[1], args[1])
	}
}

// TestCallOpenCodeEndpoint_UpstreamErrorBody 上游 4xx/5xx 时返回体必须携带
// 原始错误内容（而非裸 "upstream error"），否则客户端只看到网关合成的
// generic 错误，排查上游问题（如 muse-spark contributor 500）时拿不到
// 具体 message。
func TestCallOpenCodeEndpoint_UpstreamErrorBody(t *testing.T) {
	// 400: 明确不可重试，验证 body 原样透出。
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusBadRequest, body: `{"type":"error","error":{"type":"error","message":"muse-spark contributor tier gated"}}`},
	})

	rc, status, _, err := callOpenCodeEndpoint(context.Background(), "chat/completions", []byte(`{"model":"x","messages":[]}`), "x", UpstreamAuth{Mode: AuthRoutePublic})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if err != nil {
		t.Fatalf("err must be nil when a final status is available, got %v", err)
	}
	body, _ := io.ReadAll(rc)
	if rc != nil {
		rc.Close()
	}
	if !strings.Contains(string(body), "muse-spark contributor tier gated") {
		t.Fatalf("upstream error body must be preserved verbatim, got: %s", string(body))
	}
}

// TestWriteUpstreamError_WithUpstreamBody writeUpstreamError 在拿到上
// 游非空错误体时优先透传原始 body 内容，而不是笼统的 "upstream error"。
func TestWriteUpstreamError_WithUpstreamBody(t *testing.T) {
	upstreamBody := `{"type":"error","error":{"type":"error","message":"muse-spark contributor tier gated"}}`
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, http.StatusInternalServerError, &upstreamBodyError{msg: "upstream error", body: []byte(upstreamBody)}, "chat")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response must be JSON, got %s", rec.Body.String())
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["message"] != "muse-spark contributor tier gated" {
		t.Fatalf("message = %v, want upstream text", errObj["message"])
	}
	if errObj["type"] != "error" {
		t.Fatalf("type = %v, want upstream type", errObj["type"])
	}
}
