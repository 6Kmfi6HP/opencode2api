package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func resetStatsAndLogTestState(t *testing.T) {
	t.Helper()

	tokenStatsMu.Lock()
	origStatsPointer := tokenStats
	tokenStatsMu.Unlock()
	origStatsPath := getTokenStatsPath()
	origLog := logFile

	t.Cleanup(func() {
		tokenStatsMu.Lock()
		tokenStats = origStatsPointer
		tokenStatsMu.Unlock()
		setTokenStatsPath(origStatsPath)
		logFile = origLog
	})
}

// writeLocalCompatibilityFiles creates the legacy current-directory paths so a
// test can prove whether they are honored or intentionally bypassed.
func writeLocalCompatibilityFiles(t *testing.T) {
	t.Helper()
	files := map[string]string{
		"stats.json":       "{}",
		"opencode2api.log": "",
	}
	for name, content := range files {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for name := range files {
			_ = os.Remove(name)
		}
	})
}

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

func TestResolveStatsPathPrecedence(t *testing.T) {
	tmpDir := t.TempDir()
	localStats := filepath.Join(tmpDir, "stats.json")
	if err := os.WriteFile(localStats, []byte("{}"), 0o600); err != nil {
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
	userFallback := filepath.Join(userDir, "opencode2api", "stats.json")
	explicitCfg := filepath.Join(tmpDir, "explicit", "config.json")
	customCfg := filepath.Join(tmpDir, "custom", "config.json")

	tests := []struct {
		name           string
		envStats       string
		envStatsFile   string
		flagValue      string
		flagExplicit   bool
		cfgPath        string
		configExplicit bool
		want           string
		wantFallback   bool
	}{
		{
			name:           "OPENCODE2API_STATS environment wins over explicit flags",
			envStats:       "/tmp/env-stats.json",
			flagValue:      "/tmp/flag-stats.json",
			flagExplicit:   true,
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           "/tmp/env-stats.json",
		},
		{
			name:           "OPENCODE2API_STATS_FILE environment wins over explicit flags",
			envStatsFile:   "/tmp/env-stats-file.json",
			flagValue:      "/tmp/flag-stats.json",
			flagExplicit:   true,
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           "/tmp/env-stats-file.json",
		},
		{
			name:           "explicit stats flag wins over explicit config default",
			flagValue:      "/tmp/flag-stats.json",
			flagExplicit:   true,
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           "/tmp/flag-stats.json",
		},
		{
			name:           "explicit config follows its directory despite legacy local stats",
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           filepath.Join(tmpDir, "explicit", "stats.json"),
			wantFallback:   true,
		},
		{
			name:    "non-explicit config honors legacy local stats",
			cfgPath: explicitCfg,
			want:    "stats.json",
		},
		{
			name:    "non-explicit resolved config honors legacy local stats",
			cfgPath: customCfg,
			want:    "stats.json",
		},
		{
			name:           "blank environment is ignored",
			envStats:       "   ",
			envStatsFile:   "   ",
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           filepath.Join(tmpDir, "explicit", "stats.json"),
			wantFallback:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENCODE2API_STATS", tt.envStats)
			t.Setenv("OPENCODE2API_STATS_FILE", tt.envStatsFile)

			got, gotFallback := resolveStatsPath(tt.flagValue, tt.flagExplicit, tt.cfgPath, tt.configExplicit)
			if got != tt.want || gotFallback != tt.wantFallback {
				t.Fatalf("resolveStatsPath() = (%q, %v), want (%q, %v)", got, gotFallback, tt.want, tt.wantFallback)
			}
		})
	}

	if err := os.Remove(localStats); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_STATS", "")
	t.Setenv("OPENCODE2API_STATS_FILE", "")

	got, gotFallback := resolveStatsPath("", false, customCfg, false)
	wantCfgStats := filepath.Join(tmpDir, "custom", "stats.json")
	if got != wantCfgStats || !gotFallback {
		t.Fatalf("config-directory fallback = (%q, %v), want (%q, true)", got, gotFallback, wantCfgStats)
	}

	got, gotFallback = resolveStatsPath("", false, "", false)
	if got != userFallback || !gotFallback {
		t.Fatalf("user-directory fallback = (%q, %v), want (%q, true)", got, gotFallback, userFallback)
	}
}

