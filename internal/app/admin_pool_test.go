package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/config"
)

func postConfig(t *testing.T, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	adminConfigHandler(rec, req)
	return rec
}

func TestAdmin_KeyPoolValidation(t *testing.T) {
	oldSnap := config.Get()
	oldPool := func() KeyPool { keypoolMu.RLock(); defer keypoolMu.RUnlock(); return keypoolCfg }()
	configMu.Lock()
	oldCP := configPath
	configMu.Unlock()
	t.Cleanup(func() {
		config.Update(func(s *config.Snapshot) { *s = oldSnap })
		setKeyPool(oldPool)
		configMu.Lock()
		configPath = oldCP
		configMu.Unlock()
	})
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	configMu.Lock()
	configPath = cfgPath
	configMu.Unlock()

	cases := []struct {
		name    string
		keyPool map[string]any
	}{
		{"invalid strategy", map[string]any{"strategy": "bogus", "keys": []any{"sk-a"}}},
		{"duplicate id", map[string]any{"keys": []any{
			map[string]any{"id": "k1", "key": "sk-a"},
			map[string]any{"id": "k1", "key": "sk-b"},
		}}},
		{"empty key", map[string]any{"keys": []any{map[string]any{"id": "k1", "key": ""}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postConfig(t, map[string]any{"key_pool": tc.keyPool})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
				t.Fatalf("config file must not be written on validation failure: %v", err)
			}
		})
	}
}

func TestParseBatchKeys(t *testing.T) {
	text := "sk-a | first note | zen | 3\n" +
		"# a comment\n" +
		"\n" +
		"sk-b\n" +
		"sk-c|note-c|go|2\n" +
		"| no key\n" +
		"sk-d|x|y|z|extra\n" +
		"sk-e|||0\n"
	keys, added, skipped := parseBatchKeys(text)
	if added != 3 || skipped != 3 {
		t.Fatalf("added=%d skipped=%d, want 3/3", added, skipped)
	}
	if len(keys) != 3 {
		t.Fatalf("len(keys)=%d, want 3", len(keys))
	}
	if keys[0].Key != "sk-a" || keys[0].Note != "first note" || keys[0].Group != "zen" || keys[0].Weight != 3 {
		t.Fatalf("keys[0]=%+v, want sk-a/first note/zen/3", keys[0])
	}
	if keys[1].Key != "sk-b" || keys[1].Weight != 1 {
		t.Fatalf("keys[1]=%+v, want sk-b weight 1 default", keys[1])
	}
	if keys[2].Key != "sk-c" || keys[2].Group != "go" || keys[2].Weight != 2 {
		t.Fatalf("keys[2]=%+v, want sk-c/go/2", keys[2])
	}
}

func TestKeyStatusEndpoint(t *testing.T) {
	resetPool(t)
	setKeyPool(KeyPool{Enabled: true, Strategy: "round_robin", Keys: []UpstreamKey{
		{ID: "k1", Key: "sk-a"},
		{ID: "k2", Key: "sk-b", Group: "zen"},
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/key_status", nil)
	rec := httptest.NewRecorder()
	keyPoolStatusHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Keys []KeyPoolEntryStatus `json:"keys"`
		Pool struct {
			Enabled  bool   `json:"enabled"`
			Strategy string `json:"strategy"`
		} `json:"pool"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("len(keys)=%d, want 2; body=%s", len(got.Keys), rec.Body.String())
	}
	if !got.Pool.Enabled || got.Pool.Strategy != "round_robin" {
		t.Fatalf("pool=%+v, want enabled/round_robin", got.Pool)
	}
}

func TestKeyParseEndpoint(t *testing.T) {
	text := "sk-a | note a | zen | 2\n# comment\n\nsk-b\n| bad\n"
	body, _ := json.Marshal(map[string]any{"text": text})
	req := httptest.NewRequest(http.MethodPost, "/api/key_parse", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	keyPoolParseHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Keys    []UpstreamKey `json:"keys"`
		Added   int           `json:"added"`
		Skipped int           `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Added != 2 || got.Skipped != 1 || len(got.Keys) != 2 {
		t.Fatalf("got added=%d skipped=%d keys=%d, want 2/1/2; body=%s", got.Added, got.Skipped, len(got.Keys), rec.Body.String())
	}
	if got.Keys[0].Key != "sk-a" || got.Keys[0].Group != "zen" || got.Keys[0].Weight != 2 {
		t.Fatalf("keys[0]=%+v, want sk-a/zen/2", got.Keys[0])
	}
	// 方法错误 → 405。
	reqGet := httptest.NewRequest(http.MethodGet, "/api/key_parse", nil)
	recGet := httptest.NewRecorder()
	keyPoolParseHandler(recGet, reqGet)
	if recGet.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", recGet.Code)
	}
}

func TestSaveConfig_TightensPerms(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	// 预置旧 0644 文件：保存后必须收紧为 0600。
	if err := os.WriteFile(cfgPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(cfgPath, AppConfig{KeyPool: KeyPool{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config perm = %o, want 600", fi.Mode().Perm())
	}
}
