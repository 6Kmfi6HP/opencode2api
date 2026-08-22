package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// runLaunch is the entry point for the `opencode2api launch <tool>` subcommand.
// Currently only "claude" is supported.
func runLaunch(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: opencode2api launch <tool> [args...]")
		fmt.Fprintln(os.Stderr, "supported tools: claude")
		os.Exit(2)
	}
	tool := args[0]
	switch tool {
	case "claude":
		launchClaude(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unsupported launch tool: %s\nsupported: claude\n", tool)
		os.Exit(2)
	}
}

// launchClaude starts the proxy backend on a localhost port, then execs
// `claude` with ANTHROPIC_* environment variables pointing at the proxy.
// When claude exits, the proxy shuts down and opencode2api exits with
// claude's exit code.
func launchClaude(args []string) {
	var (
		model    string
		key      string
		cfgPath  = "config.json"
		portFlag int
		debug    bool
		showVer  bool
	)
	fs := flag.NewFlagSet("opencode2api launch claude", flag.ContinueOnError)
	fs.StringVar(&model, "model", "", "upstream model ID (sets ANTHROPIC_*_MODEL env vars; empty = interactive TUI selection)")
	fs.StringVar(&key, "key", "", "OpenCode key (flag > OPENCODE_API_KEY env > public)")
	fs.StringVar(&cfgPath, "config", "config.json", "config file path")
	fs.IntVar(&portFlag, "port", 0, "port to bind; 0 = random")
	fs.BoolVar(&debug, "debug", false, "enable debug logs")
	fs.BoolVar(&showVer, "version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if showVer {
		fmt.Println(versionString())
		return
	}

	// Resolve key: flag > env > "public"
	if key == "" {
		key = os.Getenv("OPENCODE_API_KEY")
	}
	if key == "" {
		key = "public"
	}

	// Set globals for initProxyCore / initLogger consumption.
	configPath = cfgPath
	adminPassword = "" // launch mode disables the admin panel
	debugMode = debug
	logLevel = "info"
	if debug {
		logLevel = "debug"
	}
	logFile = "opencode2api.log"
	logStdout = false // launch mode: logs go to file only, never stdout (would corrupt the Claude Code TUI)
	logMaxSize = 100
	logMaxBackups = 7
	logMaxAge = 14
	logCompress = true
	logBodies = false

	initLogger()
	defer closeLogRotator()

	initProxyCore()

	addr := fmt.Sprintf("127.0.0.1:%d", portFlag)
	mux := buildMux()
	server, listener, err := startServer(addr, mux)
	if err != nil {
		slog.Error("failed to start proxy", "addr", addr, "error", err)
		os.Exit(1)
	}
	defer func() { _ = listener.Close() }()

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		slog.Error("unexpected listener address type", "addr", listener.Addr().String())
		os.Exit(1)
	}
	actualPort := tcpAddr.Port
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", actualPort)

	if wErr := waitForServer(actualPort, 10*time.Second); wErr != nil {
		slog.Error("proxy failed to become ready", "error", wErr)
		os.Exit(1)
	}

	slog.Info("proxy ready",
		"url", baseURL,
		"tier", tierLabel(key),
		"bind", "127.0.0.1",
		"admin", "disabled",
	)

	// Print a concise summary to stderr so the user sees proxy status without
	// polluting stdout (which Claude Code's TUI owns).
	fmt.Fprintf(os.Stderr, "opencode2api: proxy ready at %s (tier=%s, log=%s)\n", baseURL, tierLabel(key), resolvedLogPath())
	models := getModelIDs()
	avail := models
	if len(avail) > 10 {
		avail = avail[:10]
	}
	slog.Info("upstream models available", "total", len(models), "sample", avail)

	// Fetch models.dev catalog in the background so context-window lookups
	// are available by the time we need them. The fetch starts while the
	// server boots and becomes ready; by the time we reach model selection
	// or context-window computation the fetch has almost certainly finished
	// (5 s HTTP timeout guaranteed by fetchModelsDevCatalog).
	catalogCh := make(chan modelsDevCatalog, 1)
	go func() {
		cat, ferr := fetchModelsDevCatalog()
		if ferr != nil {
			slog.Warn("failed to fetch models.dev catalog", "error", ferr)
		}
		catalogCh <- cat
	}()

	// Wait for the catalog (blocks until the background fetch completes).
	catalog := <-catalogCh

	// Throwaway --model flag may appear after `--` in the extra args (e.g.
	//   opencode2api launch claude -- --dangerously-skip-permissions --model x-preview-f
	// ) — extract it so the TUI is skipped and it is not forwarded to claude.
	extraArgs := fs.Args()
	if model == "" {
		var extraModel string
		extraModel, extraArgs = extractModelFromExtraArgs(extraArgs)
		if extraModel != "" {
			model = extraModel
		}
	}

	// Determine the model ID to use.
	modelID := ""
	if model != "" {
		modelID = strings.TrimSpace(model)
	} else {
		// Interactive TUI model selection — show models from both the
		// zen and go catalogs so the user can pick any available model.
		allModels := append(getModelIDs(), getGoModelIDs()...)
		selected, sErr := selectModelTTY(allModels, catalog)
		if sErr != nil {
			slog.Warn("model selection failed", "error", sErr)
		}
		modelID = selected
	}

	// Compute context window, suffix, and auto-compact threshold.
	autoCompactWindow := 0
	if modelID != "" {
		base, suffix := stripContextSuffix(modelID)
		_ = suffix // suffix is always empty here (we haven't appended [1m] yet)
		ctx := getContextWindow(base, catalog)
		if ctx >= 1000000 {
			modelID = base + "[1m]"
			autoCompactWindow = int(float64(ctx) * 0.9)
		} else if ctx > 0 {
			modelID = base
			autoCompactWindow = int(float64(ctx) * 0.9)
		} else {
			modelID = base
		}
		slog.Info("model resolved",
			"model_id", modelID,
			"context", ctx,
			"auto_compact", autoCompactWindow,
		)
	}

	claudePath := findClaude()
	// We no longer pass --model to claude; instead we set ANTHROPIC_*_MODEL
	// environment variables (buildClaudeEnv) so Claude Code uses our model
	// without triggering an "unrecognized_model" warning.
	claudeArgs := extraArgs

	cmd := exec.Command(claudePath, claudeArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	claudeEnv := os.Environ()
	claudeEnv = append(claudeEnv, buildClaudeEnv(baseURL, key, modelID, autoCompactWindow)...)
	cmd.Env = claudeEnv

	// Intercept signals so we can forward them to claude rather than killing
	// opencode2api directly.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		}
	}()

	slog.Info("launching claude", "path", claudePath, "args", claudeArgs, "model", modelID, "auto_compact", autoCompactWindow)

	if startErr := cmd.Start(); startErr != nil {
		slog.Error("failed to start claude", "error", startErr)
		signal.Stop(sigCh)
		_ = server.Shutdown(context.Background())
		os.Exit(1)
	}

	waitErr := cmd.Wait()
	signal.Stop(sigCh)

	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			slog.Error("claude terminated with error", "error", waitErr)
			exitCode = 1
		}
	}

	slog.Info("claude exited", "exit_code", exitCode)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("proxy graceful shutdown failed", "error", err)
	}
	os.Exit(exitCode)
}

