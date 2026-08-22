package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFetchModelsDevCatalogParsing verifies that the JSON parser correctly
// extracts context windows from both the top-level "models" section and the
// nested "providers.*.models" section of the models.dev catalog.
func TestFetchModelsDevCatalogParsing(t *testing.T) {
	// This mirrors the real catalog structure: x-preview-f-free appears only
	// under providers.opencode.models, not in the top-level models map.
	raw := `{
  "models": {
    "xai/grok-4.3": {"id":"xai/grok-4.3","limit":{"context":1000000,"output":131072}},
    "deepseek/deepseek-v4-flash": {"id":"deepseek/deepseek-v4-flash","limit":{"context":131072,"output":64000}}
  },
  "providers": {
    "opencode": {
      "id": "opencode",
      "models": {
        "gemini-3-pro": {"id":"gemini-3-pro","limit":{"context":1048576,"output":65536}},
        "x-preview-f-free": {"id":"x-preview-f-free","limit":{"context":1000000,"output":131072}},
        "deepseek-v4-flash": {"id":"deepseek-v4-flash","limit":{"context":1000000,"output":384000}}
      }
    }
  }
}`

	var parsed modelsDevResponse
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&parsed); err != nil {
		t.Fatalf("failed to parse test JSON: %v", err)
	}

	// Replicate the catalog-building logic from fetchModelsDevCatalog.
	catalog := make(modelsDevCatalog, len(parsed.Models))
	for id, entry := range parsed.Models {
		short := id
		if idx := strings.Index(id, "/"); idx >= 0 {
			short = id[idx+1:]
		}
		if entry.Limit.Context > 0 {
			catalog[short] = entry.Limit.Context
		}
	}
	for _, prov := range parsed.Providers {
		for id, entry := range prov.Models {
			if entry.Limit.Context > 0 {
				catalog[id] = entry.Limit.Context
			}
		}
	}

	// Top-level models section entries (stripped provider prefix).
	if ctx := catalog["grok-4.3"]; ctx != 1000000 {
		t.Errorf("catalog[grok-4.3] = %d, want 1000000", ctx)
	}
	if ctx := catalog["deepseek-v4-flash"]; ctx != 1000000 {
		t.Errorf("catalog[deepseek-v4-flash] = %d, want 1000000 (provider section should override top-level 131072)", ctx)
	}

	// Provider-nested model IDs (no provider prefix).
	if ctx := catalog["x-preview-f-free"]; ctx != 1000000 {
		t.Errorf("catalog[x-preview-f-free] = %d, want 1000000", ctx)
	}
	if ctx := catalog["gemini-3-pro"]; ctx != 1048576 {
		t.Errorf("catalog[gemini-3-pro] = %d, want 1048576", ctx)
	}

	// Verify getContextWindow finds x-preview-f-free via the add-"-free" strategy.
	if ctx := getContextWindow("x-preview-f", catalog); ctx != 1000000 {
		t.Errorf("getContextWindow(x-preview-f) = %d, want 1000000", ctx)
	}
}
