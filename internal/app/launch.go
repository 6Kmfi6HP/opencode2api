package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

const codexAPIKeyEnv = "OPENCODE2API_OPENAI_API_KEY"

type launchFlags struct {
	model     string
	key       string
	cfgPath   string
	logFile   string
	port      int
	debug     bool
	showVer   bool
	extraArgs []string
}

// newLaunchFlagSet parses the flags shared by `opencode2api launch claude` and
// `opencode2api launch codex`.
func newLaunchFlagSet(tool string, args []string) launchFlags {
	var f launchFlags
	fs := flag.NewFlagSet("opencode2api launch "+tool, flag.ContinueOnError)
	fs.StringVar(&f.model, "model", "", "upstream model ID (empty = interactive TUI selection)")
	fs.StringVar(&f.key, "key", "", "OpenCode key (flag > OPENCODE_API_KEY env > public)")
	fs.StringVar(&f.cfgPath, "config", "", "config file path (default: OPENCODE2API_CONFIG, ./config.json if present, or the user config directory)")
	fs.StringVar(&f.logFile, "log-file", "", "log file path (empty = platform default)")
	fs.IntVar(&f.port, "port", 0, "port to bind; 0 = random")
	fs.BoolVar(&f.debug, "debug", false, "enable debug logs")
	fs.BoolVar(&f.showVer, "version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	f.extraArgs = fs.Args()

	var explicit bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			explicit = true
		}
	})
	f.cfgPath, _ = resolveConfigPath(f.cfgPath, explicit)

	return f
}

// runLaunch is the entry point for the `opencode2api launch <tool>` subcommand.
func runLaunch(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: opencode2api launch <tool> [args...]")
		fmt.Fprintln(os.Stderr, "supported tools: claude, codex")
		os.Exit(2)
	}
	tool := args[0]
	switch tool {
	case "claude":
		launchClaude(args[1:])
	case "codex":
		launchCodex(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unsupported launch tool: %s\nsupported: claude, codex\n", tool)
		os.Exit(2)
	}
}

func resolveLaunchKey(key string) string {
	if key == "" {
		key = os.Getenv("OPENCODE_API_KEY")
	}
	if key == "" {
		key = "public"
	}
	return key
}

func configureLaunchGlobals(f launchFlags) {
	configPath = f.cfgPath
	adminPassword = "" // launch mode disables the admin panel
	debugMode = f.debug
	logLevel = "info"
	if f.debug {
		logLevel = "debug"
	}
	if f.logFile != "" {
		logFile = f.logFile
	} else {
		logFile = launchDefaultLogFile()
	}
	logStdout = false // launch mode: logs go to file only, never stdout (would corrupt the child TUI)
	logMaxSize = 100
	logMaxBackups = 7
	logMaxAge = 14
	logCompress = true
	logBodies = false

	initLogger()
}

// startLaunchProxy starts the local, read-only proxy config that both launch
// targets share. It returns the HTTP server, its listener, and the actual
// localhost base URL.
func startLaunchProxy(f launchFlags) (*http.Server, net.Listener, string) {
	initProxyCoreReadOnly()

	addr := fmt.Sprintf("127.0.0.1:%d", f.port)
	mux := buildMux()
	server, listener, err := startServer(addr, mux)
	if err != nil {
		slog.Error("failed to start proxy", "addr", addr, "error", err)
		os.Exit(1)
	}

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
		"tier", tierLabel(f.key),
		"bind", "127.0.0.1",
		"admin", "disabled",
	)

	// Print a concise summary to stderr so the user sees proxy status without
	// polluting stdout (which the child CLI TUI owns).
	fmt.Fprintf(os.Stderr, "opencode2api: proxy ready at %s (tier=%s, log=%s)\n", baseURL, tierLabel(f.key), resolvedLogPath())
	models := getModelIDs()
	avail := models
	if len(avail) > 10 {
		avail = avail[:10]
	}
	slog.Info("upstream models available", "total", len(models), "sample", avail)
	return server, listener, baseURL
}

func fetchLaunchCatalog() modelsDevCatalog {
	catalogCh := make(chan modelsDevCatalog, 1)
	go func() {
		cat, ferr := fetchModelsDevCatalog()
		if ferr != nil {
			slog.Warn("failed to fetch models.dev catalog", "error", ferr)
		}
		catalogCh <- cat
	}()
	return <-catalogCh
}

