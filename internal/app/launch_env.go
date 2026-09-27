package app

import "strings"

// claudeInheritedAuthEnv lists authentication variables that Claude Code may
// prefer over ANTHROPIC_API_KEY. launch owns the child's network credentials,
// so inherited values must not survive when the launcher supplies `public`.
var claudeInheritedAuthEnv = []string{
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_API_KEY",
}

// splitEnvEntry splits an environment entry on its first '='. Entries without
// '=' are ignored; an empty value is valid.
func splitEnvEntry(entry string) (name, value string, ok bool) {
	if idx := strings.IndexByte(entry, '='); idx >= 0 {
		return entry[:idx], entry[idx+1:], true
	}
	return "", "", false
}

// buildChildProcessEnv merges inherited and launcher-owned environment entries
// with last-value-wins semantics. Without this, appending extraEnv after
// os.Environ can leave duplicate ANTHROPIC_API_KEY entries; execve permits
// duplicates, and Claude Code may select an inherited stale key instead of the
// launcher's public key. For claude, inherited auth alternatives that can take
// precedence are removed unless the launcher explicitly supplies them.
func buildChildProcessEnv(tool string, inherited, extraEnv []string) []string {
	overrides := make(map[string]string)
	overrideOrder := make([]string, 0, len(extraEnv))
	for _, entry := range extraEnv {
		name, value, ok := splitEnvEntry(entry)
		if !ok {
			continue
		}
		if _, exists := overrides[name]; !exists {
			overrideOrder = append(overrideOrder, name)
		}
		overrides[name] = value
	}

	drop := make(map[string]struct{})
	if tool == "claude" {
		for _, name := range claudeInheritedAuthEnv {
			if _, explicitlySet := overrides[name]; !explicitlySet {
				drop[name] = struct{}{}
			}
		}
	}

	env := make([]string, 0, len(inherited)+len(overrideOrder))
	for _, entry := range inherited {
		name, _, ok := splitEnvEntry(entry)
		if !ok {
			continue
		}
		if _, isOverride := overrides[name]; isOverride {
			continue
		}
		if _, dropName := drop[name]; dropName {
			continue
		}
		env = append(env, entry)
	}
	for _, name := range overrideOrder {
		env = append(env, name+"="+overrides[name])
	}
	return env
}
