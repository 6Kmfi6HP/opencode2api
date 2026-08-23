package app

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestBuildClaudeEnv(t *testing.T) {
	env := buildClaudeEnv("http://127.0.0.1:9999", "public", "", 0)
	want := map[string]string{
		"ANTHROPIC_BASE_URL":                   "http://127.0.0.1:9999",
		"ANTHROPIC_API_KEY":                    "public",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": "1",
		"CLAUDE_CODE_ATTRIBUTION_HEADER":       "0",
		"CLAUDE_CODE_TOTAL_TOKENS_REMINDER":    "off",
		"DISABLE_ERROR_REPORTING":              "1",
		"DISABLE_FEEDBACK_COMMAND":             "1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":  "1",
	}
	parsed := make(map[string]string, len(env))
	for _, kv := range env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			t.Errorf("malformed env entry: %q", kv)
			continue
		}
		parsed[key] = val
	}
	for k, v := range want {
		got, ok := parsed[k]
		if !ok {
			t.Errorf("missing env %s", k)
			continue
		}
		if got != v {
			t.Errorf("env %s = %q, want %q", k, got, v)
		}
	}
	if len(parsed) != len(want) {
		t.Errorf("env has %d entries, want %d", len(parsed), len(want))
	}
}

func TestBuildClaudeEnvGoKey(t *testing.T) {
	env := buildClaudeEnv("http://127.0.0.1:8000", "go:sk-test123456", "", 0)
	found := false
	for _, kv := range env {
		if key, val, ok := strings.Cut(kv, "="); ok && key == "ANTHROPIC_API_KEY" {
			if val != "go:sk-test123456" {
				t.Errorf("ANTHROPIC_API_KEY = %q, want go:sk-test123456", val)
			}
			found = true
		}
	}
	if !found {
		t.Error("ANTHROPIC_API_KEY not found in env")
	}
}

func TestTierLabel(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"public", "free"},
		{"", "free"},
		{"sk-abc123def456", "auto"},
		{"go:sk-abc123def456", "go"},
		{"zen:sk-abc123def456", "zen"},
		{"foo", "free"},
	}
	for _, tc := range tests {
		if got := tierLabel(tc.key); got != tc.want {
			t.Errorf("tierLabel(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestFindClaude(t *testing.T) {
	// If claude exists in PATH or common locations, verify findClaude returns
	// a non-empty path.  Otherwise skip (we can't easily test the os.Exit path).
	installed := false
	if _, err := exec.LookPath("claude"); err == nil {
		installed = true
	} else if home, _ := os.UserHomeDir(); home != "" {
		for _, c := range []string{
			home + "/.local/bin/claude",
			home + "/.claude/local/claude",
		} {
			if _, statErr := os.Stat(c); statErr == nil {
				installed = true
				break
			}
		}
	}
	if !installed {
		t.Skip("claude not installed; skipping findClaude test")
	}
	if path := findClaude(); path == "" {
		t.Error("findClaude returned empty string despite being found")
	}
}

// parseLaunchFlags mirrors the flag set defined in launchClaude so tests can
// verify parsing without invoking the full launch flow (which calls os.Exit
// on errors).
func parseLaunchFlags(args []string) (model, key string, port int, debug, ver bool, extra []string, err error) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.StringVar(&model, "model", "", "")
	fs.StringVar(&key, "key", "", "")
	fs.IntVar(&port, "port", 0, "")
	fs.BoolVar(&debug, "debug", false, "")
	fs.BoolVar(&ver, "version", false, "")
	err = fs.Parse(args)
	extra = fs.Args()
	return
}

func TestLaunchFlagParsingWithModel(t *testing.T) {
	model, _, _, _, _, extra, err := parseLaunchFlags(
		[]string{"--model", "deepseek-v4-flash", "--", "-p", "hello"})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if model != "deepseek-v4-flash" {
		t.Errorf("model = %q, want deepseek-v4-flash", model)
	}
	if len(extra) != 2 || extra[0] != "-p" || extra[1] != "hello" {
		t.Errorf("extra args = %v, want [-p hello]", extra)
	}
}

func TestLaunchFlagParsingNoModel(t *testing.T) {
	model, _, _, _, _, extra, err := parseLaunchFlags(
		[]string{"--", "-p", "hello"})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if model != "" {
		t.Errorf("model = %q, want empty", model)
	}
	if len(extra) != 2 || extra[0] != "-p" || extra[1] != "hello" {
		t.Errorf("extra args = %v, want [-p hello]", extra)
	}
}

func TestLaunchFlagParsingKeyAndModel(t *testing.T) {
	model, key, _, _, _, _, err := parseLaunchFlags(
		[]string{"--key", "go:sk-test123456", "--model", "glm-5.2", "--"})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if key != "go:sk-test123456" {
		t.Errorf("key = %q, want go:sk-test123456", key)
	}
	if model != "glm-5.2" {
		t.Errorf("model = %q, want glm-5.2", model)
	}
}

func TestLaunchFlagParsingPort(t *testing.T) {
	_, _, port, _, _, _, err := parseLaunchFlags(
		[]string{"--port", "8000", "--"})
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if port != 8000 {
		t.Errorf("port = %d, want 8000", port)
	}
}

func TestNewLaunchFlagSetStatsFile(t *testing.T) {
	flags := newLaunchFlagSet("claude", []string{"-stats-file", "custom_stats.json", "-log-file", "custom_log.log"})
	if flags.statsFile != "custom_stats.json" {
		t.Errorf("statsFile = %q, want custom_stats.json", flags.statsFile)
	}
	if !flags.statsExplicit {
		t.Errorf("statsExplicit = false, want true")
	}
	if flags.logFile != "custom_log.log" {
		t.Errorf("logFile = %q, want custom_log.log", flags.logFile)
	}
	if !flags.logExplicit {
		t.Errorf("logExplicit = false, want true")
	}
}

func TestConfigureLaunchGlobalsStatsPath(t *testing.T) {
	tmpDir := t.TempDir()
	customStats := tmpDir + "/launch_stats.json"
	flags := launchFlags{
		statsFile:     customStats,
		statsExplicit: true,
	}
	configureLaunchGlobals(flags)
	if got := getTokenStatsPath(); got != customStats {
		t.Errorf("getTokenStatsPath() = %q, want %q", got, customStats)
	}
}