func resolveLaunchModel(model string, extraArgs []string, extract func([]string) (string, []string), catalog modelsDevCatalog, contextSuffix bool) (string, []string, int, int) {
	// Throwaway model flags may appear after `--` in the child args. Extract
	// them so the TUI is skipped and they are not forwarded to child CLI.
	if model == "" {
		extraModel, cleaned := extract(extraArgs)
		if extraModel != "" {
			model = extraModel
			extraArgs = cleaned
		}
	}

	modelID := strings.TrimSpace(model)
	if modelID == "" {
		// Interactive TUI model selection — show models from both the zen and
		// go catalogs so the user can pick any available model.
		allModels := append(getModelIDs(), getGoModelIDs()...)
		selected, sErr := selectModelInteractive(os.Stdin, os.Stdout, os.Stderr, allModels, catalog)
		if sErr != nil {
			slog.Warn("model selection failed", "error", sErr)
		}
		modelID = selected
	}

	autoCompactWindow := 0
	ctx := 0
	if modelID != "" {
		base, _ := stripContextSuffix(modelID)
		ctx = getContextWindow(base, catalog)
		if contextSuffix {
			if ctx >= 1000000 {
				modelID = base + "[1m]"
				autoCompactWindow = int(float64(ctx) * 0.9)
			} else if ctx > 0 {
				modelID = base
				autoCompactWindow = int(float64(ctx) * 0.9)
			} else {
				modelID = base
			}
		} else {
			// Codex receives a temporary per-launch model catalog later, so
			// keep the ID clean here and do not append the Claude-specific
			// [1m] suffix.
			modelID = base
		}
		slog.Info("model resolved",
			"model_id", modelID,
			"context", ctx,
			"auto_compact", autoCompactWindow,
		)
	}

	return modelID, extraArgs, ctx, autoCompactWindow
}

// runLaunchChild runs the selected local CLI with a temporary child
// environment. Signals are forwarded to the child, and opencode2api exits
// with the child's exit code once the child exits.
func runLaunchChild(tool, path string, args, extraEnv []string, server *http.Server, cleanup func()) {
	cmd := newLaunchChildCommand(path, args)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	env := os.Environ()
	env = append(env, extraEnv...)
	cmd.Env = env

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				signalLaunchChild(cmd.Process, sig)
			}
		}
	}()

	slog.Info("launching "+tool, "path", path, "args", args)

	if startErr := cmd.Start(); startErr != nil {
		slog.Error("failed to start "+tool, "error", startErr)
		signal.Stop(sigCh)
		if cleanup != nil {
			cleanup()
		}
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
			slog.Error(tool+" terminated with error", "error", waitErr)
			exitCode = 1
		}
	}

	slog.Info(tool+" exited", "exit_code", exitCode)
	if cleanup != nil {
		cleanup()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("proxy graceful shutdown failed", "error", err)
	}
	os.Exit(exitCode)
}

// launchClaude starts the proxy backend on a localhost port, then execs
// `claude` with ANTHROPIC_* environment variables pointing at the proxy.
func launchClaude(args []string) {
	f := newLaunchFlagSet("claude", args)
	if f.showVer {
		fmt.Println(versionString())
		return
	}
	f.key = resolveLaunchKey(f.key)

	configureLaunchGlobals(f)
	defer closeLogRotator()

	server, listener, baseURL := startLaunchProxy(f)
	defer func() { _ = listener.Close() }()

	catalog := fetchLaunchCatalog()
	modelID, extraArgs, _, autoCompactWindow := resolveLaunchModel(f.model, f.extraArgs, extractModelFromExtraArgs, catalog, true)

	claudePath := findClaude()
	// We no longer pass --model to claude; instead we set ANTHROPIC_*_MODEL
	// environment variables (buildClaudeEnv) so Claude Code uses our model
	// without triggering an "unrecognized_model" warning.
	runLaunchChild("claude", claudePath, extraArgs, buildClaudeEnv(baseURL, f.key, modelID, autoCompactWindow), server, nil)
}

// launchCodex starts the proxy backend on a localhost port, then execs
// `codex` with temporary `-c` overrides. It reads `~/.codex/config.toml` as
// usual, but does not write any Codex configuration back to disk.
func launchCodex(args []string) {
	f := newLaunchFlagSet("codex", args)
	if f.showVer {
		fmt.Println(versionString())
		return
	}
	f.key = resolveLaunchKey(f.key)

	configureLaunchGlobals(f)
	defer closeLogRotator()

	server, listener, baseURL := startLaunchProxy(f)
	defer func() { _ = listener.Close() }()

	catalog := fetchLaunchCatalog()
	modelID, extraArgs, _, _ := resolveLaunchModel(f.model, f.extraArgs, extractCodexModelFromExtraArgs, catalog, false)

	specs := buildCodexModelCatalogSpecs(catalog, tierLabel(f.key) == "free")
	var catalogPath string
	var cleanup func()
	if len(specs) > 0 {
		writtenPath, err := writeCodexModelCatalog(specs)
		if err != nil {
			slog.Warn("failed to write launch codex model catalog", "error", err)
		} else {
			catalogPath = writtenPath
			slog.Info("codex model catalog written", "path", catalogPath, "models", len(specs))
			cleanup = func() { _ = os.RemoveAll(filepath.Dir(catalogPath)) }
		}
	}

	codexPath := findCodex()
	codexArgs := append(buildCodexConfigArgs(baseURL, modelID, catalogPath), extraArgs...)
	runLaunchChild("codex", codexPath, codexArgs, buildCodexEnv(f.key), server, cleanup)
}

