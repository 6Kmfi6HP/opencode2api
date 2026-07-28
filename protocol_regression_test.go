package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestAnthropicRequestConversionPreservesProtocolSemantics(t *testing.T) {
	zero := 0.0
	topK := 0
	tests := []struct {
		name string
		in   any
		want any
	}{
		{"auto", map[string]any{"type": "auto"}, "auto"},
		{"any", map[string]any{"type": "any"}, "required"},
		{"named", map[string]any{"type": "tool", "name": "weather"}, map[string]any{"type": "function", "function": map[string]any{"name": "weather"}}},
		{"none", map[string]any{"type": "none"}, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ClaudeRequest{Model: "m", MaxTokens: ptr(0), Temperature: &zero, TopP: &zero, TopK: &topK, ToolChoice: tt.in,
				StopSequences: []string{"END"}, Metadata: map[string]any{"user_id": "u-1"}}
			got := convertClaudeRequest(req)
			if !reflect.DeepEqual(got.ToolChoice, tt.want) {
				t.Fatalf("tool choice = %#v, want %#v", got.ToolChoice, tt.want)
			}
			body := convertRequest(&got)
			for key, want := range map[string]any{"max_tokens": 0, "temperature": 0.0, "top_p": 0.0, "top_k": 0, "stop": []string{"END"}, "user": "u-1"} {
				if !reflect.DeepEqual(body[key], want) {
					t.Errorf("%s = %#v, want %#v", key, body[key], want)
				}
			}
		})
	}
}

func TestResponsesNonStreamLengthUsesIncompleteOutcomeEverywhere(t *testing.T) {
	body := convertChatToResponses([]byte(`{"id":"r","created":1,"choices":[{"finish_reason":"length","message":{"content":"partial","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`), "m", false, nil, nil)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "incomplete" || got["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
		t.Fatalf("bad terminal outcome: %s", body)
	}
	for _, item := range got["output"].([]any) {
		if item.(map[string]any)["status"] != "incomplete" {
			t.Fatalf("item completed during truncation: %s", body)
		}
	}
}

