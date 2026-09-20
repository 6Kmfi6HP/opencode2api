package bridge

import (
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/random"
)

// IDGen generates a random identifier body of n characters, given a prefix for
// context. The prefix is informational; the returned string contains only the n
// random characters and does not include the prefix.
type IDGen func(prefix string, n int) string

// NowFn returns the current Unix timestamp in seconds. It is injected so bridge
// logic that stamps responses/created fields stays deterministic under test.
type NowFn func() int64

// DefaultIDGen is the production IDGen: n random lowercase letters/digits from
// internal/random (the same alphabet the previous app-layer randomString used).
func DefaultIDGen(prefix string, n int) string {
	// The prefix is intentionally unused: it exists so implementations may salt
	// or domain-separate the randomness if ever required. The production path
	// keeps the original behavior of an un-prefixed random body.
	_ = prefix
	return random.String(n)
}

// HexIDGen generates a random identifier body of n hexadecimal characters,
// given a prefix for context. It matches the previous app-layer randomHex
// alphabet (0-9a-f) so reopened tool-call IDs keep the same shape upstream.
type HexIDGen func(prefix string, n int) string

// DefaultHexIDGen is the production HexIDGen: n random hex characters from
// internal/random (the same alphabet the previous app-layer randomHex used).
func DefaultHexIDGen(prefix string, n int) string {
	_ = prefix
	return random.Hex(n)
}

// DefaultNowFn is the production NowFn: the wall clock.
func DefaultNowFn() int64 { return time.Now().Unix() }
