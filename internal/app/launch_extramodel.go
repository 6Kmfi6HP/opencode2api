package app

import "strings"

// extractModelFromExtraArgs scans the extra (post-`--`) argument list for a
// --model flag in either "--model <value>" or "--model=<value>" form. When
// found, the model value is returned and the matching argument(s) are removed
// from the slice in place. This lets users put --model after `--` alongside
// passthrough flags like --dangerously-skip-permissions:
//
//	opencode2api launch claude -- --dangerously-skip-permissions --model x-preview-f
//
// Without this, the --model flag after `--` would be forwarded verbatim to
// claude, and opencode2api would show the interactive TUI selector because
// its own -model flag is empty.
func extractModelFromExtraArgs(args []string) (model string, cleaned []string) {
	cleaned = args[:0:0] // new backing array
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--model" {
			if i+1 < len(args) {
				model = strings.TrimSpace(args[i+1])
				i++ // skip the value
			}
			continue
		}
		if strings.HasPrefix(arg, "--model=") {
			model = strings.TrimSpace(strings.TrimPrefix(arg, "--model="))
			continue
		}
		// Also handle single-dash form "-model" / "-model=".
		if arg == "-model" {
			if i+1 < len(args) {
				model = strings.TrimSpace(args[i+1])
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-model=") {
			model = strings.TrimSpace(strings.TrimPrefix(arg, "-model="))
			continue
		}
		cleaned = append(cleaned, arg)
	}
	return model, cleaned
}
