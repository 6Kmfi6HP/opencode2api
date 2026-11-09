package app

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
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
	for _, want := range []string{"edit", "glob", "grep", "read", "shell", "write"} {
		if firstIndex(names, want) < 0 {
			t.Fatalf("missing manifest tool %q, got tools order %v", want, names)
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

	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "1.18.31"})
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

	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "1.18.31"})
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
	if len(names) != 7 || names[len(names)-1] != "weather" {
		t.Fatalf("tools = %v, want 6 manifest tools then weather", names)
	}
	for i, want := range []string{"edit", "glob", "grep", "read", "shell", "write"} {
		if names[i] != want {
			t.Fatalf("manifest prefix = %v, want edit,glob,grep,read,shell,write", names)
		}
	}
}

func TestHasFreeTierEnvBlock(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"standard multi-line", "<env>\nWorking directory: /tmp/lab\n</env>", true},
		{"inline single line", "<env>Working directory: /workspace</env>", true},
		{"empty working directory", "<env>Working directory:</env>", true},
		{"embedded in text", "some preamble <env>\n  Working directory: /foo\n</env> trailing instructions", true},
		{"missing closing env", "<env>\nWorking directory: /tmp/lab", false},
		{"missing opening env", "Working directory: /tmp/lab\n</env>", false},
		{"missing working directory", "<env>\nPlatform: darwin\n</env>", false},
		{"case mismatch uppercase", "<ENV>Working directory: /workspace</ENV>", false},
		{"wrong order", "</env>Working directory: /workspace<env>", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasFreeTierEnvBlock(tt.text); got != tt.want {
				t.Errorf("hasFreeTierEnvBlock(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestEnsureFreeTierSystemEnv_ChatCompletions(t *testing.T) {
	t.Run("empty messages adds system message", func(t *testing.T) {
		body := map[string]any{"messages": []any{}}
		ensureFreeTierSystemEnv(body, "chat/completions")
		msgs := body["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("len(messages) = %d, want 1", len(msgs))
		}
		m := msgs[0].(map[string]any)
		if m["role"] != "system" || !hasFreeTierEnvBlock(m["content"].(string)) {
			t.Fatalf("first message = %v, want system with env block", m)
		}
	})

	t.Run("user message only prepends system message", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		}}
		ensureFreeTierSystemEnv(body, "chat/completions")
		msgs := body["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("len(messages) = %d, want 2", len(msgs))
		}
		sys := msgs[0].(map[string]any)
		if sys["role"] != "system" || !hasFreeTierEnvBlock(sys["content"].(string)) {
			t.Fatalf("first message = %v, want system with env block", sys)
		}
		user := msgs[1].(map[string]any)
		if user["role"] != "user" || user["content"] != "hello" {
			t.Fatalf("second message = %v, want user hello", user)
		}
	})

	t.Run("existing system string without env prepends env block", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "hi"},
		}}
		ensureFreeTierSystemEnv(body, "chat/completions")
		msgs := body["messages"].([]any)
		sys := msgs[0].(map[string]any)
		content := sys["content"].(string)
		if !hasFreeTierEnvBlock(content) {
			t.Fatalf("content = %q, want env block present", content)
		}
		if !strings.HasSuffix(content, "You are a helpful assistant.") {
			t.Fatalf("content = %q, want original prompt preserved at suffix", content)
		}
	})

	t.Run("existing system string with env left untouched", func(t *testing.T) {
		orig := "<env>\n  Working directory: /my/custom/path\n</env>\n\nCustom prompt"
		body := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": orig},
		}}
		ensureFreeTierSystemEnv(body, "chat/completions")
		msgs := body["messages"].([]any)
		sys := msgs[0].(map[string]any)
		if sys["content"] != orig {
			t.Fatalf("content = %q, want untouched %q", sys["content"], orig)
		}
	})
}

