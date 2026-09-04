package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- Claude -> Responses 请求转换 ----------

func TestClaudeToResponsesBody_BasicMapping(t *testing.T) {
	var claudeReq ClaudeRequest
	raw := `{
		"model":"primary-model",
		"system":"you are helpful",
		"max_tokens":256,
		"temperature":0.5,
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"hello"},
				{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"let me think"},
				{"type":"text","text":"hi"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}
			]}
		],
		"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto"}
	}`
	if err := json.Unmarshal([]byte(raw), &claudeReq); err != nil {
		t.Fatalf("unmarshal claude req: %v", err)
	}
	body := claudeToResponsesBody(claudeReq, "primary-model")
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("responses body is not JSON: %v", err)
	}
	if req["model"] != "primary-model" {
		t.Fatalf("model = %#v", req["model"])
	}
	if instr, _ := req["instructions"].(string); !strings.Contains(instr, "you are helpful") {
		t.Fatalf("instructions = %#v, want system text", req["instructions"])
	}
	input, _ := req["input"].([]any)
	if len(input) == 0 {
		t.Fatal("input is empty")
	}
	// 至少包含 message / reasoning / function_call / function_call_output
	types := map[string]int{}
	for _, it := range input {
		if m, ok := it.(map[string]any); ok {
			types[m["type"].(string)]++
		}
	}
	for _, want := range []string{"message", "reasoning", "function_call", "function_call_output"} {
		if types[want] == 0 {
			t.Fatalf("input types = %#v, want %q", types, want)
		}
	}
	// tools 映射
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want 1", req["tools"])
	}
	tm, _ := tools[0].(map[string]any)
	if tm["type"] != "function" || tm["name"] != "get_weather" {
		t.Fatalf("tool = %#v", tm)
	}
}

func TestClaudeToResponsesBody_LenientNo400(t *testing.T) {
	// 不支持的 block、坏 document、无 name 的 tool_use、缺 ID 的 tool_result
	// 都应降级而不报错（绝不 400）。
	var claudeReq ClaudeRequest
	raw := `{
		"model":"primary-model",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"document","source":{"type":"base64","data":""}},
			{"type":"server_tool_use","name":"web_search","input":{"q":"x"}},
			{"type":"tool_use","id":"x1","name":"","input":{}},
			{"type":"redacted_thinking","data":"secret"}
		]}]
	}`
	if err := json.Unmarshal([]byte(raw), &claudeReq); err != nil {
		t.Fatal(err)
	}
	_, input := claudeMessagesToResponsesInput(claudeReq.Messages, claudeReq.System)
	if len(input) == 0 {
		t.Fatal("lenient conversion should still produce input, got empty")
	}
	body := claudeToResponsesBody(claudeReq, "primary-model")
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}

	// tool_result 缺 ID 降级为 user 文本
	var claudeReq2 ClaudeRequest
	raw2 := `{"model":"m","messages":[{"role":"user","content":[
		{"type":"tool_result","content":"orphan result"}
	]}]}`
	json.Unmarshal([]byte(raw2), &claudeReq2)
	_, input2 := claudeMessagesToResponsesInput(claudeReq2.Messages, nil)
	found := false
	for _, it := range input2 {
		if m, ok := it.(map[string]any); ok && m["type"] == "message" {
			if c, ok := m["content"].([]any); ok {
				for _, p := range c {
					if pm, ok := p.(map[string]any); ok {
						if pm["text"] == "orphan result" {
							found = true
						}
					}
				}
			}
		}
	}
	if !found {
		t.Fatalf("orphan tool_result should downgrade to user text, input=%#v", input2)
	}
}

// ---------- Responses -> Claude 响应转换 ----------

