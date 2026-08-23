package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigPathPrecedence(t *testing.T) {
	tmpDir := t.TempDir()
	localConfig := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(localConfig, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	userDir, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("user config directory unavailable: %v", err)
	}
	fallback := filepath.Join(userDir, "opencode2api", "config.json")

	tests := []struct {
		name         string
		env          string
		flagValue    string
		flagExplicit bool
		want         string
		wantFallback bool
	}{
		{
			name:         "environment wins over explicit flag",
			env:          "/tmp/env-config.json",
			flagValue:    "/tmp/flag-config.json",
			flagExplicit: true,
			want:         "/tmp/env-config.json",
		},
		{
			name:         "explicit flag wins over local file",
			flagValue:    "/tmp/flag-config.json",
			flagExplicit: true,
			want:         "/tmp/flag-config.json",
		},
		{
			name:         "existing local file is backward compatible",
			flagValue:    "config.json",
			flagExplicit: false,
			want:         "config.json",
		},
		{
			name:         "ignored flag value does not hide local file",
			flagValue:    "/tmp/ignored.json",
			flagExplicit: false,
			want:         "config.json",
		},
		{
			name:         "blank environment is ignored",
			env:          "   ",
			flagValue:    "",
			flagExplicit: true,
			want:         "config.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv("OPENCODE2API_CONFIG", "")
			} else {
				t.Setenv("OPENCODE2API_CONFIG", tt.env)
			}

			got, gotFallback := resolveConfigPath(tt.flagValue, tt.flagExplicit)
			if tt.wantFallback && got != fallback {
				t.Fatalf("resolveConfigPath() = %q, want %q", got, fallback)
			}
			if !tt.wantFallback && got != tt.want {
				t.Fatalf("resolveConfigPath() = %q, want %q", got, tt.want)
			}
			if gotFallback != tt.wantFallback {
				t.Fatalf("fallback = %t, want %t", gotFallback, tt.wantFallback)
			}
		})
	}

	// Remove the local compatibility file to exercise the final fallback.
	if err := os.Remove(localConfig); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_CONFIG", "")
	got, gotFallback := resolveConfigPath("", false)
	if got != fallback {
		t.Fatalf("user fallback = %q, want %q", got, fallback)
	}
	if !gotFallback {
		t.Fatal("user fallback reported as false")
	}
}

func TestNewLaunchFlagSetUsesSharedResolution(t *testing.T) {
	t.Setenv("OPENCODE2API_CONFIG", "/tmp/launch-env-config.json")

	f := newLaunchFlagSet("codex", nil)
	if f.cfgPath != "/tmp/launch-env-config.json" {
		t.Fatalf("launch cfgPath = %q, want /tmp/launch-env-config.json", f.cfgPath)
	}
}

func TestSaveConfigCreatesUserConfigDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode2api", "config.json")

	var wrote AppConfig
	wrote.ModelAlias = map[string]string{"example": "example-free"}
	if err := saveConfig(path, wrote); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var read AppConfig
	if err := json.Unmarshal(data, &read); err != nil {
		t.Fatal(err)
	}
	if read.ModelAlias["example"] != "example-free" {
		t.Fatalf("round-trip alias = %q, want example-free", read.ModelAlias["example"])
	}
}
