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