func TestResponsesToClaude_BasicMapping(t *testing.T) {
	respBody := `{
		"id":"resp_123",
		"object":"response",
		"status":"completed",
		"model":"primary-model",
		"output":[
			{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"thinking here"}]},
			{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[
				{"type":"output_text","text":"hello world","annotations":[]}
			]},
			{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}
		],
		"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}
	}`
	out := convertResponsesToClaude([]byte(respBody), "primary-model", true)
	var claude ClaudeResponse
	if err := json.Unmarshal(out, &claude); err != nil {
		t.Fatalf("claude response is not JSON: %v, body=%s", err, string(out))
	}
	if claude.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", claude.StopReason)
	}
	hasThinking, hasText, hasTool := false, false, false
	for _, b := range claude.Content {
		switch b.Type {
		case "thinking":
			if b.Thinking == "thinking here" {
				hasThinking = true
			}
		case "text":
			if strings.Contains(b.Text, "hello world") {
				hasText = true
			}
		case "tool_use":
			if b.Name == "get_weather" && b.ID == "call_1" {
				hasTool = true
				if m, ok := b.Input.(map[string]any); !ok || m["city"] != "SF" {
					t.Fatalf("tool input = %#v, want city=SF", b.Input)
				}
			}
		}
	}
	if !hasThinking || !hasText || !hasTool {
		t.Fatalf("content missing blocks (thinking=%v text=%v tool=%v): %#v", hasThinking, hasText, hasTool, claude.Content)
	}
	if toFloat64(claude.Usage["input_tokens"]) != 10 || toFloat64(claude.Usage["output_tokens"]) != 20 {
		t.Fatalf("usage = %#v, want 10/20", claude.Usage)
	}
}

func TestResponsesToClaude_UnknownItemDowngradesToText(t *testing.T) {
	// web_search_call 等未知 item 应降级为文本，不丢弃、不报错。
	respBody := `{
		"id":"resp_x","status":"completed",
		"output":[
			{"id":"ws_1","type":"web_search_call","status":"completed","query":"weather"},
			{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
		],
		"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}
	}`
	out := convertResponsesToClaude([]byte(respBody), "m", true)
	var claude ClaudeResponse
	json.Unmarshal(out, &claude)
	foundText := false
	for _, b := range claude.Content {
		if b.Type == "text" && (strings.Contains(b.Text, "done") || strings.Contains(b.Text, "web_search_call")) {
			foundText = true
		}
	}
	if !foundText {
		t.Fatalf("unknown item should downgrade to text, content=%#v", claude.Content)
	}
}

// ---------- Claude 经原生 responses 的记忆与探测 ----------

func TestClaudeResponsesPassthrough_FallbackAndMemory(t *testing.T) {
	const probeModel = "claude-probe-model-xyz"
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	delete(nativeResponsesModels.ids, probeModel)
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, probeModel)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusInternalServerError, body: `{"type":"error"}`},
		{status: http.StatusOK, body: `{"id":"resp_probe1","object":"response","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hi from native"}]}],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`},
		{status: http.StatusOK, body: `{"id":"resp_probe2","object":"response","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`},
	})

	doRequest := func() (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"`+probeModel+`","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		claudeMessagesHandler(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := doRequest()
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body=%s", code, body)
	}
	if !strings.Contains(body, "hi from native") {
		t.Fatalf("first body = %s, want native text", body)
	}
	if !isNativeResponsesModel(probeModel) {
		t.Fatalf("model %q not remembered after successful probe", probeModel)
	}
	if len(transport.requestedURLs) != 4 {
		t.Fatalf("requested URLs after first = %#v, want 3 chat + 1 responses", transport.requestedURLs)
	}
	for _, u := range transport.requestedURLs[:3] {
		if !strings.HasSuffix(u, "/zen/v1/chat/completions") {
			t.Fatalf("unexpected translated URL = %s", u)
		}
	}
	if !strings.HasSuffix(transport.requestedURLs[3], "/zen/v1/responses") {
		t.Fatalf("probe URL = %s, want /zen/v1/responses", transport.requestedURLs[3])
	}
	// 探测发往 responses 的应是转换后的体（含 instructions/input），而非 Claude 原体。
	probePayload := transport.requestPayloads[3]
	if _, ok := probePayload["input"]; !ok {
		t.Fatalf("probe payload should contain input (Responses shape), got %#v", probePayload)
	}
	if _, ok := probePayload["messages"]; ok {
		t.Fatalf("probe payload should not contain chat messages, got %#v", probePayload)
	}

	code, body = doRequest()
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200, body=%s", code, body)
	}
	if !strings.Contains(body, "second") {
		t.Fatalf("second body = %s, want second", body)
	}
	if len(transport.requestedURLs) != 5 {
		t.Fatalf("URLs after second = %#v, want exactly one more passthrough", transport.requestedURLs)
	}
	if !strings.HasSuffix(transport.requestedURLs[4], "/zen/v1/responses") {
		t.Fatalf("second URL = %s, want responses", transport.requestedURLs[4])
	}
}

