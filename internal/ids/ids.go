package ids

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/6Kmfi6HP/opencode2api/internal/random"
)

// Deterministic returns a stable ID for the given prefix and upstream ID.
func Deterministic(prefix, id string) string {
	if strings.HasPrefix(id, prefix) && len(id) > len(prefix) {
		return id
	}
	if id == "" {
		return prefix + random.String(24)
	}
	h := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(h[:16])
}

// NormalizeChatResponseID ensures a Chat response ID has the chatcmpl- prefix.
func NormalizeChatResponseID(id string) string { return Deterministic("chatcmpl-", id) }

// NormalizeResponsesID ensures a Responses response ID has the resp_ prefix.
func NormalizeResponsesID(id string) string { return Deterministic("resp_", id) }

// NormalizeClaudeMessageID ensures a Claude message ID has the msg_ prefix.
func NormalizeClaudeMessageID(id string) string { return Deterministic("msg_", id) }
