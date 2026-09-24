package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// codex multi_agent_v1 风格的 namespace 工具：子工具嵌套声明在 namespace.tools。
const namespaceToolsJSON = `{
	"model": "primary-model",
	"input": "spawn a subagent",
	"tools": [{
		"type": "namespace",
		"name": "multi_agent_v1",
		"description": "Tools for spawning and managing sub-agents.",
		"tools": [{
			"type": "function",
			"name": "spawn_agent",
			"description": "Spawn a sub-agent.",
			"strict": false,
			"parameters": {"type": "object", "properties": {"prompt": {"type": "string"}}, "required": ["prompt"], "additionalProperties": false}
		}, {
			"type": "function",
			"name": "wait_agent",
			"description": "Wait for agents.",
			"strict": false,
			"parameters": {"type": "object", "properties": {"targets": {"type": "array", "items": {"type": "string"}}}, "required": ["targets"], "additionalProperties": false}
		}]
	}]
}`

func Test_ResponsesHandler_flattens_namespace_tools_into_function_tools(t *testing.T) {
	// Given
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"chatcmpl_ns","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(namespaceToolsJSON))
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
	if !ok {
		t.Fatalf("tools = %#v, want array", transport.requestPayloads[0]["tools"])
	}
	// namespace 应展平为纯子工具名的 function 工具，且名字不得带命名空间前缀。
	got := map[string]map[string]any{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] != "function" {
			t.Fatalf("tool.type = %#v, want function: %#v", tool["type"], tool)
		}
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if strings.Contains(name, ".") {
			t.Fatalf("dotted function name must not leak upstream: %q", name)
		}
		got[name] = fn
	}
	for _, want := range []string{"spawn_agent", "wait_agent"} {
		fn, ok := got[want]
		if !ok {
			t.Fatalf("flattened tools missing %q, got %v", want, keysOf(got))
		}
		if d, _ := fn["description"].(string); !strings.Contains(d, "multi_agent_v1") {
			t.Fatalf("%s description should carry namespace hint, got %q", want, d)
		}
		if _, ok := fn["parameters"].(map[string]any); !ok {
			t.Fatalf("%s parameters missing", want)
		}
	}
	if _, clash := got["multi_agent_v1.spawn_agent"]; clash {
		t.Fatalf("namespace-prefixed name leaked into chat tools")
	}
}

func Test_ConvertChatToResponses_restores_namespace_on_function_call(t *testing.T) {
	// Given：namespace 展平后上游按裸名回调
	tools := decodeResponsesTools(t, namespaceToolsJSON)
	chatBody := []byte(`{
		"id": "chatcmpl_ns",
		"created": 1700000000,
		"choices": [{
			"finish_reason": "tool_calls",
			"message": {"role": "assistant", "tool_calls": [{
				"id": "call_sp1",
				"type": "function",
				"function": {"name": "spawn_agent", "arguments": "{\"agent_type\":\"default\",\"prompt\":\"reply PONG\"}"}
			}]}
		}]
	}`)

	// When
	out := convertChatToResponses(chatBody, "primary-model", false, tools, nil, nil)

	// Then
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	output, _ := resp["output"].([]any)
	var call map[string]any
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] == "function_call" {
			call = item
		}
	}
	if call == nil {
		t.Fatalf("no function_call in output: %s", out)
	}
	if got := call["name"]; got != "spawn_agent" {
		t.Fatalf("name = %#v, want spawn_agent (bare sub-tool name)", got)
	}
	if got := call["namespace"]; got != "multi_agent_v1" {
		t.Fatalf("namespace = %#v, want multi_agent_v1", got)
	}
	if got := call["call_id"]; got != "call_sp1" {
		t.Fatalf("call_id = %#v, want call_sp1", got)
	}
}

func Test_ConvertResponsesToolChoice_namespace_prefix_stripped(t *testing.T) {
	tools := decodeResponsesTools(t, namespaceToolsJSON)
	choice := map[string]any{"type": "function", "namespace": "multi_agent_v1", "name": "multi_agent_v1.spawn_agent"}
	got := convertResponsesToolChoice(choice, tools)
	m, _ := got.(map[string]any)
	fn, _ := m["function"].(map[string]any)
	if fn["name"] != "spawn_agent" {
		t.Fatalf("tool_choice name = %#v, want spawn_agent", fn["name"])
	}
}

func decodeResponsesTools(t *testing.T, reqJSON string) []ResponsesTool {
	t.Helper()
	var req struct {
		Tools []ResponsesTool `json:"tools"`
	}
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(req.Tools) == 0 {
		t.Fatalf("no tools decoded")
	}
	return req.Tools
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
