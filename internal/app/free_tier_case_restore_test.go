package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ======================== 免费层小写占位工具名还原 ========================
//
// 回归背景: Claude Code 大小写敏感地注册 Bash/Glob/Grep/Read;免费层指纹
// 门禁往请求里注入的 bash/glob/grep/read 小写占位工具被模型看到后,模型会
// 以 "read"/"glob" 等小写名发起 tool_use,客户端报
// "Error: No such tool available: glob/read"。修复 = 响应路径统一经
// restoreToolNameCase 把恰好全小写的四件占位名还原为规范 PascalCase。
//
// 覆盖四层:
//  1. restoreToolNameCase 单元表驱动(含非 stub 不变、已正确大小写幂等)。
//  2. aggregateOpenAIStream 聚合 tool_calls 的名字还原。
//  3. chat 方向的 anthropic→chat 流式 tool_use start(anthropicSSEToChatStream)。
//  4. claude 方向的 responses→claude 流式 tool_use start(claudeResponsesStreamHandler)
//     及非流 convertResponsesToClaude 的 function_call 名字还原。
//
// 另验证注入侧: 免费层 stub 描述带 anti-invoke 前缀(降低模型误调概率,
// 兜底仍由 restoreToolNameCase 完成)。

func TestRestoreToolNameCase_Table(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bash stub", "bash", "Bash"},
		{"glob stub", "glob", "Glob"},
		{"grep stub", "grep", "Grep"},
		{"read stub", "read", "Read"},
		{"already canonical idempotent", "Read", "Read"},
		{"already canonical glob idempotent", "Glob", "Glob"},
		{"non-stub passthrough", "weather", "weather"},
		{"other lowercase tool untouched", "shell", "shell"},
		{"uppercase untouched", "BASH", "BASH"},
		{"mixed case untouched", "gReP", "gReP"},
		{"empty untouched", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restoreToolNameCase(tc.in); got != tc.want {
				t.Fatalf("restoreToolNameCase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// 注入侧: 三种协议形状的免费层 stub 描述都是纯功能描述(无 anti-invoke
// 前缀),名字仍保持小写(门禁要求小写名,不能改注入行为)。
func TestFreeTierStubs_PlainDescriptions(t *testing.T) {
	check := func(t *testing.T, shape string, tm map[string]any) {
		t.Helper()
		name := freeTierToolNameOf(tm)
		if name != strings.ToLower(name) {
			t.Fatalf("%s stub name %q must stay lowercase (gate contract)", shape, name)
		}
		var desc string
		if fn, ok := tm["function"].(map[string]any); ok {
			desc, _ = fn["description"].(string)
		} else {
			desc, _ = tm["description"].(string)
		}
		if desc == "" {
			t.Fatalf("%s stub %q has empty description", shape, name)
		}
		if strings.Contains(desc, "Fingerprint only") || strings.Contains(desc, "Do NOT invoke") {
			t.Fatalf("%s stub %q description %q still carries anti-invoke prefix (should be removed)", shape, name, desc)
		}
	}
	for _, tm := range freeTierRequiredTools {
		check(t, "chat", tm)
	}
	for _, tm := range freeTierRequiredToolsAnthropic {
		check(t, "anthropic", tm)
	}
	for _, tm := range freeTierRequiredToolsResponses {
		check(t, "responses", tm)
	}
}

// aggregateOpenAIStream: 免费层强制 stream:true 的 OpenAI SSE 被本地聚合为
// 非流 chat.completion 时,tool_calls[].function.name 的小写占位名必须还原;
// 非 stub 工具名(weather)原样透传。
func TestAggregate_CaseRestore_StubNamesRestored(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","function":{"name":"glob","arguments":"{\"pat"}}]}}]}`,
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"tern\":\"**/*.go\"}"}},{"index":1,"id":"call_2","function":{"name":"read","arguments":"{\"file_path\":\"/etc/hosts\"}"}},{"index":2,"id":"call_3","function":{"name":"weather","arguments":"{\"city\":\"paris\"}"}}]}}]}`,
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	got := aggregateOpenAIStream([]byte(sse), "m", true)
	var resp map[string]any
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	if len(toolCalls) != 3 {
		t.Fatalf("tool_calls = %#v, want 3", msg["tool_calls"])
	}
	wantNames := []string{"Glob", "Read", "weather"}
	wantArgs := []string{`{"pattern":"**/*.go"}`, `{"file_path":"/etc/hosts"}`, `{"city":"paris"}`}
	for i, raw := range toolCalls {
		fn := raw.(map[string]any)["function"].(map[string]any)
		if fn["name"] != wantNames[i] {
			t.Fatalf("tool_calls[%d].function.name = %v, want %q (case-restored at aggregation)", i, fn["name"], wantNames[i])
		}
		if fn["arguments"] != wantArgs[i] {
			t.Fatalf("tool_calls[%d].function.arguments = %v, want %q", i, fn["arguments"], wantArgs[i])
		}
	}
}

