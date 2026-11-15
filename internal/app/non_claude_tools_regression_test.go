package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatToolReply_ManifestAndCustomMapping(t *testing.T) {
	tools := []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name: "bash",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
					},
				},
			},
		},
		{
			Type: "function",
			Function: ToolFunction{
				Name: "apply_patch",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"input": map[string]any{"type": "string"},
					},
				},
			},
		},
		{
			Type:     "function",
			Function: ToolFunction{Name: "task"},
		},
		{
			Type:     "function",
			Function: ToolFunction{Name: "get_weather"},
		},
	}

	reply := newAgentToolReplyContextWithSchemas(offeredToolNames(tools), offeredToolSchemas(tools), false, true)
	weatherAlias := callerToolAlias("get_weather")

	body := map[string]any{
		"id":      "chatcmpl-1",
		"object":  "chat.completion",
		"created": 1,
		"model":   "big-pickle-free",
		"choices": []any{
			map[string]any{
				"index":         0,
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "shell", "arguments": `{"command":"pwd","workdir":"/tmp"}`}},
						map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "patch", "arguments": `{"patchText":"*** Begin Patch\n*** End Patch"}`}},
						map[string]any{"id": "c3", "type": "function", "function": map[string]any{"name": "subagent", "arguments": `{"description":"inspect"}`}},
						map[string]any{"id": "c4", "type": "function", "function": map[string]any{"name": weatherAlias, "arguments": `{"city":"Tokyo"}`}},
						map[string]any{"id": "c5", "type": "function", "function": map[string]any{"name": "question", "arguments": `{"q":"hi"}`}},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(body)
	rewritten := rewriteChatCompletionBody(raw, reply)

	var out map[string]any
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatalf("unmarshal rewritten: %v", err)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	tcs := choice["message"].(map[string]any)["tool_calls"].([]any)
	if len(tcs) != 4 {
		t.Fatalf("len(tool_calls) = %d, want 4: %#v", len(tcs), tcs)
	}

	got0 := tcs[0].(map[string]any)["function"].(map[string]any)
	if got0["name"] != "bash" || strings.Contains(got0["arguments"].(string), "workdir") {
		t.Fatalf("tc[0] = %#v, want name=bash without workdir", got0)
	}
	got1 := tcs[1].(map[string]any)["function"].(map[string]any)
	if got1["name"] != "apply_patch" || !strings.Contains(got1["arguments"].(string), `"input"`) {
		t.Fatalf("tc[1] = %#v, want name=apply_patch with input arg", got1)
	}
	got2 := tcs[2].(map[string]any)["function"].(map[string]any)
	if got2["name"] != "task" {
		t.Fatalf("tc[2] = %#v, want name=task", got2)
	}
	got3 := tcs[3].(map[string]any)["function"].(map[string]any)
	if got3["name"] != "get_weather" {
		t.Fatalf("tc[3] = %#v, want name=get_weather", got3)
	}

	// Only disallowed manifest call -> finish_reason downgraded to stop.
	onlyDrop := `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"c9","type":"function","function":{"name":"question","arguments":"{}"}}]}}]}`
	dropOut := rewriteChatCompletionBody([]byte(onlyDrop), reply)
	var dropParsed map[string]any
	if err := json.Unmarshal(dropOut, &dropParsed); err != nil {
		t.Fatalf("unmarshal dropOut: %v", err)
	}
	dropChoice := dropParsed["choices"].([]any)[0].(map[string]any)
	if dropChoice["finish_reason"] != "stop" {
		t.Fatalf("drop finish_reason = %v, want stop", dropChoice["finish_reason"])
	}
	if _, hasTC := dropChoice["message"].(map[string]any)["tool_calls"]; hasTC {
		t.Fatalf("expected tool_calls removed: %s", dropOut)
	}
}

