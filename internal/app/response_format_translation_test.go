package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sendResponsesWithText issues a /v1/responses request carrying the given
// `text` parameter and returns the upstream chat payload the proxy built.
func sendResponsesWithText(t *testing.T, textJSON string) map[string]any {
	t.Helper()
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	var reqBody string
	if textJSON == "" {
		reqBody = `{"model":"primary-model","input":"hello"}`
	} else {
		reqBody = `{"model":"primary-model","input":"hello","text":` + textJSON + `}`
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(transport.requestPayloads) != 1 {
		t.Fatalf("upstream payloads=%d, want 1", len(transport.requestPayloads))
	}
	return transport.requestPayloads[0]
}

// TestResponsesTextFormat_JsonObject verifies the Responses API
// `text.format.type=json_object` is translated into the Chat Completions
// `response_format` with a top-level `type` (which upstream providers require).
func TestResponsesTextFormat_JsonObject(t *testing.T) {
	payload := sendResponsesWithText(t, `{"format":{"type":"json_object"}}`)
	rf, ok := payload["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing/not object: %#v", payload["response_format"])
	}
	if rf["type"] != "json_object" {
		t.Fatalf("response_format.type = %#v, want json_object", rf["type"])
	}
	if len(rf) != 1 {
		t.Fatalf("response_format should contain only type, got %#v", rf)
	}
}

// TestResponsesTextFormat_Text verifies `text.format.type=text` maps to
// `response_format: {"type":"text"}`.
func TestResponsesTextFormat_Text(t *testing.T) {
	payload := sendResponsesWithText(t, `{"format":{"type":"text"}}`)
	rf, ok := payload["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing/not object: %#v", payload["response_format"])
	}
	if rf["type"] != "text" {
		t.Fatalf("response_format.type = %#v, want text", rf["type"])
	}
}

// TestResponsesTextFormat_JsonSchema verifies the Responses API
// `text.format` (json_schema) is translated into the Chat Completions
// `response_format.json_schema` nested shape with all supported fields.
func TestResponsesTextFormat_JsonSchema(t *testing.T) {
	payload := sendResponsesWithText(t, `{"format":{
		"type":"json_schema",
		"name":"math_response",
		"description":"a math answer",
		"strict":true,
		"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}
	}}`)
	rf, ok := payload["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format missing/not object: %#v", payload["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format.type = %#v, want json_schema", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("response_format.json_schema missing: %#v", rf)
	}
	if js["name"] != "math_response" {
		t.Errorf("json_schema.name = %#v, want math_response", js["name"])
	}
	if js["description"] != "a math answer" {
		t.Errorf("json_schema.description = %#v", js["description"])
	}
	if js["strict"] != true {
		t.Errorf("json_schema.strict = %#v, want true", js["strict"])
	}
	if _, ok := js["schema"]; !ok {
		t.Errorf("json_schema.schema missing: %#v", js)
	}
	// The raw Responses format object must not leak into response_format.
	if _, leaked := rf["format"]; leaked {
		t.Errorf("response_format must not contain Responses-style `format` key: %#v", rf)
	}
}

// TestResponsesTextFormat_NoLeakOfVerbosity verifies `text.verbosity` is NOT
// forwarded into response_format (it has no Chat Completions equivalent).
func TestResponsesTextFormat_NoLeakOfVerbosity(t *testing.T) {
	payload := sendResponsesWithText(t, `{"format":{"type":"json_object"},"verbosity":"high"}`)
	rf, _ := payload["response_format"].(map[string]any)
	if _, leaked := rf["verbosity"]; leaked {
		t.Fatalf("verbosity must not leak into response_format: %#v", rf)
	}
	if rf["type"] != "json_object" {
		t.Fatalf("response_format.type = %#v, want json_object", rf["type"])
	}
}

// TestResponsesTextFormat_DroppedWhenUntranslatable verifies malformed or
// untranslatable `text` values are silently dropped rather than forwarded as
// a malformed response_format (which upstream rejects with 400).
func TestResponsesTextFormat_DroppedWhenUntranslatable(t *testing.T) {
	cases := []string{
		`{"verbosity":"high"}`,              // format absent
		`{"format":{"type":"json_schema"}}`, // json_schema without name/schema
		`{"format":{"type":"weird"}}`,       // unknown format type
		`"high"`,                            // text as a plain string
		`[]`,                                // non-object
	}
	for _, tc := range cases {
		payload := sendResponsesWithText(t, tc)
		if _, ok := payload["response_format"]; ok {
			t.Errorf("text=%s: response_format must be dropped, got %#v", tc, payload["response_format"])
		}
	}
}

// TestResponsesTextFormat_NoTextStillSendsUpstream verifies a request without
// the text parameter is unchanged (no response_format at all).
func TestResponsesTextFormat_NoTextStillSendsUpstream(t *testing.T) {
	payload := sendResponsesWithText(t, "")
	if _, ok := payload["response_format"]; ok {
		t.Fatalf("no text param must not add response_format: %#v", payload["response_format"])
	}
}

// TestResponsesTextFormat_ResponseEchoKeepsOriginal verifies the response
// body still echoes the client's original `text` object as-is (client-facing
// echo is independent of the upstream translation).
func TestResponsesTextFormat_ResponseEchoKeepsOriginal(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   okChatBody(),
	}})
	reqBody := `{"model":"primary-model","input":"hello","text":{"format":{"type":"json_object"}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	text, ok := resp["text"].(map[string]any)
	if !ok {
		t.Fatalf("response text missing/not object: %#v", resp["text"])
	}
	format, ok := text["format"].(map[string]any)
	if !ok || format["type"] != "json_object" {
		t.Fatalf("response text echo changed: %#v", resp["text"])
	}
	// Upstream payload must still use the translated shape.
	payload := transport.requestPayloads[0]
	rf, _ := payload["response_format"].(map[string]any)
	if rf == nil || rf["type"] != "json_object" {
		t.Fatalf("upstream response_format = %#v, want translated json_object", payload["response_format"])
	}
}