// 端到端(请求→上游→聚合响应): 默认 ctx(无 UA 快照,门控关闭)下,上游 SSE
// 以小写名发起的 tool_call 保持小写透传;注入 Claude UA 快照的 ctx 下还原
// 为 PascalCase。
func TestAggregate_CaseRestore_EndToEndViaCallOpenCodeAPI(t *testing.T) {
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "1.18.31"})
	sse := strings.Join([]string{
		`data: {"id":"c1","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","function":{"name":"glob","arguments":"{\"pattern\":\"**/*.go\"}"}}]}}]}`,
		`data: {"id":"c1","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	reqBody := []byte(`{"model":"fallback-model-free","messages":[{"role":"user","content":"hi"}],"stream":false,"tools":[{"type":"function","function":{"name":"Glob","parameters":{"type":"object"}}},{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	callOnce := func(t *testing.T, ctx context.Context) string {
		t.Helper()
		installFakeOpenCodeClient(t, []fakeUpstreamResponse{
			{status: http.StatusOK, body: sse, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
		})
		b, status, _, err := callOpenCodeAPI(ctx, reqBody, "fallback-model-free", auth)
		if err != nil {
			t.Fatalf("callOpenCodeAPI: %v (status=%d)", err, status)
		}
		var resp map[string]any
		if err := json.Unmarshal(b, &resp); err != nil {
			t.Fatalf("unmarshal response: %v; body=%s", err, b)
		}
		msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		toolCalls, _ := msg["tool_calls"].([]any)
		if len(toolCalls) != 1 {
			t.Fatalf("tool_calls = %#v, want 1", msg["tool_calls"])
		}
		fn := toolCalls[0].(map[string]any)["function"].(map[string]any)
		name, _ := fn["name"].(string)
		return name
	}
	// 无 UA 快照:门控关闭,保持小写透传。
	if got := callOnce(t, context.Background()); got != "glob" {
		t.Fatalf("gate-off tool_call name = %q, want glob (passthrough)", got)
	}
	// Claude UA 快照:门控开启,还原为 PascalCase。
	h := http.Header{}
	h.Set("User-Agent", "claude-cli/2.0.0")
	ctxClaude := context.WithValue(context.Background(), opencodeUpstreamHeadersContextKey{}, h)
	if got := callOnce(t, ctxClaude); got != "Glob" {
		t.Fatalf("claude-UA tool_call name = %q, want Glob (restored)", got)
	}
}

// chat 方向流式: 上游 Anthropic SSE 的 content_block_start(tool_use,"read")
// 经 anthropicSSEToChatStream 发给 chat 客户端时,Claude UA 下首块 delta 的
// name 必须是 "Read",无 UA 下保持 "read";非 stub 名称原样透传。
func TestAnthropicSSEToChatStream_CaseRestoreToolUseStart(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"file_path\\\": \\\"/etc/hosts\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_2\",\"name\":\"weather\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	h := http.Header{}
	h.Set("User-Agent", "claude-cli/2.0.0")
	ctxClaude := context.WithValue(context.Background(), opencodeUpstreamHeadersContextKey{}, h)
	body := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(ctxClaude, w, strings.NewReader(sse), "claude-x", false, true, nil, nil)
	})
	if !strings.Contains(body, `"name":"Read"`) {
		t.Fatalf("streaming chat response missing case-restored \"Read\": %s", body)
	}
	if strings.Contains(body, `"name":"read"`) {
		t.Fatalf("streaming chat response leaked lowercase stub name: %s", body)
	}
	if !strings.Contains(body, `"name":"weather"`) {
		t.Fatalf("non-stub tool name must pass through unchanged: %s", body)
	}
	// 门控关闭:保持小写透传。
	bodyOff := drainSSEFromHandler(func(w http.ResponseWriter) {
		anthropicSSEToChatStream(context.Background(), w, strings.NewReader(sse), "claude-x", false, true, nil, nil)
	})
	if !strings.Contains(bodyOff, `"name":"read"`) {
		t.Fatalf("gate-off streaming chat response must keep lowercase stub name: %s", bodyOff)
	}
}

// claude 方向流式: 上游 Responses SSE 的 function_call(name="read") 经
// claudeResponsesStreamHandler 发给 claude 客户端时,content_block_start
// 的 tool_use name 必须是 "Read";已正确大小写的名字(weather 自定义、"Glob")
// 不受影响。
func TestClaudeResponsesStream_CaseRestoreToolUseStart(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"file_path\":\"/etc/hosts\"}"}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"weather"}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_2","delta":"{}"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":6,"total_tokens":11}}}`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	h2 := http.Header{}
	h2.Set("User-Agent", "claude-cli/2.0.0")
	ctxClaude2 := context.WithValue(context.Background(), opencodeUpstreamHeadersContextKey{}, h2)
	_, err := claudeResponsesStreamHandler(ctxClaude2, rec, strings.NewReader(sse), "m", false, nil, nil)
	if err != nil {
		t.Fatalf("claudeResponsesStreamHandler: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"Read"`) {
		t.Fatalf("claude stream missing case-restored \"Read\" tool_use start: %s", body)
	}
	if strings.Contains(body, `"name":"read"`) {
		t.Fatalf("claude stream leaked lowercase stub name: %s", body)
	}
	if !strings.Contains(body, `"name":"weather"`) {
		t.Fatalf("non-stub tool name must pass through unchanged: %s", body)
	}
}

