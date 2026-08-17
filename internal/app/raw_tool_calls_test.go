package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeRawToolMarkers(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "pipe form",
			in:   `<|DSML|tool_calls><name>ls</name><parameters>{"path":"."}</parameters></|DSML|tool_calls>`,
			want: `<｜DSML｜tool_calls><name>ls</name><parameters>{"path":"."}</parameters></｜DSML｜tool_calls>`,
		},
		{
			name: "colon form",
			in:   `<DSML>tool_calls><DSML:invoke name="ls"><DSML:parameter name="path">.</DSML:parameter></DSML:invoke></DSML>tool_calls>`,
			want: `<｜DSML｜tool_calls><｜DSML｜invoke name="ls"><｜DSML｜parameter name="path">.</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`,
		},
		{
			name: "bare tool_calls",
			in:   `<tool_calls><name>ls</name><parameters>{}</parameters></tool_calls>`,
			want: `<｜DSML｜tool_calls><name>ls</name><parameters>{}</parameters></｜DSML｜tool_calls>`,
		},
		{
			name: "qwen unchanged",
			in:   `<tool_call><name>ls</name><parameters>{}</parameters></tool_call>`,
			want: `<tool_call><name>ls</name><parameters>{}</parameters></tool_call>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeRawToolCalls(tt.in); got != tt.want {
				t.Fatalf("normalizeRawToolCalls() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasCompleteRawToolBlock(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "fullwidth", in: `<｜DSML｜tool_calls><name>ls</name></｜DSML｜tool_calls>`, want: true},
		{name: "pipe", in: `<|DSML|tool_calls><name>ls</name></|DSML|tool_calls>`, want: true},
		{name: "colon", in: `<DSML>tool_calls><name>ls</name></DSML:tool_calls>`, want: true},
		{name: "colon alt close", in: `<DSML>tool_calls><name>ls</name></DSML>tool_calls>`, want: true},
		{name: "bare calls", in: `<tool_calls><name>ls</name></tool_calls>`, want: true},
		{name: "qwen", in: `<tool_call><name>ls</name></tool_call>`, want: true},
		{name: "qwen function", in: `<function=ls><parameter=path>.</parameter></function>`, want: true},
		{name: "open only", in: `hello <｜DSML｜tool_calls>`, want: false},
		{name: "close only", in: `hello </｜DSML｜tool_calls>`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasCompleteRawToolBlock(tt.in); got != tt.want {
				t.Fatalf("hasCompleteRawToolBlock(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFindRawToolStart(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "prefix", in: `ok<｜DSML｜tool_calls><name>ls</name></｜DSML｜tool_calls>`, want: 2},
		{name: "at start", in: `<tool_call><name>ls</name></tool_call>`, want: 0},
		{name: "pipe", in: `before <|DSML|tool_calls>`, want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findRawToolStart(tt.in); got != tt.want {
				t.Fatalf("findRawToolStart(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseRawToolCalls(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  int
		first string
		args  string
	}{
		{
			name: "dsml name parameters",
			in:   `<｜DSML｜tool_calls><name>ls</name><parameters>{"path":"."}</parameters></｜DSML｜tool_calls>`,
			want: 1, first: "ls", args: `{"path":"."}`,
		},
		{
			name: "dsml invoke",
			in:   `<｜DSML｜tool_calls><｜DSML｜invoke name="read_file"><｜DSML｜parameter name="path">/tmp/a.txt</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`,
			want: 1, first: "read_file", args: `{"path":"/tmp/a.txt"}`,
		},
		{
			name: "qwen",
			in:   `<tool_call><name>search</name><parameters>{"q":"go"}</parameters></tool_call>`,
			want: 1, first: "search", args: `{"q":"go"}`,
		},
		{
			name: "qwen function",
			in:   `<function=search><parameter=q>go</parameter></function>`,
			want: 1, first: "search", args: `{"q":"go"}`,
		},
		{
			name: "multiple",
			in:   `<｜DSML｜tool_calls><name>a</name><parameters>{}</parameters><name>b</name><parameters>{"x":1}</parameters></｜DSML｜tool_calls>`,
			want: 2, first: "a", args: `{}`,
		},
		{
			name: "malformed",
			in:   `<｜DSML｜tool_calls><name>ls</name></｜DSML｜tool_calls>`,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseRawToolCalls(tt.in)
			if len(got) != tt.want {
				t.Fatalf("parseRawToolCalls(%q) = %d calls, want %d: %#v", tt.in, len(got), tt.want, got)
			}
			if tt.want > 0 {
				if got[0].Function.Name != tt.first || got[0].Function.Arguments != tt.args {
					t.Fatalf("first call = %#v, want name=%q args=%q", got[0], tt.first, tt.args)
				}
				if !strings.HasPrefix(got[0].ID, "call_") {
					t.Fatalf("call ID = %q, want call_ prefix", got[0].ID)
				}
			}
		})
	}
}

func TestConvertRawToolCallsInBody(t *testing.T) {
	rawContent := "prefix" + dsmlOpen + "<name>ls</name><parameters>{\"path\":\".\"}</parameters>" + dsmlClose
	body, err := json.Marshal(map[string]any{
		"id": "chatcmpl_test", "model": "m", "created": 1,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": rawContent, "reasoning_content": "think",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := convertRawToolCallsInBody(body)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal converted: %v", err)
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != "prefix" {
		t.Fatalf("content = %#v, want prefix", msg["content"])
	}
	if msg["reasoning_content"] != "think" {
		t.Fatalf("reasoning_content = %#v, want think", msg["reasoning_content"])
	}
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", calls)
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "ls" || fn["arguments"] != `{"path":"."}` {
		t.Fatalf("function = %#v", fn)
	}
	if got["usage"] == nil {
		t.Fatal("usage must be preserved")
	}
}

func TestConvertRawToolCallsInBodySkipsNative(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"<|DSML|tool_calls><name>ls</name></|DSML|tool_calls>","tool_calls":[{"id":"call_native","type":"function","function":{"name":"native","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	if out := convertRawToolCallsInBody(body); string(out) != string(body) {
		t.Fatalf("native tool_calls should be preserved unchanged: %s", string(out))
	}
}

func TestConvertRawToolCallsInBodyMalformedKeepsOriginal(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"<|DSML|tool_calls><name>ls</name></|DSML|tool_calls>"},"finish_reason":"stop"}]}`)
	if out := convertRawToolCallsInBody(body); string(out) != string(body) {
		t.Fatalf("malformed raw block should be left unchanged: %s", string(out))
	}
}

