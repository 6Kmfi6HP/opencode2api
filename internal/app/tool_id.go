package app

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

// anthropicUnsafeToolUseIDCharRegex matches any character outside Anthropic's
// required tool_use/tool_result id charset (^[a-zA-Z0-9_-]+$). Some upstream
// providers (e.g. Kimi/Gemini-compatible backends) emit ids containing ':'
// or '.', which Anthropic's API rejects with a 400 when such a conversation
// is replayed through the Anthropic protocol.
var anthropicUnsafeToolUseIDCharRegex = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// maxSanitizedToolUseIDLen bounds the sanitized id length, matching the
// 64-char cap this codebase applies to tool identifiers elsewhere, so a long
// non-conforming id can't sanitize into something Anthropic still rejects
// for length.
const maxSanitizedToolUseIDLen = 64

// sanitizeToolUseID rewrites a tool_use/tool_result id to satisfy Anthropic's
// ^[a-zA-Z0-9_-]+$ requirement. The mapping is deterministic (FNV-1a hash of
// the original id), so the same id sanitizes to the same value everywhere it
// appears: an assistant tool_calls[].id and the matching tool message
// tool_call_id keep pairing after the rewrite, regardless of which converter
// touched them first. The semantic part keeps the id human-readable for
// debugging; the 64-bit hash prefix keeps distinct ids distinct.
//
// Converted ids are only produced when the original id violates the charset
// (or is empty, which the pattern also rejects) — well-formed ids pass
// through byte-identical, so ordinary OpenAI "call_…" and Anthropic
// "toolu_…" ids see no change at all.
func sanitizeToolUseID(id string) string {
	if id != "" && !anthropicUnsafeToolUseIDCharRegex.MatchString(id) {
		return id
	}
	// Use the full 64-bit hash (not a truncation) to keep collisions between
	// distinct ids astronomically unlikely, since two tool_use blocks sharing
	// an id would make Anthropic's replies ambiguous or rejected.
	h := fnv.New64a()
	h.Write([]byte(id))
	hash := fmt.Sprintf("%016x", h.Sum64())
	semantic := strings.Trim(anthropicUnsafeToolUseIDCharRegex.ReplaceAllString(id, "_"), "_")
	if semantic == "" {
		return hash
	}
	if maxSemanticLen := maxSanitizedToolUseIDLen - len(hash) - 1; len(semantic) > maxSemanticLen {
		semantic = strings.Trim(semantic[:maxSemanticLen], "_")
	}
	if semantic == "" {
		return hash
	}
	return hash + "_" + semantic
}
