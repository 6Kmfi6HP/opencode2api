package app

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// =====================================================================
// Part A: /v1/responses Anthropic-style tool_result compatibility
// =====================================================================

// TestResponsesToolResult_AnthropicStyleContentBlocks verifies that a
// Responses tool_result with Anthropic-style content blocks (string, string
// array, and typed text blocks) is normalized into a single tool message and
// the raw wrapper JSON is never emitted to the upstream.
func TestResponsesToolResult_AnthropicStyleContentBlocks(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}"},
			{"type":"tool_result","call_id":"call_1","content":[
				{"type":"text","text":"line one"},
				{"type":"text","text":"line two"}
			]},
			{"type":"function_call","call_id":"call_2","name":"calc","arguments":"{}"},
			{"type":"tool_result","tool_use_id":"call_2","content":["raw string a","raw string b"]}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("payload count = %d", len(transport.requestPayloads))
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	// Expect: assistant(call_1), tool(call_1), assistant(call_2), tool(call_2)
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	// tool message for call_1
	tool1, _ := messages[1].(map[string]any)
	if tool1["role"] != "tool" {
		t.Fatalf("tool1 role = %#v", tool1["role"])
	}
	if tool1["tool_call_id"] != "call_1" {
		t.Fatalf("tool1 tool_call_id = %#v", tool1["tool_call_id"])
	}
	if got := tool1["content"]; got != "line one\nline two" {
		t.Fatalf("tool1 content = %#v, want joined blocks", got)
	}
	// tool message for call_2 uses tool_use_id alias
	tool2, _ := messages[3].(map[string]any)
	if tool2["tool_call_id"] != "call_2" {
		t.Fatalf("tool2 tool_call_id = %#v", tool2["tool_call_id"])
	}
	if got := tool2["content"]; got != "raw string a\nraw string b" {
		t.Fatalf("tool2 content = %#v, want joined string array", got)
	}
	// No raw wrapper JSON should appear in any tool content.
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if c, ok := mm["content"].(string); ok {
			if strings.Contains(c, `"type":"tool_result"`) {
				t.Fatalf("raw tool_result wrapper JSON leaked: %s", c)
			}
		}
	}
}

// TestResponsesToolResult_IsErrorPrefix verifies is_error:true adds the stable
// "Error: " prefix without duplication.
func TestResponsesToolResult_IsErrorPrefix(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_err","name":"f","arguments":"{}"},
			{"type":"tool_result","call_id":"call_err","is_error":true,"content":"boom"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	tool, _ := messages[1].(map[string]any)
	if got := tool["content"]; got != "Error: boom" {
		t.Fatalf("is_error content = %#v, want 'Error: boom'", got)
	}
}

// TestResponsesToolResult_EmptyStringIsLegitimateOutput verifies that an
// empty string content is treated as a provided output (not missing).
func TestResponsesToolResult_EmptyStringIsLegitimateOutput(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_empty","name":"f","arguments":"{}"},
			{"type":"tool_result","call_id":"call_empty","content":""}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	tool, _ := messages[1].(map[string]any)
	if got, ok := tool["content"]; !ok || got != "" {
		t.Fatalf("empty content = %#v (ok=%v), want empty string present", got, ok)
	}
}

// TestResponsesToolResult_StandardOutputFieldStillWorks verifies the standard
// function_call_output output field still maps correctly.
func TestResponsesToolResult_StandardOutputFieldStillWorks(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_std","name":"f","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_std","output":"standard result"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	tool, _ := messages[1].(map[string]any)
	if got := tool["content"]; got != "standard result" {
		t.Fatalf("standard output = %#v", got)
	}
}

// TestResponsesToolResult_NoDuplicateToolMessages verifies fixToolCallGaps does
// not produce duplicate tool messages when call+output are already paired.
func TestResponsesToolResult_NoDuplicateToolMessages(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_dup","name":"f","arguments":"{}"},
			{"type":"tool_result","call_id":"call_dup","content":"result"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	toolCount := 0
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" {
			toolCount++
		}
	}
	if toolCount != 1 {
		t.Fatalf("tool message count = %d, want 1 (no duplicates)", toolCount)
	}
}

// =====================================================================
// Part B: document / input_file structured file parts
// =====================================================================