func readSSELines(t *testing.T, rc io.ReadCloser) []string {
	t.Helper()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read wrapped stream: %v", err)
	}
	return strings.Split(string(data), "\n\n")
}

func TestWrapRawSSEConvertsSplitDSML(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_1","model":"m","choices":[{"delta":{"role":"assistant","content":"<|DSML|tool_calls>"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"<|DSML|invoke name=\"ls\"><|DSML|parameter name=\"path\">."},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rc := wrapRawSSE(io.NopCloser(strings.NewReader(upstream)))
	defer rc.Close()
	lines := readSSELines(t, rc)
	if len(lines) == 0 {
		t.Fatal("no lines")
	}
	var foundStart, foundArgs, foundFinish, foundUsage, foundDone bool
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == "data: [DONE]" {
			foundDone = true
			continue
		}
		if !strings.HasPrefix(trimmed, "data: ") {
			t.Fatalf("unexpected line %q", line)
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &chunk); err != nil {
			t.Fatalf("bad JSON %q: %v", line, err)
		}
		if _, ok := chunk["usage"]; ok {
			foundUsage = true
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]any)
		if fr, _ := choice["finish_reason"].(string); fr == "tool_calls" {
			foundFinish = true
		}
		delta, _ := choice["delta"].(map[string]any)
		tcs, _ := delta["tool_calls"].([]any)
		if len(tcs) == 0 {
			continue
		}
		tc := tcs[0].(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		if name, _ := fn["name"].(string); name == "ls" {
			foundStart = true
		}
		if args, _ := fn["arguments"].(string); args != "" {
			foundArgs = true
		}
	}
	if !foundStart || !foundArgs || !foundFinish || !foundUsage || !foundDone {
		t.Fatalf("missing required chunks start=%v args=%v finish=%v usage=%v done=%v\n%s", foundStart, foundArgs, foundFinish, foundUsage, foundDone, strings.Join(lines, "\n"))
	}
	if strings.Contains(strings.Join(lines, ""), "<|DSML|") {
		t.Fatal("raw marker leaked")
	}
}

