package app

import (
	"os"
	"path/filepath"
	"runtime"
)

// launchCandidatePaths returns the platform-specific fallback paths checked
// after exec.LookPath fails. Windows adds .exe/.cmd PATHEXT-style candidate
// variants for npm shims and MSI/installer locations.
func launchCandidatePaths(tool string) []string {
	home, err := os.UserHomeDir()
	if err == nil {
		if runtime.GOOS == "windows" {
			return windowsLaunchCandidatePaths(home, os.Getenv("APPDATA"), tool)
		}
		switch tool {
		case "claude":
			return []string{
				filepath.Join(home, ".local", "bin", tool),
				filepath.Join(home, ".claude", "local", tool),
			}
		case "codex":
			return []string{
				filepath.Join(home, ".local", "bin", tool),
				filepath.Join(home, ".codex", "bin", tool),
			}
		}
		return []string{filepath.Join(home, ".local", "bin", tool)}
	}
	if runtime.GOOS == "windows" {
		return windowsLaunchCandidatePaths("", os.Getenv("APPDATA"), tool)
	}
	return nil
}
