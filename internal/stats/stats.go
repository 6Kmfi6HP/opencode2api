package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/6Kmfi6HP/opencode2api/internal/util"
)

// ======================== Token 统计 ========================

type ModelStats struct {
	RequestCount     int64 `json:"request_count"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// CacheTokens aggregates upstream prompt-cache accounting: read = tokens
	// served from cache, created = tokens written into the cache. Both are
	// optional and only present when the upstream reports them.
	CacheReadTokens    int64 `json:"cache_read_tokens,omitempty"`
	CacheCreatedTokens int64 `json:"cache_created_tokens,omitempty"`
}

type TokenStatsData struct {
	TotalRequests int64                  `json:"total_requests"`
	Models        map[string]*ModelStats `json:"models"`
}

var (
	tokenStats     = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu   sync.Mutex
	tokenStatsPath = "stats.json"
)

// SetPath updates the file path used to persist token usage statistics.
func SetPath(path string) {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	if path != "" {
		tokenStatsPath = path
	}
}

// GetPath returns the currently configured token stats file path.
func GetPath() string {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	return tokenStatsPath
}

// ======================== Token 统计 ========================

// readTokenStatsFromDisk loads the persisted token stats, returning an empty
// stub when the file does not yet exist or reads as invalid JSON. Callers that
// only need a stale-but-fast view should use the in-memory snapshot instead.
func readTokenStatsFromDisk(path string) (*TokenStatsData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return &TokenStatsData{Models: map[string]*ModelStats{}}, nil
	}
	var st TokenStatsData
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.Models == nil {
		st.Models = map[string]*ModelStats{}
	}
	return &st, nil
}

func cloneTokenStatsSnapshot() *TokenStatsData {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	snap := &TokenStatsData{
		TotalRequests: tokenStats.TotalRequests,
		Models:        map[string]*ModelStats{},
	}
	for k, v := range tokenStats.Models {
		cp := *v
		snap.Models[k] = &cp
	}
	return snap
}

func replaceTokenStatsSnapshot(snap *TokenStatsData) {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	if snap == nil {
		snap = &TokenStatsData{Models: map[string]*ModelStats{}}
	}
	if snap.Models == nil {
		snap.Models = map[string]*ModelStats{}
	}
	tokenStats = snap
}

// Snapshot returns the current in-memory stats snapshot. Intended for the app
// package (and its tests) to seed or restore per-test state.
func Snapshot() *TokenStatsData {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	return tokenStats
}

// SetSnapshot replaces the in-memory stats snapshot and returns the previous
// one. Intended for the app package to restore test state.
func SetSnapshot(snap *TokenStatsData) *TokenStatsData {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	previous := tokenStats
	if snap == nil {
		snap = &TokenStatsData{Models: map[string]*ModelStats{}}
	}
	if snap.Models == nil {
		snap.Models = map[string]*ModelStats{}
	}
	tokenStats = snap
	return previous
}

// LoadTokenStats reads the on-disk token stats file into the in-memory
// snapshot, which is used by /api/stats as a low-latency fallback when the
// disk read fails.
func LoadTokenStats() {
	path := GetPath()
	st, err := readTokenStatsFromDisk(path)
	if err != nil {
		return
	}
	replaceTokenStatsSnapshot(st)
}

// ensureStatsDir creates the parent directory of path when it is non-trivial.
func ensureStatsDir(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		return os.MkdirAll(dir, 0o755)
	}
	return nil
}

// writeTokenStatsAtomically opens the configured stats file under an exclusive
// cross-process advisory lock, applies modifier to the file's current state,
// then writes the result back via temp-file + atomic rename. Concurrent
// binaries (long-running server + short-lived launch subcommand) share the
// same stats.json path; without locking, the long-lived process would
// overwrite the launch process's recent increments with its own stale
// startup-loaded snapshot. Returns nil error on success.
func writeTokenStatsAtomically(path string, modifier func(cur *TokenStatsData)) error {
	if err := ensureStatsDir(path); err != nil {
		return fmt.Errorf("create stats directory: %w", err)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		// The configured path may resolve to a directory (e.g. blocking
		// test scenarios). Surface the original path so callers and tests
		// can match the failure.
		return fmt.Errorf("open stats file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := lockStatsFileExclusive(f); err != nil {
		return fmt.Errorf("lock stats file %s: %w", path, err)
	}
	defer func() { _ = unlockStatsFile(f) }()

	cur, err := readStatsFromLockedFile(f)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read stats file %s: %w", path, err)
		}
		cur = &TokenStatsData{Models: map[string]*ModelStats{}}
	}
	if cur == nil {
		cur = &TokenStatsData{Models: map[string]*ModelStats{}}
	}
	if cur.Models == nil {
		cur.Models = map[string]*ModelStats{}
	}

	modifier(cur)

	out, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token stats: %w", err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek stats file: %w", err)
	}
	if err := f.Truncate(int64(len(out))); err != nil {
		return fmt.Errorf("truncate stats file: %w", err)
	}
	if _, err := f.Write(out); err != nil {
		return fmt.Errorf("write stats file: %w", err)
	}
	if err := f.Sync(); err != nil {
		// Some filesystem/disk-backed writes do not support fsync; the
		// truncate+write already landed in the buffer cache.
		slog.Debug("stats file sync failed (continuing)", "path", path, "error", err)
	}
	// Refresh the in-memory snapshot so /api/stats reflects the freshly
	// written data, even when this binary was not previously serving
	// traffic on the file.
	replaceTokenStatsSnapshot(cur)
	return nil
}

// readStatsFromLockedFile reads and parses the stats file while a lock is
// already held. Returns (nil, nil) when the file size is zero.
func readStatsFromLockedFile(f *os.File) (*TokenStatsData, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var st TokenStatsData
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// persistTokenStatsDelta writes a single per-request delta into the shared
// stats file using read-modify-write under a cross-process exclusive lock.
// Asynchronous invocation keeps the request hot-path lock-free while
// guarding against concurrent writers from other binaries (server + launch).
func persistTokenStatsDelta(delta func(cur *TokenStatsData)) {
	path := GetPath()
	if err := writeTokenStatsAtomically(path, delta); err != nil {
		slog.Warn("failed to persist token stats", "path", path, "error", err)
	}
}

// saveTokenStats overwrites the stats file with the current in-memory snapshot
// under an exclusive lock. Used by the admin DELETE handler and tests that
// require explicit disk-mirror semantics. Pre-RecordTokenUsage writers (the
// old hot-path) are intentionally replaced by persistTokenStatsDelta which
// merges deltas against the latest disk state.
func saveTokenStats() error {
	path := GetPath()
	snap := cloneTokenStatsSnapshot()
	return writeTokenStatsAtomically(path, func(cur *TokenStatsData) {
		*cur = TokenStatsData{
			TotalRequests: snap.TotalRequests,
			Models:        make(map[string]*ModelStats, len(snap.Models)),
		}
		for k, v := range snap.Models {
			m := *v
			cur.Models[k] = &m
		}
	})
}

func RecordTokenUsage(model string, promptTokens, completionTokens, totalTokens int64) {
	// Update the in-memory snapshot synchronously so an immediately-following
	// /api/stats GET sees the fresh count even before the async disk write
	// completes on a contention-free process.
	tokenStatsMu.Lock()
	tokenStats.TotalRequests++
	ms, ok := tokenStats.Models[model]
	if !ok {
		ms = &ModelStats{}
		tokenStats.Models[model] = ms
	}
	ms.RequestCount++
	ms.PromptTokens += promptTokens
	ms.CompletionTokens += completionTokens
	ms.TotalTokens += totalTokens
	tokenStatsMu.Unlock()

	go persistTokenStatsDelta(func(cur *TokenStatsData) {
		cur.TotalRequests++
		m, ok := cur.Models[model]
		if !ok {
			m = &ModelStats{}
			cur.Models[model] = m
		}
		m.RequestCount++
		m.PromptTokens += promptTokens
		m.CompletionTokens += completionTokens
		m.TotalTokens += totalTokens
	})
}

// RecordCacheUsage aggregates upstream prompt-cache accounting per model.
// Call it with the raw upstream usage map; zero/nil inputs are no-ops so
// call sites don't need extra branching.
func RecordCacheUsage(model string, usage map[string]any) {
	if model == "" || len(usage) == 0 {
		return
	}
	read, created := parseCacheUsage(usage)
	if read == 0 && created == 0 {
		return
	}
	tokenStatsMu.Lock()
	ms, ok := tokenStats.Models[model]
	if !ok {
		ms = &ModelStats{}
		tokenStats.Models[model] = ms
	}
	ms.CacheReadTokens += read
	ms.CacheCreatedTokens += created
	tokenStatsMu.Unlock()

	go persistTokenStatsDelta(func(cur *TokenStatsData) {
		m, ok := cur.Models[model]
		if !ok {
			m = &ModelStats{}
			cur.Models[model] = m
		}
		m.CacheReadTokens += read
		m.CacheCreatedTokens += created
	})
}

// TokenUsage is a typed view over an upstream usage map. It normalizes the
// Chat spelling (prompt_tokens/completion_tokens/total_tokens) and the
// Responses spelling (input_tokens/output_tokens/total_tokens) so call sites
// do not repeat the (prompt, completion, total) triple extraction.
type TokenUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// FromMap builds a TokenUsage from a usage map. Values are read through
// util.NumberAsFloat so integer-valued JSON numbers (decoded as int/int64) are
// not dropped by a raw float64 type assertion. The Responses spelling
// (input/output_tokens) is preferred when present, matching the existing
// Chat-to-Responses fallback order in the Claude/Response helpers.
func (TokenUsage) FromMap(m map[string]any) TokenUsage {
	var u TokenUsage
	u.PromptTokens = firstUsageToken(m, "input_tokens", "prompt_tokens")
	u.CompletionTokens = firstUsageToken(m, "output_tokens", "completion_tokens")
	u.TotalTokens = firstUsageToken(m, "total_tokens")
	return u
}

// firstUsageToken returns the first present key's value as int64, or 0 when
// none of the keys hold a number.
func firstUsageToken(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := util.NumberAsFloat(m[k]); ok {
			return int64(v)
		}
	}
	return 0
}

// RecordChatUsage records token and cache usage from a Chat-protocol usage
// map (prompt_tokens/completion_tokens/total_tokens). Absent or zero totals
// are ignored; RecordCacheUsage is itself a no-op when no cache fields exist.
func RecordChatUsage(model string, usage map[string]any) {
	if usage == nil {
		return
	}
	u := TokenUsage{}.FromMap(usage)
	if u.TotalTokens > 0 {
		RecordTokenUsage(model, u.PromptTokens, u.CompletionTokens, u.TotalTokens)
		RecordCacheUsage(model, usage)
	}
}

// ReadTokenStatsSnapshot returns the most recent on-disk token stats when
// available; otherwise it falls back to the in-memory snapshot. Used by
// /api/stats GET so the admin panel reflects contributions from every binary
// instance writing the same shared stats file.
func ReadTokenStatsSnapshot() (*TokenStatsData, error) {
	path := GetPath()
	if st, err := readTokenStatsFromDisk(path); err == nil {
		return st, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		// Fall through to in-memory fallback; non-existence is the common
		// path when no stats file has been written yet.
		slog.Debug("stats disk read failed (falling back to snapshot)", "path", path, "error", err)
	}
	return cloneTokenStatsSnapshot(), nil
}

// ResetTokenStats clears the shared stats file and the in-memory copy. Used
// by the admin DELETE handler.
func ResetTokenStats() error {
	tokenStatsMu.Lock()
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu.Unlock()
	return saveTokenStats()
}

// parseCacheUsage extracts cache token counts from the various usage shapes
// seen across upstreams:
//
//	OpenAI/Anthropic: prompt_tokens_details.cached_tokens, cache_creation_input_tokens
//	DeepSeek:         prompt_cache_hit_tokens, prompt_cache_miss_tokens
//
// Returns (read, created).
func parseCacheUsage(usage map[string]any) (int64, int64) {
	var read, created int64
	// Prefer the canonical Anthropic cache field. The fallbacks use the same
	// semantic category (already-cached prompt tokens), so any combination is
	// read once instead of double-counted.
	if v, ok := util.NumberAsFloat(usage["cache_read_input_tokens"]); ok {
		read += int64(v)
	} else if v, ok := util.NumberAsFloat(usage["prompt_cache_hit_tokens"]); ok {
		read += int64(v)
	} else if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := util.NumberAsFloat(details["cached_tokens"]); ok {
			read += int64(v)
		}
	}
	if v, ok := util.NumberAsFloat(usage["cache_creation_input_tokens"]); ok {
		created += int64(v)
	}
	return read, created
}
