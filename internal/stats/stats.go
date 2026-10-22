package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/6Kmfi6HP/opencode2api/internal/util"
)

// stats.json is updated under a shared flock: each process merges its batched
// deltas into the latest disk state, preserving launch/server consistency.

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

// KeyStats 各字段汇总 key_pool 池内单把 key 的运行状态；Rate 字段由 cache
// hit/miss 导出，避免前端/日志里再做除法。
type KeyStats struct {
	RequestCount       int64  `json:"request_count"`
	ErrorCount         int64  `json:"error_count"`
	LastStatus         int    `json:"last_status"`
	LastError          string `json:"last_error,omitempty"`
	LastUsedUnix       int64  `json:"last_used_unix"`
	LastUsedUnixNano   int64  `json:"last_used_unix_nano,omitempty"`
	LastStatusUnixNano int64  `json:"last_status_unix_nano,omitempty"`
	// CacheHitRequests / CacheMissRequests 以带 token 的请求为口径：
	// 命中=usage.cached_tokens>0，miss=cached_tokens<=0（或无 usage）。
	CacheHitRequests  int64   `json:"cache_hit_requests,omitempty"`
	CacheMissRequests int64   `json:"cache_miss_requests,omitempty"`
	CacheHitRatio     float64 `json:"cache_hit_ratio,omitempty"`
}

type TokenStatsData struct {
	TotalRequests int64                  `json:"total_requests"`
	Models        map[string]*ModelStats `json:"models"`
	Keys          map[string]*KeyStats   `json:"keys,omitempty"`
	KeyLimit      int                    `json:"key_limit,omitempty"`
}

const (
	defaultKeyLimit     = 2000
	statsFlushInterval  = 5 * time.Second
	statsFlushThreshold = 128
	statsSyncInterval   = 30 * time.Second
)

var (
	tokenStats          = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu        sync.Mutex
	tokenStatsPath      = "stats.json"
	statsKeyLimit       = defaultKeyLimit
	keyLimitFromEnv     bool
	keyLimitConfigured  bool
	pendingStats        = &TokenStatsData{Models: map[string]*ModelStats{}}
	pendingEvents       int
	statsWriterOnce     sync.Once
	statsNotify         = make(chan struct{}, 1)
	statsFlushRequests  = make(chan statsWriteRequest)
	statsSignals        = make(chan os.Signal, 2)
	statsSignalRequests = make(chan statsSignalRequest)
)

type statsWriteRequest struct {
	path     string
	modifier func(cur *TokenStatsData)
	reply    chan error
}

type statsSignalRequest struct {
	signal os.Signal
	reply  chan struct{}
}

func init() {
	if raw := os.Getenv("OPENCODE2API_STATS_KEY_LIMIT"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			statsKeyLimit = n
			keyLimitFromEnv = true
			keyLimitConfigured = true
		}
	}
	ensureStatsWriter()
}

func ensureStatsWriter() {
	statsWriterOnce.Do(func() {
		signal.Notify(statsSignals, os.Interrupt, syscall.SIGTERM)
		go statsWriter()
		go func() {
			for sig := range statsSignals {
				reply := make(chan struct{})
				statsSignalRequests <- statsSignalRequest{signal: sig, reply: reply}
				<-reply
			}
		}()
	})
}

// SetPath updates the file path used to persist token usage statistics.
func SetPath(path string) {
	if err := FlushTokenStats(); err != nil {
		slog.Warn("failed to flush token stats before changing path", "error", err)
	}
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
		KeyLimit:      tokenStats.KeyLimit,
	}
	for k, v := range tokenStats.Models {
		cp := *v
		snap.Models[k] = &cp
	}
	if len(tokenStats.Keys) > 0 {
		snap.Keys = make(map[string]*KeyStats, len(tokenStats.Keys))
		for k, v := range tokenStats.Keys {
			cp := *v
			snap.Keys[k] = &cp
		}
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
	tokenStatsMu.Lock()
	if !keyLimitFromEnv && st.KeyLimit > 0 {
		statsKeyLimit = st.KeyLimit
		keyLimitConfigured = true
	}
	tokenStatsMu.Unlock()
	st.KeyLimit = currentKeyLimit()
	enforceKeyLimit(st)
	replaceTokenStatsSnapshot(st)
	recordStatsDelta(func(cur *TokenStatsData) {
		cur.KeyLimit = st.KeyLimit
	})
}

