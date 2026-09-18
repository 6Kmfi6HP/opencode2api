package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
)

var ocSessionShapeRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// 上游 2026-09-18 门禁把免费层校验锁死在 ses_ + 12 位 hex + 14 位 base62
// (见 lite opencode2api-lite.go L498-514)。1000 次生成必须全部命中且互不相同。
func TestSession_IDFormatAndUniqueness(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := newOCSessionID()
		if !ocSessionShapeRe.MatchString(id) {
			t.Fatalf("session id %q does not match upstream free-tier regex", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}
}

func TestOCRequest_FreeTierInjectsSessionHeadersAndStream(t *testing.T) {
	ocClientVer = "1.18.31"
	ocSessionID = ""
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	bodyMap := map[string]any{"messages": []any{}, "stream": false}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_sticky_000000000000aa")
	if err != nil {
		t.Fatal(err)
	}
	// 新门禁 x-session-id / x-session-affinity 复用调用方解析出的稳定
	// session(sticky egress 需要同一请求链路上 session 不漂移),并与旧
	// x-opencode-session 同值双发。
	if got := req.Header.Get("x-session-id"); got != "ses_sticky_000000000000aa" {
		t.Fatalf("x-session-id = %q, want ses_sticky_000000000000aa", got)
	}
	if got := req.Header.Get("x-session-affinity"); got != req.Header.Get("x-session-id") {
		t.Fatalf("x-session-affinity = %q differs from x-session-id = %q", got, req.Header.Get("x-session-id"))
	}
	if got := req.Header.Get("x-opencode-session"); got != req.Header.Get("x-session-id") {
		t.Fatalf("x-opencode-session = %q differs from x-session-id = %q", got, req.Header.Get("x-session-id"))
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "opencode/1.18.31 ai-sdk/") {
		t.Fatalf("User-Agent = %q, want opencode/<ver> ai-sdk/... suffix", got)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("unmarshal sent body: %v", err)
	}
	if got := sent["stream"]; got != true {
		t.Fatalf("stream = %#v, want true (free-tier upstream gateway forces it)", got)
	}
	opts, ok := sent["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v, want object", sent["stream_options"])
	}
	if got := opts["include_usage"]; got != true {
		t.Fatalf("stream_options.include_usage = %#v, want true", got)
	}
	tools, _ := sent["tools"].([]any)
	seen := map[string]bool{}
	for _, rt := range tools {
		fn, ok := rt.(map[string]any)["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := fn["name"].(string); name != "" {
			seen[name] = true
		}
	}
	for _, name := range []string{"bash", "glob", "grep", "read"} {
		if !seen[name] {
			t.Fatalf("free-tier tool %q not injected into no-tools request; got %v", name, tools)
		}
	}
}

// 付费模型不套免费层门禁: 不补工具、stream 保持客户端值。
// 注意判据是上游模型是否免费,而不是客户端 tier——sk- key + 免费模型仍会
// 被上游要求指纹(见 TestFreeTierFingerprint_SKKeyFreeModelNoTools)。
func TestOCRequest_PaidTierKeepsClientBody(t *testing.T) {
	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-validkey0123456789abcdef"}
	bodyMap := map[string]any{"messages": []any{}, "stream": false}
	req, err := buildOCRequestWithSubpath("claude-x", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_paid_0000000000000a")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("unmarshal sent body: %v", err)
	}
	if _, ok := sent["tools"]; ok {
		t.Fatalf("paid tier saw injected tools: %#v", sent["tools"])
	}
	if got := sent["stream"]; got != false {
		t.Fatalf("paid tier stream = %#v, want client-provided false", got)
	}
}

// 按模型判定后(与 free_tier_fingerprint_test.go 同一口径,issue #19 消融
// 修复建议): 免费模型 + 客户端自带工具时不再整包跳过,而是保留客户端工具
// 在前、把缺失的 bash/glob/grep/read 追加在后——上游按"有无四件"整体判定,
// 客户端带了 weather 改变不了缺四件就 403 的事实。
func TestOCRequest_FreeTierKeepsClientToolsWhenPresent(t *testing.T) {
	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	bodyMap := map[string]any{
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "weather", "parameters": map[string]any{"type": "object"}},
		}},
	}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	names := toolNames(t, sent)
	if len(names) != 5 || names[0] != "weather" {
		t.Fatalf("tools = %#v, want client weather kept first followed by the four required tools", sent["tools"])
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, want := range []string{"bash", "glob", "grep", "read"} {
		if !seen[want] {
			t.Fatalf("missing required tool %q, got %v", want, names)
		}
	}
}

