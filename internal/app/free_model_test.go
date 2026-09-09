package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
)

func TestIsFreeModel(t *testing.T) {
	blockModelsDevTransport(t)
	modelsdev.SetFreeModelsForTest("big-pickle")

	tests := []struct {
		id   string
		want bool
	}{
		{"deepseek-v4-flash-free", true}, // explicit -free suffix
		{"mimo-v2.5-free[1m]", true},     // suffix with context marker
		{"big-pickle", true},             // zero-cost in models.dev, no suffix
		{"big-pickle[1m]", true},         // suffix stripped before cost lookup
		{"deepseek-v4-flash", false},     // paid upstream model
		{"unknown-model", false},         // fail closed on unknown
	}
	for _, tc := range tests {
		if got := isFreeModel(tc.id); got != tc.want {
			t.Errorf("isFreeModel(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestListModelsHandlerIncludesSuffixlessFreeModel(t *testing.T) {
	oldModelsCache := modelsCache
	oldGoModelsCache := goModelsCache
	oldModelsLoaded := modelsLoaded
	oldModelAlias := getModelKeywordRules()
	modelMu.Lock()
	modelsCache = []ModelInfo{
		{ID: "big-pickle", Object: "model", OwnedBy: "opencode"},
		{ID: "deepseek-v4-flash", Object: "model", OwnedBy: "opencode"},
	}
	goModelsCache = nil
	modelsLoaded = true
	modelMu.Unlock()
	configMu.Lock()
	modelAliasRules = nil
	configMu.Unlock()
	blockModelsDevTransport(t)
	modelsdev.SetFreeModelsForTest("big-pickle")
	t.Cleanup(func() {
		modelMu.Lock()
		modelsCache = oldModelsCache
		goModelsCache = oldGoModelsCache
		modelsLoaded = oldModelsLoaded
		modelMu.Unlock()
		applyConfig(AppConfig{ModelAlias: oldModelAlias})
	})

	// Public tier (no Authorization): suffix-less zero-cost model must appear.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	listModelsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("listModelsHandler() status = %d, want %d", rec.Code, http.StatusOK)
	}
	var payload struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal models response: %v", err)
	}
	gotIDs := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		gotIDs = append(gotIDs, model.ID)
	}

	found := false
	for _, id := range gotIDs {
		if id == "big-pickle" {
			found = true
		}
		if id == "deepseek-v4-flash" {
			t.Errorf("public tier should not list paid model deepseek-v4-flash; got %v", gotIDs)
		}
	}
	if !found {
		t.Errorf("public tier should list suffix-less free model big-pickle; got %v", gotIDs)
	}
}
