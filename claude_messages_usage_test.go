package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type sseEvent struct {
	Name string
	Data map[string]any
}

func Test_ClaudeMessages_nonstream_includes_context_usage_when_upstream_provides_details(t *testing.T) {
	// Given
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"chatcmpl_test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":35,"cache_creation_input_tokens":8,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":12},"server_tool_use":{"web_search_requests":1},"service_tier":"priority","inference_geo":"us"}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"primary-model","messages":[]}`))
	rec := httptest.NewRecorder()

	// When
	claudeMessagesHandler(rec, req)

	// Then
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	usage, ok := body["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing or wrong type: %#v", body["usage"])
	}
	if got := int(usage["input_tokens"].(float64)); got != 120 {
		t.Fatalf("input_tokens = %d, want 120", got)
	}
	if got := int(usage["output_tokens"].(float64)); got != 35 {
		t.Fatalf("output_tokens = %d, want 35", got)
	}
	if got := int(usage["cache_creation_input_tokens"].(float64)); got != 8 {
		t.Fatalf("cache_creation_input_tokens = %d, want 8", got)
	}
	if got := int(usage["cache_read_input_tokens"].(float64)); got != 64 {
		t.Fatalf("cache_read_input_tokens = %d, want 64", got)
	}
	outputDetails, ok := usage["output_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("output_tokens_details missing or wrong type: %#v", usage["output_tokens_details"])
	}
	if got := int(outputDetails["reasoning_tokens"].(float64)); got != 12 {
		t.Fatalf("output_tokens_details.reasoning_tokens = %d, want 12", got)
	}
	serverToolUse, ok := usage["server_tool_use"].(map[string]any)
	if !ok {
		t.Fatalf("server_tool_use missing or wrong type: %#v", usage["server_tool_use"])
	}
	if got := int(serverToolUse["web_search_requests"].(float64)); got != 1 {
		t.Fatalf("server_tool_use.web_search_requests = %d, want 1", got)
	}
	if got := usage["service_tier"]; got != "priority" {
		t.Fatalf("service_tier = %#v, want priority", got)
	}
	if got := usage["inference_geo"]; got != "us" {
		t.Fatalf("inference_geo = %#v, want us", got)
	}
}

func Test_ClaudeMessages_stream_emits_cumulative_context_usage_when_final_usage_arrives(t *testing.T) {
	// Given
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body: strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":""}]}`,
			``,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":35,"cache_creation_input_tokens":8,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":12}}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"primary-model","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()

	// When
	claudeMessagesHandler(rec, req)

	// Then
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	events := parseSSEEvents(t, rec.Body.String())
	var usage map[string]any
	for _, event := range events {
		if event.Name != "message_delta" {
			continue
		}
		gotUsage, ok := event.Data["usage"].(map[string]any)
		if !ok {
			t.Fatalf("message_delta usage missing: %#v", event.Data["usage"])
		}
		usage = gotUsage
	}
	if usage == nil {
		t.Fatalf("message_delta event not found in stream: %s", rec.Body.String())
	}
	if got := int(usage["input_tokens"].(float64)); got != 120 {
		t.Fatalf("input_tokens = %d, want 120", got)
	}
	if got := int(usage["output_tokens"].(float64)); got != 35 {
		t.Fatalf("output_tokens = %d, want 35", got)
	}
	if got := int(usage["cache_creation_input_tokens"].(float64)); got != 8 {
		t.Fatalf("cache_creation_input_tokens = %d, want 8", got)
	}
	if got := int(usage["cache_read_input_tokens"].(float64)); got != 64 {
		t.Fatalf("cache_read_input_tokens = %d, want 64", got)
	}
	outputDetails, ok := usage["output_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("output_tokens_details missing or wrong type: %#v", usage["output_tokens_details"])
	}
	if got := int(outputDetails["reasoning_tokens"].(float64)); got != 12 {
		t.Fatalf("output_tokens_details.reasoning_tokens = %d, want 12", got)
	}
}

func parseSSEEvents(t *testing.T, body string) []sseEvent {
	t.Helper()

	blocks := strings.Split(body, "\n\n")
	events := make([]sseEvent, 0, len(blocks))
	for _, block := range blocks {
		trimmed := strings.TrimSpace(block)
		if trimmed == "" {
			continue
		}

		var name string
		var payload string
		for _, line := range strings.Split(trimmed, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				payload = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || payload == "" {
			continue
		}

		var data map[string]any
		if err := json.Unmarshal([]byte(payload), &data); err != nil {
			t.Fatalf("unmarshal SSE payload %q: %v", payload, err)
		}
		events = append(events, sseEvent{Name: name, Data: data})
	}

	return events
}
