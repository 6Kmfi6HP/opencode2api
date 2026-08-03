package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test_ClaudeMessages_stream_keeps_usage_when_upstream_sends_usage_after_finish(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body: strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":null}]}`,
			``,
			// OpenAI include_usage pattern: finish first, usage-only chunk later
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			``,
			`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":35,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":12}}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"primary-model","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	events := parseSSEEvents(t, rec.Body.String())
	var usage map[string]any
	for _, event := range events {
		if event.Name != "message_delta" {
			continue
		}
		gotUsage, ok := event.Data["usage"].(map[string]any)
		if !ok {
			t.Fatalf("message_delta usage missing: %#v", event.Data["usage"])
		}
		usage = gotUsage
	}
	if usage == nil {
		t.Fatalf("message_delta not found: %s", rec.Body.String())
	}
	b, _ := json.Marshal(usage)
	in, _ := usage["input_tokens"].(float64)
	out, _ := usage["output_tokens"].(float64)
	cacheRead, _ := usage["cache_read_input_tokens"].(float64)
	if int(in) != 120 || int(out) != 35 || int(cacheRead) != 64 {
		t.Fatalf("usage = %s, want input=120 output=35 cache_read=64\nfull=%s", b, rec.Body.String())
	}
}