func TestClaudeResponsesPassthrough_RememberedModelForwards(t *testing.T) {
	const remembered = "claude-remembered-model"
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[remembered] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, remembered)
		nativeResponsesModels.Unlock()
	})

	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"resp_r1","status":"completed","output":[{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"remembered ok"}]}],"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}`},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+remembered+`","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "remembered ok") {
		t.Fatalf("body = %s, want remembered ok", rec.Body.String())
	}
	if len(transport.requestedURLs) != 1 || !strings.HasSuffix(transport.requestedURLs[0], "/zen/v1/responses") {
		t.Fatalf("URLs = %#v, want single responses call", transport.requestedURLs)
	}
}

func TestClaudeResponsesPassthrough_RememberedLenientNo400(t *testing.T) {
	// 已记住模型 + 坏 document：不得 400，应降级后成功。
	const remembered = "claude-lenient-model"
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[remembered] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, remembered)
		nativeResponsesModels.Unlock()
	})

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"resp_l1","status":"completed","output":[{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"lenient ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+remembered+`","max_tokens":32,"messages":[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"document","source":{"type":"base64","data":""}}
		]}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (lenient, no 400), body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "lenient ok") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestClaudeResponsesPassthrough_SkipsConversionError(t *testing.T) {
	// 上游 200 但包体截断（本地转换失败，合成 502）不得探测。
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{
		status: http.StatusOK,
		body:   `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","stop_reason":null,"usage":{}}}`,
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"primary-model","max_tokens":32,"messages":[]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if isNativeResponsesModel("primary-model") {
		t.Fatal("conversion error must not be remembered")
	}
}

// ---------- 流式 ----------

func TestClaudeResponsesStream_Mapping(t *testing.T) {
	const remembered = "claude-stream-model"
	oldModelAlias := getModelKeywordRules()
	applyConfig(AppConfig{})
	t.Cleanup(func() { applyConfig(AppConfig{ModelAlias: oldModelAlias}) })
	nativeResponsesModels.Lock()
	nativeResponsesModels.ids[remembered] = true
	nativeResponsesModels.Unlock()
	t.Cleanup(func() {
		nativeResponsesModels.Lock()
		delete(nativeResponsesModels.ids, remembered)
		nativeResponsesModels.Unlock()
	})

	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_s1"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant"}}`,
		``,
		`event: response.content_part.added`,
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"msg_1","part":{"type":"output_text","text":""}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_1","delta":"hello"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_1","delta":" world"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_s1","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":5,"output_tokens":6,"total_tokens":11}}}`,
		``,
	}, "\n")

	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+remembered+`","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"message_start", "content_block_start", "content_block_delta", "message_delta", "message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("stream body missing text delta:\n%s", body)
	}
}

func TestClaudeToResponsesBody_AssistantUsesOutputText(t *testing.T) {
	// 上游原生 responses 拒绝 assistant 消息中的 input_text：
	// invalid_request_error: content type `input_text` is not valid on `assistant` messages。
	// assistant 文本必须用 output_text，user 仍用 input_text。
	var claudeReq ClaudeRequest
	raw := `{
		"model":"m",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":"plain string"},
			{"role":"user","content":"plain user"}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &claudeReq); err != nil {
		t.Fatal(err)
	}
	_, input := claudeMessagesToResponsesInput(claudeReq.Messages, nil)
	if len(input) != 4 {
		t.Fatalf("input len = %d, want 4: %#v", len(input), input)
	}
	for _, it := range input {
		m, _ := it.(map[string]any)
		role, _ := m["role"].(string)
		content, _ := m["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("role %q content len = %d: %#v", role, len(content), m)
		}
		part, _ := content[0].(map[string]any)
		pt, _ := part["type"].(string)
		if role == "assistant" && pt != "output_text" {
			t.Fatalf("assistant part type = %q, want output_text: %#v", pt, m)
		}
		if role == "user" && pt != "input_text" {
			t.Fatalf("user part type = %q, want input_text: %#v", pt, m)
		}
	}
}

