package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// ======================== text-only model downgrade ========================

func TestIsTextOnlyModelDefaultMatchesDeepseek(t *testing.T) {
	old := textOnlyModels
	defer func() { textOnlyModels = old }()
	textOnlyModels = []string{"deepseek"}

	if !isTextOnlyModel("deepseek-v4-flash") {
		t.Fatal("deepseek-v4-flash should be text-only")
	}
	if !isTextOnlyModel("deepseek-v4-flash-free") {
		t.Fatal("deepseek-v4-flash-free should be text-only")
	}
	if !isTextOnlyModel("DEEPSEEK-v4-flash") {
		t.Fatal("matching should be case-insensitive")
	}
	if isTextOnlyModel("gpt-5.5") {
		t.Fatal("gpt-5.5 should not be text-only")
	}
	if isTextOnlyModel("") {
		t.Fatal("empty model should not be text-only")
	}
}

func TestIsTextOnlyModelConfigOverride(t *testing.T) {
	old := textOnlyModels
	defer func() { textOnlyModels = old }()
	// An explicit config replaces the default list.
	textOnlyModels = []string{"gpt"}

	if isTextOnlyModel("deepseek-v4-flash") {
		t.Fatal("config override should drop the deepseek default")
	}
	if !isTextOnlyModel("gpt-5.5") {
		t.Fatal("configured prefix gpt should match gpt-5.5")
	}
}

func TestCountMultimodalParts(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "plain text"},
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "hi"},
			map[string]any{"type": "image_url", "image_url": map[string]string{"url": "https://example.test/a.png"}},
			map[string]any{"type": "file", "file": map[string]any{"file_data": "data:application/pdf;base64,abc"}},
		}},
	}
	if got := countMultimodalParts(msgs); got != 2 {
		t.Fatalf("countMultimodalParts = %d, want 2", got)
	}
}

// messagesWithImage returns a request whose user message mixes text, an
// image_url part, and a file part in that order.
func multimodalRequest(model string) OpenAIRequest {
	return OpenAIRequest{
		Model: model,
		Messages: []Message{
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "before"},
				map[string]any{"type": "image_url", "image_url": map[string]string{"url": "https://example.test/a.png"}},
				map[string]any{"type": "file", "file": map[string]any{"filename": "a.pdf"}},
				map[string]any{"type": "text", "text": "after"},
			}},
		},
	}
}

func contentParts(body []byte) []any {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		panic("unmarshal upstream body: " + err.Error())
	}
	msgs, _ := obj["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].([]any)
	return content
}

func contentTypes(parts []any) []string {
	var types []string
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if t, ok := pm["type"].(string); ok {
			types = append(types, t)
		}
	}
	return types
}

func TestBuildUpstreamBodyDowngradesMultimodalForTextOnlyModel(t *testing.T) {
	old := textOnlyModels
	defer func() { textOnlyModels = old }()
	textOnlyModels = []string{"deepseek"}

	req := multimodalRequest("deepseek-v4-flash-free")
	body := buildUpstreamBody(&req)
	parts := contentParts(body)
	got := contentTypes(parts)

	if strings.Join(got, ",") != "text,text,text,text" {
		t.Fatalf("content types = %v, want all text", got)
	}
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		txt, _ := pm["text"].(string)
		texts = append(texts, txt)
	}
	want := []string{"before", "[image attached]", "[document attached]", "after"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("downgraded texts = %v, want %v (part order preserved)", texts, want)
	}
}

func TestBuildUpstreamBodyPreservesMultimodalForVisionModel(t *testing.T) {
	old := textOnlyModels
	defer func() { textOnlyModels = old }()
	textOnlyModels = []string{"deepseek"}

	req := multimodalRequest("gpt-5.5")
	body := buildUpstreamBody(&req)
	got := contentTypes(contentParts(body))
	if strings.Join(got, ",") != "text,image_url,file,text" {
		t.Fatalf("vision model content types = %v, want text,image_url,file,text", got)
	}
}

func TestConvertMessagesForUpstreamTextOnlyKeepsPlainStrings(t *testing.T) {
	old := textOnlyModels
	defer func() { textOnlyModels = old }()
	textOnlyModels = []string{"deepseek"}

	req := OpenAIRequest{
		Model: "deepseek-v4-flash",
		Messages: []Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "world"},
		},
	}
	converted := convertRequest(&req)
	msgs, _ := converted["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if got := msgs[0]["content"]; got != "hello" {
		t.Fatalf("plain string content = %#v, want hello", got)
	}
	if got := msgs[1]["content"]; got != "world" {
		t.Fatalf("plain string content = %#v, want world", got)
	}
}