func okChatBody() string {
	return `{"id":"r","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
}

// TestClaudeDocument_Base64 verifies a base64 document maps to a file part.
func TestClaudeDocument_Base64(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[{"role":"user","content":[
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}},
			{"type":"text","text":"summarize this pdf"}
		]}]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	// [0]=user with content array
	user, _ := messages[0].(map[string]any)
	content, _ := user["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len = %d", len(content))
	}
	filePart, _ := content[0].(map[string]any)
	if filePart["type"] != "file" {
		t.Fatalf("file part type = %#v", filePart["type"])
	}
	file, _ := filePart["file"].(map[string]any)
	data, _ := file["file_data"].(string)
	if !strings.HasPrefix(data, "data:application/pdf;base64,JVBERi0=") {
		t.Fatalf("file_data = %#v", data)
	}
	// text part preserved in order
	textPart, _ := content[1].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "summarize this pdf" {
		t.Fatalf("text part = %#v", textPart)
	}
}

// TestClaudeDocument_URL verifies a URL document maps to a file part.
func TestClaudeDocument_URL(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[{"role":"user","content":[
			{"type":"document","source":{"type":"url","url":"https://example.test/doc.pdf"}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	content, _ := user["content"].([]any)
	filePart, _ := content[0].(map[string]any)
	if filePart["type"] != "file" {
		t.Fatalf("file part type = %#v", filePart["type"])
	}
	file, _ := filePart["file"].(map[string]any)
	if got := file["file_data"]; got != "https://example.test/doc.pdf" {
		t.Fatalf("file_data = %#v", got)
	}
}

// TestClaudeDocument_InToolResult verifies document inside tool_result becomes
// a followup user attachment, not silently dropped.
func TestClaudeDocument_InToolResult(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_doc","name":"get_doc","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_doc","content":[
				{"type":"text","text":"here is the doc"},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}
			]}]}
		]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	// Expect: assistant(tool_use), tool(result text), user(document attachment)
	if len(messages) < 3 {
		t.Fatalf("messages = %#v", messages)
	}
	follow, _ := messages[2].(map[string]any)
	if follow["role"] != "user" {
		t.Fatalf("followup role = %#v", follow["role"])
	}
	parts, _ := follow["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("followup parts = %#v", parts)
	}
	filePart, _ := parts[0].(map[string]any)
	if filePart["type"] != "file" {
		t.Fatalf("followup file type = %#v", filePart["type"])
	}
	// tool text should mention document attached
	tool, _ := messages[1].(map[string]any)
	if c, _ := tool["content"].(string); !strings.Contains(c, "[document attached]") {
		t.Fatalf("tool content missing document marker: %q", c)
	}
}

// TestClaudeDocument_MalformedReturns400 verifies a document without payload
// returns a protocol-shaped 400, not text serialization.
func TestClaudeDocument_MalformedReturns400(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[{"role":"user","content":[
			{"type":"document","source":{"type":"base64","data":""}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %s", ct)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type = %#v", errObj["type"])
	}
}

// TestResponsesInputFile_FlatFileData verifies flat file_data field.
func TestResponsesInputFile_FlatFileData(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_text","text":"analyze this"},
			{"type":"input_file","file_data":"data:application/pdf;base64,JVBERi0=","filename":"doc.pdf"}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	content, _ := user["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content len = %d", len(content))
	}
	filePart, _ := content[1].(map[string]any)
	if filePart["type"] != "file" {
		t.Fatalf("file part type = %#v", filePart["type"])
	}
	file, _ := filePart["file"].(map[string]any)
	if got := file["file_data"]; got != "data:application/pdf;base64,JVBERi0=" {
		t.Fatalf("file_data = %#v", got)
	}
	if got := file["filename"]; got != "doc.pdf" {
		t.Fatalf("filename = %#v", got)
	}
}

// TestResponsesInputFile_FileID verifies file_id field.
func TestResponsesInputFile_FileID(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_file","file_id":"file-abc123"}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	content, _ := user["content"].([]any)
	filePart, _ := content[0].(map[string]any)
	if filePart["type"] != "file" {
		t.Fatalf("file part type = %#v", filePart["type"])
	}
	file, _ := filePart["file"].(map[string]any)
	if got := file["file_id"]; got != "file-abc123" {
		t.Fatalf("file_id = %#v", got)
	}
}

// TestResponsesInputFile_FileURL verifies file_url maps to file_data.
func TestResponsesInputFile_FileURL(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_file","file_url":"https://example.test/file.pdf"}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	content, _ := user["content"].([]any)
	filePart, _ := content[0].(map[string]any)
	file, _ := filePart["file"].(map[string]any)
	if got := file["file_data"]; got != "https://example.test/file.pdf" {
		t.Fatalf("file_data = %#v", got)
	}
}

// TestResponsesInputFile_Nested verifies nested input_file object.
func TestResponsesInputFile_Nested(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_file","input_file":{"file_id":"file-nested","filename":"nested.pdf"}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	content, _ := user["content"].([]any)
	filePart, _ := content[0].(map[string]any)
	file, _ := filePart["file"].(map[string]any)
	if got := file["file_id"]; got != "file-nested" {
		t.Fatalf("file_id = %#v", got)
	}
	if got := file["filename"]; got != "nested.pdf" {
		t.Fatalf("filename = %#v", got)
	}
}

// TestResponsesInputFile_TopLevelItem verifies top-level input_file item.
func TestResponsesInputFile_TopLevelItem(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"input_file","file_id":"file-top"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("role = %#v", user["role"])
	}
	content, _ := user["content"].([]any)
	filePart, _ := content[0].(map[string]any)
	if filePart["type"] != "file" {
		t.Fatalf("file part type = %#v", filePart["type"])
	}
	file, _ := filePart["file"].(map[string]any)
	if got := file["file_id"]; got != "file-top" {
		t.Fatalf("file_id = %#v", got)
	}
}

// TestResponsesInputFile_MalformedReturns400 verifies malformed input_file
// returns 400 and is not serialized as text.
func TestResponsesInputFile_MalformedReturns400(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_file"}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %s", ct)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type = %#v", errObj["type"])
	}
	if errObj["param"] != "input_file" {
		t.Fatalf("error.param = %#v", errObj["param"])
	}
}

// TestResponsesInputFile_NoTextLeak verifies a valid input_file is not
// serialized as JSON text in the upstream body.
func TestResponsesInputFile_NoTextLeak(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_file","file_id":"file-leak"}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	raw, _ := json.Marshal(transport.requestPayloads[0])
	if strings.Contains(string(raw), `"type":"input_file"`) {
		t.Fatalf("input_file wrapper leaked as text: %s", raw)
	}
}

// =====================================================================
// Part C: temperature boundary validation
// =====================================================================

func tempReq(protocol string, temp float64) *http.Request {
	body := `{"model":"primary-model","temperature":` + jsonFloat(temp) + `,"messages":[{"role":"user","content":"hi"}]}`
	switch protocol {
	case "claude":
		body = `{"model":"primary-model","temperature":` + jsonFloat(temp) + `,"max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`
	case "responses":
		body = `{"model":"primary-model","temperature":` + jsonFloat(temp) + `,"input":"hi"}`
	}
	return httptest.NewRequest(http.MethodPost, "/"+protocolPath(protocol), strings.NewReader(body))
}

func protocolPath(protocol string) string {
	switch protocol {
	case "chat":
		return "v1/chat/completions"
	case "claude":
		return "v1/messages"
	case "responses":
		return "v1/responses"
	}
	return ""
}

func jsonFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func runTempHandler(t *testing.T, protocol string, rec *httptest.ResponseRecorder, req *http.Request) {
	t.Helper()
	switch protocol {
	case "chat":
		chatCompletionsHandler(rec, req)
	case "claude":
		claudeMessagesHandler(rec, req)
	case "responses":
		responsesHandler(rec, req)
	}
}

func assertTemp400(t *testing.T, rec *httptest.ResponseRecorder, protocol string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %s", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", body)
	}
	switch protocol {
	case "claude":
		if body["type"] != "error" {
			t.Fatalf("type = %#v", body["type"])
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj["type"] != "invalid_request_error" {
			t.Fatalf("error.type = %#v", errObj["type"])
		}
	default:
		errObj, _ := body["error"].(map[string]any)
		if errObj["type"] != "invalid_request_error" {
			t.Fatalf("error.type = %#v", errObj["type"])
		}
		if errObj["param"] != "temperature" {
			t.Fatalf("error.param = %#v", errObj["param"])
		}
	}
}

// TestTemperature_ClaudeRange verifies /v1/messages only accepts 0..1.
func TestTemperature_ClaudeRange(t *testing.T) {
	cases := []struct {
		name   string
		temp   float64
		wantOK bool
	}{
		{"zero", 0, true},
		{"one", 1, true},
		{"midpoint", 0.5, true},
		{"above_max", 1.5, false},
		{"negative", -0.1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
			rec := httptest.NewRecorder()
			runTempHandler(t, "claude", rec, tempReq("claude", tc.temp))
			if tc.wantOK {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				if len(transport.requestPayloads) != 1 {
					t.Fatalf("upstream not called, payloads = %d", len(transport.requestPayloads))
				}
			} else {
				assertTemp400(t, rec, "claude")
				if len(transport.requestPayloads) != 0 {
					t.Fatalf("upstream called on rejection, payloads = %d", len(transport.requestPayloads))
				}
			}
		})
	}
}

