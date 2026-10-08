package app

import (
	"regexp"
	"strings"
	"testing"
)

// TestSanitizeToolUseID_PassesCleanIDsThrough verifies well-formed ids are
// byte-identical: ordinary OpenAI "call_…" and Anthropic "toolu_…" ids must
// see no rewrite at all, so upstream-generated ids round-trip unchanged.
func TestSanitizeToolUseID_PassesCleanIDsThrough(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"openai call id", "call_abc123"},
		{"anthropic toolu id", "toolu_01ABCdefGHI"},
		{"all allowed chars", "A-Za-z0-9_-"},
		{"single char", "x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeToolUseID(tc.id); got != tc.id {
				t.Fatalf("sanitizeToolUseID(%q) = %q, want unchanged", tc.id, got)
			}
		})
	}
}

// TestSanitizeToolUseID_RewritesUnsafeIDs verifies ids carrying characters
// outside Anthropic's ^[a-zA-Z0-9_-]+$ requirement are rewritten: the
// semantic part keeps the id readable, and the result always satisfies the
// charset (Anthropic 400s on ids like "functions.x:0" or "a.b").
func TestSanitizeToolUseID_RewritesUnsafeIDs(t *testing.T) {
	anthropicIDPattern := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	tests := []struct {
		name            string
		id              string
		wantSemanticEnd string // 语义保留：清洗值以原 id 的下划线化安全序列收尾
	}{
		{"kimi/gemini function id", "functions.x:0", "functions_x_0"},
		{"dotted id", "a.b", "a_b"},
		{"colon and slash", "ns/tool:1", "ns_tool_1"},
		{"unicode", "工具-1", "-1"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeToolUseID(tc.id)
			if got == "" {
				t.Fatal("sanitized id is empty; Anthropic requires at least one char")
			}
			if !anthropicIDPattern.MatchString(got) {
				t.Fatalf("sanitizeToolUseID(%q) = %q, violates Anthropic charset", tc.id, got)
			}
			if got == tc.id {
				t.Fatalf("sanitizeToolUseID(%q) = %q, expected a rewrite", tc.id, got)
			}
			if tc.wantSemanticEnd != "" && !strings.HasSuffix(got, tc.wantSemanticEnd) {
				t.Fatalf("sanitized id %q lost the semantic part of %q (want suffix %q)", got, tc.id, tc.wantSemanticEnd)
			}
		})
	}
}

// TestSanitizeToolUseID_DeterministicForPairing verifies the mapping is
// deterministic: the same id sanitizes to the same value on every call, so an
// assistant tool_calls[].id and the matching tool message tool_call_id keep
// pairing after the rewrite.
func TestSanitizeToolUseID_DeterministicForPairing(t *testing.T) {
	ids := []string{"functions.x:0", "a.b", "call_1", "", "ns/tool:1"}
	for _, id := range ids {
		first := sanitizeToolUseID(id)
		for i := 0; i < 3; i++ {
			if got := sanitizeToolUseID(id); got != first {
				t.Fatalf("sanitizeToolUseID(%q) unstable: %q vs %q", id, got, first)
			}
		}
	}

	// 配对场景：assistant tool_calls 的 id 与后续 tool 消息的 tool_call_id
	// 是同一原始值，清洗后必须仍是同一值。
	original := "functions.x:0"
	toolUseSide := sanitizeToolUseID(original)
	toolResultSide := sanitizeToolUseID(original)
	if toolUseSide != toolResultSide {
		t.Fatalf("pairing broken: %q != %q", toolUseSide, toolResultSide)
	}
}

// TestSanitizeToolUseID_DistinctIDsStayDistinct verifies distinct ids map to
// distinct values (two tool_use blocks sharing a sanitized id would make
// Anthropic's replies ambiguous or rejected).
func TestSanitizeToolUseID_DistinctIDsStayDistinct(t *testing.T) {
	ids := []string{"functions.x:0", "functions.x:1", "functions.y:0", "a.b", "call_1", "call_2"}
	seen := map[string]string{}
	for _, id := range ids {
		got := sanitizeToolUseID(id)
		if prev, dup := seen[got]; dup {
			t.Fatalf("collision: %q and %q both sanitize to %q", prev, id, got)
		}
		seen[got] = id
	}
}

// TestSanitizeToolUseID_LengthCap verifies a long non-conforming id can't
// sanitize into something Anthropic still rejects for length (64-char cap).
func TestSanitizeToolUseID_LengthCap(t *testing.T) {
	long := strings.Repeat("x.:", 100)
	got := sanitizeToolUseID(long)
	if len(got) > maxSanitizedToolUseIDLen {
		t.Fatalf("sanitized length = %d, want <= %d", len(got), maxSanitizedToolUseIDLen)
	}
	// 合法但超长的 id 原样透传（清洗只针对字符集）。
	cleanLong := strings.Repeat("a", 80)
	if got := sanitizeToolUseID(cleanLong); got != cleanLong {
		t.Fatalf("clean long id rewritten: %q", got)
	}
}
