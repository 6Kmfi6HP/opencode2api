package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestWriteTokenStatsAtomicallyConcurrentWritersAccumulateExactly reproduces
// the cross-process race that broke token-stat updates after the JSON path
// refactor: long-running server pid + short-lived `opencode2api launch`
// proxies all share one stats.json, each holding its own per-process snapshot
// and overwriting the file on save. The flock-guarded read-modify-write in
// writeTokenStatsAtomically must accumulate every delta so /api/stats never
// appears to go backwards between binary restarts.
func TestWriteTokenStatsAtomicallyConcurrentWritersAccumulateExactly(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nested", "stats.json")
	setTokenStatsPath(path)
	t.Cleanup(func() { setTokenStatsPath("stats.json") })

	must := func(ok bool, msg string) {
		t.Helper()
		if !ok {
			t.Fatal(msg)
		}
	}

	if err := writeTokenStatsAtomically(path, func(cur *TokenStatsData) {}); err != nil {
		t.Fatalf("seed stats: %v", err)
	}

	const writers = 25
	const increments = 100
	totalExpected := int64(writers * increments)

	var wg sync.WaitGroup
	wg.Add(writers)
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		w := w
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < increments; i++ {
				//emodel = "m-" + surname from bucket index: 5 buckets, evenly across writers.
				model := "m-" + strconv.Itoa(w%5)
				if err := writeTokenStatsAtomically(path, func(cur *TokenStatsData) {
					cur.TotalRequests++
					m, ok := cur.Models[model]
					if !ok {
						m = &ModelStats{}
						cur.Models[model] = m
					}
					m.RequestCount++
					m.PromptTokens += 1
					m.CompletionTokens += 2
					m.TotalTokens += 3
				}); err != nil {
					t.Errorf("writer %d increment %d: %v", w, i, err)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	st, err := readTokenStatsFromDisk(path)
	must(err == nil, "read stats")
	if st.TotalRequests != totalExpected {
		t.Fatalf("TotalRequests = %d, want %d (delta loss across writers)",
			st.TotalRequests, totalExpected)
	}

	const buckets = 5
	perBucket := int64(writers/buckets) * increments
	if perBucket*buckets != totalExpected {
		t.Fatalf("test bucket math broken: perBucket=%d total=%d", perBucket, totalExpected)
	}
	for b := 0; b < buckets; b++ {
		m, ok := st.Models["m-"+strconv.Itoa(b)]
		must(ok, "missing model bucket m-"+strconv.Itoa(b))
		if m.RequestCount != perBucket {
			t.Fatalf("model m-%d RequestCount = %d, want %d", b, m.RequestCount, perBucket)
		}
		if m.PromptTokens != perBucket {
			t.Fatalf("model m-%d PromptTokens = %d, want %d", b, m.PromptTokens, perBucket)
		}
		if m.CompletionTokens != perBucket*2 {
			t.Fatalf("model m-%d CompletionTokens = %d, want %d", b, m.CompletionTokens, perBucket*2)
		}
		if m.TotalTokens != perBucket*3 {
			t.Fatalf("model m-%d TotalTokens = %d, want %d", b, m.TotalTokens, perBucket*3)
		}
	}
}

// TestRecordTokenUsagePersistDeltas proves the user-facing record path
// (recordTokenUsage) does not lose increments on disk when concurrent
// goroutines drive the stats through the file-backed read-modify-write path.
func TestRecordTokenUsagePersistDeltas(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "stats.json")
	resetStatsAndLogTestState(t)
	setTokenStatsPath(path)
	replaceTokenStatsSnapshot(&TokenStatsData{Models: map[string]*ModelStats{}})

	if err := os.WriteFile(path, []byte(`{"total_requests":0,"models":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	const total = 200
	const concurrent = 20
	per := total / concurrent

	var wg sync.WaitGroup
	wg.Add(concurrent)
	for i := 0; i < concurrent; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				recordTokenUsage("race-model", 1, 2, 3)
			}
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := readTokenStatsFromDisk(path)
		if st != nil && st.TotalRequests >= int64(total) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	st, err := readTokenStatsFromDisk(path)
	if err != nil {
		t.Fatalf("read stats: %v", err)
	}
	if st.TotalRequests != int64(total) {
		t.Fatalf("TotalRequests = %d, want %d", st.TotalRequests, total)
	}
	m := st.Models["race-model"]
	if m == nil {
		t.Fatalf("missing model race-model")
	}
	if m.RequestCount != int64(total) {
		t.Fatalf("RequestCount = %d, want %d", m.RequestCount, total)
	}
	if m.PromptTokens != int64(total) {
		t.Fatalf("PromptTokens = %d, want %d", m.PromptTokens, total)
	}
	if m.TotalTokens != int64(total)*3 {
		t.Fatalf("TotalTokens = %d, want %d", m.TotalTokens, total*3)
	}
}

// TestRecordCacheUsageMergesAcrossWriters covers the cache-token branch
// which uses the same atomic path and must also never lose updates.
func TestRecordCacheUsageMergesAcrossWriters(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "stats.json")
	resetStatsAndLogTestState(t)
	setTokenStatsPath(path)
	if err := os.WriteFile(path, []byte(`{"total_requests":0,"models":{"cache-model":{"request_count":0}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	const iters = 50
	var wg sync.WaitGroup
	wg.Add(iters)
	for i := 0; i < iters; i++ {
		go func() {
			defer wg.Done()
			recordCacheUsage("cache-model", map[string]any{
				"cache_read_input_tokens":     float64(4),
				"cache_creation_input_tokens": float64(8),
			})
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := readTokenStatsFromDisk(path)
		if err == nil && st != nil {
			if m := st.Models["cache-model"]; m != nil && m.CacheReadTokens >= int64(4*iters) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	st, _ := readTokenStatsFromDisk(path)
	if st == nil {
		t.Fatal("read stats: nil")
	}
	m := st.Models["cache-model"]
	if m == nil {
		t.Fatal("missing model cache-model")
	}
	if m.CacheReadTokens != int64(4*iters) {
		t.Fatalf("CacheReadTokens = %d, want %d", m.CacheReadTokens, 4*iters)
	}
	if m.CacheCreatedTokens != int64(8*iters) {
		t.Fatalf("CacheCreatedTokens = %d, want %d", m.CacheCreatedTokens, 8*iters)
	}

	// Sanity: response JSON marshals back to identical fields.
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !containsAll(body, `"cache_read_tokens"`, `"cache_created_tokens"`) {
		t.Fatalf("unexpected json: %s", body)
	}
}

// containsAll returns true when every needle is a substring of haystack.
func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		if !containsString(haystack, n) {
			return false
		}
	}
	return true
}

func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack[0:len(needle)] == needle || containsString(haystack[1:], needle))
}
