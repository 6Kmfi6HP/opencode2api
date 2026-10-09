package app

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 构造一个固定尺寸的状态（size=1024 由调用方指定，便于精确验证预算）。
func testStoredState(id string) StoredResponseState {
	return StoredResponseState{
		Model:        "test-model",
		Instructions: strings.Repeat("x", 64),
		Output:       []any{map[string]any{"id": id, "type": "message"}},
	}
}

// 条数上限：超出后淘汰最久未使用，且总数恒定不超过上限。
func TestResponseStoreEvictsByEntryLimit(t *testing.T) {
	store := newResponseStore(true, 8, 1<<20, 1<<20, time.Hour)
	for i := 0; i < 20; i++ {
		store.store(fmt.Sprintf("resp_%02d", i), testStoredState(fmt.Sprintf("resp_%02d", i)), 1024)
	}
	if got := store.len(); got != 8 {
		t.Fatalf("entries = %d, want 8", got)
	}
	if _, ok := store.load("resp_00"); ok {
		t.Fatalf("oldest entry should have been evicted")
	}
	if _, ok := store.load("resp_19"); !ok {
		t.Fatalf("newest entry should be retained")
	}
}

// 字节预算：即使条数没到上限，字节超预算也要淘汰。
func TestResponseStoreEvictsByByteBudget(t *testing.T) {
	store := newResponseStore(true, 1000, 10*1024, 1<<20, time.Hour)
	for i := 0; i < 50; i++ {
		store.store(fmt.Sprintf("resp_%02d", i), testStoredState("x"), 1024)
	}
	_, bytes, _, maxBytes, _, _, _, _ := store.stats()
	if bytes > maxBytes {
		t.Fatalf("bytes = %d exceeds budget %d", bytes, maxBytes)
	}
	if got := store.len(); got > 10 {
		t.Fatalf("entries = %d, want <= 10", got)
	}
}

// TTL：过期条目在 load 与 sweep 两条路径上都要消失。
func TestResponseStoreExpiresByTTL(t *testing.T) {
	store := newResponseStore(true, 100, 1<<20, 1<<20, 40*time.Millisecond)
	store.store("resp_ttl", testStoredState("resp_ttl"), 128)
	time.Sleep(70 * time.Millisecond)
	if _, ok := store.load("resp_ttl"); ok {
		t.Fatalf("expired entry should miss")
	}
	store.store("resp_ttl2", testStoredState("resp_ttl2"), 128)
	time.Sleep(70 * time.Millisecond)
	if removed := store.sweep(time.Now()); removed != 1 {
		t.Fatalf("sweep removed = %d, want 1", removed)
	}
	if got := store.len(); got != 0 {
		t.Fatalf("entries = %d, want 0", got)
	}
}

// LRU 语义：load 命中要把条目提升为最近使用，避免活跃会话被后来的新条目挤掉。
func TestResponseStoreLoadPromotesRecency(t *testing.T) {
	store := newResponseStore(true, 2, 1<<20, 1<<20, time.Hour)
	store.store("a", testStoredState("a"), 128)
	store.store("b", testStoredState("b"), 128)
	if _, ok := store.load("a"); !ok {
		t.Fatalf("a should hit")
	}
	store.store("c", testStoredState("c"), 128)
	if _, ok := store.load("a"); !ok {
		t.Fatalf("a was promoted, must survive")
	}
	if _, ok := store.load("b"); ok {
		t.Fatalf("b was least recently used, must be evicted")
	}
}

// 单条过大直接不存，避免一条 4MB 级输出把整段会话历史挤掉。
func TestResponseStoreSkipsOversizedEntry(t *testing.T) {
	store := newResponseStore(true, 100, 1<<20, 4096, time.Hour)
	store.store("small", testStoredState("small"), 1024)
	store.store("huge", testStoredState("huge"), 1<<20)
	if _, ok := store.load("huge"); ok {
		t.Fatalf("oversized entry must not be stored")
	}
	if _, ok := store.load("small"); !ok {
		t.Fatalf("small entry must survive")
	}
	_, _, _, _, _, _, skipped, _ := store.stats()
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
}