func TestWrapRawSSEIncompleteFragmentDoesNotLeak(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"before <|DSML|tool_calls><name>ls</name>"},"finish_reason":null}]}`,
		``,
	}, "\n")
	rc := wrapRawSSE(io.NopCloser(strings.NewReader(upstream)))
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "<|DSML|") {
		t.Fatalf("partial raw marker leaked: %s", body)
	}
	if !strings.Contains(string(body), "before") {
		t.Fatalf("prefix should be flushed: %s", body)
	}
}

func TestWrapRawSSENativeToolCallsPassThrough(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"text","tool_calls":[{"index":0,"id":"call_native","type":"function","function":{"name":"native","arguments":"{}"}}]},"finish_reason":null}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rc := wrapRawSSE(io.NopCloser(strings.NewReader(upstream)))
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "call_native") {
		t.Fatalf("native tool call missing: %s", body)
	}
}

func TestCallOpenCodeAPIConvertsNonStreamingRawDSML(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body: func() string {
			content := "<|DSML|tool_calls><name>ls</name><parameters>" + `{"path":"."}` + "</parameters></|DSML|tool_calls>"
			b, _ := json.Marshal(map[string]any{
				"id":    "chatcmpl_test",
				"model": "m",
				"choices": []any{map[string]any{
					"message": map[string]any{
						"role":              "assistant",
						"content":           content,
						"reasoning_content": "think",
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
			})
			return string(b)
		}(),
	}})
	body, status, _, err := callOpenCodeAPI(context.Background(), []byte(`{"model":"primary-model","messages":[]}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err != nil {
		t.Fatalf("callOpenCodeAPI: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != nil {
		t.Fatalf("content = %#v, want nil", msg["content"])
	}
	if msg["reasoning_content"] != "think" {
		t.Fatalf("reasoning_content = %#v", msg["reasoning_content"])
	}
	if len(msg["tool_calls"].([]any)) != 1 {
		t.Fatalf("tool_calls = %#v", msg["tool_calls"])
	}
	if got["usage"] == nil {
		t.Fatal("usage missing")
	}
	if len(transport.requestedURLs) != 1 {
		t.Fatalf("requested URLs = %#v", transport.requestedURLs)
	}
}

