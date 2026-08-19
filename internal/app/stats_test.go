package app

import (
	"testing"
)

func TestBuildClaudeUsageCoreDeepSeekMissIsNotCreation(t *testing.T) {
	usage := buildClaudeUsageCore(map[string]any{
		"prompt_tokens":            float64(200),
		"prompt_cache_hit_tokens":  float64(160),
		"prompt_cache_miss_tokens": float64(40),
		"completion_tokens":        float64(35),
	})
	// input_tokens excludes the cache-hit portion (Anthropic semantics:
	// input, read and creation are mutually exclusive).
	if got := usage["input_tokens"]; got != 40 {
		t.Fatalf("input_tokens = %v, want 40", got)
	}
	if got := usage["output_tokens"]; got != 35 {
		t.Fatalf("output_tokens = %v, want 35", got)
	}
	if got := usage["cache_read_input_tokens"]; got != 160 {
		t.Fatalf("cache_read_input_tokens = %v, want 160", got)
	}
	if _, ok := usage["cache_creation_input_tokens"]; ok {
		t.Fatalf("cache_creation_input_tokens should be absent for DeepSeek miss-only usage, got %v", usage["cache_creation_input_tokens"])
	}
}

func TestParseCacheUsageCanonicalFields(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"cache_read_input_tokens":     float64(64),
		"cache_creation_input_tokens": float64(8),
	})
	if read != 64 || created != 8 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (64, 8)", read, created)
	}
}

func TestParseCacheUsageDeepSeekMissIsNotCreation(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"prompt_tokens":            float64(200),
		"prompt_cache_hit_tokens":  float64(160),
		"prompt_cache_miss_tokens": float64(40),
	})
	if read != 160 || created != 0 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (160, 0)", read, created)
	}
}

func TestParseCacheUsagePrefersCanonicalRead(t *testing.T) {
	read, created := parseCacheUsage(map[string]any{
		"cache_read_input_tokens": float64(80),
		"prompt_cache_hit_tokens": float64(160),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(200),
		},
	})
	if read != 80 || created != 0 {
		t.Fatalf("parseCacheUsage = (%d, %d), want (80, 0)", read, created)
	}
}
