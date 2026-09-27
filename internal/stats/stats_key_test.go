package stats

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStats_KeyUsage(t *testing.T) {
	resetStatsForTest(t)

	// Reset verification FIRST, before any async RecordKeyUsage deltas exist.
	tmp := t.TempDir()
	SetPath(tmp + "/reset.json")
	tokenStatsMu.Lock()
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}, Keys: map[string]*KeyStats{"k1": {RequestCount: 1}}}
	tokenStatsMu.Unlock()
	if err := ResetTokenStats(); err != nil {
		t.Fatal(err)
	}
	if got := cloneTokenStatsSnapshot(); len(got.Keys) != 0 {
		t.Fatalf("keys after reset = %d, want 0", len(got.Keys))
	}

	// Record/snapshot section on an isolated path so in-flight async disk
	// merges cannot disturb the assertions above.
	SetPath(tmp + "/stats.json")
	tokenStatsMu.Lock()
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu.Unlock()

	RecordKeyUsage("k1", 200, "")
	waitKeyCount(t, tmp+"/stats.json", "k1", 1)
	RecordKeyUsage("k1", 429, "rate limited")
	waitKeyCount(t, tmp+"/stats.json", "k1", 2)
	RecordKeyUsage("k2", 0, "dial tcp: connection refused")
	waitKeyCount(t, tmp+"/stats.json", "k2", 1)
	RecordKeyUsage("", 500, "must be ignored")

	// waitKeyCount 轮询磁盘直到指定 key 的 RequestCount 达标：既串行化
	// 多个 RecordKeyUsage 的“读-改-写”窗口（否则慢 delta 会覆盖快 delta 的
	// LastStatus），又给文件句柄关闭留出时间，避免 TempDir cleanup 报
	// "directory not empty"。
	waitKeyCount(t, tmp+"/stats.json", "k1", 2)

	snap := cloneTokenStatsSnapshot()
	k1 := snap.Keys["k1"]
	if k1 == nil {
		t.Fatal("k1 missing from snapshot")
	}
	if k1.RequestCount != 2 || k1.ErrorCount != 1 {
		t.Fatalf("k1 = %+v, want requests=2 errors=1", k1)
	}
	if k1.LastStatus != 429 || k1.LastError != "rate limited" {
		t.Fatalf("k1 last = %+v, want status=429 err=rate limited", k1)
	}
	if k1.LastUsedUnix == 0 {
		t.Fatal("k1 LastUsedUnix must be set")
	}
	k2 := snap.Keys["k2"]
	if k2 == nil || k2.RequestCount != 1 || k2.ErrorCount != 1 || k2.LastStatus != 0 {
		t.Fatalf("k2 = %+v, want requests=1 errors=1 status=0", k2)
	}
	if len(snap.Keys) != 2 {
		t.Fatalf("keys = %d, want 2 (empty ID ignored)", len(snap.Keys))
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var back TokenStatsData
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Keys["k1"] == nil || back.Keys["k1"].RequestCount != 2 {
		t.Fatalf("roundtrip k1 = %+v", back.Keys["k1"])
	}
}

// waitKeyCount 轮询磁盘直到指定 key 的 RequestCount 达标（2s 超时）。
func waitKeyCount(t *testing.T, path, keyID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := readTokenStatsFromDisk(path)
		if err == nil && st.Keys[keyID] != nil && st.Keys[keyID].RequestCount == int64(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for key %q count=%d on disk", keyID, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
