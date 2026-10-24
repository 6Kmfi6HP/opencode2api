package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExtractRateLimitReset(t *testing.T) {
	tests := []struct {
		name       string
		header     http.Header
		body       []byte
		wantSubstr string
	}{
		{
			name: "Retry-After in seconds",
			header: http.Header{
				"Retry-After": []string{"120"},
			},
			wantSubstr: "in 2m0s",
		},
		{
			name: "X-Ratelimit-Reset unix timestamp",
			header: http.Header{
				"X-Ratelimit-Reset": []string{"1893456000"}, // 2030-01-01
			},
			wantSubstr: "2030-01-01",
		},
		{
			name: "X-Ratelimit-Reset-Tokens duration",
			header: http.Header{
				"X-Ratelimit-Reset-Tokens": []string{"45s"},
			},
			wantSubstr: "in 45s",
		},
		{
			name:       "Body JSON with resets_at",
			body:       []byte(`{"error":{"type":"rate_limit_error","message":"rate limited","resets_at":1893456000}}`),
			wantSubstr: "2030-01-01",
		},
		{
			name:       "Body JSON with retry_after seconds",
			body:       []byte(`{"error":{"type":"FreeUsageLimitError","message":"rate limited","retry_after":60}}`),
			wantSubstr: "in 1m0s",
		},
		{
			name:       "Body text with try again in Xs",
			body:       []byte(`{"error":{"type":"rate_limit_error","message":"Rate limit exceeded. Try again in 25s."}}`),
			wantSubstr: "in 25s",
		},
		{
			name:       "Body text with resets in minutes",
			body:       []byte(`{"error":{"type":"FreeUsageLimitError","message":"Usage cap reached. Resets in 5 minutes."}}`),
			wantSubstr: "in 5m",
		},
		{
			name:       "No reset info",
			body:       []byte(`{"error":{"type":"invalid_request_error","message":"unknown model"}}`),
			wantSubstr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRateLimitReset(tt.header, tt.body)
			if tt.wantSubstr == "" {
				if got != "" {
					t.Fatalf("expected empty string, got: %q", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantSubstr) {
				t.Fatalf("extractRateLimitReset() = %q, want substring %q", got, tt.wantSubstr)
			}
		})
	}
}

func TestFormatResetTimeValue_HTTPDate(t *testing.T) {
	future := time.Now().Add(10 * time.Minute).UTC()
	httpDate := future.Format(http.TimeFormat)
	got := formatResetTimeValue(httpDate)
	if !strings.Contains(got, future.Format("2006-01-02")) {
		t.Fatalf("expected formatted HTTP date containing date, got %q", got)
	}
}

func TestInjectRateLimitReset(t *testing.T) {
	raw := []byte(`{"error":{"type":"FreeUsageLimitError","message":"limit reached"}}`)
	resetStr := "in 5m (at 2026-10-24 18:00:00 UTC)"
	got := injectRateLimitReset(raw, resetStr)

	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("invalid json output: %v", err)
	}
	if m["rate_limit_reset"] != resetStr {
		t.Fatalf("top-level rate_limit_reset = %v, want %q", m["rate_limit_reset"], resetStr)
	}
	errObj, ok := m["error"].(map[string]any)
	if !ok || errObj["rate_limit_reset"] != resetStr {
		t.Fatalf("error.rate_limit_reset = %v, want %q", errObj["rate_limit_reset"], resetStr)
	}
	if errObj["type"] != "FreeUsageLimitError" || errObj["message"] != "limit reached" {
		t.Fatalf("existing error fields corrupted: %v", errObj)
	}
}

func TestInjectRateLimitResetWithHeader(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"too many requests"}}`)
	header := http.Header{"Retry-After": []string{"30"}}
	got := injectRateLimitResetWithHeader(raw, header)

	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("invalid json output: %v", err)
	}
	topReset, _ := m["rate_limit_reset"].(string)
	if !strings.Contains(topReset, "in 30s") {
		t.Fatalf("expected rate_limit_reset containing 'in 30s', got %q", topReset)
	}
}
