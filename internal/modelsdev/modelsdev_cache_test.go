package modelsdev

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func resetModelsDevCacheTestState(t *testing.T) {
	t.Helper()
	mu.Lock()
	origCache := memoryCache
	origTime := memoryTime
	origURL := catalogURL
	origPath := cachePath
	memoryCache = cache{}
	memoryTime = time.Time{}
	mu.Unlock()

	t.Cleanup(func() {
		mu.Lock()
		memoryCache = origCache
		memoryTime = origTime
		catalogURL = origURL
		cachePath = origPath
		mu.Unlock()
	})
}

func TestModelsDevColdStartCacheAndDisk(t *testing.T) {
	resetModelsDevCacheTestState(t)

	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "modelsdev_cache.json")
	SetCachePath(cachePath)

	var reqCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"models": {
				"vendor/model-a": {"id": "vendor/model-a", "limit": {"context": 128000}, "modalities": {"input": ["text"]}}
			}
		}`))
	}))
	defer server.Close()

	mu.Lock()
	catalogURL = server.URL
	mu.Unlock()

	// 1. Cold start: Memory and disk are empty. Calling GetCachedCatalog should fetch from network.
	cat := GetCachedCatalog()
	if cat["model-a"] != 128000 {
		t.Fatalf("expected model-a context 128000, got %d", cat["model-a"])
	}
	if atomic.LoadInt32(&reqCount) != 1 {
		t.Fatalf("expected 1 network request on cold start, got %d", atomic.LoadInt32(&reqCount))
	}

	// Verify disk cache file was created and is valid
	diskCache, _, err := loadDiskCache(cachePath)
	if err != nil {
		t.Fatalf("failed to read written disk cache: %v", err)
	}
	if diskCache.catalog["model-a"] != 128000 {
		t.Fatalf("disk cache model-a = %d, want 128000", diskCache.catalog["model-a"])
	}
	if got := diskCache.modalities["model-a"]; len(got) != 1 || got[0] != "text" {
		t.Fatalf("disk cache model-a modalities = %v, want [text]", got)
	}
	mods := GetCachedModalities()
	if !IsTextOnly("model-a", mods) {
		t.Fatal("model-a should be text-only from cached modalities")
	}
	if IsTextOnly("model-b", mods) {
		t.Fatal("unknown model-b should not be text-only")
	}

	// 2. Warm call: In-memory cache hit.
	cat2 := GetCachedCatalog()
	if cat2["model-a"] != 128000 {
		t.Fatalf("expected model-a context 128000 from memory, got %d", cat2["model-a"])
	}
	if atomic.LoadInt32(&reqCount) != 1 {
		t.Fatalf("expected network requests to stay 1 on warm cache hit, got %d", atomic.LoadInt32(&reqCount))
	}
}

func TestModelsDevDiskCacheStaleWhileRevalidate(t *testing.T) {
	resetModelsDevCacheTestState(t)

	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "modelsdev_cache.json")
	SetCachePath(cachePath)

	// Pre-seed disk cache with data 2 hours old (< 24 hours, so stale-while-revalidate)
	diskCache := diskCache{
		UpdatedAt: time.Now().Add(-2 * time.Hour),
		Catalog: Catalog{
			"stale-model": 65536,
		},
	}
	data, _ := json.Marshal(diskCache)
	_ = os.WriteFile(cachePath, data, 0o644)

	var reqCount int32
	refreshDone := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"models": {
				"vendor/refreshed-model": {"id": "vendor/refreshed-model", "limit": {"context": 200000}}
			}
		}`))
		select {
		case refreshDone <- struct{}{}:
		default:
		}
	}))
	defer server.Close()

	mu.Lock()
	catalogURL = server.URL
	mu.Unlock()

	// Calling GetCachedCatalog should immediately return stale disk data
	cat := GetCachedCatalog()
	if cat["stale-model"] != 65536 {
		t.Fatalf("expected immediate stale disk data (65536), got %d", cat["stale-model"])
	}

	// Wait for background revalidation
	select {
	case <-refreshDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background refresh")
	}

	// Give a slight moment for memory and disk to be saved
	time.Sleep(50 * time.Millisecond)

	// Now memory cache should have the new refreshed model
	mu.RLock()
	newCat := cloneCatalog(memoryCache.catalog)
	mu.RUnlock()
	if newCat["refreshed-model"] != 200000 {
		t.Fatalf("expected refreshed-model in memory after async update, got %d", newCat["refreshed-model"])
	}
}

func TestModelsDevNetworkFailureFallback(t *testing.T) {
	resetModelsDevCacheTestState(t)

	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "modelsdev_cache.json")
	SetCachePath(cachePath)

	// Pre-seed disk cache with older data (e.g. 30 hours old)
	diskCache := diskCache{
		UpdatedAt: time.Now().Add(-30 * time.Hour),
		Catalog: Catalog{
			"fallback-model": 50000,
		},
	}
	data, _ := json.Marshal(diskCache)
	_ = os.WriteFile(cachePath, data, 0o644)

	// Server returns 500 Internal Server Error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	mu.Lock()
	catalogURL = server.URL
	mu.Unlock()

	// Despite synchronous network failure, GetCachedCatalog falls back to stale disk cache
	cat := GetCachedCatalog()
	if cat["fallback-model"] != 50000 {
		t.Fatalf("expected fallback disk data (50000), got %d", cat["fallback-model"])
	}
}

func TestRefreshModelsDevCatalogBackground(t *testing.T) {
	resetModelsDevCacheTestState(t)

	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "modelsdev_cache.json")
	SetCachePath(cachePath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"models": {
				"vendor/bg-model": {"id": "vendor/bg-model", "limit": {"context": 524288}}
			}
		}`))
	}))
	defer server.Close()

	mu.Lock()
	catalogURL = server.URL
	mu.Unlock()

	cat, err := RefreshCatalog()
	if err != nil {
		t.Fatalf("RefreshCatalog failed: %v", err)
	}
	if cat["bg-model"] != 524288 {
		t.Fatalf("expected bg-model context 524288, got %d", cat["bg-model"])
	}

	diskCache, _, err := loadDiskCache(cachePath)
	if err != nil {
		t.Fatalf("loadDiskCache failed: %v", err)
	}
	if diskCache.catalog["bg-model"] != 524288 {
		t.Fatalf("diskCache bg-model = %d, want 524288", diskCache.catalog["bg-model"])
	}
}