// findClaude locates the claude CLI binary by checking PATH, then common
// local install locations.
func findClaude() string {
	if path, err := exec.LookPath("claude"); err == nil {
		return path
	}
	home, err := os.UserHomeDir()
	if err == nil {
		for _, candidate := range []string{
			filepath.Join(home, ".local", "bin", "claude"),
			filepath.Join(home, ".claude", "local", "claude"),
		} {
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	fmt.Fprintln(os.Stderr, "claude not found in PATH, ~/.local/bin/claude, or ~/.claude/local/claude")
	fmt.Fprintln(os.Stderr, "install with: npm install -g @anthropic-ai/claude-code")
	os.Exit(1)
	return ""
}

// buildClaudeEnv returns the environment variables that redirect claude
// HTTP traffic to the local opencode2api proxy and suppress interactive
// features that would interfere with unattended operation.
//
// When modelID is non-empty, five ANTHROPIC_*_MODEL variables are set to it
// so Claude Code uses the specified upstream model without needing --model.
// When autoCompactWindow > 0, CLAUDE_CODE_AUTO_COMPACT_WINDOW is set to
// trigger auto-compaction at 90% of the model's context window.
//
// Key design decisions (verified against Claude Code 2.1.x):
//   - ANTHROPIC_API_KEY=<key> (non-empty) is the primary auth source. Claude
//     Code sends it as the x-api-key header; opencode2api's extractUpstreamAuth
//     reads x-api-key and routes by prefix (public/sk-/go:/zen:).
//   - ANTHROPIC_MODEL / ANTHROPIC_DEFAULT_OPUS_MODEL / ANTHROPIC_DEFAULT_SONNET_MODEL /
//     ANTHROPIC_DEFAULT_HAIKU_MODEL / ANTHROPIC_SMALL_FAST_MODEL are all set to
//     modelID. This avoids the [claude-code:unrecognized_model] warning that
//     occurs when passing an unknown model name via --model, and ensures every
//     internal model slot (opus/sonnet/haiku/small-fast) routes through the proxy.
//   - CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1 tells Claude Code the host owns
//     the provider, so it must NOT fall back to OAuth/keychain/subscription
//     login. Without this flag Claude Code ignores the custom base URL when
//     it finds stored OAuth credentials.
//   - ANTHROPIC_AUTH_TOKEN is intentionally omitted: setting it alongside
//     ANTHROPIC_API_KEY triggers "gateway mode" in Claude Code, which changes
//     the auth flow and can prompt for key approval even in non-interactive
//     mode.
func buildClaudeEnv(baseURL, key, modelID string, autoCompactWindow int) []string {
	env := []string{
		"ANTHROPIC_BASE_URL=" + baseURL,
		"ANTHROPIC_API_KEY=" + key,
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1",
		"CLAUDE_CODE_ATTRIBUTION_HEADER=0",
		"CLAUDE_CODE_TOTAL_TOKENS_REMINDER=off",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_FEEDBACK_COMMAND=1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1",
	}
	if modelID != "" {
		env = append(env,
			"ANTHROPIC_MODEL="+modelID,
			"ANTHROPIC_DEFAULT_OPUS_MODEL="+modelID,
			"ANTHROPIC_DEFAULT_SONNET_MODEL="+modelID,
			"ANTHROPIC_DEFAULT_HAIKU_MODEL="+modelID,
			"ANTHROPIC_SMALL_FAST_MODEL="+modelID,
		)
	}
	if autoCompactWindow > 0 {
		env = append(env, fmt.Sprintf("CLAUDE_CODE_AUTO_COMPACT_WINDOW=%d", autoCompactWindow))
	}
	return env
}

// waitForServer polls GET /health on 127.0.0.1:port until it returns 200 or
// the timeout expires.
func waitForServer(port int, timeout time.Duration) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("proxy not ready after %s", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// tierLabel maps an OpenCode key prefix to a human-readable tier label.
func tierLabel(key string) string {
	switch {
	case strings.HasPrefix(key, "go:"):
		return "go"
	case strings.HasPrefix(key, "zen:"):
		return "zen"
	case strings.HasPrefix(key, "sk-"):
		return "auto"
	default:
		return "free"
	}
}
