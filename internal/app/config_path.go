package app

import (
	"os"
	"path/filepath"
	"strings"
)

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
