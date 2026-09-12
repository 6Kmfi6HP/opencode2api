package app

import (
	"flag"
	"os"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/logging"
)

// chdirTemp switches into a fresh working directory free of legacy
// config/stats/log files so Run's path resolution falls back predictably.
func chdirTemp(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
}

// resetRunGlobals isolates the flag/logging globals and the default FlagSet
// that Run mutates on each call.
func resetRunGlobals(t *testing.T) {
	t.Helper()

	origCfg := configPath
	origLevel := logging.LevelString()
	origStdout := logging.Stdout
	origDebug := debugMode
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine

	// Run registers flags into flag.CommandLine; replace it with a fresh set
	// so this test (and the next Run call) does not panic on re-registration.
	flag.CommandLine = flag.NewFlagSet(oldArgs[0], flag.ExitOnError)

	t.Cleanup(func() {
		flag.CommandLine = oldCommandLine
		configPath = origCfg
		logging.Stdout = origStdout
		debugMode = origDebug
		os.Args = oldArgs
		logging.SetLevelString(origLevel)
		logging.CloseRotator()
	})
}

func TestRun_DebugFlagAppliesDebugLevel(t *testing.T) {
	resetStatsAndLogTestState(t)
	resetRunGlobals(t)
	chdirTemp(t)

	os.Args = []string{"opencode2api", "-debug", "-version"}

	Run()

	if got := logging.LevelString(); got != "debug" {
		t.Fatalf("logging level = %q, want debug when -debug is passed without -log-level", got)
	}
}

func TestRun_ExplicitLogLevelBeatsDebugFlag(t *testing.T) {
	resetStatsAndLogTestState(t)
	resetRunGlobals(t)
	chdirTemp(t)

	os.Args = []string{"opencode2api", "-debug", "-log-level", "warn", "-version"}

	Run()

	if got := logging.LevelString(); got != "warn" {
		t.Fatalf("logging level = %q, want explicit -log-level=warn to win over -debug", got)
	}
}
