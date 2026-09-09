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