// 总开关关闭时既不存也不取（无状态客户端场景）。
func TestResponseStoreDisabled(t *testing.T) {
	store := newResponseStore(false, 100, 1<<20, 1<<20, time.Hour)
	store.store("a", testStoredState("a"), 128)
	if _, ok := store.load("a"); ok {
		t.Fatalf("disabled store must not serve entries")
	}
	if got := store.len(); got != 0 {
		t.Fatalf("entries = %d, want 0", got)
	}
}

// store:false 的客户端请求不得落盘（沿用 OpenAI 语义）。
func TestStoreResponseStateRespectsStoreFalse(t *testing.T) {
	previous := responseStateStore
	responseStateStore = newResponseStore(true, 100, 1<<20, 1<<20, time.Hour)
	defer func() { responseStateStore = previous }()

	no := false
	storeResponseState(map[string]any{"id": "resp_1", "output": []any{}}, ResponsesAPIRequest{Store: &no})
	if _, ok := loadResponseState("resp_1"); ok {
		t.Fatalf("store=false must not persist state")
	}
	yes := true
	storeResponseState(map[string]any{"id": "resp_2", "output": []any{}}, ResponsesAPIRequest{Store: &yes})
	if _, ok := loadResponseState("resp_2"); !ok {
		t.Fatalf("store=true must persist state")
	}
}

// 内存回归：线上根因是 1627 条状态驻留 ~390MB。有界化后同样流量下堆增量
// 必须落在条数上限 × 单条约 250KB 的预算内（这里给足余量，<100MB）。
func TestResponseStoreBoundsHeapGrowth(t *testing.T) {
	previous := responseStateStore
	// 用线上默认值（256 条 / 128MB / 2h），只把 TTL 拉长以免测试期间过期。
	responseStateStore = newResponseStore(true, defaultResponseStoreMaxEntries, defaultResponseStoreMaxBytes, defaultResponseStoreMaxEntry, 24*time.Hour)
	defer func() { responseStateStore = previous }()

	instructions := strings.Repeat("You are a coding agent. ", 1000) // ~24KB
	tools := make([]ResponsesTool, 20)
	for i := range tools {
		tools[i] = ResponsesTool{
			Type:        "function",
			Name:        fmt.Sprintf("tool_%d_with_a_fairly_long_name", i),
			Description: strings.Repeat("describe the tool. ", 200),
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string", "description": strings.Repeat("x", 200)}}},
		}
	}
	mkOutput := func(seed int) []any {
		out := make([]any, 0, 12)
		for i := 0; i < 12; i++ {
			out = append(out, map[string]any{
				"id":   fmt.Sprintf("msg_%d_%d", seed, i),
				"type": "message",
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": strings.Repeat("lorem ipsum dolor sit amet ", 400), // ~10.8KB
				}},
			})
		}
		return out
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const n = 1600
	for i := 0; i < n; i++ {
		storeResponseState(
			map[string]any{"id": fmt.Sprintf("resp_%06d", i), "output": mkOutput(i)},
			ResponsesAPIRequest{Model: "some-free-model", Instructions: instructions, Tools: tools},
		)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	entries, bytes, _, _, _, _, _, _ := responseStateStore.stats()
	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("entries=%d bytes=%.1fMB heap_delta=%.1fMB (有界化前同流量为 388.7MB)",
		entries, float64(bytes)/1048576, float64(delta)/1048576)
	if entries > defaultResponseStoreMaxEntries {
		t.Fatalf("entries = %d exceeds max %d", entries, defaultResponseStoreMaxEntries)
	}
	if delta > 100*1048576 {
		t.Fatalf("heap delta = %.1fMB, want < 100MB", float64(delta)/1048576)
	}
}