// TestTemperature_ChatRange verifies /v1/chat/completions accepts 0..2.
func TestTemperature_ChatRange(t *testing.T) {
	cases := []struct {
		name   string
		temp   float64
		wantOK bool
	}{
		{"zero", 0, true},
		{"two", 2, true},
		{"midpoint", 1.5, true},
		{"above_max", 2.5, false},
		{"negative", -0.1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
			rec := httptest.NewRecorder()
			runTempHandler(t, "chat", rec, tempReq("chat", tc.temp))
			if tc.wantOK {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				if len(transport.requestPayloads) != 1 {
					t.Fatalf("upstream not called, payloads = %d", len(transport.requestPayloads))
				}
			} else {
				assertTemp400(t, rec, "chat")
				if len(transport.requestPayloads) != 0 {
					t.Fatalf("upstream called on rejection, payloads = %d", len(transport.requestPayloads))
				}
			}
		})
	}
}

// TestTemperature_ResponsesRange verifies /v1/responses accepts 0..2.
func TestTemperature_ResponsesRange(t *testing.T) {
	cases := []struct {
		name   string
		temp   float64
		wantOK bool
	}{
		{"zero", 0, true},
		{"two", 2, true},
		{"above_max", 3, false},
		{"negative", -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
			rec := httptest.NewRecorder()
			runTempHandler(t, "responses", rec, tempReq("responses", tc.temp))
			if tc.wantOK {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", rec.Code)
				}
				if len(transport.requestPayloads) != 1 {
					t.Fatalf("upstream not called, payloads = %d", len(transport.requestPayloads))
				}
			} else {
				assertTemp400(t, rec, "responses")
				if len(transport.requestPayloads) != 0 {
					t.Fatalf("upstream called on rejection, payloads = %d", len(transport.requestPayloads))
				}
			}
		})
	}
}

// TestTemperature_NilAccepted verifies absent temperature is accepted.
func TestTemperature_NilAccepted(t *testing.T) {
	for _, protocol := range []string{"chat", "claude", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
			var body string
			switch protocol {
			case "chat":
				body = `{"model":"primary-model","messages":[{"role":"user","content":"hi"}]}`
			case "claude":
				body = `{"model":"primary-model","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`
			case "responses":
				body = `{"model":"primary-model","input":"hi"}`
			}
			rec := httptest.NewRecorder()
			runTempHandler(t, protocol, rec, httptest.NewRequest(http.MethodPost, "/"+protocolPath(protocol), strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if len(transport.requestPayloads) != 1 {
				t.Fatalf("upstream not called, payloads = %d", len(transport.requestPayloads))
			}
		})
	}
}

// TestValidateTemperature_NaNRejected verifies NaN is rejected at helper level.
// JSON cannot represent NaN, so this is tested at the helper level.
func TestValidateTemperature_NaNRejected(t *testing.T) {
	v := math.NaN()
	if msg := validateTemperature(&v, 0, 2); msg == "" {
		t.Fatal("NaN should be rejected")
	}
	if msg := validateTemperature(&v, 0, 1); msg == "" {
		t.Fatal("NaN should be rejected for claude range")
	}
}

// TestValidateTemperature_InfRejected verifies +Inf is rejected at helper level.
func TestValidateTemperature_InfRejected(t *testing.T) {
	v := math.Inf(1)
	if msg := validateTemperature(&v, 0, 2); msg == "" {
		t.Fatal("+Inf should be rejected")
	}
	negInf := math.Inf(-1)
	if msg := validateTemperature(&negInf, 0, 2); msg == "" {
		t.Fatal("-Inf should be rejected")
	}
}

// TestValidateTemperature_NilAccepted verifies nil is accepted at helper level.
func TestValidateTemperature_NilAccepted(t *testing.T) {
	if msg := validateTemperature(nil, 0, 2); msg != "" {
		t.Fatalf("nil should be accepted, got %q", msg)
	}
}

// TestTemperature_StreamRequestReturnsJSON400 verifies streaming requests also
// get a plain JSON 400 before any SSE starts.
func TestTemperature_StreamRequestReturnsJSON400(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
	body := `{"model":"primary-model","temperature":3,"stream":true,"input":"hi"}`
	rec := httptest.NewRecorder()
	responsesHandler(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %s, want application/json (not SSE)", ct)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	errObj, _ := resp["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type = %#v", errObj["type"])
	}
}

// =====================================================================
// Part A unit-level: helpers
// =====================================================================

func TestNormalizeToolResultOutput_StandardOutputString(t *testing.T) {
	text, present := normalizeToolResultOutput(map[string]any{"output": "hello"})
	if !present || text != "hello" {
		t.Fatalf("got (%q, %v)", text, present)
	}
}

func TestNormalizeToolResultOutput_ContentString(t *testing.T) {
	text, present := normalizeToolResultOutput(map[string]any{"content": "world"})
	if !present || text != "world" {
		t.Fatalf("got (%q, %v)", text, present)
	}
}

func TestNormalizeToolResultOutput_ContentTypedBlocks(t *testing.T) {
	text, present := normalizeToolResultOutput(map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "output_text", "text": "b"},
		map[string]any{"type": "input_text", "text": "c"},
	}})
	if !present || text != "a\nb\nc" {
		t.Fatalf("got (%q, %v)", text, present)
	}
}

func TestNormalizeToolResultOutput_EmptyStringPresent(t *testing.T) {
	text, present := normalizeToolResultOutput(map[string]any{"content": ""})
	if !present || text != "" {
		t.Fatalf("got (%q, %v), want present empty", text, present)
	}
}

func TestNormalizeToolResultOutput_MissingPayload(t *testing.T) {
	_, present := normalizeToolResultOutput(map[string]any{"type": "tool_result"})
	if present {
		t.Fatal("want not present for missing payload")
	}
}

func TestApplyErrorPrefix_NoDoublePrefix(t *testing.T) {
	if got := applyErrorPrefix("Error: already"); got != "Error: already" {
		t.Fatalf("got %q", got)
	}
	if got := applyErrorPrefix("boom"); got != "Error: boom" {
		t.Fatalf("got %q", got)
	}
	if got := applyErrorPrefix(""); got != "Error: " {
		t.Fatalf("got %q", got)
	}
}

func TestCountClaudeThinkingSignatures(t *testing.T) {
	msgs := []ClaudeMessage{{
		Role: "assistant",
		Content: []any{
			map[string]any{"type": "thinking", "thinking": "h1", "signature": "sig1"},
			map[string]any{"type": "thinking", "thinking": "h2", "signature": ""},
			map[string]any{"type": "thinking", "thinking": "h3"},
			map[string]any{"type": "text", "text": "answer"},
		},
	}}
	if n := countClaudeThinkingSignatures(msgs); n != 1 {
		t.Fatalf("signature count = %d, want 1", n)
	}
}

// =====================================================================
// Fix 1: is_error order-independence and missing payload
// =====================================================================

