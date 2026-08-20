package app

import (
	"context"
	"encoding/json"
	"fmt"
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
			got, _ := convertClaudeRequest(req)
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
	body := convertChatToResponses([]byte(`{"id":"r","created":1,"choices":[{"finish_reason":"length","message":{"content":"partial","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`), "m", false, nil, nil, nil)
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
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)

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

func TestPromoteMisplacedReasoningWhenThinkingDisabled(t *testing.T) {
	delta := map[string]any{"reasoning_content": "hello from go gateway"}
	promoteMisplacedReasoning(delta, false)
	if delta["content"] != "hello from go gateway" {
		t.Fatalf("content = %#v, want promoted text", delta["content"])
	}
	if _, ok := delta["reasoning_content"]; ok {
		t.Fatalf("reasoning_content should be removed: %#v", delta)
	}
}

func TestPromoteMisplacedReasoningKeepsCoTWhenThinkingEnabled(t *testing.T) {
	delta := map[string]any{"reasoning_content": "chain of thought"}
	promoteMisplacedReasoning(delta, true)
	if delta["reasoning_content"] != "chain of thought" {
		t.Fatalf("reasoning lost: %#v", delta)
	}
	if _, ok := delta["content"]; ok {
		t.Fatalf("should not promote while thinking enabled: %#v", delta)
	}
}

func TestChatStreamPromotesReasoningToContentWhenThinkingDisabled(t *testing.T) {
	line := `data: {"choices":[{"delta":{"reasoning_content":"2"},"finish_reason":null}]}`
	got, _ := convertStreamChunkWithUsage(line, false)
	if !strings.Contains(got, `"content":"2"`) {
		t.Fatalf("expected promoted content:\n%s", got)
	}
	if strings.Contains(got, "reasoning_content") {
		t.Fatalf("reasoning_content should be stripped:\n%s", got)
	}
}

func TestClaudeStreamPromotesReasoningToTextWhenThinkingDisabled(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"The answer is "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"2"},"finish_reason":"stop"}]}`,
		`data: [DONE]`, "",
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", false)
	out := rr.Body.String()
	if !strings.Contains(out, `"type":"text_delta"`) {
		t.Fatalf("missing text_delta:\n%s", out)
	}
	if strings.Contains(out, `"type":"thinking_delta"`) {
		t.Fatalf("should not emit thinking when keepReasoning=false:\n%s", out)
	}
	if !strings.Contains(out, `"text":"The answer is "`) || !strings.Contains(out, `"text":"2"`) {
		t.Fatalf("missing promoted text:\n%s", out)
	}
}

func TestClaudeStreamFallbackEmitsTextWhenOnlyReasoningWithThinkingEnabled(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"only reasoning"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`, "",
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, io.NopCloser(strings.NewReader(upstream)), "m", true)
	out := rr.Body.String()
	if !strings.Contains(out, `"type":"thinking_delta"`) {
		t.Fatalf("expected thinking while keepReasoning=true:\n%s", out)
	}
	if !strings.Contains(out, `"type":"text_delta"`) || !strings.Contains(out, `"text":"only reasoning"`) {
		t.Fatalf("expected empty-content text fallback:\n%s", out)
	}
}

func TestClaudeNonStreamPromotesEmptyContentFromReasoning(t *testing.T) {
	body := openAIToClaudeResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"2"},"finish_reason":"stop"}]}`), "m", false)
	var got ClaudeResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "2" {
		t.Fatalf("expected promoted text content, got %#v", got.Content)
	}
}

func TestClaudeNonStreamKeepsThinkingAndTextFallbackWhenReasoningEnabled(t *testing.T) {
	body := openAIToClaudeResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"step by step"},"finish_reason":"stop"}]}`), "m", true)
	var got ClaudeResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 2 || got.Content[0].Type != "thinking" || got.Content[0].Thinking != "step by step" {
		t.Fatalf("expected thinking block, got %#v", got.Content)
	}
	if got.Content[1].Type != "text" || got.Content[1].Text != "step by step" {
		t.Fatalf("expected text fallback for agents, got %#v", got.Content)
	}
}

