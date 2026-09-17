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

// TestAdminConfig_ProtocolRulesRoundTrip 验证 protocol_rules 经 GET 返回、
// POST 合法值持久化落盘并热生效、非法值 400 且不落盘。
func TestAdminConfig_ProtocolRulesRoundTrip(t *testing.T) {
	oldSnapRules := getProtocolRules()
	configMu.Lock()
	oldCP := configPath
	configMu.Unlock()
	t.Cleanup(func() {
		setProtocolRules(oldSnapRules)
		configMu.Lock()
		configPath = oldCP
		configMu.Unlock()
	})

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	configMu.Lock()
	configPath = cfgPath
	configMu.Unlock()

	// --- POST 合法规则 → 200 + 落盘 + 热生效 ---
	payload := map[string]any{
		"protocol_rules": []any{
			map[string]any{"pattern": "claude-*", "protocol": "anthropic"},
			map[string]any{"pattern": "gpt-*", "protocol": "responses"},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rules := getProtocolRules()
	if len(rules) != 2 || rules[0].Pattern != "claude-*" || rules[0].Protocol != "anthropic" {
		t.Fatalf("rules after POST = %#v", rules)
	}
	if got, ok := matchProtocolRule("claude-haiku-4.6"); !ok || got != upstreamProtocolAnthropic {
		t.Fatalf("rule not hot-applied after POST: %v,%v", got, ok)
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var read AppConfig
	if err := json.Unmarshal(data, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.ProtocolRules) != 2 || read.ProtocolRules[1].Protocol != "responses" {
		t.Fatalf("persisted ProtocolRules = %#v", read.ProtocolRules)
	}

	// --- GET 返回 protocol_rules ---
	req2 := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec2 := httptest.NewRecorder()
	adminConfigHandler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec2.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	pr, _ := got["protocol_rules"].([]any)
	if len(pr) != 2 {
		t.Fatalf("GET protocol_rules = %#v, want 2 entries", got["protocol_rules"])
	}
	first, _ := pr[0].(map[string]any)
	if first["pattern"] != "claude-*" || first["protocol"] != "anthropic" {
		t.Fatalf("GET protocol_rules[0] = %#v", first)
	}

	// --- POST 非法规则 → 400 + 不落盘不生效 ---
	badBody, _ := json.Marshal(map[string]any{
		"protocol_rules": []any{
			map[string]any{"pattern": "claude-*", "protocol": "anthropic"},
			map[string]any{"pattern": "gpt-*", "protocol": "grpc"},
		},
	})
	req3 := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(badBody))
	rec3 := httptest.NewRecorder()
	adminConfigHandler(rec3, req3)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("invalid POST status = %d, want 400; body=%s", rec3.Code, rec3.Body.String())
	}
	// 内存规则未被部分更新（第二条非法，第一条也不应生效——严格模式整体拒绝）。
	rules = getProtocolRules()
	if len(rules) != 2 || rules[1].Protocol != "responses" {
		t.Fatalf("rules must be untouched after 400: %#v", rules)
	}
	// config.json 未被 400 请求破坏。
	data, err = os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var read2 AppConfig
	if err := json.Unmarshal(data, &read2); err != nil {
		t.Fatal(err)
	}
	if len(read2.ProtocolRules) != 2 || read2.ProtocolRules[1].Protocol != "responses" {
		t.Fatalf("persisted rules corrupted by failed POST: %#v", read2.ProtocolRules)
	}
}

// TestLoadConfig_ProtocolRulesLenient 验证 config.json 加载走宽松编译：
// 非法条目剔除、合法条目保留。
func TestLoadConfig_ProtocolRulesLenient(t *testing.T) {
	oldSnapRules := getProtocolRules()
	configMu.Lock()
	oldCP := configPath
	configMu.Unlock()
	t.Cleanup(func() {
		setProtocolRules(oldSnapRules)
		configMu.Lock()
		configPath = oldCP
		configMu.Unlock()
	})

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	raw := `{"protocol_rules":[
		{"pattern":"claude-*","protocol":"anthropic"},
		{"pattern":"a b","protocol":"anthropic"},
		{"pattern":"gpt-*","protocol":"responses"}
	]}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	configMu.Lock()
	configPath = cfgPath
	configMu.Unlock()

	cfg := loadConfig(cfgPath)
	applyConfig(cfg)
	t.Cleanup(func() { applyConfig(AppConfig{}) })

	rules := getProtocolRules()
	if len(rules) != 2 {
		t.Fatalf("lenient load kept %d rules, want 2: %#v", len(rules), rules)
	}
	if got, ok := matchProtocolRule("gpt-5.6"); !ok || got != upstreamProtocolResponses {
		t.Fatalf("gpt-5.6 after load = %v,%v", got, ok)
	}
}