// SetKeyLimit changes the LRU key-stat cap. A positive value is persisted in stats.json.
func SetKeyLimit(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("stats key limit must be positive")
	}
	if err := FlushTokenStats(); err != nil {
		return err
	}
	snapshot := cloneTokenStatsSnapshot()
	tokenStatsMu.Lock()
	previousLimit := statsKeyLimit
	previousExplicit := keyLimitFromEnv
	previousConfigured := keyLimitConfigured
	statsKeyLimit = limit
	keyLimitFromEnv = false
	keyLimitConfigured = true
	tokenStats.KeyLimit = limit
	enforceKeyLimit(tokenStats)
	tokenStatsMu.Unlock()
	err := writeTokenStatsAtomically(GetPath(), func(cur *TokenStatsData) {
		cur.KeyLimit = limit
	})
	if err != nil {
		tokenStatsMu.Lock()
		statsKeyLimit = previousLimit
		keyLimitFromEnv = previousExplicit
		keyLimitConfigured = previousConfigured
		tokenStatsMu.Unlock()
		replaceTokenStatsSnapshot(snapshot)
	}
	return err
}

func currentKeyLimit() int {
	limit, _ := configuredKeyLimit()
	return limit
}

func configuredKeyLimit() (int, bool) {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	if statsKeyLimit <= 0 {
		return defaultKeyLimit, keyLimitFromEnv
	}
	return statsKeyLimit, keyLimitFromEnv
}

func enforceKeyLimit(st *TokenStatsData) {
	if st == nil {
		return
	}
	limit := st.KeyLimit
	if limit <= 0 {
		limit = defaultKeyLimit
	}
	st.KeyLimit = limit
	if len(st.Keys) <= limit {
		return
	}
	type keyAge struct {
		id   string
		nano int64
	}
	ages := make([]keyAge, 0, len(st.Keys))
	for id, key := range st.Keys {
		if key.LastUsedUnixNano == 0 {
			key.LastUsedUnixNano = key.LastUsedUnix * int64(time.Second)
		}
		ages = append(ages, keyAge{id: id, nano: key.LastUsedUnixNano})
	}
	sort.Slice(ages, func(i, j int) bool {
		if ages[i].nano == ages[j].nano {
			return ages[i].id < ages[j].id
		}
		return ages[i].nano < ages[j].nano
	})
	for _, age := range ages[:len(ages)-limit] {
		delete(st.Keys, age.id)
	}
}

// ensureStatsDir creates the parent directory of path when it is non-trivial.
func ensureStatsDir(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		return os.MkdirAll(dir, 0o755)
	}
	return nil
}

// writeTokenStatsAtomically routes synchronous maintenance writes through the
// dedicated writer. Its flock-protected read-modify-write preserves deltas
// across the server and launch processes sharing stats.json.
func writeTokenStatsAtomically(path string, modifier func(cur *TokenStatsData)) error {
	ensureStatsWriter()
	reply := make(chan error, 1)
	statsFlushRequests <- statsWriteRequest{path: path, modifier: modifier, reply: reply}
	return <-reply
}

