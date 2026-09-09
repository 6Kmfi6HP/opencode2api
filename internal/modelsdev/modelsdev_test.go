package modelsdev

import "testing"

func TestGetContextWindow(t *testing.T) {
	catalog := Catalog{
		"deepseek-v4-flash": 131072,
		"qwen3-coder-flash": 1000000,
		"glm-5.2":           200000,
		"claude-opus-4-8":   1000000,
		"x-preview-f-free":  1000000,
		"grok-4.3-free":     1000000,
	}

	tests := []struct {
		modelID string
		want    int
	}{
		// Exact match.
		{"deepseek-v4-flash", 131072},
		{"qwen3-coder-flash", 1000000},
		{"glm-5.2", 200000},
		// Strip "-free" suffix: "deepseek-v4-flash-free" → "deepseek-v4-flash".
		{"deepseek-v4-flash-free", 131072},
		{"qwen3-coder-flash-free", 1000000},
		{"claude-opus-4-8-free", 1000000},
		// Add "-free" suffix: "x-preview-f" → "x-preview-f-free".
		{"x-preview-f", 1000000}, // add -free: x-preview-f → x-preview-f-free (1M)
		{"grok-4.3", 1000000},
		// No match by any strategy.
		{"unknown-model", 0},
		{"unknown-model-free", 0},
		{"totally-different", 0},
		// Empty.
		{"", 0},
	}

	for _, tc := range tests {
		got := ContextWindow(tc.modelID, catalog)
		if got != tc.want {
			t.Errorf("ContextWindow(%q) = %d, want %d", tc.modelID, got, tc.want)
		}
	}
}

func TestIsTextOnly(t *testing.T) {
	mods := Modalities{
		"deepseek-v4-flash":      {"text"},
		"glm-5.3-flash":          {"text", "image", "video", "pdf"},
		"deepseek-vision-exp":    {"text", "image"},
		"x-preview-f-free":       {"text"},
		"explicit-free":          {"text", "image"},
		"explicit-free-free":     {"text"},
		"empty-input-considered": {},
	}

	tests := []struct {
		modelID string
		want    bool
	}{
		// Exact match.
		{"deepseek-v4-flash", true},
		{"glm-5.3-flash", false},
		{"deepseek-vision-exp", false},
		// Strip "-free": deepseek-v4-flash-free inherits the base model entry.
		{"deepseek-v4-flash-free", true},
		// Add "-free": x-preview-f matches x-preview-f-free.
		{"x-preview-f", true},
		// Exact entry wins over the strip-"-free" fallback.
		{"explicit-free-free", true},
		{"explicit-free", false},
		// Unknown / empty data fails open (not text-only).
		{"unknown-model", false},
		{"unknown-model-free", false},
		{"", false},
		{"empty-input-considered", false},
	}

	for _, tc := range tests {
		if got := IsTextOnly(tc.modelID, mods); got != tc.want {
			t.Errorf("IsTextOnly(%q) = %v, want %v", tc.modelID, got, tc.want)
		}
	}

	if IsTextOnly("deepseek-v4-flash", nil) {
		t.Error("nil modalities should never report text-only")
	}
	if IsTextOnly("deepseek-v4-flash", Modalities{"deepseek-v4-flash": {"Text"}}) != true {
		t.Error("modality matching should be case-insensitive")
	}
}