func TestConvertRequestForwardsReasoningEffortAndThinkingBudget(t *testing.T) {
	body := convertRequest(&OpenAIRequest{
		Model:           "m",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "high",
		Thinking:        map[string]any{"type": "enabled", "budget_tokens": 12000},
	})
	if body["reasoning_effort"] != "high" {
		t.Fatalf("effort = %#v, want high", body["reasoning_effort"])
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != 12000 {
		t.Fatalf("thinking = %#v", thinking)
	}
}

func TestConvertRequestDerivesEffortFromBudgetTokens(t *testing.T) {
	body := convertRequest(&OpenAIRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Thinking: map[string]any{"type": "enabled", "budget_tokens": 1000},
	})
	if body["reasoning_effort"] != "low" {
		t.Fatalf("derived effort = %#v, want low", body["reasoning_effort"])
	}
}

func TestBuildOCRequestKeepsReasoningEffort(t *testing.T) {
	body := convertRequest(&OpenAIRequest{
		Model:           "m",
		Messages:        []Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "medium",
		Thinking:        map[string]any{"type": "enabled"},
	})
	req, err := buildOCRequestWithEndpoint("deepseek-v4-flash-free", body, UpstreamAuth{Mode: AuthRoutePublic}, false, "https://opencode.ai")
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
		t.Fatal(err)
	}
	if sent["reasoning_effort"] != "medium" {
		t.Fatalf("upstream body dropped effort: %#v", sent)
	}
}

func TestConvertClaudeRequestMapsOutputConfigEffort(t *testing.T) {
	out, _ := convertClaudeRequest(ClaudeRequest{
		Model:        "m",
		Thinking:     map[string]any{"type": "adaptive"},
		OutputConfig: map[string]any{"effort": "max"},
	})
	if out.ReasoningEffort != "max" {
		t.Fatalf("ReasoningEffort = %q, want max", out.ReasoningEffort)
	}
	thinking, _ := out.Thinking.(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("adaptive should normalize to enabled, got %#v", out.Thinking)
	}
	if thinking["effort"] != "max" {
		t.Fatalf("thinking.effort = %#v, want max", thinking["effort"])
	}

	body := convertRequest(&out)
	if body["reasoning_effort"] != "max" {
		t.Fatalf("upstream reasoning_effort = %#v, want max", body["reasoning_effort"])
	}
	upThinking, _ := body["thinking"].(map[string]any)
	if upThinking["type"] != "enabled" {
		t.Fatalf("upstream thinking = %#v", upThinking)
	}
}

func TestEffortFromOutputConfig(t *testing.T) {
	if got := effortFromOutputConfig(map[string]any{"effort": "max"}); got != "max" {
		t.Fatalf("got %q", got)
	}
	if got := effortFromOutputConfig(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
}

func TestConvertClaudeRequestForwardsThinking(t *testing.T) {
	thinking := map[string]any{"type": "enabled", "budget_tokens": 8000}
	out, _ := convertClaudeRequest(ClaudeRequest{Model: "m", Thinking: thinking})
	got, _ := out.Thinking.(map[string]any)
	if got["type"] != "enabled" || got["budget_tokens"] != 8000 {
		t.Fatalf("thinking not forwarded: %#v", out.Thinking)
	}
}

func TestReasoningEffortMapAppliesAndSurvivesUpstreamBody(t *testing.T) {
	old := reasoningEffortMap
	reasoningEffortMap = map[string]string{"xhigh": "max", "minimal": "low"}
	t.Cleanup(func() { reasoningEffortMap = old })

	body := convertRequest(&OpenAIRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "xhigh", Thinking: map[string]any{"type": "enabled"},
	})
	if body["reasoning_effort"] != "max" {
		t.Fatalf("mapped effort = %#v, want max", body["reasoning_effort"])
	}
	req, err := buildOCRequestWithEndpoint("m", body, UpstreamAuth{Mode: AuthRoutePublic}, false, "https://opencode.ai")
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
		t.Fatal(err)
	}
	if sent["reasoning_effort"] != "max" {
		t.Fatalf("upstream effort = %#v, want max", sent["reasoning_effort"])
	}
}

