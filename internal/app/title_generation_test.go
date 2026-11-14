package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSynthesizeTitle_FirstUserTurn(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You generate a descriptive title for this conversation."},
			map[string]any{"role": "user", "content": "Please help me debug a memory leak in the Go gateway with pprof"},
		},
	}
	if got, want := synthesizeTitle(payload), "Please help me debug a memory"; got != want {
		t.Fatalf("synthesizeTitle = %q, want %q", got, want)
	}
}

func TestSynthesizeTitle_StripsV1Prompt(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "Generate a title for this conversation\n\nFix free tier 429"},
		},
	}
	if got, want := synthesizeTitle(payload), "Fix free tier 429"; got != want {
		t.Fatalf("synthesizeTitle = %q, want %q", got, want)
	}
}

func TestSynthesizeTitle_Default(t *testing.T) {
	if got := synthesizeTitle(map[string]any{"messages": []any{}}); got != defaultTitle {
		t.Fatalf("synthesizeTitle = %q, want %q", got, defaultTitle)
	}
}

func TestWriteSynthesizedTitle_NonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSynthesizedTitle(rec, "big-pickle-free", "Fix free tier 429", false)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("response = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	choices := body["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "Fix free tier 429" {
		t.Fatalf("content = %#v", message["content"])
	}
}

func TestWriteSynthesizedTitle_Stream(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSynthesizedTitle(rec, "big-pickle-free", "Fix free tier 429", true)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Fix free tier 429"`) {
		t.Fatalf("stream missing content: %s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream missing [DONE]: %s", body)
	}
}

func TestWriteTitleFallbackIfNeeded(t *testing.T) {
	raw := []byte(`{
		"model": "big-pickle-free",
		"stream": false,
		"messages": [
			{"role": "system", "content": "You generate a descriptive title for this conversation."},
			{"role": "user", "content": "hello"}
		]
	}`)
	rec := httptest.NewRecorder()
	if !writeTitleFallbackIfNeeded(rec, "big-pickle-free", raw, 429, false) {
		t.Fatal("429 title request did not fall back")
	}
	if !strings.Contains(rec.Body.String(), `"content":"hello"`) {
		t.Fatalf("fallback body = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	if writeTitleFallbackIfNeeded(rec, "big-pickle-free", raw, 500, false) {
		t.Fatal("non-429 request must not fall back")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("failed fallback wrote %q", rec.Body.String())
	}
}

func TestIsTitleGenerationRequest_RejectsAgentHistory(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You generate a descriptive title for this conversation."},
			map[string]any{"role": "assistant", "content": "previous answer"},
		},
	}
	if isTitleGenerationRequest(payload) {
		t.Fatal("agent history must not be classified as a title request")
	}
}

const titleRequestBody = `{
	"model": "fallback-model-free",
	"stream": %s,
	"messages": [
		{"role": "system", "content": "You generate a descriptive title for this conversation."},
		{"role": "user", "content": "Fix the free tier 429"}
	]
}`

func TestChatCompletionsHandler_Title429FallsBack(t *testing.T) {
	stubTitleRetryTiming(t)
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
		{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
		{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
		{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(strings.Replace(titleRequestBody, "%s", "false", 1)))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"Fix the free tier 429"`) {
		t.Fatalf("fallback body = %s", rec.Body.String())
	}
}

func TestChatCompletionsHandler_Title429StreamFallsBack(t *testing.T) {
	stubTitleRetryTiming(t)
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
		{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
		{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
		{status: http.StatusTooManyRequests, body: `{"error":"rate_limited"}`},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(strings.Replace(titleRequestBody, "%s", "true", 1)))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Fix the free tier 429"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("fallback stream = %s", body)
	}
}

func stubTitleRetryTiming(t *testing.T) {
	t.Helper()
	stubRetryConfig(t, 0, 5000)
	old := upstream429Backoff
	upstream429Backoff = func(int, string) time.Duration { return 0 }
	t.Cleanup(func() { upstream429Backoff = old })
}
