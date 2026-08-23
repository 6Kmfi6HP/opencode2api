package app

import (
	"strings"
	"testing"
)

// ---- stripContextSuffix + resolveModel + mapPublicToFreeModel with [1m] ----

func TestResolveModelWithSuffix(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelAlias := modelAliasRules
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "deepseek-v4-flash"}, {ID: "deepseek-v4-flash-free"}}
	goModelsCache = nil
	modelMu.Unlock()
	configMu.Lock()
	modelAliasRules = nil
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
		configMu.Lock()
		modelAliasRules = oldModelAlias
		configMu.Unlock()
	})

	// No suffix: existing model in cache → returned as-is.
	if got := resolveModel("deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Errorf("resolveModel(deepseek-v4-flash) = %q, want deepseek-v4-flash", got)
	}

	// With [1m] suffix: should resolve base and preserve suffix.
	if got := resolveModel("deepseek-v4-flash[1m]"); got != "deepseek-v4-flash[1m]" {
		t.Errorf("resolveModel(deepseek-v4-flash[1m]) = %q, want deepseek-v4-flash[1m]", got)
	}
}

func TestResolveModelFreeOnlyWithSuffix(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelAlias := modelAliasRules
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "mimo-v2.5-free"}}
	goModelsCache = nil
	modelMu.Unlock()
	configMu.Lock()
	modelAliasRules = nil
	configMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
		configMu.Lock()
		modelAliasRules = oldModelAlias
		configMu.Unlock()
	})

	// Only free variant exists; the [1m] suffix should be preserved on the free ID.
	if got := resolveModel("mimo-v2.5[1m]"); got != "mimo-v2.5-free[1m]" {
		t.Fatalf("resolveModel(mimo-v2.5[1m]) = %q, want mimo-v2.5-free[1m]", got)
	}
	// Without suffix → still maps to free.
	if got := resolveModel("mimo-v2.5"); got != "mimo-v2.5-free" {
		t.Fatalf("resolveModel(mimo-v2.5) = %q, want mimo-v2.5-free", got)
	}
}

func TestMapPublicToFreeModelWithSuffix(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "deepseek-v4-flash"}, {ID: "deepseek-v4-flash-free"}}
	goModelsCache = nil
	modelMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
	})

	public := UpstreamAuth{Mode: AuthRoutePublic}

	// Suffix preserved when mapping to free variant.
	if got := mapPublicToFreeModel(public, "deepseek-v4-flash[1m]"); got != "deepseek-v4-flash-free[1m]" {
		t.Errorf("mapPublicToFreeModel(public, deepseek-v4-flash[1m]) = %q, want deepseek-v4-flash-free[1m]", got)
	}

	// Already-free base with suffix → stays unchanged.
	if got := mapPublicToFreeModel(public, "deepseek-v4-flash-free[1m]"); got != "deepseek-v4-flash-free[1m]" {
		t.Errorf("mapPublicToFreeModel(public, deepseek-v4-flash-free[1m]) = %q, want deepseek-v4-flash-free[1m]", got)
	}

	// Model without free variant and with suffix → stays unchanged.
	if got := mapPublicToFreeModel(public, "laguna-s-2.1[1m]"); got != "laguna-s-2.1[1m]" {
		t.Errorf("mapPublicToFreeModel(public, laguna-s-2.1[1m]) = %q, want laguna-s-2.1[1m]", got)
	}
}

// ---- buildClaudeEnv new signature ----

