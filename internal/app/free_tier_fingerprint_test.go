package app

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/modelsdev"
)

var ocSessionShapeStrictRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// sentBody 读取 buildOCRequestWithSubpath 生成的上游请求体。
func sentBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("unmarshal sent body: %v", err)
	}
	return sent
}

// toolNames 按原始顺序收集 tools 数组里的工具名，同时兼容 OpenAI 形状
// (tools[].function.name)与 Anthropic/Responses 原生形状 (tools[].name)。
func toolNames(t *testing.T, sent map[string]any) []string {
	t.Helper()
	rawTools, _ := sent["tools"].([]any)
	names := make([]string, 0, len(rawTools))
	for _, rt := range rawTools {
		tm, ok := rt.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tm["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				names = append(names, name)
				continue
			}
		}
		if name, _ := tm["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func firstIndex(names []string, target string) int {
	for i, n := range names {
		if n == target {
			return i
		}
	}
	return -1
}

func assertFreeTierToolsPresent(t *testing.T, sent map[string]any) []string {
	t.Helper()
	names := toolNames(t, sent)
	for _, want := range []string{"bash", "glob", "grep", "read"} {
		if firstIndex(names, want) < 0 {
			t.Fatalf("missing required free-tier tool %q, got tools order %v", want, names)
		}
	}
	return names
}

// A1: Bearer sk-(真实 key, tier=Paid) + big-pickle(免费模型) + stream:false + 无 tools
// ⇒ 上游 body 必须具备全套免费层指纹: stream:true、bash/glob/grep/read 四件、
// 双会话头同值。判据必须是"模型免费"而非"客户端 tier"。
func TestFreeTierFingerprint_SKKeyFreeModelNoTools(t *testing.T) {
	modelsdev.SetFreeModelsForTest("big-pickle")
	t.Cleanup(func() { modelsdev.SetFreeModelsForTest() })

	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-realkey0123456789abcdef"}
	bodyMap := map[string]any{"messages": []any{}, "stream": false}
	req, err := buildOCRequestWithSubpath("big-pickle", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_0123abcdef45ABCDEf01234567")
	if err != nil {
		t.Fatal(err)
	}
	if auth.tier() != TierPaid {
		t.Fatalf("precondition: sk- key must route to TierPaid, got %v", auth.tier())
	}
	sent := sentBody(t, req)
	if got := sent["stream"]; got != true {
		t.Fatalf("stream = %#v, want true (free-model fingerprint applies regardless of client tier)", got)
	}
	opts, ok := sent["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v, want object", sent["stream_options"])
	}
	if got := opts["include_usage"]; got != true {
		t.Fatalf("stream_options.include_usage = %#v, want true", got)
	}
	assertFreeTierToolsPresent(t, sent)

	session := req.Header.Get("x-session-id")
	if !ocSessionShapeStrictRe.MatchString(session) {
		t.Fatalf("x-session-id = %q, want shape-compliant value", session)
	}
	if got := req.Header.Get("x-session-affinity"); got != session {
		t.Fatalf("x-session-affinity = %q differs from x-session-id = %q", got, session)
	}
	if got := req.Header.Get("x-opencode-session"); got != session {
		t.Fatalf("x-opencode-session = %q differs from x-session-id = %q", got, session)
	}
	if !ocSessionShapeStrictRe.MatchString(session) {
		t.Fatalf("session %q does not match upstream free-tier regex", session)
	}
}

// A2: Bearer public + big-pickle + 客户端自带 weather ⇒ 保留 weather 在前,
// 仅追加缺失的 bash/glob/grep/read(顺序 bash,glob,grep,read)。
func TestFreeTierFingerprint_PublicPartialClientTools(t *testing.T) {
	modelsdev.SetFreeModelsForTest("big-pickle")
	t.Cleanup(func() { modelsdev.SetFreeModelsForTest() })

	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	weather := map[string]any{
		"type":     "function",
		"function": map[string]any{"name": "weather", "parameters": map[string]any{"type": "object"}},
	}
	bodyMap := map[string]any{"messages": []any{}, "tools": []any{weather}}
	req, err := buildOCRequestWithSubpath("big-pickle", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	if got := sent["stream"]; got != true {
		t.Fatalf("stream = %#v, want true", got)
	}
	names := assertFreeTierToolsPresent(t, sent)
	if len(names) != 5 {
		t.Fatalf("tools = %v, want exactly weather + 4 required", names)
	}
	if names[0] != "weather" {
		t.Fatalf("client tool must stay first, got order %v", names)
	}
	for i, want := range []string{"bash", "glob", "grep", "read"} {
		if names[1+i] != want {
			t.Fatalf("appended tools order = %v, want bash,glob,grep,read appended after weather", names)
		}
	}
}

// A3: Bearer sk- + muse-spark-1.3-contributor-free(目录可能未知,靠 -free 后缀短路) +
// stream:false ⇒ 指纹完整就绪后才会打到上游(上游仍因档位 500,但网关不能再先被
// 第一道 403 指纹门拦下)。
func TestFreeTierFingerprint_SKKeyContributorFreeModel(t *testing.T) {
	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-realkey0123456789abcdef"}
	bodyMap := map[string]any{"messages": []any{}, "stream": false}
	req, err := buildOCRequestWithSubpath("muse-spark-1.3-contributor-free", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	if got := sent["stream"]; got != true {
		t.Fatalf("stream = %#v, want true so the request reaches the tier gate instead of the 403 fingerprint gate", got)
	}
	assertFreeTierToolsPresent(t, sent)
}

// A4: anthropic 原生 subpath "messages" + 付费 key + -free 模型 ⇒ 同样重做指纹。
// 客户端已带 bash(Anthropic 形状)时保留且不重复,仅追加余下三件;只强制 stream,
// 不往 Anthropic schema 里塞 OpenAI 专属的 stream_options。
func TestFreeTierFingerprint_SubpathMessages(t *testing.T) {
	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-realkey0123456789abcdef"}
	bodyMap := map[string]any{
		"messages":   []any{},
		"max_tokens": 64,
		"stream":     false,
		"tools": []any{map[string]any{
			"name":         "bash",
			"description":  "client bash",
			"input_schema": map[string]any{"type": "object"},
		}},
	}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", bodyMap, auth, false, "https://opencode.ai", "messages", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	if got := sent["stream"]; got != true {
		t.Fatalf("stream = %#v, want true for free-model on messages subpath", got)
	}
	if _, ok := sent["stream_options"]; ok {
		t.Fatalf("stream_options must not be injected into anthropic schema, got %#v", sent["stream_options"])
	}
	names := assertFreeTierToolsPresent(t, sent)
	if len(names) != 4 {
		t.Fatalf("tools = %v, want client bash + glob/grep/read (no duplicate bash)", names)
	}
	if names[0] != "bash" {
		t.Fatalf("client-provided bash must stay first, got %v", names)
	}
	// Anthropic 形状: 补齐的工具也应挂在 name 字段下,而不是 OpenAI 的 function 包装。
	rawTools, _ := sent["tools"].([]any)
	for i, rt := range rawTools {
		tm, _ := rt.(map[string]any)
		if _, hasFn := tm["function"]; hasFn {
			t.Fatalf("tool %d on messages subpath carries OpenAI function wrapper: %#v", i, tm)
		}
	}
	// 流式 anthropic 请求应切 SSE Accept 头。
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream for forced-streamed messages request", got)
	}
}

// A5: responses subpath + 付费 key + -free 模型 ⇒ 同样重做指纹;
// 客户端已给的 stream_options 保留自有键,仅补 include_usage。
// tools 断言兼容 Responses 形状(tools[].name)。
func TestFreeTierFingerprint_SubpathResponses(t *testing.T) {
	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-realkey0123456789abcdef"}
	bodyMap := map[string]any{
		"input":  []any{},
		"stream": false,
		"stream_options": map[string]any{
			"event_frequency": "full",
		},
	}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", bodyMap, auth, false, "https://opencode.ai", "responses", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	if got := sent["stream"]; got != true {
		t.Fatalf("stream = %#v, want true for free-model on responses subpath", got)
	}
	opts, ok := sent["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v, want preserved object", sent["stream_options"])
	}
	if got := opts["event_frequency"]; got != "full" {
		t.Fatalf("client stream_options.event_frequency = %#v, want preserved %q", got, "full")
	}
	if got := opts["include_usage"]; got != true {
		t.Fatalf("stream_options.include_usage = %#v, want true", got)
	}
	assertFreeTierToolsPresent(t, sent)
}