func TestChatToolReply_SSEFlushAndDropFinishReason(t *testing.T) {
	tools := []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name: "Read",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file_path": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	// Partial key prefix held in argPending must be flushed on separate finish_reason chunk.
	reply := newAgentToolReplyContextWithSchemas(offeredToolNames(tools), offeredToolSchemas(tools), false, true)
	c1 := rewriteChatSSELine(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{\"p"}}]}}]}`, reply)
	c2 := rewriteChatSSELine(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ath\":\"/a\",\"x"}}]}}]}`, reply)
	c3 := rewriteChatSSELine(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, reply)

	var combinedArgs strings.Builder
	for _, line := range []string{c1, c2, c3} {
		payload := strings.TrimPrefix(line, "data: ")
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("unmarshal chunk %q: %v", line, err)
		}
		delta := m["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				fn := tc.(map[string]any)["function"].(map[string]any)
				if a, _ := fn["arguments"].(string); a != "" {
					combinedArgs.WriteString(a)
				}
			}
		}
	}
	if got := combinedArgs.String(); got != `{"file_path":"/a","x` {
		t.Fatalf("flushed args = %q, want %q", got, `{"file_path":"/a","x`)
	}

	// Separate finish_reason="tool_calls" chunk after all tool_calls were dropped must downgrade to "stop".
	replyDrop := newAgentToolReplyContextWithSchemas(offeredToolNames(tools), offeredToolSchemas(tools), false, true)
	_ = rewriteChatSSELine(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"question","arguments":"{}"}}]}}]}`, replyDrop)
	finishLine := rewriteChatSSELine(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, replyDrop)
	if !strings.Contains(finishLine, `"finish_reason":"stop"`) {
		t.Fatalf("expected finish_reason downgraded to stop, got %s", finishLine)
	}
}

func TestOpenCodeAPI_RawToolCallDecodedAfterExtraction(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "get_weather"}},
	}
	reply := newAgentToolReplyContext(offeredToolNames(tools), false, true)
	alias := callerToolAlias("get_weather")
	rawResp := `{"id":"chatcmpl-raw","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"<tool_call><function=` + alias + `><parameter=city>Tokyo</parameter></function></tool_call>"}}]}`

	extracted := convertRawToolCallsInBody([]byte(rawResp))
	rewritten := rewriteChatCompletionBody(extracted, reply)

	var out map[string]any
	if err := json.Unmarshal(rewritten, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tc := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("raw tool call name = %v, want get_weather", fn["name"])
	}
}

