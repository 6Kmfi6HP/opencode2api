package app

import (
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

func TestMaxTokensCapGlobal(t *testing.T) {
	// Set up global cap
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 131072
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	tests := []struct {
		name      string
		maxTokens int
		want      int
	}{
		{"below cap", 1000, 1000},
		{"exactly cap", 131072, 131072},
		{"above cap", 200000, 131072},
		{"far above cap", 1000000, 131072},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &OpenAIRequest{Model: "test-model", MaxTokens: ptr(tt.maxTokens)}
			body := convertRequest(req)
			got, ok := body["max_tokens"].(int)
			if !ok {
				t.Fatalf("max_tokens not int: %#v", body["max_tokens"])
			}
			if got != tt.want {
				t.Fatalf("max_tokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMaxTokensCapPerModel(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 131072
		s.MaxTokensCapPerModel = map[string]int{
			"model-a": 50000,
			"model-b": 0, // 0 = no cap for this model
		}
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	tests := []struct {
		name      string
		model     string
		maxTokens int
		want      int
	}{
		{"per-model cap applies", "model-a", 100000, 50000},
		{"per-model cap exact", "model-a", 50000, 50000},
		{"per-model cap below", "model-a", 100, 100},
		{"per-model zero disables cap", "model-b", 999999, 999999},
		{"falls back to global", "model-c", 200000, 131072},
		{"global cap below", "model-c", 100, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &OpenAIRequest{Model: tt.model, MaxTokens: ptr(tt.maxTokens)}
			body := convertRequest(req)
			got, ok := body["max_tokens"].(int)
			if !ok {
				t.Fatalf("max_tokens not int: %#v", body["max_tokens"])
			}
			if got != tt.want {
				t.Fatalf("max_tokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMaxTokensCapNoCapWhenZero(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 0
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	req := &OpenAIRequest{Model: "any-model", MaxTokens: ptr(9999999)}
	body := convertRequest(req)
	got, ok := body["max_tokens"].(int)
	if !ok {
		t.Fatalf("max_tokens not int: %#v", body["max_tokens"])
	}
	if got != 9999999 {
		t.Fatalf("max_tokens = %d, want 9999999 (no cap)", got)
	}
}

func TestMaxTokensCapNilMaxTokens(t *testing.T) {
	old := config.Get()
	config.Update(func(s *config.Snapshot) {
		s.MaxTokensCap = 131072
		s.MaxTokensCapPerModel = nil
	})
	t.Cleanup(func() { config.Update(func(s *config.Snapshot) { *s = old }) })

	// When max_tokens is not set, it should not appear in the converted request
	req := &OpenAIRequest{Model: "any-model", MaxTokens: nil}
	body := convertRequest(req)
	if _, exists := body["max_tokens"]; exists {
		t.Fatalf("max_tokens should not be set when nil")
	}
}
