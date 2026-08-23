package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	flag.StringVar(&logLevel, "log-level", "info", "日志级别: debug/info/warn/error")
	flag.StringVar(&logFile, "log-file", "opencode2api.log", "日志文件路径")
	flag.BoolVar(&logStdout, "log-stdout", true, "是否同时写 stdout")
	flag.IntVar(&logMaxSize, "log-max-size", 100, "单日志文件最大 MB，超过即轮换")
	flag.IntVar(&logMaxBackups, "log-max-backups", 7, "保留的旧日志文件个数")
	flag.IntVar(&logMaxAge, "log-max-age", 14, "旧日志保留天数")
	flag.BoolVar(&logCompress, "log-compress", true, "轮换后 gzip 压缩")
	flag.BoolVar(&logBodies, "log-bodies", false, "Debug 下记录截断的 body 摘要")
	flag.BoolVar(&showVersion, "version", false, "显示版本信息")
	flag.Parse()

	configExplicit := flagSet("config")
	configPath, _ = resolveConfigPath(configPath, configExplicit)
	logFile, _ = resolveLogFilePath(logFile, flagSet("log-file"), configPath, configExplicit)
	resolvedStats, _ := resolveStatsPath(statsFile, flagSet("stats-file"), configPath, configExplicit)
	setTokenStatsPath(resolvedStats)

	initLogger()
	defer closeLogRotator()

	if showVersion {
		fmt.Println(versionString())
		return
	}

	initProxyCore()

	slog.Info("server starting",
		"port", port,
		"log_level", getLogLevelString(),
		"models", len(getModelIDs()),
		"aliases", len(modelAlias),
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

// buildMux constructs the HTTP mux with all route registrations.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", loggingMiddleware(chatCompletionsHandler))
	mux.HandleFunc("/v1/responses", loggingMiddleware(responsesHandler))
	mux.HandleFunc("/v1/messages", loggingMiddleware(claudeMessagesHandler))
	mux.HandleFunc("/v1/messages/count_tokens", loggingMiddleware(claudeCountTokensHandler))
	mux.HandleFunc("/v1/models", loggingMiddleware(listModelsHandler))
	mux.HandleFunc("/login", loggingMiddleware(loginHandler))
	mux.HandleFunc("/logout", loggingMiddleware(logoutHandler))
	mux.HandleFunc("/api/config", loggingMiddleware(requireAuth(adminConfigHandler)))
	mux.HandleFunc("/api/stats", loggingMiddleware(requireAuth(adminStatsHandler)))
	mux.HandleFunc("/api/reload", loggingMiddleware(requireAuth(reloadHandler)))
	mux.HandleFunc("/health", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	mux.HandleFunc("/", loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
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

	loadTokenStats()
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
