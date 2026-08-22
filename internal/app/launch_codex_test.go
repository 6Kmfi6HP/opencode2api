package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildCodexConfigArgsWithModel(t *testing.T) {
	args := buildCodexConfigArgs("http://127.0.0.1:1234", "deepseek-v4-flash", "/tmp/opencode2api-codex-models.json")

	configOverrides := map[string]string{}
	var passthrough []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			if i+1 >= len(args) {
				t.Fatalf("-c at end of args: %v", args)
			}
			i++
			key, val, ok := strings.Cut(args[i], "=")
			if !ok {
				t.Fatalf("malformed -c override: %q", args[i])
			}
			configOverrides[key] = val
			continue
		}
		passthrough = append(passthrough, args[i])
	}

	want := map[string]string{
		"model_provider":                                    `"opencode2api"`,
		"model_providers.opencode2api.name":                 `"opencode2api"`,
		"model_providers.opencode2api.base_url":             `"http://127.0.0.1:1234/v1"`,
		"model_providers.opencode2api.wire_api":             `"responses"`,
		"model_providers.opencode2api.requires_openai_auth": "true",
		"model_providers.opencode2api.env_key":              `"OPENCODE2API_OPENAI_API_KEY"`,
		"model_catalog_json":                                `"/tmp/opencode2api-codex-models.json"`,
	}
	if len(configOverrides) != len(want) {
		t.Fatalf("override count = %d, want %d\noverrides=%v", len(configOverrides), len(want), configOverrides)
	}
	for k, v := range want {
		if got := configOverrides[k]; got != v {
			t.Errorf("override %s = %q, want %q", k, got, v)
		}
	}

	if !reflect.DeepEqual(passthrough, []string{"--model", "deepseek-v4-flash"}) {
		t.Errorf("passthrough args = %v, want [--model deepseek-v4-flash]", passthrough)
	}
}

func TestBuildCodexConfigArgsNoModel(t *testing.T) {
	args := buildCodexConfigArgs("http://127.0.0.1:1234", "", "/tmp/opencode2api-codex-models.json")
	for _, arg := range args {
		if arg == "--model" {
			t.Fatalf("--model should not be present when modelID is empty; args=%v", args)
		}
	}
}

func TestBuildCodexConfigArgsNoModelCatalog(t *testing.T) {
	args := buildCodexConfigArgs("http://127.0.0.1:1234", "deepseek-v4-flash", "")
	for _, arg := range args {
		if strings.HasPrefix(arg, "model_catalog_json=") {
			t.Fatalf("model_catalog_json should not be present when path is empty; args=%v", args)
		}
	}
}

func TestBuildCodexModelCatalogSpecs(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	modelMu.Lock()
	modelsCache = []ModelInfo{{ID: "deepseek-v4-flash"}, {ID: "deepseek-v4-flash-free"}, {ID: "paid-only-model"}}
	goModelsCache = nil
	modelMu.Unlock()
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelMu.Unlock()
	})

	catalog := modelsDevCatalog{
		"deepseek-v4-flash":      1048576,
		"deepseek-v4-flash-free": 1048576,
		"paid-only-model":        200000,
	}

	freeSpecs := buildCodexModelCatalogSpecs(catalog, true)
	if len(freeSpecs) != 1 || freeSpecs[0].ID != "deepseek-v4-flash" {
		t.Fatalf("free specs = %#v, want only deepseek-v4-flash", freeSpecs)
	}
	if freeSpecs[0].ContextWindow != 1048576 {
		t.Fatalf("free spec context = %d, want 1048576", freeSpecs[0].ContextWindow)
	}

	allSpecs := buildCodexModelCatalogSpecs(catalog, false)
	if len(allSpecs) != 2 {
		t.Fatalf("all specs = %#v, want 2 models", allSpecs)
	}
	allIDs := []string{allSpecs[0].ID, allSpecs[1].ID}
	if allIDs[0] != "deepseek-v4-flash" || allIDs[1] != "paid-only-model" {
		t.Fatalf("all spec ids = %v, want [deepseek-v4-flash paid-only-model] (ordered by context desc)", allIDs)
	}
}

