package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func resetModelsDevCacheTestState(t *testing.T) {
	t.Helper()
	modelsDevMu.Lock()
	origCache := modelsDevMemoryCache
	origTime := modelsDevMemoryTime
	origURL := modelsDevCatalogURL
	origPath := modelsDevCachePath
	modelsDevMemoryCache = nil
	modelsDevMemoryTime = time.Time{}
	modelsDevMu.Unlock()

	t.Cleanup(func() {
		modelsDevMu.Lock()
		modelsDevMemoryCache = origCache
		modelsDevMemoryTime = origTime
		modelsDevCatalogURL = origURL
		modelsDevCachePath = origPath
		modelsDevMu.Unlock()
	})
}

func TestModelsDevProxyRouting(t *testing.T) {
	resetModelsDevCacheTestState(t)

	var proxyHits int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&proxyHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"models": {
				"test/proxy-model": {"id": "test/proxy-model", "limit": {"context": 32768}}
			}
		}`))
	}))
	defer proxyServer.Close()

	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	socks5Mu.Lock()
	oldProxies := socks5Proxies
	oldActive := activeSocks5
	socks5Proxies = nil
	activeSocks5 = ""
	socks5Mu.Unlock()

	oldTransport := httpClient.Transport
	httpClient.Transport = &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}
	t.Cleanup(func() {
		socks5Mu.Lock()
		socks5Proxies = oldProxies
		activeSocks5 = oldActive
		socks5Mu.Unlock()
		httpClient.Transport = oldTransport
	})

	modelsDevMu.Lock()
	modelsDevCatalogURL = "http://models.dev/catalog.json"
	modelsDevMu.Unlock()

	cat, err := fetchModelsDevCatalog()
	if err != nil {
		t.Fatalf("fetchModelsDevCatalog failed: %v", err)
	}

	if hits := atomic.LoadInt32(&proxyHits); hits == 0 {
		t.Fatalf("expected proxy hits > 0, got %d", hits)
	}

	if ctx := cat["proxy-model"]; ctx != 32768 {
		t.Fatalf("catalog[proxy-model] = %d, want 32768", ctx)
	}
}

func TestModelsDevColdStartCacheAndDisk(t *testing.T) {
	resetModelsDevCacheTestState(t)

	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "modelsdev_cache.json")
	setModelsDevCachePath(cachePath)

	var reqCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"models": {
				"vendor/model-a": {"id": "vendor/model-a", "limit": {"context": 128000}}
			}
		}`))
	}))
	defer server.Close()

	modelsDevMu.Lock()
	modelsDevCatalogURL = server.URL
	modelsDevMu.Unlock()

	// 1. Cold start: Memory and disk are empty. Calling getCachedModelsDevCatalog should fetch from network.
	cat := getCachedModelsDevCatalog()
	if cat["model-a"] != 128000 {
		t.Fatalf("expected model-a context 128000, got %d", cat["model-a"])
	}
	if atomic.LoadInt32(&reqCount) != 1 {
		t.Fatalf("expected 1 network request on cold start, got %d", atomic.LoadInt32(&reqCount))
	}

	// Verify disk cache file was created and is valid
	diskCat, _, err := loadModelsDevDiskCache(cachePath)
	if err != nil {
		t.Fatalf("failed to read written disk cache: %v", err)
	}
	if diskCat["model-a"] != 128000 {
		t.Fatalf("disk cache model-a = %d, want 128000", diskCat["model-a"])
	}

	// 2. Warm call: In-memory cache hit.
	cat2 := getCachedModelsDevCatalog()
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
	setModelsDevCachePath(cachePath)

	// Pre-seed disk cache with data 2 hours old (< 24 hours, so stale-while-revalidate)
	diskCache := modelsDevDiskCache{
		UpdatedAt: time.Now().Add(-2 * time.Hour),
		Catalog: modelsDevCatalog{
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

	modelsDevMu.Lock()
	modelsDevCatalogURL = server.URL
	modelsDevMu.Unlock()

	// Calling getCachedModelsDevCatalog should immediately return stale disk data
	cat := getCachedModelsDevCatalog()
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
	modelsDevMu.RLock()
	newCat := cloneModelsDevCatalog(modelsDevMemoryCache)
	modelsDevMu.RUnlock()
	if newCat["refreshed-model"] != 200000 {
		t.Fatalf("expected refreshed-model in memory after async update, got %d", newCat["refreshed-model"])
	}
}

func TestModelsDevNetworkFailureFallback(t *testing.T) {
	resetModelsDevCacheTestState(t)

	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "modelsdev_cache.json")
	setModelsDevCachePath(cachePath)

	// Pre-seed disk cache with older data (e.g. 30 hours old)
	diskCache := modelsDevDiskCache{
		UpdatedAt: time.Now().Add(-30 * time.Hour),
		Catalog: modelsDevCatalog{
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

	modelsDevMu.Lock()
	modelsDevCatalogURL = server.URL
	modelsDevMu.Unlock()

	// Despite synchronous network failure, getCachedModelsDevCatalog falls back to stale disk cache
	cat := getCachedModelsDevCatalog()
	if cat["fallback-model"] != 50000 {
		t.Fatalf("expected fallback disk data (50000), got %d", cat["fallback-model"])
	}
}

func TestRefreshModelsDevCatalogBackground(t *testing.T) {
	resetModelsDevCacheTestState(t)

	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "modelsdev_cache.json")
	setModelsDevCachePath(cachePath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"models": {
				"vendor/bg-model": {"id": "vendor/bg-model", "limit": {"context": 524288}}
			}
		}`))
	}))
	defer server.Close()

	modelsDevMu.Lock()
	modelsDevCatalogURL = server.URL
	modelsDevMu.Unlock()

	cat, err := refreshModelsDevCatalogBackground()
	if err != nil {
		t.Fatalf("refreshModelsDevCatalogBackground failed: %v", err)
	}
	if cat["bg-model"] != 524288 {
		t.Fatalf("expected bg-model context 524288, got %d", cat["bg-model"])
	}

	diskCat, _, err := loadModelsDevDiskCache(cachePath)
	if err != nil {
		t.Fatalf("loadModelsDevDiskCache failed: %v", err)
	}
	if diskCat["bg-model"] != 524288 {
		t.Fatalf("diskCache bg-model = %d, want 524288", diskCat["bg-model"])
	}
}
