//go:build !windows

package app

import (
	"io"
	"os"
	"os/exec"
)

// newLaunchChildCommand creates the child CLI command on Unix-likes.
func newLaunchChildCommand(path string, args []string) *exec.Cmd {
	return exec.Command(path, args...)
}

// signalLaunchChild forwards an interrupt/SIGTERM signal to the child process.
func signalLaunchChild(proc *os.Process, sig os.Signal) {
	_ = proc.Signal(sig)
}

// selectModelInteractive is the platform selection entry point. Unix-likes
// keep the existing terminal raw-mode selector.
func selectModelInteractive(in io.Reader, out io.Writer, errOut io.Writer, modelIDs []string, catalog modelsDevCatalog) (string, error) {
	return selectModelTTY(modelIDs, catalog)
}

// launchDefaultLogFile returns the historical Unix log path. The behavior of
// macOS, Linux, and FreeBSD must remain unchanged.
func launchDefaultLogFile() string {
	return "opencode2api.log"
}
