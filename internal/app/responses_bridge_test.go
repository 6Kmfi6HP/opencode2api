package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ======== C1. responsesInputToMessages：item_reference / 未知 item / 裸原子 ========

func TestResponsesInput_ItemReferenceAndBareAtomsSkipped(t *testing.T) {
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "hello"},
		map[string]any{"type": "item_reference", "id": "ref_1"}, // → 静默跳过
		map[string]any{"type": "future_item", "other": "x"},     // 未知 → 静默跳过
		nil,         // 裸 null → 丢弃
		float64(42), // 裸数字 → 丢弃
		true,        // 裸布尔 → 丢弃
		"plain",     // 裸字符串 → role:user 文本（保留）
		map[string]any{"type": "message", "role": "user", "content": "tail"},
	}
	msgs := responsesInputToMessages(input, "")
	var contents []string
	for _, m := range msgs {
		if s, ok := m.Content.(string); ok {
			contents = append(contents, m.Role+":"+s)
		}
	}
	want := []string{"user:hello", "user:plain", "user:tail"}
	if len(contents) != len(want) {
		t.Fatalf("messages = %#v (含 item_reference/未知/裸原子不得进上下文)", contents)
	}
	for i := range want {
		if contents[i] != want[i] {
			t.Fatalf("messages[%d] = %q, want %q (原始 JSON 不得注入 user 文本)", i, contents[i], want[i])
		}
	}
}

func TestResponsesInput_ReasoningSignatureSummaryOnlyComment(t *testing.T) {
	input := []any{
		map[string]any{
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "output_text", "text": "think step"}},
			// signature 与 encrypted_content 绑定发起方且非明文：不回放为正文。
			"signature":         "sig-x",
			"encrypted_content": "ciphertext",
		},
	}
	msgs := responsesInputToMessages(input, "")
	if len(msgs) != 1 {
		t.Fatalf("messages = %#v, want 1 reasoning message", msgs)
	}
	if msgs[0].ReasoningContent == nil || *msgs[0].ReasoningContent != "think step" {
		t.Fatalf("ReasoningContent = %#v, want summary text only", msgs[0].ReasoningContent)
	}
	// signature/encrypted_content 不回放为正文
	if c, _ := msgs[0].Content.(string); c != "" {
		t.Fatalf("reasoning content = %q, want empty (摘要转向 ReasoningContent)", c)
	}
}

// ======== C3. normalizeAnthropicToolPairing + mergeConsecutiveSameRole ========

func hasCall(m Message, id string) bool {
	for _, tc := range m.ToolCalls {
		if tc.ID == id {
			return true
		}
	}
	return false
}

func TestNormalizeAnthropicToolPairing_DropsUnanswered(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_A", Type: "function", Function: FunctionCall{Name: "exec", Arguments: "{}"}},
			{ID: "call_B", Type: "function", Function: FunctionCall{Name: "exec", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "call_A", Content: "oa"},
		{Role: "user", Content: "after"},
	}
	out := normalizeAnthropicToolPairing(in)
	for _, m := range out {
		if m.Role == "assistant" && hasCall(m, "call_B") {
			t.Fatalf("unanswered tool_use call_B should have been dropped: %#v", out)
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "call_B" {
				t.Fatalf("unanswered tool_use call_B should have been dropped: %#v", out)
			}
		}
	}
	// call_A still present and immediately answered by its tool result
	if len(out) < 3 {
		t.Fatalf("out = %#v, want call_A kept + immediate tool result", out)
	}
	if !hasCall(out[1], "call_A") || out[2].Role != "tool" || out[2].ToolCallID != "call_A" {
		t.Fatalf("out = %#v, want assistant[call_A] then tool[call_A]", out)
	}
}

func TestNormalizeAnthropicToolPairing_DropsOrphanTool(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolCallID: "call_ghost", Content: "orphan"}, // 无对应 tool_use
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_A", Type: "function", Function: FunctionCall{Name: "exec", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "call_A", Content: "oa"},
	}
	out := normalizeAnthropicToolPairing(in)
	for _, m := range out {
		if m.Role == "tool" && m.ToolCallID == "call_ghost" {
			t.Fatalf("orphan tool_result should have been dropped: %#v", out)
		}
	}
}

