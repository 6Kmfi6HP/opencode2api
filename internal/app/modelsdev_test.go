package app

import "testing"

func TestStripContextSuffix(t *testing.T) {
	tests := []struct {
		input      string
		wantBase   string
		wantSuffix string
	}{
		{"deepseek-v4-flash[1m]", "deepseek-v4-flash", "[1m]"},
		{"deepseek-v4-flash-free[1m]", "deepseek-v4-flash-free", "[1m]"},
		{"deepseek-v4-flash", "deepseek-v4-flash", ""},
		{"glm-5.2[1m]", "glm-5.2", "[1m]"},
		{"qwen3-coder-flash[1m]", "qwen3-coder-flash", "[1m]"},
		{"", "", ""},
		{"foo[2m]", "foo", "[2m]"},
		// Edge: bracket at position 0 means no base before it, so no suffix.
		{"[1m]", "[1m]", ""},
		// Edge: bracket not at end is not a suffix.
		{"foo[bar]baz", "foo[bar]baz", ""},
		// Trailing whitespace is trimmed.
		{"  deepseek-v4-flash[1m]  ", "deepseek-v4-flash", "[1m]"},
	}
	for _, tc := range tests {
		base, suffix := stripContextSuffix(tc.input)
		if base != tc.wantBase || suffix != tc.wantSuffix {
			t.Errorf("stripContextSuffix(%q) = (%q, %q), want (%q, %q)",
				tc.input, base, suffix, tc.wantBase, tc.wantSuffix)
		}
	}
}

func TestGetContextWindow(t *testing.T) {
	catalog := modelsDevCatalog{
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
		got := getContextWindow(tc.modelID, catalog)
		if got != tc.want {
			t.Errorf("getContextWindow(%q) = %d, want %d", tc.modelID, got, tc.want)
		}
	}
}
