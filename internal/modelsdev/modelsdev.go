// Package modelsdev fetches and caches the models.dev catalog, which maps
// OpenCode-style model IDs to their context-window sizes and input
// modalities. The cache lives in memory and on disk; the HTTP client is
// injected by the caller so upstream proxy/SOCKS5 configuration is respected.
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

// Modalities maps an OpenCode-style model ID to the input modalities the
// models.dev catalog reports for it (e.g. ["text"], ["text", "image"]).
type Modalities map[string][]string

// cache pairs the context-window catalog with input modalities; both come
// from the same fetch, so they always travel together.
type cache struct {
	catalog    Catalog
	modalities Modalities
}

// response is the JSON envelope returned by models.dev.
type response struct {
	Models    map[string]entry    `json:"models"`
	Providers map[string]provider `json:"providers"`
}

type entry struct {
	ID         string     `json:"id"`
	Limit      limit      `json:"limit"`
	Modalities modalities `json:"modalities"`
}

type provider struct {
	Models map[string]entry `json:"models"`
}

type limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type modalities struct {
	Input []string `json:"input"`
}

type diskCache struct {
	UpdatedAt  time.Time  `json:"updated_at"`
	Catalog    Catalog    `json:"catalog"`
	Modalities Modalities `json:"modalities,omitempty"`
}

var (
	mu           sync.RWMutex
	memoryCache  cache
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
	memoryCache = cache{}
	memoryTime = time.Time{}
	cachePath = "modelsdev_cache.json"
	catalogURL = defaultCatalogURL
	clientGetter = func() *http.Client { return http.DefaultClient }
}

// SetModalitiesForTest injects in-memory modalities (with a fresh timestamp)
// so tests can drive IsTextOnly without touching disk or network.
func SetModalitiesForTest(mods Modalities) {
	mu.Lock()
	defer mu.Unlock()
	memoryCache.modalities = cloneModalities(mods)
	memoryTime = time.Now()
}

// ClearModalitiesForTest drops only the in-memory catalog/modalities state,
// leaving the injected client getter and URL untouched.
func ClearModalitiesForTest() {
	mu.Lock()
	defer mu.Unlock()
	memoryCache = cache{}
	memoryTime = time.Time{}
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

func cloneModalities(src Modalities) Modalities {
	if src == nil {
		return nil
	}
	dst := make(Modalities, len(src))
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	return dst
}

func cloneCache(src cache) cache {
	return cache{catalog: cloneCatalog(src.catalog), modalities: cloneModalities(src.modalities)}
}

func loadDiskCache(path string) (cache, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cache{}, time.Time{}, err
	}
	var dc diskCache
	if err := json.Unmarshal(data, &dc); err != nil {
		return cache{}, time.Time{}, err
	}
	c := cache{catalog: dc.Catalog, modalities: dc.Modalities}
	if c.catalog == nil {
		c.catalog = Catalog{}
	}
	if c.modalities == nil {
		c.modalities = Modalities{}
	}
	return c, dc.UpdatedAt, nil
}

