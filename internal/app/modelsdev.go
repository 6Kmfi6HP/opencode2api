package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
const modelsDevCatalogURL = "https://models.dev/catalog.json"

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

// fetchModelsDevCatalog downloads the models.dev catalog and builds a map
// from OpenCode-style model IDs to their context window sizes. The HTTP
// timeout is 5 s; on any error an empty map is returned so callers can treat
// every model as "unknown context" without blocking startup.
//
// A random query parameter is appended to the URL to bypass CDN edge caches
// (models.dev is served via Cloudflare with max-age=0,must-revalidate, but edge
// nodes may serve stale copies for newly added models like x-preview-f-free).
func fetchModelsDevCatalog() (modelsDevCatalog, error) {
	cacheBustURL := fmt.Sprintf("%s?v=%d", modelsDevCatalogURL, time.Now().UnixNano())
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(cacheBustURL)
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
	// keys are bare model IDs without a "provider/" prefix (e.g. the
	// "opencode" provider lists "x-preview-f-free", "gemini-3-pro", etc.).
	// These override top-level entries on key collision because they are
	// more specific to the provider we actually use.
	for _, prov := range parsed.Providers {
		for id, entry := range prov.Models {
			if entry.Limit.Context > 0 {
				catalog[id] = entry.Limit.Context
			}
		}
	}

	return catalog, nil
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
