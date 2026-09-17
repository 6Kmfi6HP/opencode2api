package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertAnthropicToResponses(t *testing.T) {
	anthropic := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
		"content":[{"type":"text","text":"answer here"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":4,"output_tokens":6}}`
	out := convertAnthropicToResponses([]byte(anthropic), "claude-x", false)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["object"] != "response" {
		t.Fatalf("object = %#v", got["object"])
	}
	output := got["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %#v", output)
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" {
		t.Fatalf("item type = %#v", item["type"])
	}
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "answer here" {
		t.Fatalf("part = %#v", part)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(6) {
		t.Fatalf("usage = %#v (Responses 口径应保留 input/output_tokens)", usage)
	}
}

func TestConvertAnthropicToResponses_ToolUse(t *testing.T) {
	anthropic := `{"id":"msg_2","type":"message","role":"assistant","model":"claude-x",
		"content":[{"type":"tool_use","id":"toolu_9","name":"lookup","input":{"q":"x"}}],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":2,"output_tokens":3}}`
	out := convertAnthropicToResponses([]byte(anthropic), "claude-x", false)
	var got map[string]any
	json.Unmarshal(out, &got)
	if got["status"] != "incomplete" {
		// convertChatToResponses: finish_reason=tool_calls → completed（非 length）。
		if got["status"] != "completed" {
			t.Fatalf("status = %#v", got["status"])
		}
	}
	output := got["output"].([]any)
	var sawCall bool
	for _, item := range output {
		im := item.(map[string]any)
		if im["type"] == "function_call" {
			sawCall = true
		}
	}
	if !sawCall {
		t.Fatalf("output missing function_call: %#v", output)
	}
}

func TestAnthropicSSEToResponsesStream(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hey\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"k\\\":1}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, strings.NewReader(sse), "claude-x", true)
	body := rec.Body.String()

	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_item.done",
		"event: response.function_call_arguments.delta",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing event %q; body=%s", want, body)
		}
	}
	if !strings.Contains(body, `"delta":"hey"`) {
		t.Fatalf("missing text delta payload: %s", body)
	}
	if !strings.Contains(body, `{\"k\":1}`) {
		t.Fatalf("missing arguments delta: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing [DONE]: %s", body)
	}
	// sequence_number 递增。
	if !strings.Contains(body, `"sequence_number":1`) {
		t.Fatalf("missing sequence_number: %s", body)
	}
}

func TestAnthropicSSEToResponsesStream_IncompleteOnMaxTokens(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rec := httptest.NewRecorder()
	anthropicSSEToResponsesStream(context.Background(), rec, strings.NewReader(sse), "claude-x", false)
	body := rec.Body.String()
	if !strings.Contains(body, "response.incomplete") {
		t.Fatalf("missing incomplete: %s", body)
	}
	if !strings.Contains(body, `"reason":"max_output_tokens"`) {
		t.Fatalf("missing incomplete_details: %s", body)
	}
}
