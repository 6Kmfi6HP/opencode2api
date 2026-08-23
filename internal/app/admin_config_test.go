package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestAdminConfigRoundTripFourFields verifies that the four config fields
// (prompt_cache_retention, cache_control_breakpoints, socks5_sticky,
// text_only_models) are exposed by GET /api/config and survive a POST
// "save all config" round-trip without being silently wiped.
func TestAdminConfigRoundTripFourFields(t *testing.T) {
	// --- save global state ---
	configMu.Lock()
	oldPCR := promptCacheRetention
	oldCB := cacheBreakpoints
	oldTOM := textOnlyModels
	oldCP := configPath
	configMu.Unlock()
	socks5Mu.Lock()
	oldSticky := socks5Sticky
	socks5Mu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		promptCacheRetention = oldPCR
		cacheBreakpoints = oldCB
		textOnlyModels = oldTOM
		configPath = oldCP
		configMu.Unlock()
		socks5Mu.Lock()
		socks5Sticky = oldSticky
		socks5Mu.Unlock()
	})

	// --- set up a temp config file ---
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	configMu.Lock()
	configPath = cfgPath
	configMu.Unlock()

	// Write non-default values for all four fields.
	bp := false
	initial := AppConfig{
		PromptCacheRetention:    "off",
		CacheControlBreakpoints: &bp,
		Socks5Sticky:            &bp,
		TextOnlyModels:          []string{"deepseek", "glm"},
	}
	if err := saveConfig(cfgPath, initial); err != nil {
		t.Fatal(err)
	}
	applyConfig(initial)

	// --- GET /api/config should return the four fields ---
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); nil != err {
		t.Fatal(err)
	}
	if v, _ := got["prompt_cache_retention"].(string); v != "off" {
		t.Fatalf("prompt_cache_retention = %#v, want off", got["prompt_cache_retention"])
	}
	if v, _ := got["cache_control_breakpoints"].(bool); v != false {
		t.Fatalf("cache_control_breakpoints = %#v, want false", got["cache_control_breakpoints"])
	}
	if v, _ := got["socks5_sticky"].(bool); v != false {
		t.Fatalf("socks5_sticky = %#v, want false", got["socks5_sticky"])
	}
	tom, _ := got["text_only_models"].([]any)
	if len(tom) != 2 || tom[0] != "deepseek" || tom[1] != "glm" {
		t.Fatalf("text_only_models = %#v, want [deepseek glm]", got["text_only_models"])
	}

	// --- POST /api/config simulating a frontend "save all config" ---
	// The frontend always sends explicit values, so the four fields must be
	// preserved exactly as submitted (no silent wipe to zero values).
	payload := map[string]any{
		"prompt_cache_retention":    "in_memory",
		"cache_control_breakpoints": true,
		"socks5_sticky":             false,
		"text_only_models":          []any{"deepseek", "glm"},
	}
	body, _ := json.Marshal(payload)
	req2 := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	adminConfigHandler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}

	// --- read back config.json and verify all four fields persisted ---
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var read AppConfig
	if err := json.Unmarshal(data, &read); err != nil {
		t.Fatal(err)
	}
	if read.PromptCacheRetention != "in_memory" {
		t.Fatalf("persisted PromptCacheRetention = %#v, want in_memory", read.PromptCacheRetention)
	}
	if read.CacheControlBreakpoints == nil || *read.CacheControlBreakpoints != true {
		t.Fatalf("persisted CacheControlBreakpoints = %#v, want *true", read.CacheControlBreakpoints)
	}
	if read.Socks5Sticky == nil || *read.Socks5Sticky != false {
		t.Fatalf("persisted Socks5Sticky = %#v, want *false", read.Socks5Sticky)
	}
	if len(read.TextOnlyModels) != 2 || read.TextOnlyModels[0] != "deepseek" || read.TextOnlyModels[1] != "glm" {
		t.Fatalf("persisted TextOnlyModels = %#v, want [deepseek glm]", read.TextOnlyModels)
	}
}