// TestResponsesToolResult_IsErrorOutputBeforeCall verifies that is_error
// prefix is applied regardless of whether the output item appears before or
// after the call item in the array. Also asserts the exact message sequence
// is assistant(call) then exactly one tool result (no leading duplicate).
func TestResponsesToolResult_IsErrorOutputBeforeCall(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	// output item appears BEFORE the call item in the array
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"tool_result","call_id":"call_err","is_error":true,"content":"boom"},
			{"type":"function_call","call_id":"call_err","name":"f","arguments":"{}"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	// Assert exact sequence: assistant(call_err) then exactly one tool message
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2 (assistant + tool, no duplicate): %#v", len(messages), messages)
	}
	m0, _ := messages[0].(map[string]any)
	if m0["role"] != "assistant" {
		t.Fatalf("messages[0] role = %#v, want assistant", m0["role"])
	}
	m1, _ := messages[1].(map[string]any)
	if m1["role"] != "tool" {
		t.Fatalf("messages[1] role = %#v, want tool", m1["role"])
	}
	if m1["tool_call_id"] != "call_err" {
		t.Fatalf("tool_call_id = %#v, want call_err", m1["tool_call_id"])
	}
	if got := m1["content"]; got != "Error: boom" {
		t.Fatalf("tool content = %#v, want 'Error: boom'", got)
	}
}

// TestResponsesToolResult_IsErrorOutputAfterCall verifies is_error when
// output appears after the call item (the original order). Also asserts the
// exact message sequence is assistant(call) then exactly one tool result.
func TestResponsesToolResult_IsErrorOutputAfterCall(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_err","name":"f","arguments":"{}"},
			{"type":"tool_result","call_id":"call_err","is_error":true,"content":"boom"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	// Assert exact sequence: assistant(call_err) then exactly one tool message
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want 2 (assistant + tool, no duplicate): %#v", len(messages), messages)
	}
	m0, _ := messages[0].(map[string]any)
	if m0["role"] != "assistant" {
		t.Fatalf("messages[0] role = %#v, want assistant", m0["role"])
	}
	m1, _ := messages[1].(map[string]any)
	if m1["role"] != "tool" {
		t.Fatalf("messages[1] role = %#v, want tool", m1["role"])
	}
	if m1["tool_call_id"] != "call_err" {
		t.Fatalf("tool_call_id = %#v, want call_err", m1["tool_call_id"])
	}
	if got := m1["content"]; got != "Error: boom" {
		t.Fatalf("tool content = %#v, want 'Error: boom'", got)
	}
}

// TestResponsesToolResult_NoPayloadGivesMissing verifies that a tool_result
// with no output and no content field is treated as missing (not empty), and
// gets "[tool output missing]".
func TestResponsesToolResult_NoPayloadGivesMissing(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_no","name":"f","arguments":"{}"},
			{"type":"tool_result","call_id":"call_no"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" {
			if got := mm["content"]; got != "[tool output missing]" {
				t.Fatalf("tool content = %#v, want '[tool output missing]'", got)
			}
			return
		}
	}
	t.Fatal("no tool message found")
}

// TestNormalizeToolResultOutput_IsErrorApplied verifies the helper itself
// applies the is_error prefix.
func TestNormalizeToolResultOutput_IsErrorApplied(t *testing.T) {
	text, present := normalizeToolResultOutput(map[string]any{
		"content":  "failure",
		"is_error": true,
	})
	if !present {
		t.Fatal("want present")
	}
	if text != "Error: failure" {
		t.Fatalf("got %q, want 'Error: failure'", text)
	}
}

// TestNormalizeToolResultOutput_IsErrorNotAppliedWhenFalse verifies no prefix
// when is_error is absent or false.
func TestNormalizeToolResultOutput_IsErrorNotAppliedWhenFalse(t *testing.T) {
	text, _ := normalizeToolResultOutput(map[string]any{"content": "ok"})
	if text != "ok" {
		t.Fatalf("got %q, want 'ok'", text)
	}
	text, _ = normalizeToolResultOutput(map[string]any{
		"content":  "ok",
		"is_error": false,
	})
	if text != "ok" {
		t.Fatalf("got %q, want 'ok'", text)
	}
}

// TestCollectFunctionOutputs_IsErrorBakedIntoMap verifies that
// collectFunctionOutputs already includes the is_error prefix in the map,
// making it order-independent.
func TestCollectFunctionOutputs_IsErrorBakedIntoMap(t *testing.T) {
	outputs := collectFunctionOutputs([]any{
		map[string]any{
			"type":     "tool_result",
			"call_id":  "call_x",
			"content":  "boom",
			"is_error": true,
		},
	})
	if got, ok := outputs["call_x"]; !ok || got != "Error: boom" {
		t.Fatalf("outputs[call_x] = %q (ok=%v), want 'Error: boom'", got, ok)
	}
}

// TestCollectFunctionOutputs_NoPayloadAbsent verifies that a tool_result with
// no payload leaves the key absent (not set to empty string).
func TestCollectFunctionOutputs_NoPayloadAbsent(t *testing.T) {
	outputs := collectFunctionOutputs([]any{
		map[string]any{
			"type":    "tool_result",
			"call_id": "call_missing",
		},
	})
	if _, ok := outputs["call_missing"]; ok {
		t.Fatal("key should be absent for missing payload")
	}
}

