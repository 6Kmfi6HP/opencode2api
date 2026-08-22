package app

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWindowsChildCommandArgs(t *testing.T) {
	tests := []struct {
		name string
		path string
		args []string
		want []string
	}{
		{
			name: "cmd shim",
			path: `C:\Users\me\AppData\Roaming\npm\claude.cmd`,
			args: []string{"--dangerously-skip-permissions"},
			want: []string{"cmd.exe", "/C", `C:\Users\me\AppData\Roaming\npm\claude.cmd`, "--dangerously-skip-permissions"},
		},
		{
			name: "bat shim",
			path: `C:\tools\claude.BAT`,
			args: []string{"-p", "hi"},
			want: []string{"cmd.exe", "/C", `C:\tools\claude.BAT`, "-p", "hi"},
		},
		{
			name: "native exe",
			path: `C:\Program Files\Claude\claude.exe`,
			args: []string{"--version"},
			want: []string{`C:\Program Files\Claude\claude.exe`, "--version"},
		},
		{
			name: "unix-like path with exe",
			path: "/usr/local/bin/codex.exe",
			args: nil,
			want: []string{"/usr/local/bin/codex.exe"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := windowsChildCommandArgs(tc.path, tc.args)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("windowsChildCommandArgs(%q, %v) = %v, want %v", tc.path, tc.args, got, tc.want)
			}
		})
	}
}

func TestWindowsLaunchCandidatePathsClaude(t *testing.T) {
	got := windowsLaunchCandidatePaths(`C:\Users\me`, `C:\Users\me\AppData\Roaming`, "claude")
	want := []string{
		`C:\Users\me\.local\bin\claude.exe`,
		`C:\Users\me\.local\bin\claude.cmd`,
		`C:\Users\me\.claude\local\claude.exe`,
		`C:\Users\me\.claude\local\claude.cmd`,
		`C:\Users\me\AppData\Roaming\npm\claude.cmd`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("windows claude candidates = %v, want %v", got, want)
	}
}

func TestWindowsLaunchCandidatePathsCodex(t *testing.T) {
	got := windowsLaunchCandidatePaths(`C:\Users\me`, `C:\Users\me\AppData\Roaming`, "codex")
	want := []string{
		`C:\Users\me\.local\bin\codex.exe`,
		`C:\Users\me\.local\bin\codex.cmd`,
		`C:\Users\me\.codex\bin\codex.exe`,
		`C:\Users\me\.codex\bin\codex.cmd`,
		`C:\Users\me\AppData\Roaming\npm\codex.cmd`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("windows codex candidates = %v, want %v", got, want)
	}
}

func TestWindowsLaunchDefaultLogFileSuffix(t *testing.T) {
	got := windowsLaunchDefaultLogFile()
	wantSuffix := filepath.Join("opencode2api", "logs", "opencode2api.log")
	if !strings.HasSuffix(filepath.Clean(got), wantSuffix) {
		t.Fatalf("windowsLaunchDefaultLogFile() = %q, want suffix %q", got, wantSuffix)
	}
}

func TestSelectModelNumberedNormal(t *testing.T) {
	entries := []launchModelSelectionEntry{
		{ID: "deepseek-v4-flash", ContextWindow: 1048576},
		{ID: "mimo-v2.5", ContextWindow: 200000},
	}
	in := strings.NewReader("2\n")
	var out, errOut bytes.Buffer
	got, err := selectModelNumbered(in, &out, &errOut, entries)
	if err != nil {
		t.Fatalf("selectModelNumbered error = %v", err)
	}
	if got != "mimo-v2.5" {
		t.Fatalf("selected = %q, want mimo-v2.5", got)
	}
	if !strings.Contains(out.String(), "[2] mimo-v2.5") {
		t.Fatalf("output missing numbered entry: %q", out.String())
	}
}

func TestSelectModelNumberedInvalidThenValid(t *testing.T) {
	entries := []launchModelSelectionEntry{{ID: "one"}, {ID: "two"}}
	in := strings.NewReader("0\nnope\n1\n")
	var out, errOut bytes.Buffer
	got, err := selectModelNumbered(in, &out, &errOut, entries)
	if err != nil {
		t.Fatalf("selectModelNumbered error = %v", err)
	}
	if got != "one" {
		t.Fatalf("selected = %q, want one", got)
	}
	if !strings.Contains(errOut.String(), "invalid selection") {
		t.Fatalf("missing invalid selection warning: %q", errOut.String())
	}
}

func TestSelectModelNumberedEmptyInput(t *testing.T) {
	entries := []launchModelSelectionEntry{{ID: "one"}}
	in := strings.NewReader("\n")
	var out, errOut bytes.Buffer
	got, err := selectModelNumbered(in, &out, &errOut, entries)
	if err != nil {
		t.Fatalf("selectModelNumbered error = %v", err)
	}
	if got != "" {
		t.Fatalf("selected = %q, want empty", got)
	}
	if !strings.Contains(errOut.String(), "no model selected") {
		t.Fatalf("missing empty warning: %q", errOut.String())
	}
}

func TestSelectModelNumberedEOF(t *testing.T) {
	entries := []launchModelSelectionEntry{{ID: "one"}}
	in := strings.NewReader("")
	var out, errOut bytes.Buffer
	got, err := selectModelNumbered(in, &out, &errOut, entries)
	if err != nil {
		t.Fatalf("selectModelNumbered error = %v", err)
	}
	if got != "" {
		t.Fatalf("selected = %q, want empty", got)
	}
	if !strings.Contains(errOut.String(), "no model selected") {
		t.Fatalf("missing EOF warning: %q", errOut.String())
	}
}

func TestModelSelectionEntriesFiltersAndSorts(t *testing.T) {
	catalog := modelsDevCatalog{
		"model-a-free": 1200000,
		"model-a":      1200000,
		"model-b-free": 200000,
		"model-b":      200000,
		"paid-only":    400000,
		"model-c":      800000,
		"model-c-free": 800000,
	}
	entries := modelSelectionEntries([]string{
		"model-b",
		"model-b-free",
		"paid-only",
		"model-a-free",
		"model-c",
		"model-c-free",
	}, catalog)

	var ids []string
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	want := []string{"model-a", "model-c", "model-b"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("model selection IDs = %v, want %v", ids, want)
	}
	if len(entries) < 2 || entries[0].ContextWindow <= entries[1].ContextWindow {
		t.Fatalf("entries not sorted by context descending: %#v", entries)
	}
}

func TestLaunchFlagParsingLogFile(t *testing.T) {
	f := newLaunchFlagSet("claude", []string{"--log-file", "windows-launch.log"})
	if f.logFile != "windows-launch.log" {
		t.Fatalf("logFile flag = %q, want windows-launch.log", f.logFile)
	}
}