func TestClaudeSystemMessagesMergedToLeadingSystem(t *testing.T) {
	msgs := claudeToOpenAIMessages([]ClaudeMessage{
		{Role: "user", Content: "hi"},
		{Role: "system", Content: []any{
			map[string]any{"type": "text", "text": "tail system"},
		}},
	}, []any{
		map[string]any{"type": "text", "text": "top system"},
	})
	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2: %#v", len(msgs), msgs)
	}
	if msgs[0].Role != "system" {
		t.Fatalf("first role = %q", msgs[0].Role)
	}
	got, _ := msgs[0].Content.(string)
	if got != "top system\n\ntail system" {
		t.Fatalf("system content = %q", got)
	}
	if msgs[1].Role != "user" {
		t.Fatalf("second role = %q", msgs[1].Role)
	}
	for _, m := range msgs[1:] {
		if m.Role == "system" {
			t.Fatalf("trailing system remains: %#v", msgs)
		}
	}
}

func TestClaudeToolResultImageBecomesFollowupUser(t *testing.T) {
	msgs := claudeToOpenAIMessages([]ClaudeMessage{{
		Role: "user",
		Content: []any{
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": "toolu_1",
				"content": []any{
					map[string]any{"type": "text", "text": "screenshot"},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       "abc",
						},
					},
				},
			},
		},
	}}, nil)
	if len(msgs) < 2 {
		t.Fatalf("want tool + followup user, got %#v", msgs)
	}
	tool := msgs[0]
	if tool.Role != "tool" {
		t.Fatalf("first = %#v", tool)
	}
	text, _ := tool.Content.(string)
	if !strings.Contains(text, "screenshot") || !strings.Contains(text, "[image attached]") {
		t.Fatalf("tool text = %q", text)
	}
	follow := msgs[1]
	if follow.Role != "user" {
		t.Fatalf("followup role = %q", follow.Role)
	}
	parts, ok := follow.Content.([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("followup content = %#v", follow.Content)
	}
	part, _ := parts[0].(map[string]any)
	if part["type"] != "image_url" {
		t.Fatalf("part = %#v", part)
	}
	iu, _ := part["image_url"].(map[string]string)
	if !strings.HasPrefix(iu["url"], "data:image/png;base64,abc") {
		t.Fatalf("url = %#v", iu)
	}
}

func TestClaudeToolChoiceDisableParallelMapsExtraBody(t *testing.T) {
	out, _ := convertClaudeRequest(ClaudeRequest{
		Model: "m",
		ToolChoice: map[string]any{
			"type":                      "auto",
			"disable_parallel_tool_use": true,
		},
	})
	if out.ToolChoice != "auto" {
		t.Fatalf("ToolChoice = %#v", out.ToolChoice)
	}
	if out.ExtraBody["parallel_tool_calls"] != false {
		t.Fatalf("ExtraBody = %#v", out.ExtraBody)
	}
	body := convertRequest(&out)
	if body["parallel_tool_calls"] != false {
		t.Fatalf("upstream body = %#v", body["parallel_tool_calls"])
	}
}

func TestNarrowClaudeMetadataUserKeepsSessionID(t *testing.T) {
	meta := map[string]any{
		"user_id": `{"device_id":"dev-1","session_id":"sess-42"}`,
	}
	out, _ := convertClaudeRequest(ClaudeRequest{Model: "m", Metadata: meta})
	if out.ExtraBody["user"] != "sess-42" {
		t.Fatalf("user = %#v, want sess-42", out.ExtraBody["user"])
	}
	body := convertRequest(&out)
	user, _ := body["user"].(string)
	if user != "sess-42" {
		t.Fatalf("upstream user = %q", user)
	}
	if strings.Contains(user, "device_id") || strings.Contains(fmt.Sprintf("%v", body), "device_id") {
		t.Fatalf("device_id leaked into upstream: %#v", body)
	}
}

func TestClaudeServerToolsSkipped(t *testing.T) {
	tools, skipped := claudeToOpenAITools([]ClaudeTool{
		{Name: "Read", InputSchema: map[string]any{"type": "object"}},
		{Name: "web_search", Type: "web_search_20250305"},
	})
	if len(tools) != 1 || tools[0].Function.Name != "Read" {
		t.Fatalf("tools = %#v", tools)
	}
	if len(skipped) != 1 || skipped[0] != "web_search" {
		t.Fatalf("skipped = %#v", skipped)
	}
	_, skipped2 := convertClaudeRequest(ClaudeRequest{
		Model: "m",
		Tools: []ClaudeTool{{Name: "web_search", Type: "web_search_20250305"}},
	})
	if len(skipped2) != 1 || skipped2[0] != "web_search" {
		t.Fatalf("convert skipped = %#v", skipped2)
	}
}

