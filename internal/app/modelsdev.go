package app

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

// modelsDevCatalogURL is the endpoint providing context-window metadata for
// every known model. The JSON shape is:
//
//	{"models":{"provider/model-id":{...,"limit":{"context":N}}, ...},
//	 "providers":{"opencode":{"models":{"model-id":{...,"limit":{"context":N}}, ...}}, ...}}
//
// We build a map keyed by the model ID after stripping any "provider/" prefix,
// matching the IDs returned by the OpenCode upstream model catalog.
var modelsDevCatalogURL = "https://models.dev/catalog.json"

// modelsDevCatalog maps an OpenCode-style model ID (the suffix after the
// provider "/") to its context window size in tokens.
type modelsDevCatalog map[string]int

// modelsDevResponse is the JSON envelope returned by models.dev.
type modelsDevResponse struct {
	Models    map[string]modelsDevEntry    `json:"models"`
	Providers map[string]modelsDevProvider `json:"providers"`
}

type modelsDevEntry struct {
	ID    string         `json:"id"`
	Limit modelsDevLimit `json:"limit"`
}

type modelsDevProvider struct {
	Models map[string]modelsDevEntry `json:"models"`
}

type modelsDevLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type modelsDevDiskCache struct {
	UpdatedAt time.Time        `json:"updated_at"`
	Catalog   modelsDevCatalog `json:"catalog"`
}

const (
	modelsDevMemoryTTL = 1 * time.Hour
	modelsDevDiskTTL   = 24 * time.Hour
)

var (
	modelsDevMu          sync.RWMutex
	modelsDevMemoryCache modelsDevCatalog
	modelsDevMemoryTime  time.Time
	modelsDevCachePath   = "modelsdev_cache.json"
)

// setModelsDevCachePath updates the file path used to persist models.dev catalog cache.
func setModelsDevCachePath(path string) {
	modelsDevMu.Lock()
	defer modelsDevMu.Unlock()
	if path != "" {
		modelsDevCachePath = path
	}
}

// getModelsDevCachePath returns the currently configured models.dev cache file path.
func getModelsDevCachePath() string {
	modelsDevMu.RLock()
	defer modelsDevMu.RUnlock()
	return modelsDevCachePath
}

func cloneModelsDevCatalog(src modelsDevCatalog) modelsDevCatalog {
	if src == nil {
		return nil
	}
	dst := make(modelsDevCatalog, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func loadModelsDevDiskCache(path string) (modelsDevCatalog, time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	var diskCache modelsDevDiskCache
	if err := json.Unmarshal(data, &diskCache); err != nil {
		return nil, time.Time{}, err
	}
	if diskCache.Catalog == nil {
		diskCache.Catalog = modelsDevCatalog{}
	}
	return diskCache.Catalog, diskCache.UpdatedAt, nil
}

func saveModelsDevDiskCache(path string, cat modelsDevCatalog) error {
	if path == "" {
		return fmt.Errorf("empty cache path")
	}
	diskCache := modelsDevDiskCache{
		UpdatedAt: time.Now(),
		Catalog:   cat,
	}
	data, err := json.MarshalIndent(diskCache, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal models.dev cache: %w", err)
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create cache directory %s: %w", dir, err)
		}
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return fmt.Errorf("write tmp models.dev cache: %w", err)
	}
	if err := os.Rename(tmpFile, path); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("rename models.dev cache to %s: %w", path, err)
	}
	return nil
}

// fetchModelsDevCatalog downloads the models.dev catalog and builds a map
// from OpenCode-style model IDs to their context window sizes. It uses getHTTPClient()
// with a 5s context timeout.
//
// A random query parameter is appended to the URL to bypass CDN edge caches.
func fetchModelsDevCatalog() (modelsDevCatalog, error) {
	cacheBustURL := fmt.Sprintf("%s?v=%d", modelsDevCatalogURL, time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cacheBustURL, nil)
	if err != nil {
		return modelsDevCatalog{}, err
	}

	client := getHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return modelsDevCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return modelsDevCatalog{}, fmt.Errorf("models.dev returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return modelsDevCatalog{}, err
	}
	var parsed modelsDevResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return modelsDevCatalog{}, err
	}
	catalog := make(modelsDevCatalog, len(parsed.Models))

	// Top-level "models" section — keys are "provider/model-id".
	for id, entry := range parsed.Models {
		short := id
		if idx := strings.Index(id, "/"); idx >= 0 {
			short = id[idx+1:]
		}
		if entry.Limit.Context > 0 {
			catalog[short] = entry.Limit.Context
		}
	}

	// "providers" section — each provider has a nested "models" map whose
	// keys are bare model IDs without a "provider/" prefix.
	for _, prov := range parsed.Providers {
		for id, entry := range prov.Models {
			if entry.Limit.Context > 0 {
				catalog[id] = entry.Limit.Context
			}
		}
	}

	return catalog, nil
}