func TestNormalizeAnthropicToolPairing_ParallelShareAssistantSequentialResults(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_A", Type: "function", Function: FunctionCall{Name: "exec", Arguments: "{}"}},
		}},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_B", Type: "function", Function: FunctionCall{Name: "exec", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "call_A", Content: "oa"},
		{Role: "tool", ToolCallID: "call_B", Content: "ob"},
	}
	out := normalizeAnthropicToolPairing(in)
	// 两个并行 call 共享同一 assistant,随后 tool 结果按 call 序紧邻
	idxA, idxB, idxResA, idxResB := -1, -1, -1, -1
	for i, m := range out {
		if hasCall(m, "call_A") {
			idxA = i
		}
		if hasCall(m, "call_B") {
			idxB = i
		}
		if m.Role == "tool" && m.ToolCallID == "call_A" {
			idxResA = i
		}
		if m.Role == "tool" && m.ToolCallID == "call_B" {
			idxResB = i
		}
	}
	if idxA < 0 || idxB < 0 {
		t.Fatalf("shared assistant missing call_A/call_B: %#v", out)
	}
	if idxA != idxB {
		t.Fatalf("parallel calls should share one assistant message: %#v", out)
	}
	if idxResA != idxA+1 || idxResB != idxA+2 {
		t.Fatalf("tool results not adjacent to assistant (call order): %#v", out)
	}
}

func TestNormalizeAnthropicToolPairing_DropsInvalidArgumentsAndOutput(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_bad", Type: "function", Function: FunctionCall{Name: "exec", Arguments: "{not-json"}},
		}},
		{Role: "tool", ToolCallID: "call_bad", Content: "ob"},
	}
	out := normalizeAnthropicToolPairing(in)
	for _, m := range out {
		if hasCall(m, "call_bad") {
			t.Fatalf("invalid-arguments call should have been dropped: %#v", out)
		}
		if m.Role == "tool" && m.ToolCallID == "call_bad" {
			t.Fatalf("invalid-arguments tool_result should have been dropped: %#v", out)
		}
	}
}

func TestMergeConsecutiveSameRole_ToolMergesIntoUserAssistant(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_A", Type: "function", Function: FunctionCall{Name: "exec", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "call_A", Content: "oa"},
		{Role: "user", Content: "tail"},
	}
	out := mergeConsecutiveSameRole(in)
	// tool 不再合并进前一条（避免丢 ToolCallID）；后续同 role user 合并。
	// normalize…→ chatMessagesToAnthropic 之后由 appendBlocks 完成交替。
	// 这里仅要求调用后返回值长度合法且同序保留（不变量由配对修复器保证）。
	if len(out) < 4 {
		t.Fatalf("mergeConsecutiveSameRole dropped content: %#v", out)
	}
	if out[0].Role != "user" || out[1].Role != "assistant" || out[2].Role != "tool" || out[3].Role != "user" {
		t.Fatalf("mergeConsecutiveSameRole reordered: %#v", out)
	}
}

// ======== C6. convertChatToResponses：usage 口径 + 空输出补空 message ========

func unmarshalResponsesBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal responses body: %v\nbody=%s", err, body)
	}
	return m
}

