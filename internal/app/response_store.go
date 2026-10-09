package app

import (
	"container/list"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ======================== /v1/responses 状态存储（有界） ========================
//
// 背景（线上内存占用过高的根因）：
// OpenAI Responses API 的 previous_response_id 续链要求网关保留上一轮的
// instructions / tools / tool_choice / output。v0.14.17 用一个只增不减的全局
// map 保存这些状态：`storedResponses map[string]StoredResponseState`，没有任何
// TTL、条数上限或字节预算。
//
// 单条实测约 250KB（instructions ~24KB + tools 克隆 ~20KB + output 克隆 ~200KB，
// 对齐线上 bytes_out 均值 210KB）。线上 6.7 天 1627 次 /v1/responses 即驻留约
// 390MB，进程 RSS 达 542MB / 峰值 927MB —— 1.9GB 内存的机器（new-api、cline2api、
// Caddy 等共存）被拖到 OOM 边缘。
//
// 这里改成 TTL + 条数上限 + 字节预算 三维有界的 LRU：任一维度超限就淘汰最久
// 未使用的条目；单条超过 maxEntry 直接不存（避免一次 4MB 级输出把预算吃光）。
// 全部维度都可用环境变量调整，`OPENCODE2API_RESPONSE_STORE=off` 可整体关闭
// （纯无状态客户端不用 previous_response_id，关掉最省内存）。

const (
	// 默认 256 条 ≈ 64MB 常驻（按线上 250KB/条估）；TTL 2h 远大于单次 agent
	// 会话的续链间隔，正常多轮对话不会因淘汰而丢链。
	defaultResponseStoreMaxEntries = 256
	defaultResponseStoreMaxBytes   = 128 << 20 // 128 MiB
	defaultResponseStoreTTL        = 2 * time.Hour
	defaultResponseStoreMaxEntry   = 16 << 20 // 单条 16 MiB，超过不存
	// 惰性淘汰只在写入时触发；空闲期靠这个 ticker 主动清过期条目并把
	// 实际占用写进日志/管理面板。
	responseStoreSweepInterval = 5 * time.Minute
)

type responseStoreEntry struct {
	key      string
	state    StoredResponseState
	size     int
	lastUsed time.Time
}

// responseStore 是 storedResponses 的有界替代品。lru 队首为最近使用，
// 队尾为最久未使用（淘汰对象）。
type responseStore struct {
	mu      sync.Mutex
	entries map[string]*list.Element // response id -> *responseStoreEntry
	lru     *list.List
	bytes   int

	enabled    bool
	maxEntries int
	maxBytes   int
	maxEntry   int
	ttl        time.Duration

	// 统计（只增计数，供日志/管理面板观测淘汰行为）
	evictedEntries int64
	evictedBytes   int64
	skippedOvers   int64
}

// responseStateStore 是进程内唯一实例。用包级变量保持与旧代码相同的调用姿势
// （storeResponseState / loadResponseState 两个函数签名不变）。
var responseStateStore = newResponseStoreFromEnv()

func newResponseStoreFromEnv() *responseStore {
	return newResponseStore(
		responseStoreEnabled(),
		envInt("OPENCODE2API_RESPONSE_STORE_MAX_ENTRIES", defaultResponseStoreMaxEntries),
		envInt("OPENCODE2API_RESPONSE_STORE_MAX_BYTES", defaultResponseStoreMaxBytes),
		envInt("OPENCODE2API_RESPONSE_STORE_MAX_ENTRY_BYTES", defaultResponseStoreMaxEntry),
		envDuration("OPENCODE2API_RESPONSE_STORE_TTL", defaultResponseStoreTTL),
	)
}

// newResponseStore 显式构造，便于测试注入小上限；非法/非正值回落到默认。
func newResponseStore(enabled bool, maxEntries, maxBytes, maxEntry int, ttl time.Duration) *responseStore {
	store := &responseStore{
		entries:    map[string]*list.Element{},
		lru:        list.New(),
		enabled:    enabled,
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		maxEntry:   maxEntry,
		ttl:        ttl,
	}
	if store.maxEntries <= 0 {
		store.maxEntries = defaultResponseStoreMaxEntries
	}
	if store.maxBytes <= 0 {
		store.maxBytes = defaultResponseStoreMaxBytes
	}
	if store.ttl <= 0 {
		store.ttl = defaultResponseStoreTTL
	}
	return store
}

// responseStoreEnabled 解析总开关；缺省开启（previous_response_id 语义默认可用）。
func responseStoreEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENCODE2API_RESPONSE_STORE"))) {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}

func envInt(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		slog.Warn("invalid integer env value, using default", "env", name, "value", raw, "default", def)
		return def
	}
	return value
}

func envDuration(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		slog.Warn("invalid duration env value, using default", "env", name, "value", raw, "default", def.String())
		return def
	}
	return value
}

