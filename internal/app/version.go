package app

import (
	"fmt"
	"runtime/debug"
	"strings"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		if (version == "dev" || version == "") && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "none" || commit == "" {
					if len(setting.Value) >= 12 {
						commit = setting.Value[:12]
					} else {
						commit = setting.Value
					}
				}
			case "vcs.time":
				if date == "unknown" || date == "" {
					date = setting.Value
				}
			case "vcs.modified":
				if setting.Value == "true" && commit != "none" && commit != "" && !strings.HasSuffix(commit, "-dirty") {
					commit += "-dirty"
				}
			}
		}
	}
}

func versionString() string {
	return fmt.Sprintf("opencode2api %s (commit=%s, date=%s)", version, commit, date)
}