func TestResponsesStreamLengthEndsIncompleteAndFunctionDoneHasName(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"reasoning_content":"brief thought"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"partial answer","tool_calls":[{"index":0,"id":"c","function":{"name":"weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`data: [DONE]`, "",
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(upstream)), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", true, nil, nil, ResponsesAPIRequest{})
	out := rr.Body.String()
	if !strings.Contains(out, "event: response.incomplete") || strings.Contains(out, "event: response.completed") {
		t.Fatalf("wrong terminal event:\n%s", out)
	}
	if !strings.Contains(out, `"type":"response.function_call_arguments.done"`) || !strings.Contains(out, `"name":"weather"`) {
		t.Fatalf("function done is incomplete:\n%s", out)
	}
	if !strings.Contains(out, `"incomplete_details":{"reason":"max_output_tokens"}`) {
		t.Fatalf("missing incomplete details:\n%s", out)
	}
	doneCount := 0
	for _, event := range parseSSEEvents(t, out) {
		if event.Name != "response.output_item.done" {
			continue
		}
		doneCount++
		item, _ := event.Data["item"].(map[string]any)
		if item["status"] != "incomplete" {
			t.Fatalf("done item status = %#v, want incomplete:\n%s", item["status"], out)
		}
	}
	if doneCount != 3 {
		t.Fatalf("done items = %d, want reasoning, message, and tool:\n%s", doneCount, out)
	}
}

func TestAnthropicStreamKeepsParallelToolArgumentDeltasOnTheirOwnBlocks(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c0","function":{"name":"first","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c1","function":{"name":"second","arguments":"{\"b\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}},{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`, "",
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(rr, io.NopCloser(strings.NewReader(upstream)), "m", false)

	var starts, deltas []sseEvent
	for _, event := range parseSSEEvents(t, rr.Body.String()) {
		switch event.Name {
		case "content_block_start":
			starts = append(starts, event)
		case "content_block_delta":
			if delta, _ := event.Data["delta"].(map[string]any); delta["type"] == "input_json_delta" {
				deltas = append(deltas, event)
			}
		}
	}
	if len(starts) != 2 {
		t.Fatalf("tool starts = %d, want 2:\n%s", len(starts), rr.Body.String())
	}
	blockByName := map[string]any{}
	for _, event := range starts {
		block := event.Data["content_block"].(map[string]any)
		blockByName[block["name"].(string)] = event.Data["index"]
	}
	wantIndices := []any{blockByName["first"], blockByName["second"], blockByName["first"], blockByName["second"]}
	if len(deltas) != len(wantIndices) {
		t.Fatalf("argument deltas = %d, want %d:\n%s", len(deltas), len(wantIndices), rr.Body.String())
	}
	for i, event := range deltas {
		if event.Data["index"] != wantIndices[i] {
			t.Fatalf("delta %d index = %#v, want %#v:\n%s", i, event.Data["index"], wantIndices[i], rr.Body.String())
		}
	}
}

func TestResponsesStreamAllocatesUniqueIndicesWhenToolPrecedesText(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		`data: [DONE]`, "",
	}, "\n")
	rr := httptest.NewRecorder()
	responsesStreamHandler(rr, nil, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	var added []map[string]any
	for _, block := range strings.Split(rr.Body.String(), "\n\n") {
		if !strings.HasPrefix(block, "event: response.output_item.added") {
			continue
		}
		var event map[string]any
		lines := strings.Split(block, "\n")
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &event); err == nil {
			added = append(added, event)
		}
	}
	if len(added) != 2 || added[0]["output_index"] == added[1]["output_index"] {
		t.Fatalf("indices are not unique: %#v\n%s", added, rr.Body.String())
	}
}

func TestAnthropicContentPreservesTextImageOrderAndToolErrors(t *testing.T) {
	msgs := claudeToOpenAIMessages([]ClaudeMessage{{Role: "user", Content: []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_1", "is_error": true, "content": "boom"},
		map[string]any{"type": "text", "text": "before"},
		map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.test/a.png"}},
		map[string]any{"type": "text", "text": "after"},
	}}}, nil)
	if got := msgs[0].Content; got != "Error: boom" {
		t.Fatalf("tool error = %#v", got)
	}
	parts, ok := msgs[1].Content.([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("content = %#v", msgs[0].Content)
	}
	if parts[0].(map[string]any)["text"] != "before" || parts[1].(map[string]any)["type"] != "image_url" || parts[2].(map[string]any)["text"] != "after" {
		t.Fatalf("order not preserved: %#v", parts)
	}
}

func TestJSONSchemaCleaningReturnsCopyAndPreservesConstraints(t *testing.T) {
	original := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"when": map[string]any{"type": "string", "format": "date-time", "title": "When"}}}
	before, _ := json.Marshal(original)
	clean := cleanJsonSchema(original).(map[string]any)
	after, _ := json.Marshal(original)
	if string(before) != string(after) {
		t.Fatalf("input mutated: before=%s after=%s", before, after)
	}
	if clean["additionalProperties"] != false {
		t.Fatalf("constraint removed: %#v", clean)
	}
	when := clean["properties"].(map[string]any)["when"].(map[string]any)
	if when["format"] != "date-time" {
		t.Fatalf("format removed: %#v", when)
	}
}

func TestChatUsageOnlyChunkIsForwardedWithFullUsage(t *testing.T) {
	line := `data: {"id":"x","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}}}`
	got, usage := convertStreamChunkWithUsage(line, true)
	if got == "" {
		t.Fatal("usage-only chunk was dropped")
	}
	if usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != float64(2) {
		t.Fatalf("usage details lost: %#v", usage)
	}
}

func TestChatResponsePreservesUsageDetailsAndSystemFingerprint(t *testing.T) {
	in := []byte(`{"id":"x","system_fingerprint":"fp_1","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":{"reasoning_tokens":2}}}`)
	out, err := convertResponse(in, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["system_fingerprint"] != "fp_1" {
		t.Fatalf("fingerprint lost: %s", out)
	}
	if got["usage"].(map[string]any)["completion_tokens_details"] == nil {
		t.Fatalf("usage details lost: %s", out)
	}
}