// claude 方向非流: convertResponsesToClaude 聚合 output 里 function_call 的
// 小写占位名还原;非 stub 名原样透传。
func TestConvertResponsesToClaude_CaseRestoreNonStream(t *testing.T) {
	resp := map[string]any{
		"id":     "resp_1",
		"status": "completed",
		"output": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "glob", "arguments": `{"pattern":"**/*.go"}`},
			map[string]any{"type": "function_call", "call_id": "call_2", "name": "Read", "arguments": `{}`},
			map[string]any{"type": "function_call", "call_id": "call_3", "name": "weather", "arguments": `{}`},
		},
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 6, "total_tokens": 11},
	}
	raw, _ := json.Marshal(resp)
	out := convertResponsesToClaude(raw, "m", false, true, nil)
	var claudeResp struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &claudeResp); err != nil {
		t.Fatalf("unmarshal claude response: %v; body=%s", err, out)
	}
	var got []string
	for _, c := range claudeResp.Content {
		if c.Type == "tool_use" {
			got = append(got, c.Name)
		}
	}
	want := []string{"Glob", "Read", "weather"}
	if len(got) != len(want) {
		t.Fatalf("tool_use names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool_use[%d] name = %q, want %q", i, got[i], want[i])
		}
	}
	// 门控关闭:保持小写透传。
	outOff := convertResponsesToClaude(raw, "m", false, false, nil)
	var offResp struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(outOff, &offResp); err != nil {
		t.Fatalf("unmarshal gate-off response: %v; body=%s", err, outOff)
	}
	var gotOff []string
	for _, c := range offResp.Content {
		if c.Type == "tool_use" {
			gotOff = append(gotOff, c.Name)
		}
	}
	wantOff := []string{"glob", "Read", "weather"}
	if len(gotOff) != len(wantOff) {
		t.Fatalf("gate-off tool_use names = %v, want %v", gotOff, wantOff)
	}
	for i := range wantOff {
		if gotOff[i] != wantOff[i] {
			t.Fatalf("gate-off tool_use[%d] name = %q, want %q", i, gotOff[i], wantOff[i])
		}
	}
}

// ======================== 对抗验证补漏: byte-relay 与 responses 透传 ========================
//
// 首轮实现漏掉了四条路径(verify.isCorrect=false 抓出): claude→anthropic 直通
// byte-relay(流式+缓冲)、chat→responses 流式/聚合、responses 原生透传。
// 以下测试覆盖 anthropic 直通 relay 的流式行级还原与缓冲整体还原,以及
// responses 透传 restore fallback。