func TestResolveLogFilePathPrecedence(t *testing.T) {
	tmpDir := t.TempDir()
	localLog := filepath.Join(tmpDir, "opencode2api.log")
	if err := os.WriteFile(localLog, []byte("log"), 0o600); err != nil {
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
	userFallback := filepath.Join(userDir, "opencode2api", "opencode2api.log")
	explicitCfg := filepath.Join(tmpDir, "explicit", "config.json")
	customCfg := filepath.Join(tmpDir, "custom", "config.json")

	tests := []struct {
		name           string
		env            string
		flagValue      string
		flagExplicit   bool
		cfgPath        string
		configExplicit bool
		want           string
		wantFallback   bool
	}{
		{
			name:           "environment wins over explicit flags",
			env:            "/tmp/env-opencode2api.log",
			flagValue:      "/tmp/flag-opencode2api.log",
			flagExplicit:   true,
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           "/tmp/env-opencode2api.log",
		},
		{
			name:           "explicit log flag wins over explicit config default",
			flagValue:      "/tmp/flag-opencode2api.log",
			flagExplicit:   true,
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           "/tmp/flag-opencode2api.log",
		},
		{
			name:           "explicit config follows its directory despite legacy local log",
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           filepath.Join(tmpDir, "explicit", "opencode2api.log"),
			wantFallback:   true,
		},
		{
			name:    "non-explicit config honors legacy local log",
			cfgPath: explicitCfg,
			want:    "opencode2api.log",
		},
		{
			name:           "blank environment is ignored",
			env:            "   ",
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           filepath.Join(tmpDir, "explicit", "opencode2api.log"),
			wantFallback:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENCODE2API_LOG_FILE", tt.env)

			got, gotFallback := resolveLogFilePath(tt.flagValue, tt.flagExplicit, tt.cfgPath, tt.configExplicit)
			if got != tt.want || gotFallback != tt.wantFallback {
				t.Fatalf("resolveLogFilePath() = (%q, %v), want (%q, %v)", got, gotFallback, tt.want, tt.wantFallback)
			}
		})
	}

	if err := os.Remove(localLog); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_LOG_FILE", "")

	got, gotFallback := resolveLogFilePath("", false, customCfg, false)
	wantCfgLog := filepath.Join(tmpDir, "custom", "opencode2api.log")
	if got != wantCfgLog || !gotFallback {
		t.Fatalf("config-directory fallback = (%q, %v), want (%q, true)", got, gotFallback, wantCfgLog)
	}

	got, gotFallback = resolveLogFilePath("", false, "", false)
	if got != userFallback || !gotFallback {
		t.Fatalf("user-directory fallback = (%q, %v), want (%q, true)", got, gotFallback, userFallback)
	}
}

func TestNewLaunchFlagSetUsesSharedResolution(t *testing.T) {
	t.Setenv("OPENCODE2API_CONFIG", "/tmp/launch-env-config.json")

	f := newLaunchFlagSet("codex", nil)
	if f.cfgPath != "/tmp/launch-env-config.json" {
		t.Fatalf("launch cfgPath = %q, want /tmp/launch-env-config.json", f.cfgPath)
	}
}