func TestEnsureFreeTierSystemEnv_MessagesSubpath(t *testing.T) {
	t.Run("nil system sets default env block", func(t *testing.T) {
		body := map[string]any{"messages": []any{}}
		ensureFreeTierSystemEnv(body, "messages")
		sys, ok := body["system"].(string)
		if !ok || !hasFreeTierEnvBlock(sys) {
			t.Fatalf("body[system] = %v, want string with env block", body["system"])
		}
	})

	t.Run("string system without env prepends env block", func(t *testing.T) {
		body := map[string]any{"system": "System instructions here"}
		ensureFreeTierSystemEnv(body, "messages")
		sys, ok := body["system"].(string)
		if !ok || !hasFreeTierEnvBlock(sys) || !strings.HasSuffix(sys, "System instructions here") {
			t.Fatalf("body[system] = %v, want env block + instructions", body["system"])
		}
	})

	t.Run("string system with env untouched", func(t *testing.T) {
		orig := "<env>\nWorking directory: /tmp\n</env>"
		body := map[string]any{"system": orig}
		ensureFreeTierSystemEnv(body, "messages")
		if body["system"] != orig {
			t.Fatalf("body[system] = %v, want untouched %q", body["system"], orig)
		}
	})
}

func TestEnsureFreeTierSystemEnv_ResponsesSubpath(t *testing.T) {
	t.Run("nil instructions sets default env block", func(t *testing.T) {
		body := map[string]any{}
		ensureFreeTierSystemEnv(body, "responses")
		instr, ok := body["instructions"].(string)
		if !ok || !hasFreeTierEnvBlock(instr) {
			t.Fatalf("body[instructions] = %v, want env block", body["instructions"])
		}
	})

	t.Run("string instructions without env prepends env block", func(t *testing.T) {
		body := map[string]any{"instructions": "Base instructions"}
		ensureFreeTierSystemEnv(body, "responses")
		instr, ok := body["instructions"].(string)
		if !ok || !hasFreeTierEnvBlock(instr) || !strings.HasSuffix(instr, "Base instructions") {
			t.Fatalf("body[instructions] = %v, want env block + Base instructions", body["instructions"])
		}
	})
}

// 免费层指纹补齐：走到上游 responses 子路径时，缺失的四件工具必须按
// OpenAI Responses 形状（{"type":"function","name","parameters"}）注入；
// Anthropic 的 ("name"+input_schema) 形状会被上游按 `did not match any
// supported type` 拒绝。
func TestFreeTierFingerprint_ResponsesSubpathInjectsResponsesShape(t *testing.T) {
	auth := UpstreamAuth{Mode: AuthRoutePublic, Token: ""}
	bodyMap := map[string]any{"input": []any{}, "stream": false}
	req, err := buildOCRequestWithSubpath("muse-spark-1.3-contributor-free", bodyMap, auth, false, "https://opencode.ai", "responses", "ses_12345678901234567890123")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.NewDecoder(req.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("expected 6 manifest tools on responses subpath, got %d: %s", len(tools), mustJSON(out))
	}
	for i, raw := range tools {
		tm, _ := raw.(map[string]any)
		if tm["type"] != "function" {
			t.Fatalf("tools[%d].type = %v, want function: %s", i, tm["type"], mustJSON(tm))
		}
		if name, _ := tm["name"].(string); name == "" {
			t.Fatalf("tools[%d] missing top-level name: %s", i, mustJSON(tm))
		}
		if _, ok := tm["parameters"].(map[string]any); !ok {
			t.Fatalf("tools[%d] missing parameters object: %s", i, mustJSON(tm))
		}
		if _, hasFn := tm["function"]; hasFn {
			t.Fatalf("tools[%d] must NOT use chat nested function shape: %s", i, mustJSON(tm))
		}
		if _, hasInputSchema := tm["input_schema"]; hasInputSchema {
			t.Fatalf("tools[%d] must NOT use anthropic input_schema shape: %s", i, mustJSON(tm))
		}
	}
	if out["stream"] != true {
		t.Fatalf("free-tier responses must force stream:true, got %v", out["stream"])
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestFreeTierFingerprint_SKKeyContributorFreeModel(t *testing.T) {
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "1.18.31"})
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
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "1.18.31"})
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
	if len(names) != 7 || names[len(names)-1] != "bash" {
		t.Fatalf("tools = %v, want 6 manifest tools then client bash", names)
	}
	if names[0] == "bash" {
		t.Fatalf("manifest must precede client bash, got %v", names)
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
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "1.18.31"})
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
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "1.18.31"})
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

