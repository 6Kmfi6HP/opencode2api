package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test_ResponsesHandler_uses_previous_response_id_to_replay_prior_tool_call_context(t *testing.T) {
	// Given
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{
			status: http.StatusOK,
			body:   `{"id":"resp_prev","choices":[{"message":{"tool_calls":[{"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			status: http.StatusOK,
			body:   `{"id":"resp_next","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`,
		},
	})

	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":"edit file",
		"tools":[{"type":"apply_patch"}]
	}`))
	firstRec := httptest.NewRecorder()

	// When
	responsesHandler(firstRec, firstReq)

	var firstResp map[string]any
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("unmarshal first response: %v", err)
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"previous_response_id":"resp_prev",
		"input":[
			{
				"type":"apply_patch_call_output",
				"call_id":"call_patch",
				"output":"patch applied",
				"status":"completed"
			}
		]
	}`))
	secondRec := httptest.NewRecorder()
	responsesHandler(secondRec, secondReq)

	// Then
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d, body=%s", secondRec.Code, http.StatusOK, secondRec.Body.String())
	}
	if len(transport.requestPayloads) != 2 {
		t.Fatalf("request payload count = %d, want 2", len(transport.requestPayloads))
	}

	messages, ok := transport.requestPayloads[1]["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %#v, want array", transport.requestPayloads[1]["messages"])
	}
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2, messages=%#v", len(messages), messages)
	}

	assistantMessage, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("assistant message = %#v, want object", messages[0])
	}
	toolCalls, ok := assistantMessage["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %#v, want one prior tool call", assistantMessage["tool_calls"])
	}
	toolCall, ok := toolCalls[0].(map[string]any)
	if !ok {
		t.Fatalf("tool_call = %#v, want object", toolCalls[0])
	}
	function, ok := toolCall["function"].(map[string]any)
	if !ok {
		t.Fatalf("function = %#v, want object", toolCall["function"])
	}
	if got := function["name"]; got != "apply_patch" {
		t.Fatalf("function.name = %#v, want apply_patch", got)
	}

	toolMessage, ok := messages[1].(map[string]any)
	if !ok {
		t.Fatalf("tool message = %#v, want object", messages[1])
	}
	if got := toolMessage["role"]; got != "tool" {
		t.Fatalf("tool message role = %#v, want tool", got)
	}
	if got := toolMessage["tool_call_id"]; got != "call_patch" {
		t.Fatalf("tool message tool_call_id = %#v, want call_patch", got)
	}
	if got := toolMessage["content"]; got != "patch applied" {
		t.Fatalf("tool message content = %#v, want patch applied", got)
	}

	if got, ok := transport.requestPayloads[1]["tools"].([]any); !ok || len(got) != 1 {
		t.Fatalf("second request tools = %#v, want prior tools reused", transport.requestPayloads[1]["tools"])
	}
}

func Test_ResponsesHandler_replays_previous_response_id_context_preserving_original_tool_definitions(t *testing.T) {
	// Given
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{
			status: http.StatusOK,
			body:   `{"id":"resp_tools","choices":[{"message":{"tool_calls":[{"id":"call_shell","type":"function","function":{"name":"shell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			status: http.StatusOK,
			body:   `{"id":"resp_done","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		},
	})
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":"inspect repo",
		"tools":[{"type":"shell"}]
	}`))
	firstRec := httptest.NewRecorder()

	// When
	responsesHandler(firstRec, firstReq)
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"previous_response_id":"resp_tools",
		"input":[{"type":"shell_call_output","call_id":"call_shell","output":"ok","status":"completed"}]
	}`))
	secondRec := httptest.NewRecorder()
	responsesHandler(secondRec, secondReq)

	// Then
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d, body=%s", secondRec.Code, http.StatusOK, secondRec.Body.String())
	}
	gotTools, ok := transport.requestPayloads[1]["tools"].([]any)
	if !ok || len(gotTools) != 1 {
		t.Fatalf("second request tools = %#v, want preserved tool list", transport.requestPayloads[1]["tools"])
	}
	gotTool, ok := gotTools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v, want object", gotTools[0])
	}
	if got := gotTool["type"]; got != "function" {
		t.Fatalf("tool.type = %#v, want function", got)
	}
	function, ok := gotTool["function"].(map[string]any)
	if !ok {
		t.Fatalf("function = %#v, want object", gotTool["function"])
	}
	if got := function["name"]; got != "shell" {
		t.Fatalf("function.name = %#v, want shell", got)
	}
}