func TestChatHandlerNonStreamingRawDSML(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"chatcmpl_test","choices":[{"message":{"role":"assistant","content":"<|DSML|tool_calls><name>ls</name><parameters>{}</parameters></|DSML|tool_calls>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"primary-model","messages":[],"stream":false}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"].([]any); !ok {
		t.Fatalf("tool_calls missing: %#v", msg)
	}
	if strings.Contains(rec.Body.String(), "<|DSML|") {
		t.Fatal("raw marker leaked")
	}
}

func TestClaudeHandlerNonStreamingRawDSML(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"chatcmpl_test","choices":[{"message":{"role":"assistant","content":"<tool_call><name>ls</name><parameters>{}</parameters></tool_call>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"primary-model","messages":[]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	content := got["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("content = %#v, want tool_use", content)
	}
}

func TestResponsesHandlerNonStreamingRawDSML(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"id":"chatcmpl_test","created":1,"choices":[{"message":{"role":"assistant","content":"<tool_call><name>ls</name><parameters>{}</parameters></tool_call>"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"primary-model","input":"hi"}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	output := got["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["type"] != "function_call" {
		t.Fatalf("output = %#v, want function_call", output)
	}
}

func TestClaudeStreamHandlerRawDSML(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"<|DSML|tool_calls><name>ls</name><parameters>{}</parameters></|DSML|tool_calls>"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	claudeStreamHandler(context.Background(), rr, wrapRawSSE(io.NopCloser(strings.NewReader(upstream))), "m", false)
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "content_block_start") {
		t.Fatalf("missing content_block_start:\n%s", rr.Body.String())
	}
	foundTool := false
	for _, e := range events {
		if e.Name == "content_block_start" {
			if cb, _ := e.Data["content_block"].(map[string]any); cb["type"] == "tool_use" {
				foundTool = true
			}
		}
	}
	if !foundTool {
		t.Fatalf("tool_use not found:\n%s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "<|DSML|") {
		t.Fatal("raw marker leaked")
	}
}

func TestResponsesStreamHandlerRawDSML(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"r","created":1,"choices":[{"delta":{"role":"assistant","content":"<tool_call><name>ls</name><parameters>{}</parameters></tool_call>"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rr := httptest.NewRecorder()
	resp := &http.Response{StatusCode: 200, Body: wrapRawSSE(io.NopCloser(strings.NewReader(upstream))), Header: make(http.Header)}
	responsesStreamHandler(rr, nil, resp, "m", "m", false, nil, nil, ResponsesAPIRequest{})
	events := parseSSEEvents(t, rr.Body.String())
	if !hasEvent(events, "response.function_call_arguments.done") {
		t.Fatalf("missing function_call done:\n%s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "<tool_call>") {
		t.Fatal("raw marker leaked")
	}
}

func TestCallOpenCodeAPIStreamConvertsRawDSML(t *testing.T) {
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body: strings.Join([]string{
			`data: {"id":"chatcmpl_test","model":"m","choices":[{"delta":{"role":"assistant","content":"<|DSML|tool_calls>"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{"content":"<|DSML|invoke name=\"ls\"><|DSML|parameter name=\"path\">."},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{"content":"</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>"},"finish_reason":null}]}`,
			``,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			``,
			`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"),
	}})
	rc, status, _, err := callOpenCodeAPIStream(context.Background(), []byte(`{"model":"primary-model","messages":[],"stream":true}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err != nil {
		t.Fatalf("callOpenCodeAPIStream: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "<|DSML|") {
		t.Fatal("raw marker leaked")
	}
	if !strings.Contains(string(body), `"name":"ls"`) || !strings.Contains(string(body), `"finish_reason":"tool_calls"`) {
		t.Fatalf("converted stream missing tool call/finish: %s", body)
	}
	if !strings.Contains(string(body), `"total_tokens":3`) {
		t.Fatalf("usage missing: %s", body)
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("[DONE] missing: %s", body)
	}
	if len(transport.requestedURLs) != 1 {
		t.Fatalf("requested URLs = %#v", transport.requestedURLs)
	}
}

func rawStreamBody() string {
	content := "<|DSML|tool_calls><name>ls</name><parameters>" + `{"path":"."}` + "</parameters></|DSML|tool_calls>"
	chunks := []any{
		map[string]any{"id": "chatcmpl_stream", "model": "m", "choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}}},
		map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}}},
		map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}},
	}
	var lines []string
	for _, chunk := range chunks {
		b, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		lines = append(lines, "data: "+string(b), "")
	}
	lines = append(lines, "data: [DONE]", "")
	return strings.Join(lines, "\n")
}

func TestChatStreamHandlerFakeRawDSML(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   rawStreamBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"primary-model","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<|DSML|") {
		t.Fatalf("raw marker leaked: %s", body)
	}
	if !strings.Contains(body, `"name":"ls"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("converted stream missing tool_calls/finish: %s", body)
	}
	if !strings.Contains(body, `"total_tokens":3`) {
		t.Fatalf("usage missing: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("DONE missing: %s", body)
	}
}

func TestClaudeStreamHandlerFakeRawDSML(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   rawStreamBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"primary-model","messages":[],"stream":true}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<|DSML|") {
		t.Fatalf("raw marker leaked: %s", body)
	}
	events := parseSSEEvents(t, body)
	foundTool := false
	for _, e := range events {
		if e.Name == "content_block_start" {
			if cb, _ := e.Data["content_block"].(map[string]any); cb["type"] == "tool_use" {
				foundTool = true
			}
		}
	}
	if !foundTool {
		t.Fatalf("tool_use missing: %s", body)
	}
}

func TestResponsesStreamHandlerFakeRawDSML(t *testing.T) {
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   rawStreamBody(),
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"primary-model","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "<|DSML|") {
		t.Fatalf("raw marker leaked: %s", body)
	}
	events := parseSSEEvents(t, body)
	if !hasEvent(events, "response.function_call_arguments.done") {
		t.Fatalf("function_call done missing: %s", body)
	}
}

func TestWrapRawSSEPreservesUsageOnRawCompletionChunk(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_u","model":"m","choices":[{"delta":{"role":"assistant","content":"<|DSML|tool_calls><name>ls</name><parameters>{}</parameters></|DSML|tool_calls>"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rc := wrapRawSSE(io.NopCloser(strings.NewReader(upstream)))
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), `"total_tokens":3`) {
		t.Fatalf("usage missing from wrapped stream: %s", body)
	}
}

func TestWrapRawSSEMaintainsFinishBeforeUsageOrder(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl_order","model":"m","choices":[{"delta":{"role":"assistant","content":"<|DSML|tool_calls>"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"<name>ls</name><parameters>{}</parameters></|DSML|tool_calls>"},"finish_reason":null}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"choices":[],"usage":{"total_tokens":3}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rc := wrapRawSSE(io.NopCloser(strings.NewReader(upstream)))
	defer rc.Close()
	lines := readSSELines(t, rc)
	var sawFinish, sawUsage, sawDone bool
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == "data: [DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(trimmed, "data: ")), &chunk); err != nil {
			continue
		}
		if _, ok := chunk["usage"]; ok {
			sawUsage = true
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		if fr, _ := choices[0].(map[string]any)["finish_reason"].(string); fr != "" {
			if sawUsage {
				t.Fatalf("finish_reason chunk emitted after usage-only chunk: %s", strings.Join(lines, "\n"))
			}
			sawFinish = true
		}
	}
	if !sawFinish || !sawUsage || !sawDone {
		t.Fatalf("missing finish/usage/done finish=%v usage=%v done=%v", sawFinish, sawUsage, sawDone)
	}
}
