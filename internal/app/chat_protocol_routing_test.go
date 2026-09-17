package app

import (
	"context"
	"encoding/json"
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