// refreshModelsDevCatalogBackground fetches the latest models.dev catalog and updates
// both memory and disk caches.
func refreshModelsDevCatalogBackground() (modelsDevCatalog, error) {
	cat, err := fetchModelsDevCatalog()
	if err != nil {
		return nil, err
	}

	modelsDevMu.Lock()
	modelsDevMemoryCache = cloneModelsDevCatalog(cat)
	modelsDevMemoryTime = time.Now()
	path := modelsDevCachePath
	modelsDevMu.Unlock()

	if err := saveModelsDevDiskCache(path, cat); err != nil {
		slog.Warn("failed to save models.dev disk cache", "path", path, "error", err)
	}
	return cat, nil
}

// getCachedModelsDevCatalog retrieves the models.dev catalog from memory cache,
// disk cache, or by fetching it from the network. It applies stale-while-revalidate
// logic when the disk cache is reasonably fresh, and falls back to stale cache on network errors.
func getCachedModelsDevCatalog() modelsDevCatalog {
	modelsDevMu.RLock()
	memCache := cloneModelsDevCatalog(modelsDevMemoryCache)
	memTime := modelsDevMemoryTime
	path := modelsDevCachePath
	modelsDevMu.RUnlock()

	if len(memCache) > 0 && time.Since(memTime) < modelsDevMemoryTTL {
		return memCache
	}

	diskCat, diskTime, diskErr := loadModelsDevDiskCache(path)
	if diskErr == nil && len(diskCat) > 0 {
		age := time.Since(diskTime)
		if age < modelsDevDiskTTL {
			modelsDevMu.Lock()
			modelsDevMemoryCache = cloneModelsDevCatalog(diskCat)
			modelsDevMemoryTime = diskTime
			modelsDevMu.Unlock()

			if age > modelsDevMemoryTTL {
				go func() {
					if _, rErr := refreshModelsDevCatalogBackground(); rErr != nil {
						slog.Warn("async refresh of models.dev catalog failed", "error", rErr)
					}
				}()
			}
			return diskCat
		}
	}

	freshCat, fetchErr := fetchModelsDevCatalog()
	if fetchErr == nil && len(freshCat) > 0 {
		modelsDevMu.Lock()
		modelsDevMemoryCache = cloneModelsDevCatalog(freshCat)
		modelsDevMemoryTime = time.Now()
		modelsDevMu.Unlock()

		if err := saveModelsDevDiskCache(path, freshCat); err != nil {
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
	return modelsDevCatalog{}
}

// getContextWindow looks up the context window for the given model ID in the
// catalog. It tries three matching strategies in order:
//  1. Exact match (e.g. "deepseek-v4-flash").
//  2. Strip "-free" suffix: "deepseek-v4-flash-free" → "deepseek-v4-flash".
//  3. Add "-free" suffix: "x-preview-f" → "x-preview-f-free" (the TUI strips
//     "-free" for display; this re-adds it so the upstream free-variant ID can
//     still match a catalog entry).
//
// Returns 0 when the context window is unknown (not found by any strategy).
func getContextWindow(modelID string, catalog modelsDevCatalog) int {
	// 1. Exact match.
	if ctx, ok := catalog[modelID]; ok {
		return ctx
	}
	// 2. Strip "-free" suffix (e.g. "deepseek-v4-flash-free" → "deepseek-v4-flash").
	if base := strings.TrimSuffix(modelID, "-free"); base != modelID {
		if ctx, ok := catalog[base]; ok {
			return ctx
		}
	}
	// 3. Add "-free" suffix (e.g. "x-preview-f" → "x-preview-f-free").
	if !strings.HasSuffix(modelID, "-free") {
		if ctx, ok := catalog[modelID+"-free"]; ok {
			return ctx
		}
	}
	return 0
}
