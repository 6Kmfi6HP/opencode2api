package app

import (
	"path/filepath"
	"strings"
)

// windowsPathJoin joins two Windows path components with a backslash. It is
// intentionally platform-independent so candidate-path tests can run on any
// host without depending on the runner's OS separator.
func windowsPathJoin(base, leaf string) string {
	base = strings.TrimRight(base, `\/`)
	leaf = strings.TrimLeft(leaf, `\/`)
	if base == "" {
		return leaf
	}
	return base + "\\" + leaf
}

// windowsChildCommandArgs composes the full argv used to start a Windows CLI.
// child shim. cmd.exe is used for .cmd/.bat shims; native executables start
// directly.
func windowsChildCommandArgs(path string, args []string) []string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".cmd" || ext == ".bat" {
		argv := []string{"cmd.exe", "/C", path}
		return append(argv, args...)
	}
	argv := []string{path}
	return append(argv, args...)
}

// windowsLaunchCandidatePaths returns common Windows CLI install candidates for
// claude/codex. It supplements PATH lookup with the most common node/npm and
// MSI/installer locations, including .exe/.cmd variants.
func windowsLaunchCandidatePaths(home, appdata, tool string) []string {
	var candidates []string
	add := func(dir string, exts ...string) {
		if dir == "" {
			return
		}
		for _, ext := range exts {
			candidates = append(candidates, windowsPathJoin(dir, tool+ext))
		}
	}

	// Common user-local binary directories.
	add(windowsPathJoin(home, ".local\\bin"), ".exe", ".cmd")
	switch tool {
	case "claude":
		add(windowsPathJoin(home, ".claude\\local"), ".exe", ".cmd")
	case "codex":
		add(windowsPathJoin(home, ".codex\\bin"), ".exe", ".cmd")
	}

	// npm global shim installed through AppData\npm.
	if appdata != "" {
		candidates = append(candidates, windowsPathJoin(appdata, "npm\\"+tool+".cmd"))
	}
	return candidates
}
