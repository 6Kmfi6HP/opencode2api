package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test_ResponsesHandler_preserves_multimodal_input_when_message_item_contains_image(t *testing.T) {
	// Given
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"chatcmpl_test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":[
			{
				"role":"user",
				"content":[
					{"type":"input_text","text":"what is in this image?"},
					{"type":"input_image","image_url":"https://example.com/cat.png","detail":"high"}
				]
			}
		]
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
	messages, ok := transport.requestPayloads[0]["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want one upstream message", transport.requestPayloads[0]["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", messages[0])
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %#v, want text+image content array", message["content"])
	}
	textPart, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("text part = %#v, want object", content[0])
	}
	if got := textPart["type"]; got != "text" {
		t.Fatalf("text part type = %#v, want text", got)
	}
	if got := textPart["text"]; got != "what is in this image?" {
		t.Fatalf("text part text = %#v, want preserved prompt", got)
	}
	imagePart, ok := content[1].(map[string]any)
	if !ok {
		t.Fatalf("image part = %#v, want object", content[1])
	}
	if got := imagePart["type"]; got != "image_url" {
		t.Fatalf("image part type = %#v, want image_url", got)
	}
	imageURL, ok := imagePart["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url = %#v, want object", imagePart["image_url"])
	}
	if got := imageURL["url"]; got != "https://example.com/cat.png" {
		t.Fatalf("image url = %#v, want preserved image url", got)
	}
	if got := imageURL["detail"]; got != "high" {
		t.Fatalf("image detail = %#v, want high", got)
	}
}

func Test_ResponsesHandler_merges_stream_options_and_parallel_tool_calls_when_streaming(t *testing.T) {
	// Given
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body: strings.Join([]string{
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"primary-model",
		"input":"hello",
		"stream":true,
		"parallel_tool_calls":true,
		"stream_options":{"event_frequency":"token"}
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
	if got := transport.requestPayloads[0]["parallel_tool_calls"]; got != true {
		t.Fatalf("parallel_tool_calls = %#v, want true", got)
	}
	streamOptions, ok := transport.requestPayloads[0]["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v, want object", transport.requestPayloads[0]["stream_options"])
	}
	if got := streamOptions["event_frequency"]; got != "token" {
		t.Fatalf("stream_options.event_frequency = %#v, want token", got)
	}
	if got := streamOptions["include_usage"]; got != true {
		t.Fatalf("stream_options.include_usage = %#v, want true default", got)
	}
}

func Test_ConvertChatToResponses_preserves_array_message_content_when_upstream_returns_parts(t *testing.T) {
	// Given
	chatBody := []byte(`{
		"id":"chatcmpl_test",
		"created":123,
		"choices":[
			{
				"finish_reason":"stop",
				"message":{
					"content":[
						{"type":"text","text":"first"},
						{"type":"text","text":"second","annotations":[{"type":"citation"}]}
					]
				}
			}
		]
	}`)

	// When
	body := convertChatToResponses(chatBody, "primary-model", false, nil, nil)

	// Then
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output = %#v, want one message item", response["output"])
	}
	message, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", output[0])
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %#v, want two output_text parts", message["content"])
	}
	firstPart, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("first part = %#v, want object", content[0])
	}
	if got := firstPart["type"]; got != "output_text" {
		t.Fatalf("first part type = %#v, want output_text", got)
	}
	if got := firstPart["text"]; got != "first" {
		t.Fatalf("first part text = %#v, want first", got)
	}
	secondPart, ok := content[1].(map[string]any)
	if !ok {
		t.Fatalf("second part = %#v, want object", content[1])
	}
	annotations, ok := secondPart["annotations"].([]any)
	if !ok || len(annotations) != 1 {
		t.Fatalf("second part annotations = %#v, want preserved annotations", secondPart["annotations"])
	}
	if got := secondPart["text"]; got != "second" {
		t.Fatalf("second part text = %#v, want second", got)
	}
}