// TestCollectFunctionOutputs_EmptyContentPresent verifies that an explicit
// empty content string IS present in the map.
func TestCollectFunctionOutputs_EmptyContentPresent(t *testing.T) {
	outputs := collectFunctionOutputs([]any{
		map[string]any{
			"type":    "tool_result",
			"call_id": "call_empty",
			"content": "",
		},
	})
	got, ok := outputs["call_empty"]
	if !ok {
		t.Fatal("key should be present for explicit empty content")
	}
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

// =====================================================================
// Fix 2: responsesInputFileToFile field selection
// =====================================================================

// TestResponsesInputFile_NestedOnlyFileURL verifies that a nested-only
// file_url maps to file.file_data.
func TestResponsesInputFile_NestedOnlyFileURL(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_file","input_file":{"file_url":"https://example.test/nested.pdf"}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	content, _ := user["content"].([]any)
	filePart, _ := content[0].(map[string]any)
	file, _ := filePart["file"].(map[string]any)
	if got := file["file_data"]; got != "https://example.test/nested.pdf" {
		t.Fatalf("file_data = %#v", got)
	}
}

// TestResponsesInputFile_NestedEmptyPayloadReturns400 verifies that a nested
// input_file with only empty strings returns 400.
func TestResponsesInputFile_NestedEmptyPayloadReturns400(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_file","input_file":{"file_data":"","file_id":"","file_url":""}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestResponsesInputFile_UnknownFieldNotLeaked verifies that unknown fields in
// the input_file (flat or nested) do not leak into the Chat file object.
func TestResponsesInputFile_UnknownFieldNotLeaked(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_file","file_id":"file-abc","filename":"doc.pdf","custom_field":"leak","priority":99,
			 "input_file":{"file_id":"file-nested","extra":"should-not-leak"}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	content, _ := user["content"].([]any)
	filePart, _ := content[0].(map[string]any)
	file, _ := filePart["file"].(map[string]any)
	// Only file_data / file_id / filename should exist
	for key := range file {
		if key != "file_data" && key != "file_id" && key != "filename" {
			t.Fatalf("unexpected field %q in file object: %#v", key, file)
		}
	}
	if _, leaked := file["custom_field"]; leaked {
		t.Fatal("custom_field leaked")
	}
	if _, leaked := file["priority"]; leaked {
		t.Fatal("priority leaked")
	}
	if _, leaked := file["extra"]; leaked {
		t.Fatal("nested extra leaked")
	}
}

// =====================================================================
// Fix 3: per-tool_result attachment labeling
// =====================================================================

// TestClaudeParallelToolResults_DocumentOnlyFirst verifies that when two
// parallel tool_results exist (first with a document, second pure text), only
// the first gets [document attached] and the second does not.
func TestClaudeParallelToolResults_DocumentOnlyFirst(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_doc","name":"get_doc","input":{}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_txt","name":"get_text","input":{}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_doc","content":[
					{"type":"text","text":"here is the doc"},
					{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}
				]},
				{"type":"tool_result","tool_use_id":"call_txt","content":[
					{"type":"text","text":"plain text result"}
				]}
			]}
		]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	// Find both tool messages
	var docTool, txtTool map[string]any
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" {
			switch mm["tool_call_id"] {
			case "call_doc":
				docTool = mm
			case "call_txt":
				txtTool = mm
			}
		}
	}
	if docTool == nil {
		t.Fatal("call_doc tool message not found")
	}
	if txtTool == nil {
		t.Fatal("call_txt tool message not found")
	}
	docContent, _ := docTool["content"].(string)
	if !strings.Contains(docContent, "[document attached]") {
		t.Fatalf("doc tool content should contain [document attached]: %q", docContent)
	}
	if !strings.Contains(docContent, "here is the doc") {
		t.Fatalf("doc tool content should contain text: %q", docContent)
	}
	txtContent, _ := txtTool["content"].(string)
	if strings.Contains(txtContent, "[document attached]") {
		t.Fatalf("text tool should NOT contain [document attached]: %q", txtContent)
	}
	if strings.Contains(txtContent, "[image attached]") {
		t.Fatalf("text tool should NOT contain [image attached]: %q", txtContent)
	}
	if txtContent != "plain text result" {
		t.Fatalf("text tool content = %q, want 'plain text result'", txtContent)
	}
}

// TestClaudeToolResult_MixedImageAndDocument verifies image and document
// attachments in the same tool_result are both labeled in original order.
func TestClaudeToolResult_MixedImageAndDocument(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_mix","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_mix","content":[
				{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}},
				{"type":"document","source":{"type":"url","url":"https://example.test/d.pdf"}}
			]}]}
		]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" {
			content, _ := mm["content"].(string)
			// Both labels should appear in image-then-document order
			imgIdx := strings.Index(content, "[image attached]")
			docIdx := strings.Index(content, "[document attached]")
			if imgIdx < 0 || docIdx < 0 {
				t.Fatalf("missing labels in content: %q", content)
			}
			if imgIdx > docIdx {
				t.Fatalf("image should come before document: %q", content)
			}
			return
		}
	}
	t.Fatal("no tool message found")
}

// =====================================================================
// Fix 4: Responses validator does not handle document alias
// =====================================================================

// TestResponsesValidator_DocumentAliasNotValidated verifies that a Responses
// input containing a type:"document" item does NOT trigger the file validator
// (document is a /v1/messages concept, not a Responses alias). The validator
// only checks input_file; a document item is not rejected by the file-input
// 400 check and passes through to the upstream.
func TestResponsesValidator_DocumentAliasNotValidated(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	// An actual type:"document" item in Responses input. It must NOT produce
	// a 400 from the file validator — the validator only handles input_file.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"document","source":{"type":"url","url":"https://example.test/doc.pdf"}}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	// The document item is not validated by the file-input check, so it does
	// not get a 400 from that check. It reaches the upstream (as text via the
	// default item handler), confirming no file-input validation error.
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = 400, want non-400 (document must not trigger file validator): %s", rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream should be called (document not blocked by file validator), payloads = %d", len(transport.requestPayloads))
	}
}

// =====================================================================
// Fix A: cache_control counting (tool-level field, not schema properties)
// =====================================================================

// TestCountClaudeCacheControlBlocks_ToolLevelField verifies that a
// cache_control on a tool definition (the official tool-level breakpoint) is
// counted, but a cache_control property inside input_schema is NOT counted.
func TestCountClaudeCacheControlBlocks_ToolLevelField(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"max_tokens":100,
		"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}
		],
		"tools":[
			{
				"name":"get_weather",
				"description":"Get weather",
				"input_schema":{
					"type":"object",
					"properties":{
						"location":{"type":"string"},
						"cache_control":{"type":"string","description":"not a real breakpoint"}
					}
				},
				"cache_control":{"type":"ephemeral"}
			}
		]
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	n := countClaudeCacheControlBlocks(req)
	// Expected: 1 (system) + 1 (message content block) + 1 (tool-level) = 3
	// The input_schema property named "cache_control" must NOT be counted.
	if n != 3 {
		t.Fatalf("cache_control_blocks = %d, want 3 (system + message + tool-level, not schema property)", n)
	}
}

// TestCountClaudeCacheControlBlocks_ToolUseInputNotCounted verifies that a
// cache_control key inside a tool_use input object is not counted.
func TestCountClaudeCacheControlBlocks_ToolUseInputNotCounted(t *testing.T) {
	raw := []byte(`{
		"model":"claude-test",
		"max_tokens":100,
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_1","name":"f","input":{"cache_control":"not a breakpoint"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}
		],
		"tools":[
			{"name":"f","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}
		]
	}`)
	var req ClaudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	n := countClaudeCacheControlBlocks(req)
	// No actual breakpoints — tool_use input cache_control is NOT counted.
	if n != 0 {
		t.Fatalf("cache_control_blocks = %d, want 0 (tool_use input cache_control is not a breakpoint)", n)
	}
}

// TestClaudeToolCacheControlUnmarshals verifies that the CacheControl field
// on ClaudeTool is populated from JSON (not silently dropped).
func TestClaudeToolCacheControlUnmarshals(t *testing.T) {
	raw := []byte(`{
		"name":"f",
		"input_schema":{"type":"object"},
		"cache_control":{"type":"ephemeral"}
	}`)
	var tool ClaudeTool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatal(err)
	}
	if tool.CacheControl == nil {
		t.Fatal("CacheControl should be unmarshaled, got nil")
	}
	m, ok := tool.CacheControl.(map[string]any)
	if !ok {
		t.Fatalf("CacheControl = %#v, want map", tool.CacheControl)
	}
	if m["type"] != "ephemeral" {
		t.Fatalf("CacheControl.type = %#v, want ephemeral", m["type"])
	}
}