func TestLaunchFlagResolution(t *testing.T) {
	resetStatsAndLogTestState(t)

	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	writeLocalCompatibilityFiles(t)

	// Environment wins over an explicit launch flag at global configuration time.
	t.Setenv("OPENCODE2API_LOG_FILE", "/tmp/env-launch.log")
	f := newLaunchFlagSet("claude", []string{"--log-file", "flag-launch.log"})
	if f.logFile != "flag-launch.log" {
		t.Fatalf("launch raw logFile = %q, want flag-launch.log", f.logFile)
	}
	if !f.logExplicit {
		t.Fatal("launch logExplicit = false, want true")
	}
	configureLaunchGlobals(f)
	if logFile != "/tmp/env-launch.log" {
		t.Fatalf("configured launch logFile = %q, want /tmp/env-launch.log", logFile)
	}
	if err := os.Unsetenv("OPENCODE2API_LOG_FILE"); err != nil {
		t.Fatal(err)
	}

	// An explicit config directory supplies both implicit defaults.
	explicitConfig := filepath.Join(tmpDir, "launch-config", "config.json")
	if err := os.MkdirAll(filepath.Dir(explicitConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicitConfig, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_CONFIG", explicitConfig)

	f = newLaunchFlagSet("codex", []string{"--config", explicitConfig})
	if f.cfgPath != explicitConfig || !f.configExplicit {
		t.Fatalf("launch config resolution = (%q, %v), want (%q, true)", f.cfgPath, f.configExplicit, explicitConfig)
	}
	wantLog := filepath.Join(tmpDir, "launch-config", "opencode2api.log")

	configureLaunchGlobals(f)
	if got := getTokenStatsPath(); got != filepath.Join(tmpDir, "launch-config", "stats.json") {
		t.Fatalf("launch statsPath = %q, want config-directory stats path", got)
	}
	if logFile != wantLog {
		t.Fatalf("configured logFile = %q, want %q", logFile, wantLog)
	}

	// The stats environment remains the highest precedence.
	statsPath := filepath.Join(tmpDir, "env-stats.json")
	t.Setenv("OPENCODE2API_STATS", statsPath)
	resolvedStats, _ := resolveStatsPath("", false, f.cfgPath, true)
	if resolvedStats != statsPath {
		t.Fatalf("stats env = %q, want %q", resolvedStats, statsPath)
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

func TestServerExplicitConfigResolution(t *testing.T) {
	resetStatsAndLogTestState(t)

	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	writeLocalCompatibilityFiles(t)
	explicitConfig := filepath.Join(tmpDir, "explicit-config", "config.json")
	if err := os.MkdirAll(filepath.Dir(explicitConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicitConfig, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	statsPath, _ := resolveStatsPath("stats.json", false, explicitConfig, true)
	wantStats := filepath.Join(tmpDir, "explicit-config", "stats.json")
	if statsPath != wantStats {
		t.Fatalf("statsPath = %q, want %q", statsPath, wantStats)
	}

	logPath, _ := resolveLogFilePath("opencode2api.log", false, explicitConfig, true)
	wantLog := filepath.Join(tmpDir, "explicit-config", "opencode2api.log")
	if logPath != wantLog {
		t.Fatalf("logPath = %q, want %q", logPath, wantLog)
	}
}

func TestResolveModelsDevCachePathPrecedence(t *testing.T) {
	tmpDir := t.TempDir()
	localCache := filepath.Join(tmpDir, "modelsdev_cache.json")
	if err := os.WriteFile(localCache, []byte("{}"), 0o600); err != nil {
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
	userFallback := filepath.Join(userDir, "opencode2api", "modelsdev_cache.json")
	explicitCfg := filepath.Join(tmpDir, "explicit", "config.json")
	customCfg := filepath.Join(tmpDir, "custom", "config.json")

	tests := []struct {
		name           string
		envCache       string
		envCacheFile   string
		cfgPath        string
		configExplicit bool
		want           string
		wantFallback   bool
	}{
		{
			name:           "OPENCODE2API_MODELSDEV_CACHE environment wins",
			envCache:       "/tmp/env-modelsdev.json",
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           "/tmp/env-modelsdev.json",
		},
		{
			name:           "OPENCODE2API_MODELSDEV_CACHE_FILE environment wins",
			envCacheFile:   "/tmp/env-modelsdev-file.json",
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           "/tmp/env-modelsdev-file.json",
		},
		{
			name:           "explicit config follows its directory despite legacy local cache",
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           filepath.Join(tmpDir, "explicit", "modelsdev_cache.json"),
			wantFallback:   true,
		},
		{
			name:    "non-explicit config honors legacy local cache",
			cfgPath: explicitCfg,
			want:    "modelsdev_cache.json",
		},
		{
			name:    "non-explicit resolved config honors legacy local cache",
			cfgPath: customCfg,
			want:    "modelsdev_cache.json",
		},
		{
			name:           "blank environment is ignored",
			envCache:       "   ",
			envCacheFile:   "   ",
			cfgPath:        explicitCfg,
			configExplicit: true,
			want:           filepath.Join(tmpDir, "explicit", "modelsdev_cache.json"),
			wantFallback:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENCODE2API_MODELSDEV_CACHE", tt.envCache)
			t.Setenv("OPENCODE2API_MODELSDEV_CACHE_FILE", tt.envCacheFile)

			got, gotFallback := resolveModelsDevCachePath(tt.cfgPath, tt.configExplicit)
			if got != tt.want || gotFallback != tt.wantFallback {
				t.Fatalf("resolveModelsDevCachePath() = (%q, %v), want (%q, %v)", got, gotFallback, tt.want, tt.wantFallback)
			}
		})
	}

	if err := os.Remove(localCache); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_MODELSDEV_CACHE", "")
	t.Setenv("OPENCODE2API_MODELSDEV_CACHE_FILE", "")

	got, gotFallback := resolveModelsDevCachePath(customCfg, false)
	wantCfgCache := filepath.Join(tmpDir, "custom", "modelsdev_cache.json")
	if got != wantCfgCache || !gotFallback {
		t.Fatalf("config-directory fallback = (%q, %v), want (%q, true)", got, gotFallback, wantCfgCache)
	}

	got, gotFallback = resolveModelsDevCachePath("", false)
	if got != userFallback || !gotFallback {
		t.Fatalf("user-directory fallback = (%q, %v), want (%q, true)", got, gotFallback, userFallback)
	}
}
