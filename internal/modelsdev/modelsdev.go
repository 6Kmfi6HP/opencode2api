// Package modelsdev fetches and caches the models.dev catalog, which maps
// OpenCode-style model IDs to their context-window sizes. The cache lives in
// memory and on disk; the HTTP client is injected by the caller so upstream
// proxy/SOCKS5 configuration is respected.
package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultCatalogURL = "https://models.dev/catalog.json"
	memoryTTL         = 1 * time.Hour
	diskTTL           = 24 * time.Hour
)

// Catalog maps an OpenCode-style model ID (the suffix after the provider "/")
// to its context window size in tokens.
type Catalog map[string]int

// response is the JSON envelope returned by models.dev.
type response struct {
	Models    map[string]entry    `json:"models"`
	Providers map[string]provider `json:"providers"`
}

type entry struct {
	ID    string `json:"id"`
	Limit limit  `json:"limit"`
}

type provider struct {
	Models map[string]entry `json:"models"`
}

type limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type diskCache struct {
	UpdatedAt time.Time `json:"updated_at"`
	Catalog   Catalog   `json:"catalog"`
}

var (
	mu           sync.RWMutex
	memoryCache  Catalog
	memoryTime   time.Time
	cachePath    = "modelsdev_cache.json"
	catalogURL   = defaultCatalogURL
	clientGetter = func() *http.Client { return http.DefaultClient }
)

// SetCachePath updates the file path used to persist the models.dev cache.
func SetCachePath(path string) {
	mu.Lock()
	defer mu.Unlock()
	if path != "" {
		cachePath = path
	}
}

// GetCachePath returns the currently configured cache file path.
func GetCachePath() string {
	mu.RLock()
	defer mu.RUnlock()
	return cachePath
}

// SetCatalogURL overrides the catalog endpoint (used by tests).
func SetCatalogURL(url string) {
	mu.Lock()
	defer mu.Unlock()
	catalogURL = url
}

// SetClientGetter injects the HTTP client factory; the returned client must
// honor the caller's proxy/SOCKS5 configuration.
func SetClientGetter(getter func() *http.Client) {
	mu.Lock()
	defer mu.Unlock()
	if getter != nil {
		clientGetter = getter
	}
}

// ResetForTest clears the in-memory cache and restores default path/URL so a
// test starts from a known state.
func ResetForTest() {
	mu.Lock()
	defer mu.Unlock()
	memoryCache = nil
	memoryTime = time.Time{}
	cachePath = "modelsdev_cache.json"
	catalogURL = defaultCatalogURL
	clientGetter = func() *http.Client { return http.DefaultClient }
}

func cloneCatalog(src Catalog) Catalog {
	if src == nil {
		return nil
	}
	dst := make(Catalog, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func loadDiskCache(path string) (Catalog, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	var dc diskCache
	if err := json.Unmarshal(data, &dc); err != nil {
		return nil, time.Time{}, err
	}
	if dc.Catalog == nil {
		dc.Catalog = Catalog{}
	}
	return dc.Catalog, dc.UpdatedAt, nil
}

func saveDiskCache(path string, cat Catalog) error {
	if path == "" {
		return fmt.Errorf("empty cache path")
	}
	dc := diskCache{UpdatedAt: time.Now(), Catalog: cat}
	data, err := json.MarshalIndent(dc, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

// fetchCatalog downloads the models.dev catalog and builds a map from
// OpenCode-style model IDs to their context window sizes.
func fetchCatalog() (Catalog, error) {
	mu.RLock()
	url := catalogURL + fmt.Sprintf("?v=%d", time.Now().UnixNano())
	getter := clientGetter
	mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Catalog{}, err
	}

	resp, err := getter().Do(req)
	if err != nil {
		return Catalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Catalog{}, fmt.Errorf("models.dev returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Catalog{}, err
	}
	var parsed response
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Catalog{}, err
	}
	catalog := make(Catalog, len(parsed.Models))

	for id, e := range parsed.Models {
		short := id
		if idx := strings.Index(id, "/"); idx >= 0 {
			short = id[idx+1:]
		}
		if e.Limit.Context > 0 {
			catalog[short] = e.Limit.Context
		}
	}
	for _, prov := range parsed.Providers {
		for id, e := range prov.Models {
			if e.Limit.Context > 0 {
				catalog[id] = e.Limit.Context
			}
		}
	}
	return catalog, nil
}

// FetchCatalog downloads the catalog without touching the memory or disk cache.
func FetchCatalog() (Catalog, error) { return fetchCatalog() }

// RefreshCatalog fetches the latest catalog and updates both caches.
func RefreshCatalog() (Catalog, error) {
	cat, err := fetchCatalog()
	if err != nil {
		return nil, err
	}
	mu.Lock()
	memoryCache = cloneCatalog(cat)
	memoryTime = time.Now()
	path := cachePath
	mu.Unlock()

	if err := saveDiskCache(path, cat); err != nil {
		slog.Warn("failed to save models.dev disk cache", "path", path, "error", err)
	}
	return cat, nil
}

// GetCachedCatalog retrieves the catalog from memory, disk, or the network,
// applying stale-while-revalidate when the disk cache is reasonably fresh.
func GetCachedCatalog() Catalog {
	mu.RLock()
	memCache := cloneCatalog(memoryCache)
	memTime := memoryTime
	path := cachePath
	mu.RUnlock()

	if len(memCache) > 0 && time.Since(memTime) < memoryTTL {
		return memCache
	}

	diskCat, diskTime, diskErr := loadDiskCache(path)
	if diskErr == nil && len(diskCat) > 0 {
		age := time.Since(diskTime)
		if age < diskTTL {
			mu.Lock()
			memoryCache = cloneCatalog(diskCat)
			memoryTime = diskTime
			mu.Unlock()

			if age > memoryTTL {
				go func() {
					if _, rErr := RefreshCatalog(); rErr != nil {
						slog.Warn("async refresh of models.dev catalog failed", "error", rErr)
					}
				}()
			}
			return diskCat
		}
	}

	freshCat, fetchErr := fetchCatalog()
	if fetchErr == nil && len(freshCat) > 0 {
		mu.Lock()
		memoryCache = cloneCatalog(freshCat)
		memoryTime = time.Now()
		mu.Unlock()

		if err := saveDiskCache(path, freshCat); err != nil {
			slog.Warn("failed to save models.dev disk cache", "path", path, "error", err)
		}
		return freshCat
	}

	if diskErr == nil && len(diskCat) > 0 {
		slog.Warn("failed to fetch fresh models.dev catalog, falling back to disk cache", "error", fetchErr)
		return diskCat
	}
	if len(memCache) > 0 {
		slog.Warn("failed to fetch fresh models.dev catalog, falling back to memory cache", "error", fetchErr)
		return memCache
	}
	if fetchErr != nil {
		slog.Warn("failed to fetch models.dev catalog and no cache available", "error", fetchErr)
	}
	return Catalog{}
}

// ContextWindow looks up the context window for a model ID in the catalog,
// trying exact, strip-"-free", and add-"-free" matching in order.
func ContextWindow(modelID string, catalog Catalog) int {
	if ctx, ok := catalog[modelID]; ok {
		return ctx
	}
	if base := strings.TrimSuffix(modelID, "-free"); base != modelID {
		if ctx, ok := catalog[base]; ok {
			return ctx
		}
	}
	if !strings.HasSuffix(modelID, "-free") {
		if ctx, ok := catalog[modelID+"-free"]; ok {
			return ctx
		}
	}
	return 0
}
