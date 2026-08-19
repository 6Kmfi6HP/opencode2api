package app

import (
	"testing"
)

// TestBuildClaudeUsageCoreInputTokensExcludesSplitHit covers the fix for the
// Anthropic-semantics deviation: when cache_read is sourced from DeepSeek/
// OpenAI-style counters, prompt_tokens includes the hit portion and
// input_tokens must exclude it. An Anthropic-style cache_read_input_tokens is
// already exclusive and must NOT be subtracted.
func TestBuildClaudeUsageCoreInputTokensExcludesSplitHit(t *testing.T) {
	cases := []struct {
		name            string
		upstream        map[string]any
		wantInput       int
		wantRead        int
		wantReadSet     bool
		wantCreation    int
		wantCreationSet bool
	}{
		{
			name: "deepseek-full-warm-form-A",
			upstream: map[string]any{
				"prompt_tokens":            float64(289),
				"completion_tokens":        float64(16),
				"total_tokens":             float64(305),
				"prompt_cache_hit_tokens":  float64(256),
				"prompt_cache_miss_tokens": float64(33),
				"prompt_tokens_details":    map[string]any{"cached_tokens": float64(256)},
			},
			wantInput: 33, wantRead: 256, wantReadSet: true,
		},
		{
			name: "deepseek-cold",
			upstream: map[string]any{
				"prompt_tokens":            float64(90),
				"completion_tokens":        float64(11),
				"prompt_cache_hit_tokens":  float64(0),
				"prompt_cache_miss_tokens": float64(90),
				"prompt_tokens_details":    map[string]any{"cached_tokens": float64(0)},
			},
			wantInput: 90, wantRead: 0, wantReadSet: true,
		},
		{
			name: "hit-only-no-cached-details",
			upstream: map[string]any{
				"prompt_tokens":            float64(289),
				"prompt_cache_hit_tokens":  float64(256),
				"prompt_cache_miss_tokens": float64(33),
			},
			wantInput: 33, wantRead: 256, wantReadSet: true,
		},
		{
			name: "cached-only-no-hit (form B)",
			upstream: map[string]any{
				"prompt_tokens":         float64(289),
				"completion_tokens":     float64(16),
				"prompt_tokens_details": map[string]any{"cached_tokens": float64(256)},
			},
			wantInput: 33, wantRead: 256, wantReadSet: true,
		},
		{
			name: "anthropic-style not subtracted",
			upstream: map[string]any{
				"input_tokens":                float64(289),
				"output_tokens":               float64(16),
				"cache_read_input_tokens":     float64(256),
				"cache_creation_input_tokens": float64(100),
			},
			wantInput: 289, wantRead: 256, wantReadSet: true,
			wantCreation: 100, wantCreationSet: true,
		},
		{
			name: "anthropic-style with prompt_tokens also not subtracted",
			upstream: map[string]any{
				"prompt_tokens":           float64(289),
				"input_tokens":            float64(289),
				"completion_tokens":       float64(16),
				"cache_read_input_tokens": float64(256),
			},
			wantInput: 289, wantRead: 256, wantReadSet: true,
		},
		{
			name: "no cache fields",
			upstream: map[string]any{
				"prompt_tokens":     float64(90),
				"completion_tokens": float64(11),
			},
			wantInput: 90, wantReadSet: false,
		},
		{
			name: "empty details (form C)",
			upstream: map[string]any{
				"prompt_tokens":         float64(90),
				"completion_tokens":     float64(16),
				"prompt_tokens_details": map[string]any{},
			},
			wantInput: 90, wantReadSet: false,
		},
		{
			name: "hit larger than prompt clamped to zero",
			upstream: map[string]any{
				"prompt_tokens":           float64(100),
				"prompt_cache_hit_tokens": float64(150),
			},
			wantInput: 0, wantRead: 150, wantReadSet: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := buildClaudeUsageCore(tc.upstream)
			if got := usage["input_tokens"]; got != tc.wantInput {
				t.Fatalf("input_tokens = %v, want %d", got, tc.wantInput)
			}
			gotRead, ok := usage["cache_read_input_tokens"]
			if !tc.wantReadSet {
				if ok {
					t.Fatalf("cache_read_input_tokens should be absent, got %v", gotRead)
				}
			} else if !ok {
				t.Fatalf("cache_read_input_tokens absent, want %d", tc.wantRead)
			} else if gotRead != tc.wantRead {
				t.Fatalf("cache_read_input_tokens = %v, want %d", gotRead, tc.wantRead)
			}
			gotCreation, ok := usage["cache_creation_input_tokens"]
			if !tc.wantCreationSet {
				if ok {
					t.Fatalf("cache_creation_input_tokens should be absent, got %v", gotCreation)
				}
			} else if !ok {
				t.Fatalf("cache_creation_input_tokens absent, want %d", tc.wantCreation)
			} else if gotCreation != tc.wantCreation {
				t.Fatalf("cache_creation_input_tokens = %v, want %d", gotCreation, tc.wantCreation)
			}
		})
	}
}