// A6: 付费认证 + 付费模型不套门禁(baseline,messages subpath);
//
//	免费模型 + 其它子路径(count_tokens)不套门禁。
func TestFreeTierFingerprint_PaidModelAndCountTokensSkipped(t *testing.T) {
	ocClientVer = "1.18.31"
	auth := UpstreamAuth{Mode: AuthRouteAuto, Token: "sk-realkey0123456789abcdef"}

	paidBody := map[string]any{"messages": []any{}, "stream": false}
	req, err := buildOCRequestWithSubpath("claude-x", paidBody, auth, false, "https://opencode.ai", "messages", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	if got := sent["stream"]; got != false {
		t.Fatalf("paid model stream = %#v, want untouched false", got)
	}
	if _, ok := sent["tools"]; ok {
		t.Fatalf("paid model saw injected tools: %#v", sent["tools"])
	}

	ctBody := map[string]any{"messages": []any{}}
	req, err = buildOCRequestWithSubpath("mimo-v2.5-free", ctBody, auth, false, "https://opencode.ai", "messages/count_tokens", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	sent = sentBody(t, req)
	if _, ok := sent["tools"]; ok {
		t.Fatalf("count_tokens saw injected tools: %#v", sent["tools"])
	}
	if got := sent["stream"]; got != nil {
		t.Fatalf("count_tokens saw forced stream: %#v", got)
	}
	if _, ok := sent["stream_options"]; ok {
		t.Fatalf("count_tokens saw stream_options: %#v", sent["stream_options"])
	}
}
