package app

import "strings"

// extractModelFlagFromExtraArgs scans the extra (post-`--`) argument list for
// a model flag in either "<flag> <value>" or "<flag>=<value>" form. When
// found, the model value is returned and the matching argument(s) are removed
// from the slice in place (a new backing array is used).
func extractModelFlagFromExtraArgs(args []string, flags ...string) (model string, cleaned []string) {
	cleaned = args[:0:0] // new backing array
	for i := 0; i < len(args); i++ {
		arg := args[i]
		matched := false
		for _, flag := range flags {
			if arg == flag {
				if i+1 < len(args) {
					model = strings.TrimSpace(args[i+1])
					i++ // skip the value
				}
				matched = true
				break
			}
			if strings.HasPrefix(arg, flag+"=") {
				model = strings.TrimSpace(strings.TrimPrefix(arg, flag+"="))
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		cleaned = append(cleaned, arg)
	}
	return model, cleaned
}

// extractModelFromExtraArgs recognizes the claude model spellings:
// `--model <value>`, `--model=<value>`, `-model <value>`, `-model=<value>`.
//
//	opencode2api launch claude -- --dangerously-skip-permissions --model x-preview-f
//
// Without this, the --model flag after `--` would be forwarded verbatim to
// claude, and opencode2api would show the interactive TUI selector because
// its own -model flag is empty.
func extractModelFromExtraArgs(args []string) (model string, cleaned []string) {
	return extractModelFlagFromExtraArgs(args, "--model", "-model")
}

// extractCodexModelFromExtraArgs recognizes the Codex model spellings:
// `--model <value>`, `--model=<value>`, `-m <value>`, and `-m=<value>`.
func extractCodexModelFromExtraArgs(args []string) (model string, cleaned []string) {
	return extractModelFlagFromExtraArgs(args, "--model", "-m")
}
