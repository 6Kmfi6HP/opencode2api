package app

import (
	"context"
	"flag"
	"fmt"
	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
	"github.com/6Kmfi6HP/opencode2api/internal/stats"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// ======================== Main ========================

// flagSet reports whether a standard-library flag was present on the command
// line, including explicit values such as -config=.
func flagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// applyLogLevel propagates the effective level string into the slog LevelVar
// so post-parse bumps (e.g. -debug) take effect even when logging was already
// initialized.
func applyLogLevel() { logging.SetLevelString(logging.Level) }

func Run() {
	// Launch subcommand: opencode2api launch <tool> [args...]
	if len(os.Args) >= 2 && os.Args[1] == "launch" {
		runLaunch(os.Args[2:])
		return
	}

	var showVersion bool
	var statsFile string
	flag.StringVar(&port, "port", "8000", "服务端口")
	flag.StringVar(&configPath, "config", "config.json", "配置文件路径")
	flag.StringVar(&statsFile, "stats-file", "stats.json", "统计文件路径")
	flag.StringVar(&adminPassword, "password", "123456", "管理面板密码（留空则不启用登录验证）")
	flag.BoolVar(&debugMode, "debug", false, "启用调试日志")
	flag.StringVar(&logging.Level, "log-level", "info", "日志级别: debug/info/warn/error")
	flag.StringVar(&logging.File, "log-file", "opencode2api.log", "日志文件路径")
	flag.BoolVar(&logging.Stdout, "log-stdout", true, "是否同时写 stdout")
	flag.IntVar(&logging.MaxSize, "log-max-size", 100, "单日志文件最大 MB，超过即轮换")
	flag.IntVar(&logging.MaxBackups, "log-max-backups", 7, "保留的旧日志文件个数")
	flag.IntVar(&logging.MaxAge, "log-max-age", 14, "旧日志保留天数")
	flag.BoolVar(&logging.Compress, "log-compress", true, "轮换后 gzip 压缩")
	flag.BoolVar(&logging.Bodies, "log-bodies", false, "Debug 下记录截断的 body 摘要")
	flag.BoolVar(&showVersion, "version", false, "显示版本信息")
	flag.Parse()

	configExplicit := flagSet("config")

	if debugMode && strings.EqualFold(logging.Level, "info") {
		logging.Level = "debug"
	}
	applyLogLevel()
	resolveAndInitRuntime(statsFile, flagSet("stats-file"), configExplicit)
	defer logging.CloseRotator()

	if showVersion {
		fmt.Println(versionString())
		return
	}

	initProxyCore()

	slog.Info("server starting",
		"port", port,
		"log_level", logging.LevelString(),
		"models", len(getModelIDs()),
		"aliases", len(getModelKeywordRules()),
	)
	if adminPassword != "" {
		slog.Info("admin panel enabled", "url", fmt.Sprintf("http://localhost:%s/", port))
	} else {
		slog.Info("admin panel disabled (no password)")
	}

	mux := buildMux()
	addr := ":" + port
	server, listener, err := startServer(addr, mux)
	if err != nil {
		slog.Error("failed to start server", "addr", addr, "error", err)
		os.Exit(1)
	}
	defer func() { _ = listener.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
}

func sessionContextMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, withSessionFromRequest(r))
	}
}

// buildMux constructs the HTTP mux with all route registrations.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", sessionContextMiddleware(logging.Middleware(chatCompletionsHandler)))
	mux.HandleFunc("/v1/responses", sessionContextMiddleware(logging.Middleware(responsesHandler)))
	mux.HandleFunc("/v1/messages", sessionContextMiddleware(logging.Middleware(claudeMessagesHandler)))
	mux.HandleFunc("/v1/messages/count_tokens", sessionContextMiddleware(logging.Middleware(claudeCountTokensHandler)))
	mux.HandleFunc("/v1/models", sessionContextMiddleware(logging.Middleware(listModelsHandler)))
	mux.HandleFunc("/login", logging.Middleware(loginHandler))
	mux.HandleFunc("/logout", logging.Middleware(logoutHandler))
	mux.HandleFunc("/api/config", logging.Middleware(requireAuth(adminConfigHandler)))
	mux.HandleFunc("/api/stats", logging.Middleware(requireAuth(adminStatsHandler)))
	mux.HandleFunc("/api/reload", logging.Middleware(requireAuth(reloadHandler)))
	mux.HandleFunc("/health", logging.Middleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	mux.HandleFunc("/", logging.Middleware(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			requireAuth(adminPageHandler)(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	return mux
}

// initProxyCore loads config, applies it, saves config, loads token stats,
// initializes the OpenCode session, fetches upstream model catalogs, and
// starts the background model refresher. Used by normal server mode.
func initProxyCore() {
	initProxyCoreWithSave(true)
}

// initProxyCoreReadOnly is the launch-mode variant of initProxyCore. It loads
// and applies config.json exactly once, but never writes it back, so launching
// claude or codex cannot mutate the user's persistent proxy configuration.
func initProxyCoreReadOnly() {
	initProxyCoreWithSave(false)
}

func initProxyCoreWithSave(save bool) {
	cfg := loadConfig(configPath)
	applyConfig(cfg)
	if save {
		if err := saveConfig(configPath, cfg); err != nil {
			slog.Warn("failed to save config", "path", configPath, "error", err)
		}
	}

	stats.LoadTokenStats()
	slog.Info("config loaded", "path", configPath)
	initOCSession()
	models, err := fetchModels()
	if err != nil {
		slog.Warn("failed to fetch models on startup", "error", err)
	} else {
		modelMu.Lock()
		modelsCache = models
		modelsLoaded = true
		modelMu.Unlock()
		slog.Info("models loaded", "count", len(models))
	}

	goModels, goErr := fetchGoModels()
	if goErr != nil {
		slog.Warn("failed to fetch go catalog on startup", "error", goErr)
	} else {
		modelMu.Lock()
		goModelsCache = goModels
		modelMu.Unlock()
		slog.Info("go catalog loaded", "count", len(goModels))
	}

	modelsDevCat := modelsdev.GetCachedCatalog()
	if len(modelsDevCat) > 0 {
		slog.Info("models.dev catalog loaded", "count", len(modelsDevCat))
	}

	startModelRefresh()
}

// startServer binds the HTTP server to addr and starts serving. Returns the
// server, the listener (so callers can read the actual port when 0 is used),
// and any error. The server runs in a background goroutine; the caller is
// responsible for Shutdown or for detecting listen errors.
func startServer(addr string, mux *http.ServeMux) (*http.Server, net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	server := &http.Server{Handler: mux}
	go func() {
		slog.Info("listening", "addr", listener.Addr().String())
		if sErr := server.Serve(listener); sErr != nil && sErr != http.ErrServerClosed {
			slog.Error("server terminated", "error", sErr)
			os.Exit(1)
		}
	}()
	return server, listener, nil
}

// readJSONRequestBody enforces POST, extracts upstream auth, and reads the body
// capped at 10 MiB. On failure it writes the HTTP error and returns ok=false.
func readJSONRequestBody(w http.ResponseWriter, r *http.Request) (auth UpstreamAuth, body []byte, ok bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return auth, nil, false
	}
	defer r.Body.Close()
	auth = extractUpstreamAuth(r)
	var err error
	body, err = io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return auth, nil, false
	}
	return auth, body, true
}