func TestClaudeCodePayloadDropsContextManagementAndCacheControl(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"max_tokens":32000,
		"stream":true,
		"system":[{"type":"text","text":"You are Claude Code","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"system","content":[{"type":"text","text":"mid system","cache_control":{"type":"ephemeral"}}]}
		],
		"tools":[{"name":"web_search","type":"web_search_20250305"}],
		"metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"s1\"}"},
		"thinking":{"type":"adaptive"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015"}]},
		"output_config":{"effort":"high"},
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.ContextManagement == nil {
		t.Fatal("context_management should bind")
	}
	out, skipped := convertClaudeRequest(req)
	body := convertRequest(&out)
	for _, key := range []string{"context_management", "anthropic-beta"} {
		if _, ok := body[key]; ok {
			t.Fatalf("upstream still has %s: %#v", key, body[key])
		}
	}
	// The gateway now injects its own controlled cache breakpoint for models
	// that accept it (client-supplied cache_control is still dropped; GLM is
	// exempt — see TestConvertRequestSkipsCacheControlForGLM).
	if cc, ok := body["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Fatalf("upstream cache_control = %#v, want {type:ephemeral ttl:1h}", body["cache_control"])
	}
	if retention, _ := body["prompt_cache_retention"].(string); retention != "24h" {
		t.Fatalf("prompt_cache_retention = %#v, want 24h", body["prompt_cache_retention"])
	}
	msgsRaw := body["messages"]
	var msgs []map[string]any
	switch m := msgsRaw.(type) {
	case []map[string]any:
		msgs = m
	case []any:
		for _, item := range m {
			mm, _ := item.(map[string]any)
			msgs = append(msgs, mm)
		}
	default:
		t.Fatalf("messages type = %T %#v", msgsRaw, msgsRaw)
	}
	if len(msgs) < 2 {
		t.Fatalf("messages = %#v", msgs)
	}
	first := msgs[0]
	if first["role"] != "system" {
		t.Fatalf("first message = %#v", first)
	}
	sys, _ := first["content"].(string)
	if !strings.Contains(sys, "You are Claude Code") || !strings.Contains(sys, "mid system") {
		t.Fatalf("merged system = %q", sys)
	}
	for i, mm := range msgs[1:] {
		if mm["role"] == "system" {
			t.Fatalf("trailing system at %d: %#v", i+1, mm)
		}
	}
	if body["user"] != "s1" {
		t.Fatalf("user = %#v", body["user"])
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v", body["parallel_tool_calls"])
	}
	if len(skipped) != 1 || skipped[0] != "web_search" {
		t.Fatalf("skipped = %#v", skipped)
	}
	if n := countClaudeCacheControlBlocks(req); n < 2 {
		t.Fatalf("cache_control_blocks = %d", n)
	}
	summary := summarizeJSONBody(raw, 0)
	if summary["context_management"] != true {
		t.Fatalf("summary missing context_management: %#v", summary)
	}
	if summary["cache_control_blocks"] == nil {
		t.Fatalf("summary missing cache_control_blocks: %#v", summary)
	}
}

// GLM/Zhipu models reject the Anthropic-style cache_control field with
// "Extra inputs are not permitted", so the gateway must not inject the
// breakpoint for them (retention stays injected — it is harmless metadata).
func TestConvertRequestSkipsCacheControlForGLM(t *testing.T) {
	req := OpenAIRequest{
		Model: "glm-5.2",
		Messages: []Message{
			{Role: "user", Content: "hi"},
		},
	}
	body := convertRequest(&req)
	if _, ok := body["cache_control"]; ok {
		t.Fatalf("glm request still has cache_control: %#v", body["cache_control"])
	}
	if retention, _ := body["prompt_cache_retention"].(string); retention != "24h" {
		t.Fatalf("prompt_cache_retention = %#v, want 24h", body["prompt_cache_retention"])
	}
	// Client-supplied explicit values win over the injected default.
	req.ExtraBody = map[string]any{
		"prompt_cache_retention": "in_memory",
		"cache_control":          map[string]any{"type": "ephemeral", "ttl": "5m"},
	}
	body = convertRequest(&req)
	if retention, _ := body["prompt_cache_retention"].(string); retention != "in_memory" {
		t.Fatalf("explicit prompt_cache_retention lost: %#v", body["prompt_cache_retention"])
	}
	if cc, ok := body["cache_control"].(map[string]any); !ok || cc["ttl"] != "5m" {
		t.Fatalf("explicit cache_control lost: %#v", body["cache_control"])
	}
}

func TestResponsesStreamUsageAddsCachedTokensWhenPromptDetailsLackIt(t *testing.T) {
	for _, tt := range []struct {
		name       string
		details    string
		wantCached float64
		wantText   float64
	}{
		{"details with cached tokens", `{"cached_tokens":7,"text_tokens":10}`, 7, 10},
		{"no cached tokens", `{"text_tokens":10}`, 0, 10},
		{"without prompt details", "", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			usageLine := `"prompt_tokens_details":` + tt.details
			if tt.details == "" {
				usageLine = ""
			}
			chunk := `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5`
			if usageLine != "" {
				usageLine = "," + usageLine
			}
			chunk += usageLine + `}}`
			upstream := strings.Join([]string{
				`data: {"id":"r","created":1,"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
				``,
				chunk,
				``,
				`data: [DONE]`,
				``,
			}, "\n")
			rr := httptest.NewRecorder()
			responsesStreamHandler(rr, nil, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "m", "m", false, nil, nil, ResponsesAPIRequest{})
			for _, event := range parseSSEEvents(t, rr.Body.String()) {
				if event.Name != "response.completed" {
					continue
				}
				response, _ := event.Data["response"].(map[string]any)
				usage, _ := response["usage"].(map[string]any)
				details, _ := usage["input_tokens_details"].(map[string]any)
				if int(details["cached_tokens"].(float64)) != int(tt.wantCached) {
					t.Fatalf("cached_tokens = %v, want %v:\n%s", details["cached_tokens"], tt.wantCached, rr.Body.String())
				}
				if tt.wantText == 0 {
					if _, ok := details["text_tokens"]; ok {
						t.Fatalf("unexpected text_tokens = %v", details["text_tokens"])
					}
				} else if int(details["text_tokens"].(float64)) != int(tt.wantText) {
					t.Fatalf("text_tokens = %v, want %v", details["text_tokens"], tt.wantText)
				}
				return
			}
			t.Fatalf("response.completed not found:\n%s", rr.Body.String())
		})
	}
}

func TestResponsesConvertChatUsageAddsCachedTokensWhenPromptDetailsLackIt(t *testing.T) {
	for _, tt := range []struct {
		name       string
		details    string
		wantCached float64
		wantText   float64
	}{
		{"details with cached tokens", `{"cached_tokens":7,"text_tokens":10}`, 7, 10},
		{"no cached tokens", `{"text_tokens":10}`, 0, 10},
		{"without prompt details", "", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			usage := `"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5`
			if tt.details != "" {
				usage += `,"prompt_tokens_details":` + tt.details
			}
			usage += `}`
			in := `{"id":"r","created":1,"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],` + usage + `}`
			var got map[string]any
			if err := json.Unmarshal(convertChatToResponses([]byte(in), "m", false, nil, nil, nil), &got); err != nil {
				t.Fatal(err)
			}
			respUsage, _ := got["usage"].(map[string]any)
			details, _ := respUsage["input_tokens_details"].(map[string]any)
			if int(details["cached_tokens"].(float64)) != int(tt.wantCached) {
				t.Fatalf("cached_tokens = %v, want %v", details["cached_tokens"], tt.wantCached)
			}
			if tt.wantText == 0 {
				if _, ok := details["text_tokens"]; ok {
					t.Fatalf("unexpected text_tokens = %v", details["text_tokens"])
				}
			} else if int(details["text_tokens"].(float64)) != int(tt.wantText) {
				t.Fatalf("text_tokens = %v, want %v", details["text_tokens"], tt.wantText)
			}
		})
	}
}
