package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/6Kmfi6HP/opencode2api/internal/util"
)

// JSONString serializes any value to a JSON string, falling back to "" on
// error. Used only for token estimation. (Moved from count_tokens.go.)
func JSONString(v any) string {
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
}

// NumberToInt leniently converts an any number to int (SSE JSON decoding
// mostly yields float64). (Moved from chat_to_anthropic.go.)
func NumberToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// FinishReasonOr returns nil for an empty finish reason and the reason
// otherwise, so the wire field is omitted (null) until a reason is known.
// (Moved from chat_to_anthropic.go.)
func FinishReasonOr(fr string) any {
	if fr == "" {
		return nil
	}
	return fr
}

// NumberAsFloat converts a JSON-decoded number (float64, int, int64) to
// float64, reporting whether the value was numeric. It is a thin forward to
// internal/util so bridge callers share one tolerant numeric reader.
func NumberAsFloat(v any) (float64, bool) { return util.NumberAsFloat(v) }

// ToString converts a value to string, returning "" for non-strings. Thin
// forward to internal/util.
func ToString(v any) string { return util.ToString(v) }

// ToFloat64 converts an any number to float64, returning 0 for non-numeric.
// (Moved from claude.go.)
func ToFloat64(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

// DeterministicResponseID returns id unchanged when it already carries prefix
// (and is longer than the prefix), gives an empty id a random suffix from
// idGen (callers should cache), and otherwise maps id onto a stable,
// prefix-isolated identifier via sha256[0:16] so a Claude client never sees a
// chatcmpl_/resp_ identifier leaked across protocols. (Moved from chat.go.)
//
// idGen(prefix, n) supplies the random suffix for empty ids; production wires
// it to internal/random equivalent (n random lowercase letters/digits).
func DeterministicResponseID(idGen func(prefix string, n int) string, prefix, id string) string {
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix) {
		return id
	}
	if id == "" {
		return prefix + idGen(prefix, 24)
	}
	h := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(h[:16])
}

// NormalizeChatResponseID ensures a Chat response ID has the chatcmpl- prefix.
func NormalizeChatResponseID(idGen func(prefix string, n int) string, id string) string {
	return DeterministicResponseID(idGen, "chatcmpl-", id)
}

// NormalizeResponsesID ensures a Responses response ID has the resp_ prefix.
func NormalizeResponsesID(idGen func(prefix string, n int) string, id string) string {
	return DeterministicResponseID(idGen, "resp_", id)
}

// NormalizeClaudeMessageID ensures a Claude message ID has the msg_ prefix.
func NormalizeClaudeMessageID(idGen func(prefix string, n int) string, id string) string {
	return DeterministicResponseID(idGen, "msg_", id)
}