func writeStatsFileLocked(path string, modifier func(cur *TokenStatsData), forceSync bool) error {
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
	limit, explicitLimit := configuredKeyLimit()
	if cur.KeyLimit <= 0 || explicitLimit {
		cur.KeyLimit = limit
	}

	modifier(cur)
	limit, explicitLimit = configuredKeyLimit()
	if explicitLimit {
		cur.KeyLimit = limit
	}
	enforceKeyLimit(cur)

	out, err := json.Marshal(cur)
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
	if forceSync {
		if err := f.Sync(); err != nil {
			// Some filesystem/disk-backed writes do not support fsync; the
			// truncate+write already landed in the buffer cache.
			slog.Debug("stats file sync failed (continuing)", "path", path, "error", err)
		}
	}
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

// recordStatsDelta updates the live snapshot and coalesces the same delta for
// the dedicated writer without making the request path wait on file I/O/flock.
func recordStatsDelta(delta func(cur *TokenStatsData)) {
	ensureStatsWriter()
	tokenStatsMu.Lock()
	if tokenStats.KeyLimit <= 0 {
		tokenStats.KeyLimit = statsKeyLimit
	}
	if pendingStats.KeyLimit <= 0 && keyLimitConfigured {
		pendingStats.KeyLimit = statsKeyLimit
	}
	delta(tokenStats)
	delta(pendingStats)
	pendingEvents++
	flush := pendingEvents >= statsFlushThreshold
	tokenStatsMu.Unlock()
	if flush {
		select {
		case statsNotify <- struct{}{}:
		default:
		}
	}
}

func takePendingStats() (*TokenStatsData, int) {
	tokenStatsMu.Lock()
	defer tokenStatsMu.Unlock()
	if pendingEvents == 0 {
		return nil, 0
	}
	batch, count := pendingStats, pendingEvents
	pendingStats = &TokenStatsData{Models: map[string]*ModelStats{}}
	pendingEvents = 0
	return batch, count
}

func mergeStatsDelta(dst, src *TokenStatsData) {
	if dst.Models == nil {
		dst.Models = map[string]*ModelStats{}
	}
	dst.TotalRequests += src.TotalRequests
	for model, delta := range src.Models {
		m := dst.Models[model]
		if m == nil {
			m = &ModelStats{}
			dst.Models[model] = m
		}
		m.RequestCount += delta.RequestCount
		m.PromptTokens += delta.PromptTokens
		m.CompletionTokens += delta.CompletionTokens
		m.TotalTokens += delta.TotalTokens
		m.CacheReadTokens += delta.CacheReadTokens
		m.CacheCreatedTokens += delta.CacheCreatedTokens
	}
	if len(src.Keys) > 0 {
		if dst.Keys == nil {
			dst.Keys = map[string]*KeyStats{}
		}
		for id, delta := range src.Keys {
			k := dst.Keys[id]
			if k == nil {
				k = &KeyStats{}
				dst.Keys[id] = k
			}
			k.RequestCount += delta.RequestCount
			k.ErrorCount += delta.ErrorCount
			k.CacheHitRequests += delta.CacheHitRequests
			k.CacheMissRequests += delta.CacheMissRequests
			if delta.LastStatusUnixNano > k.LastStatusUnixNano {
				k.LastStatus = delta.LastStatus
				k.LastError = delta.LastError
				k.LastStatusUnixNano = delta.LastStatusUnixNano
			}
			if delta.LastUsedUnixNano >= k.LastUsedUnixNano {
				k.LastUsedUnix = delta.LastUsedUnix
				k.LastUsedUnixNano = delta.LastUsedUnixNano
			}
			total := k.CacheHitRequests + k.CacheMissRequests
			if total > 0 {
				k.CacheHitRatio = float64(k.CacheHitRequests) / float64(total)
			}
		}
	}
	if src.KeyLimit > 0 {
		dst.KeyLimit = src.KeyLimit
	}
}

func flushPending(forceSync bool) error {
	batch, count := takePendingStats()
	if count == 0 {
		if forceSync {
			return syncStatsFile(GetPath())
		}
		return nil
	}
	path := GetPath()
	err := writeStatsFileLocked(path, func(cur *TokenStatsData) {
		mergeStatsDelta(cur, batch)
	}, forceSync)
	if err != nil {
		tokenStatsMu.Lock()
		mergeStatsDelta(pendingStats, batch)
		pendingEvents += count
		tokenStatsMu.Unlock()
		slog.Warn("failed to persist token stats", "path", path, "error", err)
	}
	return err
}

func syncStatsFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	if err := lockStatsFileExclusive(f); err != nil {
		return err
	}
	defer func() { _ = unlockStatsFile(f) }()
	return f.Sync()
}

func statsWriter() {
	ticker := time.NewTicker(statsFlushInterval)
	defer ticker.Stop()
	lastSync := time.Now()
	for {
		select {
		case <-statsNotify:
			forceSync := time.Since(lastSync) >= statsSyncInterval
			if err := flushPending(forceSync); err == nil && forceSync {
				lastSync = time.Now()
			}
		case <-ticker.C:
			forceSync := time.Since(lastSync) >= statsSyncInterval
			if err := flushPending(forceSync); err == nil && forceSync {
				lastSync = time.Now()
			}
		case req := <-statsFlushRequests:
			err := flushPending(true)
			if err == nil && req.modifier != nil {
				err = writeStatsFileLocked(req.path, req.modifier, true)
			}
			if err == nil {
				lastSync = time.Now()
			}
			req.reply <- err
		case request := <-statsSignalRequests:
			flushStatsOnSignal(request.signal)
			lastSync = time.Now()
			close(request.reply)
		}
	}
}

func flushStatsOnSignal(sig os.Signal) {
	slog.Info("flushing token stats on shutdown", "signal", sig.String())
	if err := flushPending(true); err != nil {
		slog.Error("failed to flush token stats on shutdown", "error", err)
	}
}

// FlushTokenStats waits for queued deltas to be written and synced. Call during
// graceful shutdown; the package also drains automatically on SIGINT/SIGTERM.
func FlushTokenStats() error {
	ensureStatsWriter()
	reply := make(chan error, 1)
	statsFlushRequests <- statsWriteRequest{reply: reply}
	return <-reply
}