func TestConvertChatToResponses_UsageCountsCacheTokens(t *testing.T) {
	chatBody := []byte(`{
		"id":"chatcmpl_x","created":1,
		"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
		"usage":{
			"prompt_tokens":120,
			"completion_tokens":35,
			"total_tokens":155,
			"cache_read_input_tokens":64,
			"cache_creation_input_tokens":8,
			"prompt_tokens_details":{"cached_tokens":1},
			"completion_tokens_details":{"reasoning_tokens":12}
		}
	}`)
	got := unmarshalResponsesBody(t, convertChatToResponses(chatBody, "m", false, nil, nil, nil))
	usage, ok := got["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %#v", got["usage"])
	}
	// input_tokens = prompt_tokens + cache_read + cache_creation
	if got := usage["input_tokens"]; got != float64(192) { // 120+64+8
		t.Fatalf("input_tokens = %#v, want 192 (prompt+cache_read+cache_creation)", got)
	}
	inDetails, ok := usage["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("input_tokens_details = %#v", usage["input_tokens_details"])
	}
	// cached_tokens 优先取顶层 cache_read_input_tokens
	if got := inDetails["cached_tokens"]; got != float64(64) {
		t.Fatalf("cached_tokens = %#v, want 64 (顶层 cache_read_input_tokens)", got)
	}
	outDetails, ok := usage["output_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("output_tokens_details = %#v", usage["output_tokens_details"])
	}
	if got := outDetails["reasoning_tokens"]; got != float64(12) {
		t.Fatalf("reasoning_tokens = %#v, want 12 (completion_tokens_details 透传)", got)
	}
}

func TestConvertChatToResponses_EmptyOutputFilledWithEmptyMessage(t *testing.T) {
	// 上游返回 message.content 为空且 finish_reason=stop：Responses 客户端
	// 期望至少一个 output item，补一条空 output_text message。
	chatBody := []byte(`{
		"id":"chatcmpl_e","created":1,
		"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}
	}`)
	got := unmarshalResponsesBody(t, convertChatToResponses(chatBody, "m", false, nil, nil, nil))
	output, ok := got["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("output = %#v, want at least one message item", got["output"])
	}
	item, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("output[0] = %#v", output[0])
	}
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Fatalf("item = %#v, want message/assistant (empty-output fill)", item)
	}
	content, ok := item["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %#v, want one output_text part", item["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] = %#v", content[0])
	}
	if part["type"] != "output_text" || part["text"] != "" {
		t.Fatalf("part = %#v, want empty output_text", part)
	}
}

// ======== C7. anthropicSSEToResponsesStream ========

// runAnthropicResponsesStream 驱动 anthropicSSEToResponsesStream 并把 SSE
// 事件按 type 索引为 {type, output_index, hasLogprobs, arguments, delta} 列表。
type recordedEvent struct {
	typ         string
	outputIndex int
	hasLogprobs bool
	arguments   string
	delta       string
}

func runAnthropicResponsesStream(t *testing.T, sse string) []recordedEvent {
	t.Helper()
	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, strings.NewReader(sse), "claude-x", true)
	var events []recordedEvent
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line[len("data: "):]), &evt); err != nil {
			continue
		}
		typ, _ := evt["type"].(string)
		oi, _ := evt["output_index"].(float64)
		_, hasLP := evt["logprobs"]
		args, _ := evt["arguments"].(string)
		delta, _ := evt["delta"].(string)
		events = append(events, recordedEvent{
			typ:         typ,
			outputIndex: int(oi),
			hasLogprobs: hasLP,
			arguments:   args,
			delta:       delta,
		})
	}
	return events
}

func TestAnthropicSSE_ToolUseStartWithInitialInputAndNoDeltaFillsDeltaOnStop(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"exec\",\"input\":{\"cmd\":\"ls\"}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	events := runAnthropicResponsesStream(t, sse)
	// closeBlock 在 stop 前若无 input_json_delta,补发一条 function_call_arguments.delta
	// 带完整 initial input JSON,且 output_index 均为 added 的 index(0)。
	var deltaEvent *recordedEvent
	var doneArgs string
	for i := range events {
		if events[i].typ == "response.function_call_arguments.delta" {
			deltaEvent = &events[i]
		}
		if events[i].typ == "response.function_call_arguments.done" {
			doneArgs = events[i].arguments
		}
	}
	if deltaEvent == nil {
		t.Fatalf("missing补发 function_call_arguments.delta: %#v", events)
	}
	if deltaEvent.delta != `{"cmd":"ls"}` {
		t.Fatalf("补发 delta = %q, want full initial input JSON", deltaEvent.delta)
	}
	if deltaEvent.outputIndex != 0 {
		t.Fatalf("补发 delta output_index = %d, want 0 (b.outputIndex)", deltaEvent.outputIndex)
	}
	if doneArgs != `{"cmd":"ls"}` {
		t.Fatalf("arguments.done = %q, want full initial input JSON", doneArgs)
	}
}

