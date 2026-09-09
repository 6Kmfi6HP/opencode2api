package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
)

// ======================== text-only model downgrade ========================

func TestIsTextOnlyModelDefaultMatchesDeepseek(t *testing.T) {
	old := config.Get()
	defer func() { config.Update(func(s *config.Snapshot) { *s = old }) }()
	config.Update(func(s *config.Snapshot) { s.TextOnlyModels = []string{"deepseek"} })

	if !config.IsTextOnlyModel("deepseek-v4-flash") {
		t.Fatal("deepseek-v4-flash should be text-only")
	}
	if !config.IsTextOnlyModel("deepseek-v4-flash-free") {
		t.Fatal("deepseek-v4-flash-free should be text-only")
	}
	if !config.IsTextOnlyModel("DEEPSEEK-v4-flash") {
		t.Fatal("matching should be case-insensitive")
	}
	if config.IsTextOnlyModel("gpt-5.5") {
		t.Fatal("gpt-5.5 should not be text-only")
	}
	if config.IsTextOnlyModel("") {
		t.Fatal("empty model should not be text-only")
	}
}

func TestIsTextOnlyModelConfigOverride(t *testing.T) {
	old := config.Get()
	defer func() { config.Update(func(s *config.Snapshot) { *s = old }) }()
	// An explicit config replaces the default list.
	config.Update(func(s *config.Snapshot) { s.TextOnlyModels = []string{"gpt"} })

	if config.IsTextOnlyModel("deepseek-v4-flash") {
		t.Fatal("config override should drop the deepseek default")
	}
	if !config.IsTextOnlyModel("gpt-5.5") {
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
	old := config.Get()
	defer func() { config.Update(func(s *config.Snapshot) { *s = old }) }()
	config.Update(func(s *config.Snapshot) { s.TextOnlyModels = []string{"deepseek"} })

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
	old := config.Get()
	defer func() { config.Update(func(s *config.Snapshot) { *s = old }) }()
	config.Update(func(s *config.Snapshot) { s.TextOnlyModels = []string{"deepseek"} })

	req := multimodalRequest("gpt-5.5")
	body := buildUpstreamBody(&req)
	got := contentTypes(contentParts(body))
	if strings.Join(got, ",") != "text,image_url,file,text" {
		t.Fatalf("vision model content types = %v, want text,image_url,file,text", got)
	}
}

func TestConvertMessagesForUpstreamTextOnlyKeepsPlainStrings(t *testing.T) {
	old := config.Get()
	defer func() { config.Update(func(s *config.Snapshot) { *s = old }) }()
	config.Update(func(s *config.Snapshot) { s.TextOnlyModels = []string{"deepseek"} })

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

// withTextOnlyState pins both judgment inputs: the configured prefixes and the
// models.dev modalities the union decision reads.
func withTextOnlyState(t *testing.T, prefixes []string, mods modelsdev.Modalities) {
	t.Helper()
	blockModelsDevTransport(t)
	old := config.Get()
	tmpCachePath := filepath.Join(t.TempDir(), "modelsdev_cache.json")
	modelsdev.SetCachePath(tmpCachePath)
	os.Remove(tmpCachePath) // drop anything an earlier test fetched into it
	modelsdev.ClearModalitiesForTest()
	modelsdev.SetModalitiesForTest(mods)
	config.Update(func(s *config.Snapshot) { s.TextOnlyModels = prefixes })
	t.Cleanup(func() {
		config.Update(func(s *config.Snapshot) { *s = old })
		modelsdev.ClearModalitiesForTest()
	})
}

func TestModelIsTextOnlyDrivenByModelsDevData(t *testing.T) {
	withTextOnlyState(t, nil, modelsdev.Modalities{
		"glm-5.2":             {"text"},
		"deepseek-v4-flash":   {"text"},
		"deepseek-vision-exp": {"text", "image"},
		"grok-4.5":            {"text", "image"},
	})

	if !modelIsTextOnly("glm-5.2") {
		t.Fatal("glm-5.2 (input=[text]) should be text-only without any configured prefix")
	}
	if !modelIsTextOnly("deepseek-v4-flash-free") {
		t.Fatal("deepseek-v4-flash-free should inherit the base model's text-only data")
	}
	if modelIsTextOnly("deepseek-vision-exp") {
		t.Fatal("vision-capable model must not be treated as text-only")
	}
	if modelIsTextOnly("grok-4.5") {
		t.Fatal("image-capable model must not be treated as text-only")
	}
	if modelIsTextOnly("model-unknown-to-catalog") {
		t.Fatal("unknown models must fail open (not text-only)")
	}
}

func TestModelIsTextOnlyConfigPrefixUnion(t *testing.T) {
	withTextOnlyState(t, []string{"deepseek"}, modelsdev.Modalities{
		"deepseek-vision-exp": {"text", "image"},
	})

	if !modelIsTextOnly("deepseek-vision-exp") {
		t.Fatal("configured prefix must keep its force-downgrade power on top of the data")
	}
	if !modelIsTextOnly("deepseek-anything-new") {
		t.Fatal("configured prefix covers models the catalog does not know")
	}
}

func TestBuildUpstreamBodyDowngradesForModelsDevTextOnlyModel(t *testing.T) {
	withTextOnlyState(t, nil, modelsdev.Modalities{"glm-5.2": {"text"}})

	req := multimodalRequest("glm-5.2")
	body := buildUpstreamBody(&req)
	got := contentTypes(contentParts(body))
	if strings.Join(got, ",") != "text,text,text,text" {
		t.Fatalf("content types = %v, want all text for a catalog text-only model", got)
	}
}

func TestModelIsTextOnlyWithoutAnyCatalogData(t *testing.T) {
	withTextOnlyState(t, nil, nil)

	if modelIsTextOnly("deepseek-v4-flash-free") {
		t.Fatal("with no catalog data and no prefixes nothing may be judged text-only")
	}
}