// TestRestoreAnthropicStreamLineCase_StubNameRestored 验证 anthropic 直通
// byte-relay 的逐行还原: content_block_start 携带小写占位名时被改写,其它行
// (文本块、content_block_stop、非占位名)原样保留,保证大小写敏感的 Claude
// Code 不再收到小写 tool_use name。
func TestRestoreAnthropicStreamLineCase_StubNameRestored(t *testing.T) {
	cases := []struct {
		name, in, wantName string
		changed            bool
	}{
		{"tool_use lowercase read restored",
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"read\",\"input\":{}}}\n",
			"Read", true},
		{"tool_use lowercase glob restored",
			"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t2\",\"name\":\"glob\",\"input\":{}}}\n",
			"Glob", true},
		{"tool_use canonical Read idempotent",
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Read\",\"input\":{}}}\n",
			"Read", false},
		{"non-stub tool weather passed through",
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"weather\",\"input\":{}}}\n",
			"weather", false},
		{"text block unchanged",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n",
			"", false},
		{"content_block_stop unchanged",
			"data: {\"type\":\"content_block_stop\",\"index\":1}\n",
			"", false},
		{"non-data line unchanged",
			"event: content_block_start\n",
			"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := restoreAnthropicStreamLineCase(tc.in, true)
			var evt map[string]any
			if strings.HasPrefix(got, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimRight(strings.TrimPrefix(got, "data: "), "\n")), &evt); err != nil {
					t.Fatalf("output not valid JSON: %v; line=%q", err, got)
				}
				if cb, ok := evt["content_block"].(map[string]any); ok {
					if n, _ := cb["name"].(string); n != tc.wantName && tc.wantName != "" {
						t.Fatalf("name = %q, want %q", n, tc.wantName)
					}
				}
			}
			if (got != tc.in) != tc.changed {
				t.Fatalf("changed = %v, want %v (in=%q out=%q)", got != tc.in, tc.changed, tc.in, got)
			}
			if strings.HasPrefix(tc.in, "data: ") && !strings.HasSuffix(got, "\n") {
				t.Fatalf("rewritten data line lost trailing newline: %q", got)
			}
		})
	}
}

// TestRestoreAnthropicBodyToolCase_StubNameRestored 验证 anthropic 直通缓冲
// (非流式)路径的整体还原: content 数组里的 tool_use 小写占位名被改写,
// 其它块与普通工具名不受影响。
func TestRestoreAnthropicBodyToolCase_StubNameRestored(t *testing.T) {
	body := []byte(`{"id":"m1","type":"message","role":"assistant","content":[` +
		`{"type":"text","text":"ok"},` +
		`{"type":"tool_use","id":"t1","name":"read","input":{"file_path":"/a"}},` +
		`{"type":"tool_use","id":"t2","name":"weather","input":{}}` +
		`],"usage":{"input_tokens":1,"output_tokens":1}}`)
	out := restoreAnthropicBodyToolCase(body, true)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, out)
	}
	content, _ := m["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content len = %d, want 3", len(content))
	}
	if n, _ := content[1].(map[string]any)["name"].(string); n != "Read" {
		t.Fatalf("tool_use[0] name = %q, want Read", n)
	}
	if n, _ := content[2].(map[string]any)["name"].(string); n != "weather" {
		t.Fatalf("tool_use[1] name = %q, want weather (non-stub untouched)", n)
	}
	// 暂无命中时应幂等原样返回
	if got := restoreAnthropicBodyToolCase(out, true); string(got) != string(out) {
		t.Fatalf("second pass not idempotent")
	}
	// 门控关闭时直接透传,不做任何改写
	if got := restoreAnthropicBodyToolCase(body, false); string(got) != string(body) {
		t.Fatalf("restoreCase=false must pass through unchanged")
	}
}

// TestResponsesRewritesRestore_FallsBackToStubCase 验证 responses 上游桥的
// restore: 即使没有任何缩短映射(rw 为恒等),小写占位名也被大小写还原。
func TestResponsesRewritesRestore_FallsBackToStubCase(t *testing.T) {
	rw := newResponsesNameRewrites(true)
	if got := rw.restore("glob"); got != "Glob" {
		t.Fatalf("restore(glob) = %q, want Glob (stub case fallback)", got)
	}
	if got := rw.restore("weather"); got != "weather" {
		t.Fatalf("restore(weather) = %q, want weather (non-stub untouched)", got)
	}
	if got := rw.restore("Read"); got != "Read" {
		t.Fatalf("restore(Read) = %q, want Read (idempotent)", got)
	}
	// 门控关闭时 fallback 不还原,保持小写透传
	rwOff := newResponsesNameRewrites(false)
	if got := rwOff.restore("glob"); got != "glob" {
		t.Fatalf("restore(glob) with gate off = %q, want glob (passthrough)", got)
	}
	// 已登记的缩短映射优先于 stub fallback
	rw.outbound["MyLongTool"] = "x-shrt"
	rw.inbound["x-shrt"] = "MyLongTool"
	if got := rw.restore("x-shrt"); got != "MyLongTool" {
		t.Fatalf("restore(x-shrt) = %q, want MyLongTool (shorten mapping wins)", got)
	}
}