// =====================================================================
// Fix B: countClaudeThinkingSignatures false-positive regression
// =====================================================================

// TestCountClaudeThinkingSignatures_NoNestedFalsePositive verifies that a
// user/tool input object with type:"thinking" and a signature key is NOT
// counted, because only top-level message content blocks are valid history
// thinking blocks.
func TestCountClaudeThinkingSignatures_NoNestedFalsePositive(t *testing.T) {
	msgs := []ClaudeMessage{
		{
			Role: "assistant",
			Content: []any{
				map[string]any{"type": "thinking", "thinking": "real", "signature": "real_sig"},
				map[string]any{"type": "text", "text": "answer"},
			},
		},
		{
			Role: "user",
			Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": []any{
					// This is inside tool_result content, NOT a history thinking block.
					map[string]any{"type": "thinking", "signature": "not_a_history_block"},
				}},
			},
		},
	}
	n := countClaudeThinkingSignatures(msgs)
	if n != 1 {
		t.Fatalf("signature count = %d, want 1 (only the real top-level thinking block)", n)
	}
}

// TestCountClaudeThinkingSignatures_StringContentNotCounted verifies that a
// message with string content (not a content array) does not crash and
// returns 0.
func TestCountClaudeThinkingSignatures_StringContentNotCounted(t *testing.T) {
	msgs := []ClaudeMessage{
		{Role: "user", Content: "just a string"},
	}
	n := countClaudeThinkingSignatures(msgs)
	if n != 0 {
		t.Fatalf("signature count = %d, want 0 for string content", n)
	}
}

// =====================================================================
// Fix 1: output-before-call exact sequence regression
// =====================================================================

// TestResponsesToolResult_OutputBeforeCall_NoLeadingDuplicate verifies that
// when output precedes call, the result is assistant(call) then exactly one
// tool message — no leading duplicate tool message.
func TestResponsesToolResult_OutputBeforeCall_NoLeadingDuplicate(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"tool_result","call_id":"call_x","content":"result"},
			{"type":"function_call","call_id":"call_x","name":"f","arguments":"{}"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want exactly 2 (assistant + 1 tool, no duplicate): %#v", len(messages), messages)
	}
	// First must be assistant, not tool
	m0, _ := messages[0].(map[string]any)
	if m0["role"] != "assistant" {
		t.Fatalf("messages[0] role = %#v, want assistant (no leading tool)", m0["role"])
	}
	// Second must be the single tool message
	m1, _ := messages[1].(map[string]any)
	if m1["role"] != "tool" {
		t.Fatalf("messages[1] role = %#v, want tool", m1["role"])
	}
	if m1["tool_call_id"] != "call_x" {
		t.Fatalf("tool_call_id = %#v, want call_x", m1["tool_call_id"])
	}
	if m1["content"] != "result" {
		t.Fatalf("content = %#v, want 'result'", m1["content"])
	}
}

// TestResponsesToolResult_CallBeforeOutput_NoDuplicate verifies the original
// order (call before output) also produces exactly assistant + 1 tool.
func TestResponsesToolResult_CallBeforeOutput_NoDuplicate(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_y","name":"f","arguments":"{}"},
			{"type":"tool_result","call_id":"call_y","content":"result_y"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want exactly 2: %#v", len(messages), messages)
	}
	m0, _ := messages[0].(map[string]any)
	if m0["role"] != "assistant" {
		t.Fatalf("messages[0] role = %#v, want assistant", m0["role"])
	}
	m1, _ := messages[1].(map[string]any)
	if m1["role"] != "tool" {
		t.Fatalf("messages[1] role = %#v, want tool", m1["role"])
	}
	if m1["tool_call_id"] != "call_y" {
		t.Fatalf("tool_call_id = %#v, want call_y", m1["tool_call_id"])
	}
	if m1["content"] != "result_y" {
		t.Fatalf("content = %#v, want 'result_y'", m1["content"])
	}
}

// TestResponsesToolResult_StandaloneOutputStillWorks verifies that a
// standalone output with no matching call in the same array still produces
// a tool message (for previous-response-id replay scenarios).
func TestResponsesToolResult_StandaloneOutputStillWorks(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	// Only an output, no matching call — should still produce a tool message
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"tool_result","call_id":"call_standalone","content":"standalone result"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	// Should have at least one tool message with the standalone output
	found := false
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" && mm["tool_call_id"] == "call_standalone" {
			if mm["content"] != "standalone result" {
				t.Fatalf("content = %#v, want 'standalone result'", mm["content"])
			}
			found = true
		}
	}
	if !found {
		t.Fatal("standalone tool message not found")
	}
}

// =====================================================================
// Fix 2: validateClaudeDocumentBlocks content-structure-aware
// =====================================================================

// TestClaudeValidateDocument_ToolUseInputNotFalsePositive verifies that a
// tool_use block whose input contains a field like {kind:{type:"document"}}
// does NOT trigger a false document validation (it must reach upstream).
func TestClaudeValidateDocument_ToolUseInputNotFalsePositive(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[{"role":"assistant","content":[
			{"type":"tool_use","id":"call_1","name":"classify","input":{"kind":{"type":"document"}}}
		]}]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (tool_use input should not trigger document validation)", rec.Code)
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream should be called, payloads = %d", len(transport.requestPayloads))
	}
}

// TestClaudeValidateDocument_MalformedInToolResultContent verifies that a
// malformed document nested inside tool_result.content is still rejected
// with a 400 (without reaching upstream).
func TestClaudeValidateDocument_MalformedInToolResultContent(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call_1","content":[
				{"type":"text","text":"result"},
				{"type":"document","source":{"type":"base64","data":""}}
			]}
		]}]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (malformed document in tool_result content)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %s", ct)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	errObj, _ := body["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type = %#v", errObj["type"])
	}
}

// TestClaudeValidateDocument_ValidInToolResultContent verifies that a valid
// document inside tool_result.content passes validation and reaches upstream.
func TestClaudeValidateDocument_ValidInToolResultContent(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"primary-model",
		"max_tokens":256,
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call_1","content":[
				{"type":"text","text":"result"},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}
			]}
		]}]
	}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (valid document should pass)", rec.Code)
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream should be called, payloads = %d", len(transport.requestPayloads))
	}
}

// =====================================================================
// Test hardening: streaming temperature 400 across all protocols
// =====================================================================