func TestWriteCodexModelCatalog(t *testing.T) {
	path, err := writeCodexModelCatalog([]codexModelCatalogSpec{{ID: "laguna-s-2.1", ContextWindow: 1048576}})
	if err != nil {
		t.Fatalf("writeCodexModelCatalog() error = %v", err)
	}
	defer os.RemoveAll(filepath.Dir(path))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated catalog: %v", err)
	}
	var catalog codexModelCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("generated catalog is invalid JSON: %v", err)
	}
	if len(catalog.Models) != 1 {
		t.Fatalf("generated catalog model count = %d, want 1", len(catalog.Models))
	}
	m := catalog.Models[0]
	if m.Slug != "laguna-s-2.1" {
		t.Errorf("slug = %q, want laguna-s-2.1", m.Slug)
	}
	if m.ContextWindow != 1048576 || m.MaxContextWindow != 1048576 {
		t.Errorf("context window = %d/%d, want 1048576/1048576", m.ContextWindow, m.MaxContextWindow)
	}
	if m.AutoCompactTokenLimit != 943718 {
		t.Errorf("auto compact token limit = %d, want 943718", m.AutoCompactTokenLimit)
	}
	if m.EffectiveContextWindowPercent != 100 {
		t.Errorf("effective_context_window_percent = %d, want 100", m.EffectiveContextWindowPercent)
	}
}

func TestBuildCodexEnv(t *testing.T) {
	env := buildCodexEnv("go:sk-test123456")
	want := []string{"OPENCODE2API_OPENAI_API_KEY=go:sk-test123456"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("buildCodexEnv() = %v, want %v", env, want)
	}
}

func TestExtractCodexModelFromExtraArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantModel   string
		wantCleaned []string
	}{
		{
			name:        "long space form",
			args:        []string{"exec", "--model", "deepseek-v4-flash", "--ephemeral"},
			wantModel:   "deepseek-v4-flash",
			wantCleaned: []string{"exec", "--ephemeral"},
		},
		{
			name:        "long equals form",
			args:        []string{"exec", "--model=deepseek-v4-flash"},
			wantModel:   "deepseek-v4-flash",
			wantCleaned: []string{"exec"},
		},
		{
			name:        "short space form",
			args:        []string{"exec", "-m", "deepseek-v4-flash", "--json"},
			wantModel:   "deepseek-v4-flash",
			wantCleaned: []string{"exec", "--json"},
		},
		{
			name:        "short equals form",
			args:        []string{"-m=deepseek-v4-flash", "exec"},
			wantModel:   "deepseek-v4-flash",
			wantCleaned: []string{"exec"},
		},
		{
			name:        "no model",
			args:        []string{"exec", "--ephemeral"},
			wantModel:   "",
			wantCleaned: []string{"exec", "--ephemeral"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model, cleaned := extractCodexModelFromExtraArgs(tc.args)
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
			if !reflect.DeepEqual(cleaned, tc.wantCleaned) {
				t.Errorf("cleaned = %v, want %v", cleaned, tc.wantCleaned)
			}
		})
	}
}

func TestFindCodex(t *testing.T) {
	installed := false
	if _, err := exec.LookPath("codex"); err == nil {
		installed = true
	} else if home, _ := os.UserHomeDir(); home != "" {
		for _, c := range []string{
			home + "/.local/bin/codex",
			home + "/.codex/bin/codex",
		} {
			if _, statErr := os.Stat(c); statErr == nil {
				installed = true
				break
			}
		}
	}
	if !installed {
		t.Skip("codex not installed; skipping findCodex test")
	}
	if path := findCodex(); path == "" {
		t.Error("findCodex returned empty string despite being found")
	}
}
