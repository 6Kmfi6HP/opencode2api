package app

import "testing"

func TestBuildChildProcessEnv_ClaudePublicLaunchDoesNotLeakInheritedAuth(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=stale-inherited-key",
		"ANTHROPIC_AUTH_TOKEN=stale-inherited-token",
		"ANTHROPIC_OAUTH_TOKEN=oauth-stale",
		"CLAUDE_CODE_OAUTH_TOKEN=oauth-stale",
		"CLAUDE_CODE_API_KEY=stale-claude-key",
		"KEEP_ME=1",
	}
	overrides := []string{
		"ANTHROPIC_BASE_URL=http://127.0.0.1:12345",
		"ANTHROPIC_API_KEY=public",
	}

	got := buildChildProcessEnv("claude", base, overrides)
	values := envValues(got)

	if got := values["ANTHROPIC_API_KEY"]; got != "public" {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want public", got)
	}
	for _, name := range []string{
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_API_KEY",
	} {
		if _, ok := values[name]; ok {
			t.Fatalf("inherited Claude auth variable %s leaked into child environment", name)
		}
	}
	if got := values["KEEP_ME"]; got != "1" {
		t.Fatalf("unrelated inherited variable KEEP_ME = %q, want 1", got)
	}
	if got := envCount(got, "ANTHROPIC_API_KEY"); got != 1 {
		t.Fatalf("ANTHROPIC_API_KEY appears %d times, want exactly once", got)
	}
}

func TestBuildChildProcessEnv_OverridesDuplicateValues(t *testing.T) {
	base := []string{
		"OPENCODE2API_OPENAI_API_KEY=stale",
		"UNCHANGED=base",
	}
	overrides := []string{
		"OPENCODE2API_OPENAI_API_KEY=public",
		"NEW=value",
	}

	got := buildChildProcessEnv("codex", base, overrides)
	values := envValues(got)
	if got := values["OPENCODE2API_OPENAI_API_KEY"]; got != "public" {
		t.Fatalf("OPENCODE2API_OPENAI_API_KEY = %q, want public", got)
	}
	if got := values["UNCHANGED"]; got != "base" {
		t.Fatalf("UNCHANGED = %q, want base", got)
	}
	if got := values["NEW"]; got != "value" {
		t.Fatalf("NEW = %q, want value", got)
	}
	if got := envCount(got, "OPENCODE2API_OPENAI_API_KEY"); got != 1 {
		t.Fatalf("OPENCODE2API_OPENAI_API_KEY appears %d times, want exactly once", got)
	}
}

func envValues(env []string) map[string]string {
	values := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, ok := splitEnvEntry(entry)
		if ok {
			values[name] = value
		}
	}
	return values
}

func envCount(env []string, want string) int {
	count := 0
	for _, entry := range env {
		name, _, ok := splitEnvEntry(entry)
		if ok && name == want {
			count++
		}
	}
	return count
}
