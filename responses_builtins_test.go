package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test_ResponsesHandler_converts_built_in_tools_into_upstream_function_tools(t *testing.T) {
	tests := []struct {
		name     string
		toolJSON string
		wantName string
	}{
		{
			name:     "apply_patch",
			toolJSON: `{"type":"apply_patch"}`,
			wantName: "apply_patch",
		},
		{
			name:     "shell",
			toolJSON: `{"type":"shell"}`,
			wantName: "shell",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Given
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
				status: http.StatusOK,
				body:   `{"id":"chatcmpl_test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
			}})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
				"model":"primary-model",
				"input":"inspect repo",
				"tools":[`+tt.toolJSON+`]
			}`))
			rec := httptest.NewRecorder()

			// When
			responsesHandler(rec, req)

			// Then
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if len(transport.requestPayloads) != 1 {
				t.Fatalf("request payload count = %d, want 1", len(transport.requestPayloads))
			}
			tools, ok := transport.requestPayloads[0]["tools"].([]any)
			if !ok || len(tools) != 1 {
				t.Fatalf("tools = %#v, want one synthetic function tool", transport.requestPayloads[0]["tools"])
			}
			tool, ok := tools[0].(map[string]any)
			if !ok {
				t.Fatalf("tool = %#v, want object", tools[0])
			}
			if got := tool["type"]; got != "function" {
				t.Fatalf("tool.type = %#v, want function", got)
			}
			function, ok := tool["function"].(map[string]any)
			if !ok {
				t.Fatalf("function = %#v, want object", tool["function"])
			}
			if got := function["name"]; got != tt.wantName {
				t.Fatalf("function.name = %#v, want %q", got, tt.wantName)
			}
		})
	}
}

func Test_ResponsesHandler_maps_built_in_function_calls_back_to_built_in_output_items(t *testing.T) {
	tests := []struct {
		name         string
		toolJSON     string
		toolCallName string
		wantType     string
	}{
		{
			name:         "apply_patch",
			toolJSON:     `{"type":"apply_patch"}`,
			toolCallName: "apply_patch",
			wantType:     "apply_patch_call",
		},
		{
			name:         "shell",
			toolJSON:     `{"type":"shell"}`,
			toolCallName: "shell",
			wantType:     "shell_call",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Given
			installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
				status: http.StatusOK,
				body: `{
					"id":"chatcmpl_test",
					"created":123,
					"choices":[
						{
							"finish_reason":"tool_calls",
							"message":{
								"tool_calls":[
									{
										"id":"call_123",
										"type":"function",
										"function":{
											"name":"` + tt.toolCallName + `",
											"arguments":"{\"command\":\"pwd\"}"
										}
									}
								]
							}
						}
					]
				}`,
			}})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
				"model":"primary-model",
				"input":"inspect repo",
				"tools":[`+tt.toolJSON+`]
			}`))
			rec := httptest.NewRecorder()

			// When
			responsesHandler(rec, req)

			// Then
			var response map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			output, ok := response["output"].([]any)
			if !ok || len(output) != 1 {
				t.Fatalf("output = %#v, want one tool item", response["output"])
			}
			item, ok := output[0].(map[string]any)
			if !ok {
				t.Fatalf("item = %#v, want object", output[0])
			}
			if got := item["type"]; got != tt.wantType {
				t.Fatalf("item.type = %#v, want %q", got, tt.wantType)
			}
			if got := item["call_id"]; got != "call_123" {
				t.Fatalf("item.call_id = %#v, want call_123", got)
			}
		})
	}
}