func TestResponsesToolReply_OfferedBuiltinAndNamespaceTools(t *testing.T) {
	respTools := []ResponsesTool{
		{Type: "shell"},
		{Type: "apply_patch"},
		{
			Type: "namespace",
			Name: "agents",
			Tools: []ResponsesTool{
				{
					Type: "function",
					Name: "spawn_agent",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"prompt": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}

	offered := offeredResponsesToolNames(respTools)
	schemas := offeredResponsesToolSchemas(respTools)
	for _, want := range []string{"shell", "apply_patch", "spawn_agent"} {
		if !offered[want] {
			t.Fatalf("offeredResponsesToolNames missing %q: %#v", want, offered)
		}
	}
	if !schemas["apply_patch"]["input"] {
		t.Fatalf("expected apply_patch schema to include input: %#v", schemas["apply_patch"])
	}
	if !schemas["spawn_agent"]["prompt"] {
		t.Fatalf("expected spawn_agent schema to include prompt: %#v", schemas["spawn_agent"])
	}

	reply := newAgentToolReplyContextWithSchemas(offered, schemas, false, len(offered) > 0)
	spawnAlias := callerToolAlias("spawn_agent")
	body := `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"type":"function_call","id":"fc1","call_id":"c1","name":"shell","arguments":"{\"command\":\"ls\",\"workdir\":\"/tmp\"}"},` +
		`{"type":"function_call","id":"fc2","call_id":"c2","name":"patch","arguments":"{\"patchText\":\"*** Begin Patch\\n*** End Patch\"}"},` +
		`{"type":"function_call","id":"fc3","call_id":"c3","name":"` + spawnAlias + `","arguments":"{\"prompt\":\"hi\"}"},` +
		`{"type":"function_call","id":"fc4","call_id":"c4","name":"question","arguments":"{}"}` +
		`]}`
	out := rewriteResponsesOutputBody([]byte(body), reply)

	var parsed struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Output) != 3 {
		t.Fatalf("len(output) = %d, want 3: %s", len(parsed.Output), out)
	}
	if parsed.Output[0]["name"] != "shell" {
		t.Fatalf("output[0] = %#v, want name=shell", parsed.Output[0])
	}
	if parsed.Output[1]["name"] != "apply_patch" || !strings.Contains(parsed.Output[1]["arguments"].(string), `"input"`) {
		t.Fatalf("output[1] = %#v, want apply_patch with input", parsed.Output[1])
	}
	if parsed.Output[2]["name"] != "spawn_agent" {
		t.Fatalf("output[2] = %#v, want spawn_agent", parsed.Output[2])
	}
}

func TestResponsesViaAnthropic_FreeTierToolReplyAndStreamArgs(t *testing.T) {
	respTools := []ResponsesTool{
		{
			Type: "function",
			Name: "Read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string"},
				},
			},
		},
		{Type: "function", Name: "get_weather"},
	}
	ctx := attachFreeTierToolReplyWithSchemas(
		context.Background(),
		"big-pickle-free",
		offeredResponsesToolNames(respTools),
		offeredResponsesToolSchemas(respTools),
		false,
	)
	weatherAlias := callerToolAlias("get_weather")

	// 1. Streaming path: disallowed manifest block dropped, read->Read path->file_path remapped,
	//    clnt_* decoded to get_weather, and accumulated arguments preserved in done & completed events.
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_r\",\"model\":\"big-pickle-free\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_drop\",\"name\":\"question\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"skip\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_read\",\"name\":\"read\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"p\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"ath\\\":\\\"/tmp/x\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_w\",\"name\":\"" + weatherAlias + "\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Tokyo\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	rec := httptest.NewRecorder()
	_, err := anthropicSSEToResponsesStream(ctx, rec, strings.NewReader(sse), "big-pickle-free", false, nil, nil)
	if err != nil {
		t.Fatalf("anthropicSSEToResponsesStream err: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "question") || strings.Contains(body, weatherAlias) {
		t.Fatalf("leaked dropped/aliased tool name in stream: %s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream missing trailing [DONE]: %s", body)
	}

	// Verify event order and response.completed output contents.
	createdPos := strings.Index(body, "event: response.created")
	addedPos := strings.Index(body, "event: response.output_item.added")
	deltaPos := strings.Index(body, "event: response.function_call_arguments.delta")
	argsDonePos := strings.Index(body, "event: response.function_call_arguments.done")
	itemDonePos := strings.Index(body, "event: response.output_item.done")
	completedPos := strings.Index(body, "event: response.completed")
	if !(createdPos >= 0 && createdPos < addedPos && addedPos < deltaPos && deltaPos < argsDonePos && argsDonePos < itemDonePos && itemDonePos < completedPos) {
		t.Fatalf("unexpected event order in stream: %s", body)
	}

	var completedOutput []map[string]any
	for _, chunk := range strings.Split(body, "\n\n") {
		if !strings.Contains(chunk, "event: response.completed") {
			continue
		}
		for _, ln := range strings.Split(chunk, "\n") {
			if payload, ok := strings.CutPrefix(ln, "data: "); ok {
				var evt struct {
					Response struct {
						Output []map[string]any `json:"output"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(payload), &evt); err != nil {
					t.Fatalf("unmarshal response.completed: %v", err)
				}
				completedOutput = evt.Response.Output
			}
		}
	}
	if len(completedOutput) != 2 {
		t.Fatalf("len(completedOutput) = %d, want 2: %#v", len(completedOutput), completedOutput)
	}
	if completedOutput[0]["name"] != "Read" || completedOutput[0]["arguments"] != `{"file_path":"/tmp/x"}` {
		t.Fatalf("completedOutput[0] = %#v, want Read with file_path", completedOutput[0])
	}
	if completedOutput[1]["name"] != "get_weather" || completedOutput[1]["arguments"] != `{"city":"Tokyo"}` {
		t.Fatalf("completedOutput[1] = %#v, want get_weather with city", completedOutput[1])
	}

	// 2. Non-streaming path: presentAnthropicBody + convertAnthropicToResponses.
	nonStreamAnthropic := `{"id":"msg_ns","type":"message","role":"assistant","model":"big-pickle-free","content":[` +
		`{"type":"tool_use","id":"toolu_drop","name":"question","input":{"q":"skip"}},` +
		`{"type":"tool_use","id":"toolu_r","name":"read","input":{"path":"/tmp/y"}},` +
		`{"type":"tool_use","id":"toolu_w","name":"` + weatherAlias + `","input":{"city":"Osaka"}}` +
		`],"stop_reason":"tool_use"}`
	presented := presentAnthropicBody([]byte(nonStreamAnthropic), nil, agentToolReplyFromContext(ctx))
	respOut := convertAnthropicToResponses(presented, "big-pickle-free", false)
	var nsParsed struct {
		Status string           `json:"status"`
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(respOut, &nsParsed); err != nil {
		t.Fatalf("unmarshal non-stream resp: %v", err)
	}
	if nsParsed.Status != "completed" || len(nsParsed.Output) != 2 {
		t.Fatalf("non-stream parsed = %#v", nsParsed)
	}
	if nsParsed.Output[0]["name"] != "Read" || !strings.Contains(nsParsed.Output[0]["arguments"].(string), `"file_path"`) {
		t.Fatalf("non-stream output[0] = %#v", nsParsed.Output[0])
	}
	if nsParsed.Output[1]["name"] != "get_weather" {
		t.Fatalf("non-stream output[1] = %#v", nsParsed.Output[1])
	}
}