func saveDiskCache(path string, c cache) error {
	if path == "" {
		return fmt.Errorf("empty cache path")
	}
	dc := diskCache{UpdatedAt: time.Now(), Catalog: c.catalog, Modalities: c.modalities}
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
// OpenCode-style model IDs to their context window sizes, plus the reported
// input modalities per model.
func fetchCatalog() (cache, error) {
	mu.RLock()
	url := catalogURL + fmt.Sprintf("?v=%d", time.Now().UnixNano())
	getter := clientGetter
	mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return cache{}, err
	}

	resp, err := getter().Do(req)
	if err != nil {
		return cache{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cache{}, fmt.Errorf("models.dev returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return cache{}, err
	}
	var parsed response
	if err := json.Unmarshal(body, &parsed); err != nil {
		return cache{}, err
	}
	c := cache{
		catalog:    make(Catalog, len(parsed.Models)),
		modalities: make(Modalities, len(parsed.Models)),
	}

	for id, e := range parsed.Models {
		short := id
		if idx := strings.Index(id, "/"); idx >= 0 {
			short = id[idx+1:]
		}
		if e.Limit.Context > 0 {
			c.catalog[short] = e.Limit.Context
		}
		if len(e.Modalities.Input) > 0 {
			c.modalities[short] = append([]string(nil), e.Modalities.Input...)
		}
	}
	for _, prov := range parsed.Providers {
		for id, e := range prov.Models {
			if e.Limit.Context > 0 {
				c.catalog[id] = e.Limit.Context
			}
			if len(e.Modalities.Input) > 0 {
				c.modalities[id] = append([]string(nil), e.Modalities.Input...)
			}
		}
	}
	return c, nil
}

// FetchCatalog downloads the catalog without touching the memory or disk cache.
func FetchCatalog() (Catalog, error) {
	c, err := fetchCatalog()
	return c.catalog, err
}

// RefreshCatalog fetches the latest catalog and updates both caches.
func RefreshCatalog() (Catalog, error) {
	c, err := fetchCatalog()
	if err != nil {
		return nil, err
	}
	mu.Lock()
	memoryCache = cloneCache(c)
	memoryTime = time.Now()
	path := cachePath
	mu.Unlock()

	if err := saveDiskCache(path, c); err != nil {
		slog.Warn("failed to save models.dev disk cache", "path", path, "error", err)
	}
	return c.catalog, nil
}

// getCached retrieves the full cache from memory, disk, or the network,
// applying stale-while-revalidate when the disk cache is reasonably fresh.
func getCached() cache {
	mu.RLock()
	memCache := cloneCache(memoryCache)
	memTime := memoryTime
	path := cachePath
	mu.RUnlock()

	if len(memCache.catalog)+len(memCache.modalities) > 0 && time.Since(memTime) < memoryTTL {
		return memCache
	}

	diskCache, diskTime, diskErr := loadDiskCache(path)
	// Treat the disk entry as fresh when it carries any usable payload.
	if diskErr == nil && (len(diskCache.catalog) > 0 || len(diskCache.modalities) > 0) {
		age := time.Since(diskTime)
		if age < diskTTL {
			mu.Lock()
			memoryCache = cloneCache(diskCache)
			memoryTime = diskTime
			mu.Unlock()

			if age > memoryTTL {
				go func() {
					if _, rErr := RefreshCatalog(); rErr != nil {
						slog.Warn("async refresh of models.dev catalog failed", "error", rErr)
					}
				}()
			}
			return diskCache
		}
	}

	freshCache, fetchErr := fetchCatalog()
	if fetchErr == nil && len(freshCache.catalog) > 0 {
		mu.Lock()
		memoryCache = cloneCache(freshCache)
		memoryTime = time.Now()
		mu.Unlock()

		if err := saveDiskCache(path, freshCache); err != nil {
			slog.Warn("failed to save models.dev disk cache", "path", path, "error", err)
		}
		return freshCache
	}

	if diskErr == nil && (len(diskCache.catalog) > 0 || len(diskCache.modalities) > 0) {
		slog.Warn("failed to fetch fresh models.dev catalog, falling back to disk cache", "error", fetchErr)
		return diskCache
	}
	if len(memCache.catalog)+len(memCache.modalities) > 0 {
		slog.Warn("failed to fetch fresh models.dev catalog, falling back to memory cache", "error", fetchErr)
		return memCache
	}
	if fetchErr != nil {
		slog.Warn("failed to fetch models.dev catalog and no cache available", "error", fetchErr)
	}
	return cache{}
}

// GetCachedCatalog retrieves the catalog from memory, disk, or the network,
// applying stale-while-revalidate when the disk cache is reasonably fresh.
func GetCachedCatalog() Catalog {
	return getCached().catalog
}

// GetCachedModalities retrieves the per-model input modalities through the
// same memory/disk/network path as GetCachedCatalog.
func GetCachedModalities() Modalities {
	return getCached().modalities
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

// IsTextOnly reports whether the models.dev data marks the model as accepting
// text input only. Matching uses the same exact / strip-"-free" /
// add-"-free" strategies as ContextWindow. A model is only text-only when the
// catalog knows it and reports a non-empty input list containing nothing but
// "text"; unknown models return false so callers fail open.
func IsTextOnly(modelID string, mods Modalities) bool {
	input, ok := lookupModalities(modelID, mods)
	if !ok || len(input) == 0 {
		return false
	}
	for _, m := range input {
		if !strings.EqualFold(strings.TrimSpace(m), "text") {
			return false
		}
	}
	return true
}

func lookupModalities(modelID string, mods Modalities) ([]string, bool) {
	if input, ok := mods[modelID]; ok {
		return input, true
	}
	if base := strings.TrimSuffix(modelID, "-free"); base != modelID {
		if input, ok := mods[base]; ok {
			return input, true
		}
	}
	if !strings.HasSuffix(modelID, "-free") {
		if input, ok := mods[modelID+"-free"]; ok {
			return input, true
		}
	}
	return nil, false
}