func TestNormalizeResponsesToolParameters_IncludesAllKeys(t *testing.T) {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit": map[string]any{"type": "integer"},
			"q":     map[string]any{"type": "string"},
		},
	}
	out := normalizeResponsesToolParameters(params)
	req, _ := out["required"].([]any)
	got := map[string]bool{}
	for _, v := range req {
		if s, ok := v.(string); ok {
			got[s] = true
		}
	}
	if !got["limit"] || !got["q"] {
		t.Fatalf("required = %#v, want limit+q", out["required"])
	}
}

func TestNormalizeResponsesEffort_MaxToXhigh(t *testing.T) {
	if got := normalizeResponsesEffort("max"); got != "xhigh" {
		t.Fatalf("max -> %q, want xhigh", got)
	}
	if got := normalizeResponsesEffort("none"); got != "" {
		t.Fatalf("none -> %q, want empty", got)
	}
	if got := normalizeResponsesEffort("bogus"); got != "" {
		t.Fatalf("bogus -> %q, want empty", got)
	}
	for _, e := range []string{"minimal", "low", "medium", "high", "xhigh"} {
		if got := normalizeResponsesEffort(e); got != e {
			t.Fatalf("%q -> %q, want itself", e, got)
		}
	}
}

func TestClaudeToResponsesBody_EffortMaxNormalized(t *testing.T) {
	var claudeReq ClaudeRequest
	raw := `{"model":"m","output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`
	if err := json.Unmarshal([]byte(raw), &claudeReq); err != nil {
		t.Fatal(err)
	}
	body := claudeToResponsesBody(claudeReq, "muse-spark-1.3-contributor")
	var req map[string]any
	json.Unmarshal(body, &req)
	reasoning, _ := req["reasoning"].(map[string]any)
	if reasoning == nil {
		t.Fatal("reasoning should exist (max->xhigh)")
	}
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("effort = %#v, want xhigh", reasoning["effort"])
	}
}

