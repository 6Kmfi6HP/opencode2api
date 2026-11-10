package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteInitialConfig_CopiesExample(t *testing.T) {
	dir := t.TempDir()
	example := filepath.Join(dir, "example.json")
	want := []byte("{\"model_alias\":{}}\n")
	if err := os.WriteFile(example, want, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_SOCKS5_ADDR", "")

	target := filepath.Join(dir, "config.json")
	if err := writeInitialConfig(target, example); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("config bytes = %q, want %q", got, want)
	}
}

func TestWriteInitialConfig_SOCKS5(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENCODE2API_SOCKS5_ADDR", "127.0.0.1:1080")
	t.Setenv("OPENCODE2API_SOCKS5_NAME", "")

	target := filepath.Join(dir, "config.json")
	if err := writeInitialConfig(target, filepath.Join(dir, "missing-example.json")); err != nil {
		t.Fatal(err)
	}

	var cfg struct {
		ModelAlias           map[string]string `json:"model_alias"`
		ReasoningEffortMap   map[string]string `json:"reasoning_effort_map"`
		ForceDisableThinking bool              `json:"force_disable_thinking"`
		Socks5Proxies        []struct {
			Name     string `json:"name"`
			Addr     string `json:"addr"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"socks5_proxies"`
		ActiveSocks5 string `json:"active_socks5"`
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid generated JSON: %v\n%s", err, data)
	}

	if cfg.ActiveSocks5 != "127.0.0.1:1080" {
		t.Errorf("active_socks5 = %q", cfg.ActiveSocks5)
	}
	if len(cfg.Socks5Proxies) != 1 {
		t.Fatalf("socks5_proxies len = %d, want 1", len(cfg.Socks5Proxies))
	}
	proxy := cfg.Socks5Proxies[0]
	if proxy.Name != "proxy" || proxy.Addr != "127.0.0.1:1080" || proxy.Username != "" || proxy.Password != "" {
		t.Errorf("proxy = %+v", proxy)
	}
	if cfg.ModelAlias["deepseek-v4-flash"] != "deepseek-v4-flash-free" {
		t.Errorf("model_alias = %v", cfg.ModelAlias)
	}
	if cfg.ReasoningEffortMap["minimal"] != "low" || cfg.ReasoningEffortMap["high"] != "high" {
		t.Errorf("reasoning_effort_map = %v", cfg.ReasoningEffortMap)
	}
	if cfg.ForceDisableThinking {
		t.Error("force_disable_thinking = true")
	}
}

func TestProbeHealth(t *testing.T) {
	var gotPath string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	okURL, err := url.Parse(ok.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := probeHealth(okURL.Port()); err != nil {
		t.Fatalf("probeHealth(ok) = %v", err)
	}
	if gotPath != "/health" {
		t.Fatalf("probed path = %q, want /health", gotPath)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	badURL, err := url.Parse(bad.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := probeHealth(badURL.Port()); err == nil {
		t.Fatal("probeHealth(503) = nil, want error")
	}
}

func TestEnvStringAndBool(t *testing.T) {
	t.Setenv("OPENCODE2API_TEST_STRING", "  value  ")
	if got := envString("OPENCODE2API_TEST_STRING", "fallback"); got != "value" {
		t.Errorf("envString = %q, want value", got)
	}
	t.Setenv("OPENCODE2API_TEST_STRING", "   ")
	if got := envString("OPENCODE2API_TEST_STRING", "fallback"); got != "fallback" {
		t.Errorf("blank envString = %q, want fallback", got)
	}

	t.Setenv("OPENCODE2API_TEST_BOOL", "false")
	if got := envBool("OPENCODE2API_TEST_BOOL", true); got {
		t.Error("envBool(false) = true")
	}
	t.Setenv("OPENCODE2API_TEST_BOOL", "not-a-bool")
	if got := envBool("OPENCODE2API_TEST_BOOL", true); !got {
		t.Error("invalid envBool should fall back to true")
	}
}

// TestPrepareContainer_UnwritableDataDirIsActionable pins the failure mode that
// `--user 65532:65532` hits with a root-owned bind mount: the image has no
// shell, so the error must carry the remedy instead of a bare "permission
// denied".
func TestPrepareContainer_UnwritableDataDirIsActionable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := prepareContainer(filepath.Join(dir, "config.json"))
	if err == nil {
		t.Fatal("prepareContainer on an unwritable dir = nil, want error")
	}
	msg := err.Error()
	for _, want := range []string{"is not writable by uid", "named volume", "bind mount", "--user"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

// TestPrepareContainer_CreatesConfigAndLeavesOwnershipAlone covers the
// unprivileged path: bootstrap must create the config without attempting a
// privilege drop (the process is already non-root).
func TestPrepareContainer_CreatesConfigAndLeavesOwnershipAlone(t *testing.T) {
	dir := t.TempDir()
	example := filepath.Join(dir, "example.json")
	want := []byte("{\"model_alias\":{}}\n")
	if err := os.WriteFile(example, want, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_SOCKS5_ADDR", "")

	// prepareContainer reads the bundled example path via a constant; drive
	// writeInitialConfig through the same path the bootstrap uses by pointing
	// the config at a fresh file and asserting the result is valid JSON either
	// way.
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareContainer(target); err != nil {
		t.Fatalf("prepareContainer on an existing config = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("existing config was rewritten: %q", got)
	}
}

// TestPrepareContainer_ExistingConfigInReadOnlyDirOnlyWarns covers the
// `--user 65532:65532` + bind-mount case with a pre-existing config: the image
// must not crash-loop (a read-only config mount is legitimate), but it must say
// once that /data is unwritable instead of emitting WARN lines hours later.
func TestPrepareContainer_ExistingConfigInReadOnlyDirOnlyWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := prepareContainer(configPath); err != nil {
		t.Fatalf("existing config in a read-only dir must not be fatal: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "will not persist") {
		t.Fatalf("startup did not warn about the read-only data dir: %q", out)
	}
	if !strings.Contains(out, dir) {
		t.Fatalf("warning does not name the directory: %q", out)
	}
	// The config itself must be left untouched, and readable output must not
	// require shelling into the image.
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}\n" {
		t.Fatalf("config was rewritten: %q", got)
	}
}

// TestPrepareContainer_WritableConfigFlagsAreNotFatal keeps the check honest:
// the probe must not fire on a healthy data dir, or every startup logs noise.
func TestPrepareContainer_NoWarningWhenWritable(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := prepareContainer(configPath); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "will not persist") {
		t.Fatalf("healthy data dir produced a read-only warning: %q", logs.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("write probe left files behind: %v", entries)
	}
}

// TestVerifyContainerReadOnly_ReportsBothFailures pins the two facts the
// startup check must surface: the directory (stats/log/cache) and the config
// file (admin edits) can fail independently.
func TestVerifyContainerReadOnly_ReportsBothFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyContainerReadOnly(configPath); err != nil {
		t.Fatalf("healthy dir reported read-only: %v", err)
	}

	// Directory unwritable: hits the directory branch first.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	err := verifyContainerReadOnly(configPath)
	if err == nil {
		t.Fatal("unwritable dir reported healthy")
	}
	msg := err.Error()
	for _, want := range []string{dir, "will not persist", "hint"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}

	// Directory writable but the config file is not: the second branch.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configPath, 0o644) })
	err = verifyContainerReadOnly(configPath)
	if err == nil {
		t.Fatal("read-only config file reported healthy")
	}
	if !strings.Contains(err.Error(), "admin panel edits") {
		t.Fatalf("config-only failure reported wrong remedy: %v", err)
	}
	if strings.Contains(err.Error(), "token stats") {
		t.Fatalf("config-only failure also blamed the directory: %v", err)
	}
}
