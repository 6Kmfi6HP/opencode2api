package stats

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStats_KeyUsage(t *testing.T) {
	resetStatsForTest(t)
	tmp := t.TempDir()
	SetPath(tmp + "/reset.json")
	if err := SetKeyLimit(2000); err != nil {
		t.Fatal(err)
	}

	// Reset verification FIRST, before any RecordKeyUsage deltas exist.
	tokenStatsMu.Lock()
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}, Keys: map[string]*KeyStats{"k1": {RequestCount: 1}}}
	tokenStatsMu.Unlock()
	if err := ResetTokenStats(); err != nil {
		t.Fatal(err)
	}
	if got := cloneTokenStatsSnapshot(); len(got.Keys) != 0 {
		t.Fatalf("keys after reset = %d, want 0", len(got.Keys))
	}

	// Record/snapshot section uses an isolated stats path.
	SetPath(tmp + "/stats.json")
	tokenStatsMu.Lock()
	tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
	tokenStatsMu.Unlock()

	RecordKeyUsage("k1", 200, "")
	RecordKeyUsage("k1", 429, "rate limited")
	RecordKeyCacheResult("k1", 10)
	RecordKeyUsage("k2", 0, "dial tcp: connection refused")
	RecordKeyUsage("", 500, "must be ignored")
	if err := FlushTokenStats(); err != nil {
		t.Fatal(err)
	}

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
	if k1.CacheHitRequests != 1 {
		t.Fatalf("k1 cache hits = %d, want 1", k1.CacheHitRequests)
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

func TestStats_KeyLimitEvictsLeastRecentlyUsed(t *testing.T) {
	resetStatsForTest(t)
	path := t.TempDir() + "/stats.json"
	SetPath(path)
	if err := SetKeyLimit(2); err != nil {
		t.Fatal(err)
	}
	RecordKeyUsage("old", 200, "")
	time.Sleep(time.Millisecond)
	RecordKeyUsage("middle", 200, "")
	time.Sleep(time.Millisecond)
	RecordKeyUsage("old", 200, "")
	time.Sleep(time.Millisecond)
	RecordKeyUsage("new", 200, "")
	if len(Snapshot().Keys) != 2 {
		t.Fatalf("in-memory keys = %d, want 2", len(Snapshot().Keys))
	}
	if Snapshot().Keys["old"] == nil || Snapshot().Keys["new"] == nil || Snapshot().Keys["middle"] != nil {
		t.Fatalf("unexpected LRU key set: %+v", Snapshot().Keys)
	}
	if err := FlushTokenStats(); err != nil {
		t.Fatal(err)
	}
	disk, err := readTokenStatsFromDisk(path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.KeyLimit != 2 || len(disk.Keys) != 2 || disk.Keys["middle"] != nil {
		t.Fatalf("disk stats limit/keys = %d/%+v, want limit=2 and old,new", disk.KeyLimit, disk.Keys)
	}
}
