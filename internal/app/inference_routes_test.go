package app

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInferenceRoutesRegistered verifies all /inference/* routes are registered
func TestInferenceRoutesRegistered(t *testing.T) {
	mux := buildMux()

	tests := []struct {
		method string
		path   string
	}{
		{"POST", "/inference/openai/v1/chat/completions"},
		{"POST", "/inference/openai/v1/responses"},
		{"POST", "/inference/anthropic/v1/messages"},
		{"POST", "/inference/anthropic/v1/messages/count_tokens"},
		{"GET", "/inference/v1/models"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			body := `{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test")

			mux.ServeHTTP(w, req)

			// Route must be registered (not 404). We expect 401 because we're
			// using a fake auth token, but that proves the route exists and
			// auth ran.
			if w.Code == 404 {
				t.Errorf("route not registered: %s %s returned 404", tt.method, tt.path)
			}
		})
	}
}

// TestBackwardCompatibilityV1Routes verifies old /v1/* routes still work
func TestBackwardCompatibilityV1Routes(t *testing.T) {
	mux := buildMux()

	tests := []struct {
		name string
		path string
	}{
		{"v1_chat", "/v1/chat/completions"},
		{"v1_responses", "/v1/responses"},
		{"v1_messages", "/v1/messages"},
		{"v1_count_tokens", "/v1/messages/count_tokens"},
		{"v1_models", "/v1/models"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			body := `{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`
			method := "POST"
			if tt.path == "/v1/models" {
				method = "GET"
			}
			req := httptest.NewRequest(method, tt.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test")

			mux.ServeHTTP(w, req)

			if w.Code == 404 {
				t.Errorf("%s: route not registered, got 404", tt.name)
			}
		})
	}
}
