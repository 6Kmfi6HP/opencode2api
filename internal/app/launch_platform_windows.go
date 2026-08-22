//go:build windows

package app

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// newLaunchChildCommand creates the child CLI command on Windows.
//
// npm and other Windows package managers install CLI shims as .cmd/.bat
// files; those must be executed through cmd.exe. Native .exe files start
// directly. Every child is placed in its own process group so Ctrl+C/Break
// can target the whole child tree.
func newLaunchChildCommand(path string, args []string) *exec.Cmd {
	argv := windowsChildCommandArgs(path, args)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd
}

// signalLaunchChild forwards an interrupt/SIGTERM signal to the child tree.
// It prefers GenerateConsoleCtrlEvent with CTRL_BREAK_EVENT, then falls back
// to taskkill for the whole tree if console control fails.
func signalLaunchChild(proc *os.Process, sig os.Signal) {
	if proc == nil {
		return
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(proc.Pid)); err == nil {
		return
	}
	taskKillErr := windowsTaskkillTree(proc.Pid)
	if taskKillErr == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "opencode2api: warning: failed to signal child process tree: %v\n", taskKillErr)
}

// windowsTaskkillTree kills the full child process tree and hides the
// taskkill console window.
func windowsTaskkillTree(pid int) error {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	return cmd.Run()
}

// selectModelInteractive presents the Windows numbered model selector.
func selectModelInteractive(in io.Reader, out io.Writer, errOut io.Writer, modelIDs []string, catalog modelsDevCatalog) (string, error) {
	entries := modelSelectionEntries(modelIDs, catalog)
	if len(entries) == 0 {
		fmt.Fprintln(errOut, "opencode2api: no free launch models available; using CLI defaults")
		return "", nil
	}
	return selectModelNumbered(in, out, errOut, entries)
}

// launchDefaultLogFile returns the Windows log path in the user cache
// directory, with a temp-directory fallback.
func launchDefaultLogFile() string {
	return windowsLaunchDefaultLogFile()
}