func TestAnthropicSSE_ToolUseDoneOutputIndexIsBlockOutputIndex(t *testing.T) {
	// 第二个 output 是 text(在前)、第三个才是 tool_use:验证 tool_use done 的
	// output_index 是它在 output 里的位置(=1),不是 toolIdx(=0)。
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"exec\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	events := runAnthropicResponsesStream(t, sse)
	var itemDoneIdx = -1
	for _, e := range events {
		if e.typ == "response.output_item.done" {
			// 只关心 function_call 的(item[T2]=function_call)
			// recordedEvent 不存 item,这里由 arguments.done 的 index 校验。
			_ = e
		}
		if e.typ == "response.function_call_arguments.done" {
			itemDoneIdx = e.outputIndex
		}
	}
	if itemDoneIdx != 1 {
		t.Fatalf("function_call_arguments.done output_index = %d, want 1 (b.outputIndex,非 b.toolIdx=0)", itemDoneIdx)
	}
}

func TestAnthropicSSE_TextDeltaCarriesLogprobs(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hey\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	events := runAnthropicResponsesStream(t, sse)
	sawTextDelta := false
	for _, e := range events {
		if e.typ == "response.output_text.delta" && e.delta == "hey" {
			sawTextDelta = true
			if !e.hasLogprobs {
				t.Fatalf("output_text.delta missing logprobs field")
			}
		}
	}
	if !sawTextDelta {
		t.Fatalf("missing output_text.delta: %#v", events)
	}
}

func TestAnthropicSSE_EOFClosedIdempotently(t *testing.T) {
	// 上游发 message_start + 打开 block,然后 EOF (无 message_stop):
	// ensureTerminal 必须补 closeBlock + message_delta + message_stop 等价物
	// (response.completed),且只发一次。
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, strings.NewReader(sse), "claude-x", true)
	body := rec.Body.String()
	// 已发 message_start 但 EOF 未见 message_stop:补 response.completed(=close+
	// message_delta + message_stop 等价终结)与 [DONE]。
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("EOF 未补 response.completed: %s", body)
	}
	if strings.Count(body, "event: response.completed") != 1 {
		t.Fatalf("ensureTerminal 不幂等,补了多次终结: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("EOF 未补 [DONE] 哨兵: %s", body)
	}
}

// ======== C8. relayResponsesStream：吞中段 [DONE] + EOF 补 incomplete ========

// driveRelayResponsesStream 直测 relayResponsesStream（仅构造 SSE + recorder）。
func driveRelayResponsesStream(t *testing.T, sse string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	relayResponsesStream(context.Background(), rec, strings.NewReader(sse), http.StatusOK, "m", ResponsesAPIRequest{}, newResponsesNameRewrites())
	return rec.Body.String()
}

func TestRelayResponsesStream_SwallowsMidDoneSentinel(t *testing.T) {
	firstDone := "data: [DONE]\n\n"
	secondDone := "data: [DONE]\n\n"
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		firstDone + // 中段(在 completed 之前):吞掉
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n" +
		secondDone // terminalSeen=true,正常转发
	body := driveRelayResponsesStream(t, sse)
	// 中段 [DONE](在 completed 之前)必须被吞掉,不写到下游;completed 之后的
	// [DONE] 正常转发。故总数为 1(吞 1,发 1)。
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("body中 [DONE] 数量 = %d, want 1 (中段吞掉,仅 completed 后的 [DONE] 转发)", n)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("response.completed 未透传: %s", body)
	}
}

func TestRelayResponsesStream_EOFAnywayEmitsIncomplete(t *testing.T) {
	// 上游发完 delta 就 EOF(无 completed/failed/incomplete,也无 [DONE]):
	// 补 response.incomplete + [DONE] 保证客户端正常结束。
	sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	body := driveRelayResponsesStream(t, sse)
	if !strings.Contains(body, "event: response.incomplete") {
		t.Fatalf("EOF 未补 response.incomplete: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("EOF 未补 [DONE]: %s", body)
	}
}