// saveTokenStats overwrites the stats file with the current in-memory snapshot
// under an exclusive lock. Used by the admin DELETE handler and tests that
// require explicit disk-mirror semantics. It remains serialized by statsWriter.
func saveTokenStats() error {
	path := GetPath()
	snap := cloneTokenStatsSnapshot()
	return writeTokenStatsAtomically(path, func(cur *TokenStatsData) {
		*cur = TokenStatsData{
			TotalRequests: snap.TotalRequests,
			Models:        make(map[string]*ModelStats, len(snap.Models)),
			KeyLimit:      snap.KeyLimit,
		}
		for k, v := range snap.Models {
			m := *v
			cur.Models[k] = &m
		}
		if len(snap.Keys) > 0 {
			cur.Keys = make(map[string]*KeyStats, len(snap.Keys))
			for k, v := range snap.Keys {
				ks := *v
				cur.Keys[k] = &ks
			}
		}
	})
}

func RecordTokenUsage(model string, promptTokens, completionTokens, totalTokens int64) {
	recordStatsDelta(func(cur *TokenStatsData) {
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

// RecordKeyUsage aggregates per-key upstream outcomes. It mirrors the
// RecordTokenUsage pattern: the in-memory snapshot updates synchronously and
// the writer persists a coalesced delta under a cross-process lock. Empty
// keyIDs are no-ops. errMsg should already be
// truncated by the caller; overlong values are defensively capped at 200
// bytes here as well.
func RecordKeyUsage(keyID string, status int, errMsg string) {
	if keyID == "" {
		return
	}
	if len(errMsg) > 200 {
		errMsg = errMsg[:200]
	}
	isErr := status == 0 || status >= 400
	nowTime := time.Now()
	now := nowTime.Unix()
	nowNano := nowTime.UnixNano()
	recordStatsDelta(func(cur *TokenStatsData) {
		if cur.Keys == nil {
			cur.Keys = map[string]*KeyStats{}
		}
		k, ok := cur.Keys[keyID]
		if !ok {
			k = &KeyStats{}
			cur.Keys[keyID] = k
		}
		k.RequestCount++
		if isErr {
			k.ErrorCount++
		}
		k.LastStatus = status
		k.LastError = errMsg
		k.LastUsedUnix = now
		k.LastUsedUnixNano = nowNano
		k.LastStatusUnixNano = nowNano
		enforceKeyLimit(cur)
	})
}

// RecordKeyCacheResult 在 selectPoolKey 已接管（keyID 非空）且单次尝试完成后
// 调用，把缓存命中/未命中记到对应 key 上；cachedTokens<=0 视为 miss（含 0）。
// 幂等：同一请求会被多次 attempt 时只应调用一次（由上游请求的最后一次决定）。现在调用
// 方只来自 opencode.go 的成功分支（200-299）,原样写。
func RecordKeyCacheResult(keyID string, cachedTokens int64) {
	if keyID == "" {
		return
	}
	now := time.Now()
	recordStatsDelta(func(cur *TokenStatsData) {
		if cur.Keys == nil {
			cur.Keys = map[string]*KeyStats{}
		}
		k, ok := cur.Keys[keyID]
		if !ok {
			k = &KeyStats{}
			cur.Keys[keyID] = k
		}
		k.LastUsedUnix = now.Unix()
		k.LastUsedUnixNano = now.UnixNano()
		if cachedTokens > 0 {
			k.CacheHitRequests++
		} else {
			k.CacheMissRequests++
		}
		t := k.CacheHitRequests + k.CacheMissRequests
		if t > 0 {
			k.CacheHitRatio = float64(k.CacheHitRequests) / float64(t)
		}
		enforceKeyLimit(cur)
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
	recordStatsDelta(func(cur *TokenStatsData) {
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
	if err := FlushTokenStats(); err != nil {
		return err
	}
	tokenStatsMu.Lock()
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}, KeyLimit: statsKeyLimit}
	tokenStatsMu.Unlock()
	tokenStatsMu.Lock()
	pendingStats = &TokenStatsData{Models: map[string]*ModelStats{}}
	pendingEvents = 0
	tokenStatsMu.Unlock()
	return saveTokenStats()
}

// parseCacheUsage extracts cache token counts from the various usage shapes
// seen across upstreams:
//
//	OpenAI/Anthropic: prompt_tokens_details.cached_tokens, cache_creation_input_tokens
//	muse-spark 原生 responses: input_tokens_details.cached_tokens（与 prompt
//	形态同义，先有谁用谁，不双计）
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
	} else if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if v, ok := util.NumberAsFloat(details["cached_tokens"]); ok {
			read += int64(v)
		}
	}
	if v, ok := util.NumberAsFloat(usage["cache_creation_input_tokens"]); ok {
		created += int64(v)
	}
	return read, created
}