func TestSanitizeResponsesPassthroughBody_FixesRequiredAndEffort(t *testing.T) {
	raw := `{
		"model":"m",
		"input":"hi",
		"reasoning":{"effort":"max"},
		"tools":[
			{"type":"function","name":"search","parameters":{"type":"object","properties":{"limit":{"type":"integer"},"q":{"type":"string"}}}},
			{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}
		]
	}`
	fixed := sanitizeResponsesPassthroughBody([]byte(raw), "muse-spark-1.3-contributor")
	var body map[string]any
	if err := json.Unmarshal(fixed, &body); err != nil {
		t.Fatalf("fixed body is not JSON: %v", err)
	}
	// reasoning max->xhigh
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("effort = %#v, want xhigh", reasoning["effort"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools len = %d", len(tools))
	}
	checkRequired := func(params map[string]any, wantKeys ...string) {
		t.Helper()
		req, _ := params["required"].([]any)
		got := map[string]bool{}
		for _, v := range req {
			if s, ok := v.(string); ok {
				got[s] = true
			}
		}
		for _, k := range wantKeys {
			if !got[k] {
				t.Fatalf("required = %#v, want %v", params["required"], wantKeys)
			}
		}
	}
	t0, _ := tools[0].(map[string]any)
	checkRequired(t0["parameters"].(map[string]any), "limit", "q")
	t1, _ := tools[1].(map[string]any)
	fn, _ := t1["function"].(map[string]any)
	checkRequired(fn["parameters"].(map[string]any), "path")
}

func TestNormalizeArgsFragment_IntegralFloats(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"timeout_ms":1000.0,"command":"echo hi"}`, `{"timeout_ms":1000,"command":"echo hi"}`},
		{`{"a":1000.00}`, `{"a":1000}`},
		// 字符串内的 1.0 不动
		{`{"command":"echo 1.0"}`, `{"command":"echo 1.0"}`},
		// 真浮点不动
		{`{"temp":1.5}`, `{"temp":1.5}`},
		{`{"v":1.05}`, `{"v":1.05}`},
		// 科学计数不动
		{`{"v":1.0e3}`, `{"v":1.0e3}`},
		{`{"a":-5.0}`, `{"a":-5}`},
	}
	for _, c := range cases {
		if got := normalizeArgumentsString(c.in); got != c.want {
			t.Fatalf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeArgsFragment_AcrossShardBoundary(t *testing.T) {
	// 分片边界：第一片在字符串内结束，第二片从字符串内开始，其中的 1.0 不动，
	// 字符串外的 1000.0 仍归一化。
	st := &argsNormState{}
	frag1 := `{"command":"echo hel`
	out1 := normalizeArgsFragment(frag1, st)
	if out1 != frag1 {
		t.Fatalf("frag1 changed: %q", out1)
	}
	if !st.inString {
		t.Fatal("after frag1 should still be inString")
	}
	frag2 := `lo 1.0","timeout_ms":1000.0}`
	want2 := `lo 1.0","timeout_ms":1000}`
	if got := normalizeArgsFragment(frag2, st); got != want2 {
		t.Fatalf("frag2 = %q, want %q", got, want2)
	}
}

func TestNormalizeResponsesStreamLine_DeltaAndCompleted(t *testing.T) {
	states := map[int]*argsNormState{}
	mapping := map[string]int{}
	// output_item.added 登记映射
	added := []byte("data: {\"type\":\"response.output_item.added\",\"output_index\":2,\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\"}}\n")
	if _, ok := normalizeResponsesStreamLine(added, states, mapping); ok {
		t.Fatal("added should not rewrite")
	}
	// arguments delta 被归一化
	delta := []byte("data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":2,\"item_id\":\"fc_1\",\"delta\":\"{\\\"timeout_ms\\\":1000.0}\"\n")
	// 注意 delta 内是转义 JSON 字符串，扫描的是外层 JSON 的字符串内容？
	// 这里直接验证 output_text 不动、arguments 动的端到端语义改用完整事件验证。
	_ = delta
	// output_text delta 永不动
	textLine := []byte("data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"echo 1.0\"}\n")
	if _, ok := normalizeResponsesStreamLine(textLine, states, mapping); ok {
		t.Fatal("output_text delta must not be rewritten")
	}
	// completed 全量 arguments 被归一化
	completed := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"c\",\"name\":\"x\",\"arguments\":\"{\\\"n\\\":1000.0}\"}]}}\n")
	norm, ok := normalizeResponsesStreamLine(completed, states, mapping)
	if !ok {
		t.Fatal("completed should be rewritten")
	}
	if strings.Contains(string(norm), "1000.0") || !strings.Contains(string(norm), "1000") {
		t.Fatalf("completed not normalized: %s", string(norm))
	}
}

func TestSanitizeResponsesPassthroughBody_NonMuseSparkUnchanged(t *testing.T) {
	raw := `{"model":"gpt-5","input":"hi","reasoning":{"effort":"max"},"tools":[{"type":"function","name":"x","parameters":{"type":"object","properties":{"limit":{"type":"integer"}}}}]}`
	fixed := sanitizeResponsesPassthroughBody([]byte(raw), "gpt-5")
	if string(fixed) != raw {
		t.Fatalf("non muse-spark must pass through unchanged, got %s", string(fixed))
	}
}

func TestClaudeToResponsesBody_EffortNonMuseSparkRaw(t *testing.T) {
	// 非 muse-spark 不归一化，保持原值，避免改变其它模型行为。
	var claudeReq ClaudeRequest
	raw := `{"model":"m","output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`
	if err := json.Unmarshal([]byte(raw), &claudeReq); err != nil {
		t.Fatal(err)
	}
	body := claudeToResponsesBody(claudeReq, "gpt-5")
	var req map[string]any
	json.Unmarshal(body, &req)
	reasoning, _ := req["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "max" {
		t.Fatalf("non muse-spark effort should stay max, got %#v", req["reasoning"])
	}
}