// findClaude locates the claude CLI binary by checking PATH, then common
// platform-specific local install locations.
func findClaude() string {
	if path, err := exec.LookPath("claude"); err == nil {
		return path
	}
	for _, candidate := range launchCandidatePaths("claude") {
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate
		}
	}
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "claude not found in PATH,", "%USERPROFILE%\\.local\\bin, %USERPROFILE%\\.claude\\local, or %APPDATA%\\npm")
		fmt.Fprintln(os.Stderr, "install with: npm install -g @anthropic-ai/claude-code")
	} else {
		fmt.Fprintln(os.Stderr, "claude not found in PATH, ~/.local/bin/claude, or ~/.claude/local/claude")
		fmt.Fprintln(os.Stderr, "install with: npm install -g @anthropic-ai/claude-code")
	}
	os.Exit(1)
	return ""
}

// findCodex locates the Codex CLI binary by checking PATH, then common
// platform-specific local install locations.
func findCodex() string {
	if path, err := exec.LookPath("codex"); err == nil {
		return path
	}
	for _, candidate := range launchCandidatePaths("codex") {
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate
		}
	}
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "codex not found in PATH,", "%USERPROFILE%\\.local\\bin, %USERPROFILE%\\.codex\\bin, or %APPDATA%\\npm")
		fmt.Fprintln(os.Stderr, "install Codex CLI: https://github.com/openai/codex")
	} else {
		fmt.Fprintln(os.Stderr, "codex not found in PATH, ~/.local/bin/codex, or ~/.codex/bin/codex")
		fmt.Fprintln(os.Stderr, "install Codex CLI: https://github.com/openai/codex")
	}
	os.Exit(1)
	return ""
}

// buildClaudeEnv returns the environment variables that redirect claude HTTP
// traffic to the local opencode2api proxy and suppress interactive features
// that would interfere with unattended operation.
//
// When modelID is non-empty, five ANTHROPIC_*_MODEL variables are set to it
// so Claude Code uses the specified upstream model without needing --model.
// When autoCompactWindow > 0, CLAUDE_CODE_AUTO_COMPACT_WINDOW is set to
// trigger auto-compaction at 90% of the model's context window.
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

// buildCodexConfigArgs builds the temporary `-c` arguments that point a fresh
// Codex provider at the local opencode2api Responses endpoint.
func buildCodexConfigArgs(baseURL, modelID, modelCatalogPath string) []string {
	const providerID = "opencode2api"
	providerPrefix := "model_providers." + providerID
	args := []string{
		"-c", `model_provider="` + providerID + `"`,
		"-c", `model_providers.` + providerID + `.name="opencode2api"`,
		"-c", fmt.Sprintf(`%s.base_url=%q`, providerPrefix, baseURL+"/v1"),
		"-c", fmt.Sprintf(`%s.wire_api="responses"`, providerPrefix),
		"-c", fmt.Sprintf(`%s.requires_openai_auth=true`, providerPrefix),
		"-c", fmt.Sprintf(`%s.env_key=%q`, providerPrefix, codexAPIKeyEnv),
	}
	if modelID != "" {
		args = append(args, "--model", modelID)
	}
	if modelCatalogPath != "" {
		args = append(args, "-c", fmt.Sprintf("model_catalog_json=%q", modelCatalogPath))
	}
	return args
}

type codexModelReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexModelCatalogEntry struct {
	Slug                          string                     `json:"slug"`
	DisplayName                   string                     `json:"display_name"`
	Description                   string                     `json:"description"`
	ContextWindow                 int                        `json:"context_window"`
	MaxContextWindow              int                        `json:"max_context_window"`
	EffectiveContextWindowPercent int                        `json:"effective_context_window_percent"`
	AutoCompactTokenLimit         int                        `json:"auto_compact_token_limit"`
	DefaultReasoningLevel         string                     `json:"default_reasoning_level"`
	DefaultReasoningSummary       string                     `json:"default_reasoning_summary"`
	SupportedReasoningLevels      []codexModelReasoningLevel `json:"supported_reasoning_levels"`
	InputModalities               []string                   `json:"input_modalities"`
	SupportsImageDetailOriginal   bool                       `json:"supports_image_detail_original"`
	SupportsParallelToolCalls     bool                       `json:"supports_parallel_tool_calls"`
	SupportsReasoningSummaries    bool                       `json:"supports_reasoning_summaries"`
	SupportsSearchTool            bool                       `json:"supports_search_tool"`
	ShellType                     string                     `json:"shell_type"`
	Visibility                    string                     `json:"visibility"`
	SupportedInAPI                bool                       `json:"supported_in_api"`
	Priority                      int                        `json:"priority"`
	AdditionalSpeedTiers          []any                      `json:"additional_speed_tiers"`
	ServiceTiers                  []any                      `json:"service_tiers"`
	BaseInstructions              string                     `json:"base_instructions"`
	SupportVerbosity              bool                       `json:"support_verbosity"`
	TruncationPolicy              map[string]any             `json:"truncation_policy"`
	ExperimentalSupportedTools    []any                      `json:"experimental_supported_tools"`
}