// store 写入一条状态并执行三维淘汰。size 为调用方估算的字节数（instructions
// 长度 + 克隆后的 JSON 长度）；size 超 maxEntry 时丢弃并计数，不让单条巨物
// 挤掉整段会话历史。
func (s *responseStore) store(key string, state StoredResponseState, size int) {
	if !s.enabled || key == "" {
		return
	}
	if s.maxEntry > 0 && size > s.maxEntry {
		s.mu.Lock()
		s.skippedOvers++
		s.mu.Unlock()
		slog.Warn("response state too large to store", "response_id", key, "size", size, "max_entry", s.maxEntry)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if element, ok := s.entries[key]; ok {
		// 同一 response id 重复写入（重试/flush 兜底路径）按更新处理。
		entry := element.Value.(*responseStoreEntry)
		s.bytes -= entry.size
		entry.state, entry.size, entry.lastUsed = state, size, now
		s.bytes += entry.size
		s.lru.MoveToFront(element)
		s.evictLocked(now)
		return
	}
	entry := &responseStoreEntry{key: key, state: state, size: size, lastUsed: now}
	s.entries[key] = s.lru.PushFront(entry)
	s.bytes += size
	s.evictLocked(now)
}

// load 读取状态并把条目提升为最近使用（true LRU）。返回的是深拷贝，调用方
// 可以自由改写而不污染存储。
func (s *responseStore) load(key string) (StoredResponseState, bool) {
	if !s.enabled || key == "" {
		return StoredResponseState{}, false
	}
	s.mu.Lock()
	element, ok := s.entries[key]
	if !ok {
		s.mu.Unlock()
		return StoredResponseState{}, false
	}
	entry := element.Value.(*responseStoreEntry)
	if s.ttl > 0 && time.Since(entry.lastUsed) > s.ttl {
		s.removeElementLocked(element)
		s.mu.Unlock()
		return StoredResponseState{}, false
	}
	entry.lastUsed = time.Now()
	s.lru.MoveToFront(element)
	state := entry.state
	s.mu.Unlock()
	return cloneJSONValue(state), true
}

// evictLocked 按 TTL → 条数 → 字节预算依次淘汰；调用方必须持锁。
func (s *responseStore) evictLocked(now time.Time) {
	if s.ttl > 0 {
		for element := s.lru.Back(); element != nil; {
			previous := element.Prev()
			entry := element.Value.(*responseStoreEntry)
			// LRU 顺序即 lastUsed 顺序：从最旧一端扫，遇到未过期即可停。
			if now.Sub(entry.lastUsed) <= s.ttl {
				break
			}
			s.removeElementLocked(element)
			element = previous
		}
	}
	for len(s.entries) > s.maxEntries {
		element := s.lru.Back()
		if element == nil {
			break
		}
		s.removeElementLocked(element)
	}
	for s.bytes > s.maxBytes {
		element := s.lru.Back()
		if element == nil {
			break
		}
		s.removeElementLocked(element)
	}
}

// sweep 周期性清理过期条目，返回本次清掉的数量。
func (s *responseStore) sweep(now time.Time) int {
	if !s.enabled {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.entries)
	s.evictLocked(now)
	return before - len(s.entries)
}

func (s *responseStore) removeElementLocked(element *list.Element) {
	entry := element.Value.(*responseStoreEntry)
	s.lru.Remove(element)
	delete(s.entries, entry.key)
	s.bytes -= entry.size
	s.evictedEntries++
	s.evictedBytes += int64(entry.size)
}

// stats 暴露给 /api/config 与周期日志，便于线上确认有界化是否在生效。
func (s *responseStore) stats() (entries int, bytes int, maxEntries int, maxBytes int, ttl time.Duration, evicted int64, skipped int64, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries), s.bytes, s.maxEntries, s.maxBytes, s.ttl, s.evictedEntries, s.skippedOvers, s.enabled
}

// reset 清空存储（测试与 admin reload 用）。
func (s *responseStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = map[string]*list.Element{}
	s.lru = list.New()
	s.bytes = 0
}

func (s *responseStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// startResponseStoreJanitor 周期清过期条目。日志刻意只在有淘汰时打 Info，
// 常规心跳走 Debug，避免给本来就少的日志再加噪音。
func startResponseStoreJanitor() {
	entries, bytes, maxEntries, maxBytes, ttl, _, _, enabled := responseStateStore.stats()
	slog.Info("response state store ready",
		"enabled", enabled, "max_entries", maxEntries, "max_bytes", maxBytes, "ttl", ttl.String(), "entries", entries, "bytes", bytes)
	if !enabled {
		return
	}
	go func() {
		ticker := time.NewTicker(responseStoreSweepInterval)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			removed := responseStateStore.sweep(now)
			entries, bytes, _, _, _, evicted, skipped, _ := responseStateStore.stats()
			if removed > 0 || evicted > 0 {
				slog.Info("response state store sweep", "removed", removed, "entries", entries, "bytes", bytes,
					"evicted_total", evicted, "skipped_oversized", skipped)
				continue
			}
			slog.Debug("response state store sweep", "entries", entries, "bytes", bytes)
		}
	}()
}

// responseStoreStatsView 是 /api/config 里 `response_store` 字段的内容：
// 运维只需一眼看出「有没有界、当前占了多少、淘汰过多少」，不必抓 pprof。
func responseStoreStatsView() map[string]any {
	entries, bytes, maxEntries, maxBytes, ttl, evicted, skipped, enabled := responseStateStore.stats()
	return map[string]any{
		"enabled":           enabled,
		"entries":           entries,
		"bytes":             bytes,
		"max_entries":       maxEntries,
		"max_bytes":         maxBytes,
		"ttl_seconds":       int(ttl.Seconds()),
		"evicted_total":     evicted,
		"skipped_oversized": skipped,
	}
}

// cloneJSONValueSized 与 cloneJSONValue 等价，额外返回编码后的字节数：存储时
// 复用它做字节预算计量，避免为了量尺寸再 marshal 一遍。
func cloneJSONValueSized[T any](value T) (T, int) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value, 0
	}
	var cloned T
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return value, 0
	}
	return cloned, len(encoded)
}
