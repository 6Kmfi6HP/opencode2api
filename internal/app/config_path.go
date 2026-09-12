package app

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/6Kmfi6HP/opencode2api/internal/logging"
	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
	"github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// setTokenStatsPath is a thin delegate over stats.SetPath so existing app
// callers keep working after the stats domain moved into internal/stats.
func setTokenStatsPath(path string) { stats.SetPath(path) }

// getTokenStatsPath is a thin delegate over stats.GetPath so existing app
// callers keep working after the stats domain moved into internal/stats.
func getTokenStatsPath() string { return stats.GetPath() }

// activeLogFile is the resolved log file path handed to logging.Init. It is
// package state because the launch status line reports it after the path
// left the logging package.
var activeLogFile string

// resolvedLogPath returns the absolute path of the active log file, or
// "(stdout)" when no file is configured. Used for user-facing status lines.
func resolvedLogPath() string {
	if activeLogFile == "" {
		return "(stdout)"
	}
	abs, err := filepath.Abs(activeLogFile)
	if err != nil {
		return activeLogFile
	}
	return abs
}

// resolveConfigPath returns the effective config file path and reports whether
// the path came from the user-level fallback.
//
// Precedence:
//  1. OPENCODE2API_CONFIG
//  2. an explicitly supplied -config value
//  3. an existing ./config.json (for backward compatibility)
//  4. <os.UserConfigDir()>/opencode2api/config.json
//
// The final value is the historical ./config.json path when the user
// configuration directory cannot be determined.
func resolveConfigPath(flagValue string, flagExplicit bool) (string, bool) {
	if envPath := strings.TrimSpace(os.Getenv("OPENCODE2API_CONFIG")); envPath != "" {
		return envPath, false
	}
	if flagValue = strings.TrimSpace(flagValue); flagExplicit && flagValue != "" {
		return flagValue, false
	}
	const localPath = "config.json"
	if _, err := os.Stat(localPath); err == nil {
		return localPath, false
	}

	if userDir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(userDir) != "" {
		return filepath.Join(userDir, "opencode2api", localPath), true
	}
	return localPath, false
}

// resolveRelatedFilePath is a shared helper for resolving auxiliary files (such
// as stats.json and opencode2api.log) that follow the same configuration fallback
// rules.
func resolveRelatedFilePath(envKeys []string, flagValue string, flagExplicit bool, configPath string, configExplicit bool, localFileName string) (string, bool) {
	for _, envKey := range envKeys {
		if envPath := strings.TrimSpace(os.Getenv(envKey)); envPath != "" {
			return envPath, false
		}
	}
	if flagValue = strings.TrimSpace(flagValue); flagExplicit && flagValue != "" {
		return flagValue, false
	}

	configPath = strings.TrimSpace(configPath)
	if configExplicit && configPath != "" {
		return filepath.Join(filepath.Dir(configPath), localFileName), true
	}

	// Without an explicit config, honor files left by older deployments in
	// the current directory before selecting the newer fallback location.
	if _, err := os.Stat(localFileName); err == nil {
		return localFileName, false
	}
	if configPath != "" {
		return filepath.Join(filepath.Dir(configPath), localFileName), true
	}

	if userDir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(userDir) != "" {
		return filepath.Join(userDir, "opencode2api", localFileName), true
	}
	return localFileName, false
}

// resolveStatsPath returns the effective stats file path and reports whether
// the path came from a configuration/user-directory fallback.
//
// Precedence:
//  1. OPENCODE2API_STATS or OPENCODE2API_STATS_FILE
//  2. an explicitly supplied -stats-file value
//  3. <dir(configPath)>/stats.json when -config was explicit
//  4. an existing ./stats.json (for backward compatibility), then the config
//     fallback directory, normally <os.UserConfigDir()>/opencode2api
func resolveStatsPath(flagValue string, flagExplicit bool, configPath string, configExplicit bool) (string, bool) {
	return resolveRelatedFilePath(
		[]string{"OPENCODE2API_STATS", "OPENCODE2API_STATS_FILE"},
		flagValue,
		flagExplicit,
		configPath,
		configExplicit,
		"stats.json",
	)
}

// resolveLogFilePath returns the effective log file path and reports whether
// the path came from a configuration/user-directory fallback.
//
// Precedence:
//  1. OPENCODE2API_LOG_FILE
//  2. an explicitly supplied -log-file value
//  3. <dir(configPath)>/opencode2api.log when -config was explicit
//  4. an existing ./opencode2api.log (for backward compatibility), then the
//     config fallback directory, normally <os.UserConfigDir()>/opencode2api
func resolveLogFilePath(flagValue string, flagExplicit bool, configPath string, configExplicit bool) (string, bool) {
	return resolveRelatedFilePath(
		[]string{"OPENCODE2API_LOG_FILE"},
		flagValue,
		flagExplicit,
		configPath,
		configExplicit,
		"opencode2api.log",
	)
}

// resolveModelsDevCachePath returns the effective models.dev cache file path and reports whether
// the path came from a configuration/user-directory fallback.
//
// Precedence:
//  1. OPENCODE2API_MODELSDEV_CACHE or OPENCODE2API_MODELSDEV_CACHE_FILE
//  2. <dir(configPath)>/modelsdev_cache.json when -config was explicit
//  3. an existing ./modelsdev_cache.json (for backward compatibility), then the
//     config fallback directory, normally <os.UserConfigDir()>/opencode2api
func resolveModelsDevCachePath(configPath string, configExplicit bool) (string, bool) {
	return resolveRelatedFilePath(
		[]string{"OPENCODE2API_MODELSDEV_CACHE", "OPENCODE2API_MODELSDEV_CACHE_FILE"},
		"",
		false,
		configPath,
		configExplicit,
		"modelsdev_cache.json",
	)
}

// resolveAndInitRuntime resolves all runtime paths derived from the config
// (log file, stats file, models.dev cache), records the resolved log path in
// activeLogFile, and initializes logging with the given log file flag value,
// level, and body-summary flag. Callers must have already finished flag/env
// resolution for configPath itself and set the logging rotation knobs
// (Stdout/MaxSize/MaxBackups/MaxAge/Compress) before calling.
//
// TODO: launch.go configureLaunchGlobals duplicates this tail; switch it over
// in a follow-up that owns launch.go.
func resolveAndInitRuntime(logFileFlag, level string, bodies bool, statsFlagValue string, statsExplicit, configExplicit bool) {
	configPath, _ = resolveConfigPath(configPath, configExplicit)
	activeLogFile, _ = resolveLogFilePath(logFileFlag, flagSet("log-file"), configPath, configExplicit)
	resolvedStats, _ := resolveStatsPath(statsFlagValue, statsExplicit, configPath, configExplicit)
	setTokenStatsPath(resolvedStats)
	resolvedModelsDevCache, _ := resolveModelsDevCachePath(configPath, configExplicit)
	modelsdev.SetCachePath(resolvedModelsDevCache)
	modelsdev.SetClientGetter(getHTTPClient)

	installLoggingHooks()
	logging.Init(activeLogFile, level, bodies)
}