// TestTemperature_Stream400_AllProtocols verifies that invalid streaming
// temperature returns JSON 400 (not SSE) across chat, claude, and responses,
// and that the upstream is never called.
func TestTemperature_Stream400_AllProtocols(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		body     string
	}{
		{
			name:     "chat",
			protocol: "chat",
			body:     `{"model":"primary-model","temperature":3,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "claude",
			protocol: "claude",
			body:     `{"model":"primary-model","temperature":2,"stream":true,"max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "responses",
			protocol: "responses",
			body:     `{"model":"primary-model","temperature":3,"stream":true,"input":"hi"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
				status: http.StatusOK,
				body:   okChatBody(),
			}})
			rec := httptest.NewRecorder()
			runTempHandler(t, tc.protocol, rec, httptest.NewRequest(http.MethodPost, "/"+protocolPath(tc.protocol), strings.NewReader(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type = %s, want application/json (not SSE)", ct)
			}
			var resp map[string]any
			json.Unmarshal(rec.Body.Bytes(), &resp)
			switch tc.protocol {
			case "claude":
				if resp["type"] != "error" {
					t.Fatalf("type = %#v", resp["type"])
				}
				errObj, _ := resp["error"].(map[string]any)
				if errObj["type"] != "invalid_request_error" {
					t.Fatalf("error.type = %#v", errObj["type"])
				}
			default:
				errObj, _ := resp["error"].(map[string]any)
				if errObj["type"] != "invalid_request_error" {
					t.Fatalf("error.type = %#v", errObj["type"])
				}
				if errObj["param"] != "temperature" {
					t.Fatalf("error.param = %#v", errObj["param"])
				}
			}
			if len(transport.requestPayloads) != 0 {
				t.Fatalf("upstream should not be called on rejection, payloads = %d", len(transport.requestPayloads))
			}
		})
	}
}

// TestResponsesToolResult_OutputBeforeCall_NestedToolUseWrapper verifies
// that output-before-call with a nested tool_use wrapper (where call ID is
// extracted from tool_use.id, not top-level call_id) does not produce a
// leading duplicate tool message.
func TestResponsesToolResult_OutputBeforeCall_NestedToolUseWrapper(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	// output (tool_use_id) before call (tool_use.id wrapper)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"tool_result","tool_use_id":"c_nested","content":"nested result"},
			{"type":"tool_call","tool_use":{"id":"c_nested","name":"f","input":{}}}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	// Assert exact sequence: assistant(call) then exactly one tool message
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want exactly 2 (assistant + 1 tool, no duplicate): %#v", len(messages), messages)
	}
	m0, _ := messages[0].(map[string]any)
	if m0["role"] != "assistant" {
		t.Fatalf("messages[0] role = %#v, want assistant (no leading tool)", m0["role"])
	}
	m1, _ := messages[1].(map[string]any)
	if m1["role"] != "tool" {
		t.Fatalf("messages[1] role = %#v, want tool", m1["role"])
	}
	if m1["tool_call_id"] != "c_nested" {
		t.Fatalf("tool_call_id = %#v, want c_nested", m1["tool_call_id"])
	}
	if m1["content"] != "nested result" {
		t.Fatalf("content = %#v, want 'nested result'", m1["content"])
	}
}

// TestResponsesToolResult_CallBeforeOutput_NestedToolUseWrapper verifies the
// call-before-output order with the nested tool_use wrapper also produces
// exactly assistant + 1 tool.
func TestResponsesToolResult_CallBeforeOutput_NestedToolUseWrapper(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"tool_call","tool_use":{"id":"c_nested2","name":"f","input":{}}},
			{"type":"tool_result","tool_use_id":"c_nested2","content":"nested result 2"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want exactly 2: %#v", len(messages), messages)
	}
	m0, _ := messages[0].(map[string]any)
	if m0["role"] != "assistant" {
		t.Fatalf("messages[0] role = %#v, want assistant", m0["role"])
	}
	m1, _ := messages[1].(map[string]any)
	if m1["role"] != "tool" {
		t.Fatalf("messages[1] role = %#v, want tool", m1["role"])
	}
	if m1["tool_call_id"] != "c_nested2" {
		t.Fatalf("tool_call_id = %#v, want c_nested2", m1["tool_call_id"])
	}
	if m1["content"] != "nested result 2" {
		t.Fatalf("content = %#v, want 'nested result 2'", m1["content"])
	}
}

// TestResponsesValidator_ToolCallInputNotFalsePositive verifies that a
// function_call item whose arguments contain a nested object with
// type:"input_file" does NOT trigger the file validator (it must reach
// upstream). The validator only inspects top-level items and message content
// arrays, not function/tool arguments or nested tool_use.input.
func TestResponsesValidator_ToolCallInputNotFalsePositive(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	// A function_call with arguments that happen to contain type:"input_file"
	// nested inside a tool argument — must NOT be rejected by the file
	// validator.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{
				"type":"function_call",
				"call_id":"call_fp",
				"name":"upload",
				"arguments":"{\"files\":[{\"type\":\"input_file\",\"file_id\":\"nonexistent\"}]}"
			}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = 400, want non-400 (tool arguments must not trigger file validator): %s", rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream should be called, payloads = %d", len(transport.requestPayloads))
	}
}

// TestResponsesValidator_ToolCallNestedToolUseInputNotFalsePositive verifies
// that a tool_call with a nested tool_use.input containing type:"input_file"
// is not falsely rejected.
func TestResponsesValidator_ToolCallNestedToolUseInputNotFalsePositive(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{
				"type":"tool_call",
				"tool_use":{
					"id":"call_tu",
					"name":"process",
					"input":{"attachment":{"type":"input_file"}}
				}
			}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = 400, want non-400 (nested tool_use.input must not trigger file validator): %s", rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream should be called, payloads = %d", len(transport.requestPayloads))
	}
}

// TestResponsesValidator_MessageContentInputFileStillValidated verifies that
// an actual input_file content part inside a message content array IS still
// validated (the structure-aware validator does not skip real content parts).
func TestResponsesValidator_MessageContentInputFileStillValidated(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
	// A real input_file content part with no payload inside a message — must
	// still be rejected with 400.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[{"role":"user","content":[
			{"type":"input_text","text":"hi"},
			{"type":"input_file"}
		]}]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (actual input_file content part must be validated)", rec.Code)
	}
}

// TestResponsesValidator_TopLevelInputFileStillValidated verifies that a
// top-level input_file item with no payload is still rejected.
func TestResponsesValidator_TopLevelInputFileStillValidated(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: okChatBody()}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"input_file"}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (top-level input_file must be validated)", rec.Code)
	}
}

// TestResponsesMessages_FieldUsesChatShapes_NotResponsesInputFile verifies
// that the nonstandard `messages` compatibility field uses Chat content
// shapes. A Responses-style input_file part in messages[].content is NOT
// validated or converted — it is forwarded as-is. Use the official `input`
// field for input_file support.
func TestResponsesMessages_FieldUsesChatShapes_NotResponsesInputFile(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	// A Responses-style input_file in messages[].content. It should NOT be
	// rejected by the validator (the messages field uses Chat content shapes),
	// and it should be forwarded as-is (not converted to type:"file").
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"messages":[
			{"role":"user","content":[
				{"type":"input_file","file_id":"file-msg-test"}
			]}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	// Should reach upstream (not rejected by file validator)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = 400, want non-400 (messages field should not validate Responses input_file): %s", rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream should be called, payloads = %d", len(transport.requestPayloads))
	}
	// The input_file part is forwarded as-is (not converted to type:"file")
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	user, _ := messages[0].(map[string]any)
	contentParts, _ := user["content"].([]any)
	part, _ := contentParts[0].(map[string]any)
	if part["type"] != "input_file" {
		t.Fatalf("part type = %#v, want input_file (messages field forwards as-is, not converted to file)", part["type"])
	}
}

// TestResponsesValidator_InputFileInToolResultNotValidated verifies that an
// input_file-looking object inside a tool_result/function_call_output content
// array is NOT treated as a supported file input. It is not validated (no
// 400), and it is silently normalized to empty output by the text-only
// joinToolResultContent path — proving this location is not supported.
func TestResponsesValidator_InputFileInToolResultNotValidated(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	// An input_file-looking object inside a tool_result content array.
	// It must NOT be validated as a file input (no 400), because
	// tool_result content uses text shapes only.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_tr","name":"f","arguments":"{}"},
			{"type":"tool_result","call_id":"call_tr","content":[
				{"type":"input_file","file_id":"should-be-ignored"},
				{"type":"text","text":"actual text result"}
			]}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	// Must NOT be rejected by the file validator
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = 400, want non-400 (input_file in tool_result content must not be validated): %s", rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream should be called, payloads = %d", len(transport.requestPayloads))
	}
	// The input_file part is silently dropped (not converted to file), and
	// only the text part is preserved as the tool output.
	messages, _ := transport.requestPayloads[0]["messages"].([]any)
	for _, m := range messages {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" {
			if got := mm["content"]; got != "actual text result" {
				t.Fatalf("tool content = %#v, want 'actual text result' (input_file in tool_result is not supported, only text)", got)
			}
			return
		}
	}
	t.Fatal("no tool message found")
}

// TestResponsesValidator_MalformedInputFileInToolResultNotRejected verifies
// that a malformed (no payload) input_file inside tool_result content does
// NOT produce a file-specific 400 — it is silently ignored.
func TestResponsesValidator_MalformedInputFileInToolResultNotRejected(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{"type":"function_call","call_id":"call_mal","name":"f","arguments":"{}"},
			{"type":"tool_result","call_id":"call_mal","content":[
				{"type":"input_file"},
				{"type":"text","text":"ok"}
			]}
		]
	}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("status = 400, want non-400 (malformed input_file in tool_result must not trigger file 400): %s", rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream should be called, payloads = %d", len(transport.requestPayloads))
	}
}

func TestResponsesEcho_MissingFieldsAreEchoed(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	reqBody := `{
		"model":"primary-model",
		"input":"hello",
		"instructions":"You are a helpful assistant.",
		"user":"user-123",
		"previous_response_id":"resp_abc",
		"stop":["stop1","stop2"],
		"frequency_penalty":0.5,
		"presence_penalty":0.3,
		"text":{"format":{"type":"json_object"}},
		"truncation":"auto",
		"service_tier":"default",
		"prompt_cache_key":"cache-key-1",
		"reasoning":{"effort":"high","summary":"auto"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()

	responsesHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream payload count = %d, want 1", len(transport.requestPayloads))
	}

	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	checks := []struct {
		key  string
		want any
	}{
		{"instructions", "You are a helpful assistant."},
		{"user", "user-123"},
		{"previous_response_id", "resp_abc"},
		{"truncation", "auto"},
		{"service_tier", "default"},
		{"prompt_cache_key", "cache-key-1"},
	}
	for _, c := range checks {
		if got := response[c.key]; got != c.want {
			t.Errorf("response[%q] = %#v, want %#v", c.key, got, c.want)
		}
	}

	// stop is an array
	stop, ok := response["stop"].([]any)
	if !ok || len(stop) != 2 {
		t.Errorf("response[\"stop\"] = %#v, want 2-element array", response["stop"])
	} else {
		if stop[0] != "stop1" || stop[1] != "stop2" {
			t.Errorf("response[\"stop\"] = %#v, want [stop1,stop2]", stop)
		}
	}

	// frequency_penalty
	if fp, ok := response["frequency_penalty"].(float64); !ok || fp != 0.5 {
		t.Errorf("response[\"frequency_penalty\"] = %#v, want 0.5", response["frequency_penalty"])
	}

	// presence_penalty
	if pp, ok := response["presence_penalty"].(float64); !ok || pp != 0.3 {
		t.Errorf("response[\"presence_penalty\"] = %#v, want 0.3", response["presence_penalty"])
	}

	// text (echoed as-is)
	text, ok := response["text"].(map[string]any)
	if !ok {
		t.Errorf("response[\"text\"] = %#v, want object", response["text"])
	} else {
		format, ok := text["format"].(map[string]any)
		if !ok {
			t.Errorf("response[\"text\"][\"format\"] = %#v, want object", text["format"])
		} else if format["type"] != "json_object" {
			t.Errorf("response[\"text\"][\"format\"][\"type\"] = %#v, want json_object", format["type"])
		}
	}

	// reasoning echo includes both effort and summary
	reasoning, ok := response["reasoning"].(map[string]any)
	if !ok {
		t.Errorf("response[\"reasoning\"] = %#v, want object", response["reasoning"])
	} else {
		if reasoning["effort"] != "high" {
			t.Errorf("response[\"reasoning\"][\"effort\"] = %#v, want high", reasoning["effort"])
		}
		if reasoning["summary"] != "auto" {
			t.Errorf("response[\"reasoning\"][\"summary\"] = %#v, want auto", reasoning["summary"])
		}
	}
}

func TestResponsesInclude_EncryptedContentOnlyWhenRequested(t *testing.T) {
	// Without include: no encrypted_content field
	body := convertChatToResponses([]byte(`{"id":"r","created":1,"choices":[{"finish_reason":"stop","message":{"reasoning_content":"thinking...","content":"answer"}}]}`), "m", true, nil, nil, nil)
	var resp map[string]any
	json.Unmarshal(body, &resp)
	for _, item := range resp["output"].([]any) {
		m := item.(map[string]any)
		if m["type"] == "reasoning" {
			if _, ok := m["encrypted_content"]; ok {
				t.Fatal("encrypted_content should be absent when include is nil")
			}
		}
	}

	// With include: encrypted_content present
	body = convertChatToResponses([]byte(`{"id":"r","created":1,"choices":[{"finish_reason":"stop","message":{"reasoning_content":"thinking...","content":"answer"}}]}`), "m", true, nil, nil, []string{"reasoning.encrypted_content"})
	json.Unmarshal(body, &resp)
	for _, item := range resp["output"].([]any) {
		m := item.(map[string]any)
		if m["type"] == "reasoning" {
			if _, ok := m["encrypted_content"]; !ok {
				t.Fatal("encrypted_content should be present when include has reasoning.encrypted_content")
			}
		}
	}
}