// 免费层 hits count_tokens 计费子路径时不动 body(计费接口不套该门禁)。
func TestOCRequest_FreeTierSkipsInjectionOnNonChatCompletions(t *testing.T) {
	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	bodyMap := map[string]any{"messages": []any{}}
	req, err := buildOCRequestWithSubpath("claude-x", bodyMap, auth, false, "https://opencode.ai", "messages/count_tokens", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent["tools"]; ok {
		t.Fatalf("count_tokens saw injected tools: %#v", sent["tools"])
	}
	if got := sent["stream"]; got != nil {
		t.Fatalf("count_tokens saw forced stream: %#v", got)
	}
}

// 免费层请求上游强制 stream:true 后,非流式入站路径必须把上游 SSE 聚合回完整
// chat.completion JSON;且聚合发生在 isAnthropicFormat 之后,Anthropic 直发不会被
// 误判成 OpenAI chunk。按模型判定后(见 free_tier_fingerprint_test.go)需要把
// "primary-model" 显式标记为免费模型,该断言的"免费层 + stream:false"前提才成立。
func TestAggregate_callOpenCodeAPINonStreamAggregatesUpstreamSSE(t *testing.T) {
	modelsdev.SetFreeModelsForTest("primary-model")
	t.Cleanup(func() { modelsdev.SetFreeModelsForTest() })
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","created":1700000000,"model":"mimo-v2.5-free","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","created":1700000000,"model":"mimo-v2.5-free","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl_x","object":"chat.completion.chunk","created":1700000000,"model":"mimo-v2.5-free","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: sse}})

	body, status, _, err := callOpenCodeAPI(context.Background(), []byte(`{"model":"primary-model","messages":[],"stream":false}`), "primary-model", UpstreamAuth{Mode: AuthRoutePublic})
	if err != nil {
		t.Fatalf("callOpenCodeAPI: %v (status=%d)", err, status)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("response is not JSON (aggregate failed): %v; raw=%s", err, body)
	}
	if got := resp["object"]; got != "chat.completion" {
		t.Fatalf("object = %#v, want chat.completion", got)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %#v, want 1", choices)
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if got := msg["content"]; got != "Hello world" {
		t.Fatalf("aggregated content = %#v, want %q", got, "Hello world")
	}
	if got := msg["role"]; got != "assistant" {
		t.Fatalf("aggregated role = %#v, want assistant", got)
	}
	if got := resp["usage"].(map[string]any)["total_tokens"]; got != float64(15) {
		t.Fatalf("usage.total_tokens = %#v, want 15 (include_usage honored)", got)
	}

	// 出站请求必须被免费层门禁改写为 stream:true + include_usage。
	if got := transport.requestPayloads[0]["stream"]; got != true {
		t.Fatalf("upstream stream = %#v, want true", got)
	}
}

// 与 lite L1947-1983 对齐: 聚合器对"没有 chat.completion.chunk data 帧"的
// 输入(空、[DONE]-only、纯注释行)原样透传,不做臆造的补全——上游 session 层
// 保证正式答复至少有一个 chunk,这类空 SSE 本就是异常形态,让上层
// (convertAnthropicToOpenAI/JSON 解析器)按既有模糊性错误路径处理,避免
// 聚合器凭空造出缺 usage/finish_reason 的假 completion 掩盖异常。
func TestAggregate_DoneOnlyBodyPassThroughUnchanged(t *testing.T) {
	raw := "data: [DONE]\n\n"
	if got := aggregateOpenAIStream([]byte(raw), "m"); string(got) != raw {
		t.Fatalf("[DONE]-only body = %q, want passthrough %q", got, raw)
	}
	if got := aggregateOpenAIStream(nil, "m"); len(got) != 0 {
		t.Fatalf("empty body = %q, want empty passthrough", got)
	}
}

func TestAggregate_StreamWithErrorEventPassThrough(t *testing.T) {
	raw := "data: {\"error\":{\"type\":\"creditserror\",\"message\":\"insufficient balance\"}}\ndata: [DONE]\n"
	got := aggregateOpenAIStream([]byte(raw), "m")
	if string(got) != raw {
		t.Fatalf("error-event body must pass through unchanged")
	}
}

func TestAggregate_PlainJSONPassThrough(t *testing.T) {
	raw := `{"id":"chatcmpl_plain","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	got := aggregateOpenAIStream([]byte(raw), "m")
	if string(got) != raw {
		t.Fatalf("plain JSON body must pass through unchanged")
	}
}

func TestAggregate_ToolCallsAssembledByIndex(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"fi"}}]}}]}`,
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"le_path\":\"/etc/hosts\"}"}}]}}]}`,
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	got := aggregateOpenAIStream([]byte(sse), "m")
	var resp map[string]any
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	toolCalls, _ := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %#v, want 1", msg["tool_calls"])
	}
	fn := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read" || fn["arguments"] != `{"file_path":"/etc/hosts"}` {
		t.Fatalf("function = %#v, want read with joined arguments", fn)
	}
	if choices0 := resp["choices"].([]any)[0].(map[string]any); choices0["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choices0["finish_reason"])
	}
}

// normalizeOCVersion 的 426 防护: 低于 ocMinFreeTierVersion(lite 记 1.17.0,
// issue #19 消融实测已上移到 1.18.0)一律提到下限;空值/空白退到 default;
// 高于下限原样放行。
func TestOCVersion_NormalizeAgainstFreeTierFloor(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty to default", "", ocDefaultFreeTierVersion},
		{"whitespace to default", "  ", ocDefaultFreeTierVersion},
		{"older minor floats to floor", "1.15.9", ocMinFreeTierVersion},
		// 实测(issue #19 消融): 1.17.0 已越过上游下限被 426,下限=1.18.0.
		{"1.17.0 floats past lite floor", "1.17.0", ocMinFreeTierVersion},
		{"equal passes", "1.18.0", "1.18.0"},
		{"newer passes", "1.18.31", "1.18.31"},
		{"major bump passes", "2.0.0", "2.0.0"},
		{"prerelease segment keeps head digits", "1.17.9-beta", ocMinFreeTierVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeOCVersion(tc.in); got != tc.want {
				t.Fatalf("normalizeOCVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