func TestBuildClaudeEnvWithModel(t *testing.T) {
	env := buildClaudeEnv("http://127.0.0.1:9999", "public", "deepseek-v4-flash[1m]", 900000)
	parsed := make(map[string]string, len(env))
	for _, kv := range env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			t.Errorf("malformed env entry: %q", kv)
			continue
		}
		parsed[key] = val
	}

	// Base env vars always present.
	baseWant := map[string]string{
		"ANTHROPIC_BASE_URL":                   "http://127.0.0.1:9999",
		"ANTHROPIC_API_KEY":                    "public",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "1",
		"CLAUDE_CODE_ATTRIBUTION_HEADER":       "0",
		"CLAUDE_CODE_TOTAL_TOKENS_REMINDER":    "off",
		"DISABLE_ERROR_REPORTING":              "1",
		"DISABLE_FEEDBACK_COMMAND":             "1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":  "1",
	}
	for k, v := range baseWant {
		if got, ok := parsed[k]; !ok || got != v {
			t.Errorf("env %s = %q (found=%v), want %q", k, got, ok, v)
		}
	}

	// Model env vars set to modelID.
	modelKeys := []string{
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
	}
	for _, k := range modelKeys {
		if got, ok := parsed[k]; !ok || got != "deepseek-v4-flash[1m]" {
			t.Errorf("env %s = %q (found=%v), want deepseek-v4-flash[1m]", k, got, ok)
		}
	}

	// Auto-compact window.
	if got, ok := parsed["CLAUDE_CODE_AUTO_COMPACT_WINDOW"]; !ok || got != "900000" {
		t.Errorf("env CLAUDE_CODE_AUTO_COMPACT_WINDOW = %q (found=%v), want 900000", got, ok)
	}
}

func TestBuildClaudeEnvNoModelNoCompact(t *testing.T) {
	env := buildClaudeEnv("http://127.0.0.1:9999", "public", "", 0)
	parsed := make(map[string]string, len(env))
	for _, kv := range env {
		key, val, _ := strings.Cut(kv, "=")
		parsed[key] = val
	}

	// Model env vars must NOT be present.
	for _, k := range []string{
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
	} {
		if _, ok := parsed[k]; ok {
			t.Errorf("env %s should not be present when modelID is empty", k)
		}
	}
}

func TestBuildClaudeEnvModelNoCompact(t *testing.T) {
	// Model set but auto-compact = 0 (unknown context) → no compact env.
	env := buildClaudeEnv("http://127.0.0.1:9999", "public", "unknown-model", 0)
	parsed := make(map[string]string, len(env))
	for _, kv := range env {
		key, val, _ := strings.Cut(kv, "=")
		parsed[key] = val
	}

	if got := parsed["ANTHROPIC_MODEL"]; got != "unknown-model" {
		t.Errorf("ANTHROPIC_MODEL = %q, want unknown-model", got)
	}
	if _, ok := parsed["CLAUDE_CODE_AUTO_COMPACT_WINDOW"]; ok {
		t.Error("CLAUDE_CODE_AUTO_COMPACT_WINDOW should not be present when autoCompactWindow=0")
	}
}

func TestBuildClaudeEnvCompactOnly(t *testing.T) {
	// No model but compact set (hypothetical, for completeness).
	env := buildClaudeEnv("http://127.0.0.1:9999", "public", "", 360000)
	parsed := make(map[string]string, len(env))
	for _, kv := range env {
		key, val, _ := strings.Cut(kv, "=")
		parsed[key] = val
	}
	if got := parsed["CLAUDE_CODE_AUTO_COMPACT_WINDOW"]; got != "360000" {
		t.Errorf("CLAUDE_CODE_AUTO_COMPACT_WINDOW = %q, want 360000", got)
	}
	if _, ok := parsed["ANTHROPIC_MODEL"]; ok {
		t.Error("ANTHROPIC_MODEL should not be present when modelID is empty")
	}
}

// ---- Auto-compact calculation ----

func TestAutoCompactCalculation(t *testing.T) {
	tests := []struct {
		ctx  int
		want int
	}{
		{1000000, 900000}, // 1M → 900K
		{200000, 180000},  // 200K → 180K
		{400000, 360000},  // 400K → 360K
		{131072, 117964},  // 128K → ~115K
		{0, 0},            // unknown → no compact
	}
	for _, tc := range tests {
		var compact int
		if tc.ctx > 0 {
			compact = int(float64(tc.ctx) * 0.9)
		}
		if compact != tc.want {
			t.Errorf("ctx=%d → compact=%d, want %d", tc.ctx, compact, tc.want)
		}
	}
}