type codexModelCatalog struct {
	Models []codexModelCatalogEntry `json:"models"`
}

type codexModelCatalogSpec struct {
	ID            string
	ContextWindow int
}

// buildCodexModelCatalogSpecs builds the catalog model set passed to Codex.
// When freeOnly is true (default public tier), only models with a usable
// "-free" variant are kept, matching the interactive launch model list.
func buildCodexModelCatalogSpecs(catalog modelsDevCatalog, freeOnly bool) []codexModelCatalogSpec {
	modelIDs := append(getModelIDs(), getGoModelIDs()...)

	idSet := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		idSet[id] = true
	}

	seen := make(map[string]bool, len(modelIDs))
	var specs []codexModelCatalogSpec
	for _, id := range modelIDs {
		pub := publicFacingModelID(id)
		if pub == "" || seen[pub] {
			continue
		}
		if freeOnly && !idSet[pub+"-free"] && !isFreeModel(id) {
			continue
		}
		seen[pub] = true
		specs = append(specs, codexModelCatalogSpec{
			ID:            pub,
			ContextWindow: getContextWindow(pub, catalog),
		})
	}

	sort.SliceStable(specs, func(i, j int) bool {
		if specs[i].ContextWindow != specs[j].ContextWindow {
			return specs[i].ContextWindow > specs[j].ContextWindow
		}
		return specs[i].ID < specs[j].ID
	})
	return specs
}

// writeCodexModelCatalog writes a temporary Codex model catalog containing the
// provided launch model set and its known context/auto-compact metadata. Codex
// reads this through `-c model_catalog_json=...` without writing any user
// config.
func writeCodexModelCatalog(specs []codexModelCatalogSpec) (string, error) {
	dir, err := os.MkdirTemp("", "opencode2api-codex-catalog-*")
	if err != nil {
		return "", err
	}

	entries := make([]codexModelCatalogEntry, 0, len(specs))
	for _, spec := range specs {
		entry := codexModelCatalogEntry{
			Slug:                          spec.ID,
			DisplayName:                   spec.ID,
			Description:                   "opencode2api launch override for " + spec.ID,
			ContextWindow:                 spec.ContextWindow,
			MaxContextWindow:              spec.ContextWindow,
			EffectiveContextWindowPercent: 100,
			AutoCompactTokenLimit:         0,
			DefaultReasoningLevel:         "high",
			DefaultReasoningSummary:       "none",
			SupportedReasoningLevels: []codexModelReasoningLevel{
				{Effort: "low", Description: "Fast responses with lighter reasoning"},
				{Effort: "high", Description: "Extra high reasoning depth for complex problems"},
				{Effort: "max", Description: "Maximum reasoning depth for the hardest problems"},
			},
			InputModalities:             []string{"text"},
			SupportsImageDetailOriginal: false,
			SupportsParallelToolCalls:   true,
			SupportsReasoningSummaries:  true,
			SupportsSearchTool:          true,
			ShellType:                   "shell_command",
			Visibility:                  "list",
			SupportedInAPI:              true,
			Priority:                    1000,
			AdditionalSpeedTiers:        []any{},
			ServiceTiers:                []any{},
			BaseInstructions:            "You are Codex, a coding agent. You and the user share the same workspace and collaborate to achieve the user's goals.",
			SupportVerbosity:            true,
			TruncationPolicy: map[string]any{
				"mode":  "tokens",
				"limit": 10000,
			},
			ExperimentalSupportedTools: []any{},
		}
		if spec.ContextWindow > 0 {
			entry.AutoCompactTokenLimit = int(float64(spec.ContextWindow) * 0.9)
		}
		entries = append(entries, entry)
	}

	data, err := json.MarshalIndent(codexModelCatalog{Models: entries}, "", "  ")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}

// buildCodexEnv adds the child-only API key used by buildCodexConfigArgs; it
// does not persist anything into `~/.codex`.
func buildCodexEnv(key string) []string {
	return []string{codexAPIKeyEnv + "=" + key}
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