// LowercaseStub: 客户端已带 PascalCase 四件(Bash/Glob/Grep/Read)时,
// ensureFreeTierTools 必须追加小写 stub(上游免费层门禁按小写名精确匹配,
// 大小写敏感;2026-10-07 public 实测:只带 PascalCase 四件被 403 整档拒)。
// 终态 tools = 4 原工具(PascalCase,位置保留) + 4 小写 stub。
func TestEnsureFreeTierTools_CaseInsensitiveNoDup(t *testing.T) {
	mkFn := func(name string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	mkBare := func(name string) map[string]any {
		return map[string]any{"name": name}
	}
	cases := []struct {
		subpath string
		tools   []any
	}{
		{"chat/completions", []any{mkFn("Bash"), mkFn("Glob"), mkFn("Grep"), mkFn("Read")}},
		{"messages", []any{mkBare("Bash"), mkBare("Glob"), mkBare("Grep"), mkBare("Read")}},
		{"responses", []any{mkBare("Bash"), mkBare("Glob"), mkBare("Grep"), mkBare("Read")}},
	}
	for _, tc := range cases {
		t.Run(tc.subpath, func(t *testing.T) {
			bodyMap := map[string]any{"tools": tc.tools}
			ensureFreeTierTools(bodyMap, tc.subpath)
			rawTools, _ := bodyMap["tools"].([]any)
			if len(rawTools) != 8 {
				names := toolNames(t, bodyMap)
				t.Fatalf("subpath %q: tools = %v, want 8 (4 PascalCase + 4 lowercase stub)", tc.subpath, names)
			}
			// 前 4 个保持原 PascalCase,后 4 个为小写 stub 且顺序固定。
			lower := map[string]bool{}
			for i, tl := range rawTools {
				name := freeTierToolNameOf(tl.(map[string]any))
				if i < 4 {
					continue
				}
				if name != strings.ToLower(name) {
					t.Fatalf("subpath %q: stub %d = %q, want lowercase", tc.subpath, i, name)
				}
				lower[name] = true
			}
			for _, want := range []string{"bash", "glob", "grep", "read"} {
				if !lower[want] {
					t.Fatalf("subpath %q: missing lowercase stub %q", tc.subpath, want)
				}
			}
		})
	}
}

// LowercaseHit: 客户端已带小写四件时不追加(幂等),三个 subpath 各一测。
func TestEnsureFreeTierTools_LowercaseNoDup(t *testing.T) {
	mkFn := func(name string) map[string]any {
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	mkBare := func(name string) map[string]any {
		return map[string]any{"name": name}
	}
	cases := []struct {
		subpath string
		tools   []any
	}{
		{"chat/completions", []any{mkFn("bash"), mkFn("glob"), mkFn("grep"), mkFn("read")}},
		{"messages", []any{mkBare("bash"), mkBare("glob"), mkBare("grep"), mkBare("read")}},
		{"responses", []any{mkBare("bash"), mkBare("glob"), mkBare("grep"), mkBare("read")}},
	}
	for _, tc := range cases {
		t.Run(tc.subpath, func(t *testing.T) {
			bodyMap := map[string]any{"tools": tc.tools}
			ensureFreeTierTools(bodyMap, tc.subpath)
			rawTools, _ := bodyMap["tools"].([]any)
			if len(rawTools) != 4 {
				names := toolNames(t, bodyMap)
				t.Fatalf("subpath %q: tools = %v, want 4 (no dup injection)", tc.subpath, names)
			}
		})
	}
}
