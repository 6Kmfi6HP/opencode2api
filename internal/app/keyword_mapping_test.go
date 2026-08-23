package app

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
)

func TestModelAliasLegacyMapCompatibility(t *testing.T) {
	legacyJSON := `{"model_alias":{"gpt-4o":"deepseek-v4-flash","claude-3-5-sonnet":"claude-sonnet-4.6"}}`
	var cfg domain.AppConfig
	if err := json.Unmarshal([]byte(legacyJSON), &cfg); err != nil {
		t.Fatalf("failed to unmarshal legacy config: %v", err)
	}

	if len(cfg.ModelAlias) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(cfg.ModelAlias))
	}

	applyConfig(cfg)

	// exact match
	if got := resolveModel("gpt-4o"); got != "deepseek-v4-flash" {
		t.Errorf("resolveModel(gpt-4o) = %q, want deepseek-v4-flash", got)
	}
	if got := resolveModel("claude-3-5-sonnet"); got != "claude-sonnet-4.6" {
		t.Errorf("resolveModel(claude-3-5-sonnet) = %q, want claude-sonnet-4.6", got)
	}
	// with suffix
	if got := resolveModel("gpt-4o[1m]"); got != "deepseek-v4-flash[1m]" {
		t.Errorf("resolveModel(gpt-4o[1m]) = %q, want deepseek-v4-flash[1m]", got)
	}
}

func TestModelAliasKeywordRules(t *testing.T) {
	rulesJSON := `{
		"model_alias": [
			{
				"keyword": "sonnet",
				"target": "claude-sonnet-4.6",
				"match_type": "contains",
				"case_insensitive": true,
				"enabled": true
			},
			{
				"keyword": "opus",
				"target": "claude-opus-4.6",
				"match_type": "contains",
				"case_insensitive": true,
				"enabled": true
			},
			{
				"keyword": "haiku",
				"target": "claude-haiku-4.6",
				"match_type": "contains",
				"case_insensitive": true,
				"enabled": false
			},
			{
				"keyword": "sol",
				"target": "gpt-5.6",
				"match_type": "prefix",
				"case_insensitive": true,
				"enabled": true
			},
			{
				"keyword": "luna",
				"target": "deepseek-v4",
				"match_type": "contains",
				"case_insensitive": true,
				"enabled": true
			},
			{
				"keyword": "^gpt-4o(-\\d{4}-\\d{2}-\\d{2})?$",
				"target": "deepseek-v4-flash",
				"match_type": "regex",
				"case_insensitive": true,
				"enabled": true
			}
		]
	}`

	var cfg domain.AppConfig
	if err := json.Unmarshal([]byte(rulesJSON), &cfg); err != nil {
		t.Fatalf("failed to unmarshal rules config: %v", err)
	}

	applyConfig(cfg)

	tests := []struct {
		input    string
		expected string
		desc     string
	}{
		{"claude-3-7-sonnet-20250219", "claude-sonnet-4.6", "contains sonnet"},
		{"claude-3-7-sonnet-20250219[1m]", "claude-sonnet-4.6[1m]", "contains sonnet with suffix"},
		{"CLAUDE-3-OPUS-LATEST", "claude-opus-4.6", "case insensitive opus"},
		{"haiku-v1", "haiku-v1", "disabled rule should not match"},
		{"sol-code-preview", "gpt-5.6", "prefix sol match"},
		{"my-sol-model", "my-sol-model", "prefix sol should not match when in middle"},
		{"luna-base", "deepseek-v4", "contains luna"},
		{"gpt-4o", "deepseek-v4-flash", "regex gpt-4o base"},
		{"gpt-4o-2024-08-06", "deepseek-v4-flash", "regex gpt-4o date"},
		{"gpt-4o-mini", "gpt-4o-mini", "regex gpt-4o should not match mini"},
		{"unknown-model", "unknown-model", "passthrough unchanged"},
	}

	for _, tt := range tests {
		got := resolveModel(tt.input)
		if got != tt.expected {
			t.Errorf("[%s] resolveModel(%q) = %q, want %q", tt.desc, tt.input, got, tt.expected)
		}
	}
}

func TestModelAliasPriority(t *testing.T) {
	// Exact match rule placed after contains rule
	cfg := domain.AppConfig{
		ModelAlias: []domain.ModelKeywordRule{
			{
				Keyword:         "claude-3-7-sonnet",
				Target:          "custom-exact-model",
				MatchType:       domain.MatchExact,
				CaseInsensitive: false,
				Enabled:         true,
			},
			{
				Keyword:         "sonnet",
				Target:          "claude-sonnet-4.6",
				MatchType:       domain.MatchContains,
				CaseInsensitive: true,
				Enabled:         true,
			},
		},
	}
	applyConfig(cfg)

	// First rule should match exact
	if got := resolveModel("claude-3-7-sonnet"); got != "custom-exact-model" {
		t.Errorf("exact match priority failed, got %q, want custom-exact-model", got)
	}

	// Other sonnet models should match second rule
	if got := resolveModel("claude-3-5-sonnet"); got != "claude-sonnet-4.6" {
		t.Errorf("second rule match failed, got %q, want claude-sonnet-4.6", got)
	}
}

func TestModelAliasConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cfg := domain.AppConfig{
				ModelAlias: []domain.ModelKeywordRule{
					{
						Keyword:         fmt.Sprintf("test-%d", idx),
						Target:          "target-model",
						MatchType:       domain.MatchContains,
						CaseInsensitive: true,
						Enabled:         true,
					},
				},
			}
			applyConfig(cfg)
			_ = resolveModel(fmt.Sprintf("my-test-%d-model[1m]", idx))
		}(i)
	}
	wg.Wait()
}
