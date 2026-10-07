package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/6Kmfi6HP/opencode2api/internal/domain"
	"github.com/6Kmfi6HP/opencode2api/internal/stats"
)

// ======================== 短名原语 ========================

func TestAnthropicToolNameValid(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"a-b_c9", true},
		{strings.Repeat("x", 64), true},  // 64 字节边界
		{strings.Repeat("x", 65), false}, // 65 字节超限
		{"", false},                      // 空串
		{"a.b", false},                   // 非法字符
		{"a b", false},                   // 空格
		{"工具", false},                    // 非 ASCII
		{strings.Repeat("工", 64), false}, // 64 CJK rune = 192 字节 → 字节语义非法
		{"mcp__server__tool", true},      // MCP 风格合法
		{"mcp__codex_apps__plugin_management___update_app_permissionsXYZi000", false},
	}
	for _, c := range cases {
		if got := isAnthropicToolNameValid(c.name); got != c.valid {
			t.Fatalf("isAnthropicToolNameValid(%q) = %v, want %v", c.name, got, c.valid)
		}
	}
}

func TestFoldAnthropicName(t *testing.T) {
	if got := foldAnthropicName("a.b:c d"); got != "a_b_c_d" {
		t.Fatalf("fold = %q, want a_b_c_d", got)
	}
	if got := foldAnthropicName("工具"); got != "__" {
		t.Fatalf("fold = %q, want __", got)
	}
	if got := foldAnthropicName("mcp.example.com__tool"); got != "mcp_example_com__tool" {
		t.Fatalf("fold = %q", got)
	}
	// 幂等
	if foldAnthropicName("a_b_c") != "a_b_c" {
		t.Fatal("fold must be idempotent on already-folded names")
	}
}

func TestAnthropicHashedName(t *testing.T) {
	long := strings.Repeat("m", 68)
	got := anthropicHashedName(long)
	if len(got) != anthropicMaxNameLength {
		t.Fatalf("hashed length = %d, want 64: %q", len(got), got)
	}
	if !isAnthropicToolNameValid(got) {
		t.Fatalf("hashed name invalid: %q", got)
	}
	if anthropicHashedName(long) != got {
		t.Fatal("anthropicHashedName must be deterministic")
	}
	other := anthropicHashedName(strings.Repeat("m", 67) + "x")
	if other == got {
		t.Fatal("different names must not collide")
	}
	// 与旧 shortenResponsesName 输出逐字节一致（合法字符集超长 ASCII 名）。
	if got != shortenResponsesName(long) {
		t.Fatalf("hashed %q != legacy shorten %q", got, shortenResponsesName(long))
	}
}

func TestShortenAnthropicToolName(t *testing.T) {
	// 合法直通
	const ok = "list_files"
	if shortenAnthropicToolName(ok) != ok {
		t.Fatalf("valid name must passthrough: %q", shortenAnthropicToolName(ok))
	}
	// 短名含非法字符 → 折叠
	if got := shortenAnthropicToolName("a.b"); got != "a_b" {
		t.Fatalf("folded = %q, want a_b", got)
	}
	// 68 字符 ASCII → 恒 64 哈希形（生产 bug 场景）
	long := strings.Repeat("n", 68)
	got := shortenAnthropicToolName(long)
	if len(got) != 64 || !isAnthropicToolNameValid(got) {
		t.Fatalf("shortened = %q", got)
	}
	// 幂等
	if shortenAnthropicToolName(got) != got {
		t.Fatalf("shorten must be idempotent: %q -> %q", got, shortenAnthropicToolName(got))
	}
	// 长 CJK → 纯 ASCII ≤64
	cjk := strings.Repeat("工", 70)
	got = shortenAnthropicToolName(cjk)
	if !isAnthropicToolNameValid(got) {
		t.Fatalf("CJK shortened invalid: %q", got)
	}
}

func TestShortenRecord_CollisionResolution(t *testing.T) {
	// 走真实 walker 流程（prepare 先占位合法名）："a.b" 折叠出的 "a_b" 与
	// 声明的 "a_b" 碰撞：声明过的合法名恒得净形，折叠名强制哈希形，双向
	// 映射保证两个原名都正确还原。
	rw := newResponsesNameRewrites(false)
	body := map[string]any{
		"tools": []map[string]any{{"name": "a.b"}, {"name": "a_b"}},
	}
	if !rw.shortenAnthropicBodyNames(body) {
		t.Fatal("expected change")
	}
	tools := body["tools"].([]map[string]any)
	aDotB, aUnderscoreB := tools[0]["name"].(string), tools[1]["name"].(string)
	if aUnderscoreB != "a_b" {
		t.Fatalf("declared valid name must passthrough: %q", aUnderscoreB)
	}
	if aDotB == aUnderscoreB {
		t.Fatalf("folded name collided with declared name: %q", aDotB)
	}
	if !isAnthropicToolNameValid(aDotB) {
		t.Fatalf("disambiguated name invalid: %q", aDotB)
	}
	if rw.restore(aDotB) != "a.b" || rw.restore(aUnderscoreB) != "a_b" {
		t.Fatalf("restore mismatch: %q->%q %q->%q", aDotB, rw.restore(aDotB), aUnderscoreB, rw.restore(aUnderscoreB))
	}
	// 同一原名两次调用同短名
	if rw.shortenRecord("a.b") != aDotB {
		t.Fatal("same original must map to same shortened name")
	}
}

func TestShortenRecord_EmptyName(t *testing.T) {
	rw := newResponsesNameRewrites(false)
	if got := rw.shortenRecord(""); got != unnamedAnthropicToolName {
		t.Fatalf("empty name = %q, want unnamed_tool", got)
	}
	if got := rw.restore(unnamedAnthropicToolName); got != "" {
		t.Fatalf("restore(unnamed_tool) = %q, want empty", got)
	}
	// 声明过的 unnamed_tool 与空名消歧：声明的合法名恒得净形。
	rw2 := newResponsesNameRewrites(false)
	rw2.shortenRecord(unnamedAnthropicToolName) // prepare 占位
	if got := rw2.shortenRecord(""); got == unnamedAnthropicToolName {
		t.Fatal("empty name must not steal a declared unnamed_tool")
	}
}

func TestShortenAnthropicBodyNames_Table(t *testing.T) {
	const long68 = "mcp__codex_apps__plugin_management___update_app_permissionsXYZi000"
	// []map[string]any 形状（转换器构造体）
	t.Run("converter shapes", func(t *testing.T) {
		rw := newResponsesNameRewrites(false)
		body := map[string]any{
			"tools":       []map[string]any{{"name": long68, "input_schema": map[string]any{}}},
			"tool_choice": map[string]any{"type": "tool", "name": long68},
			"messages": []map[string]any{{
				"role":    "assistant",
				"content": []map[string]any{{"type": "tool_use", "id": "tu1", "name": long68, "input": map[string]any{}}},
			}},
		}
		if !rw.shortenAnthropicBodyNames(body) {
			t.Fatal("expected change")
		}
		short := rw.outbound[long68]
		if short == "" || len(short) != 64 {
			t.Fatalf("outbound = %q", short)
		}
		tools := body["tools"].([]map[string]any)
		if tools[0]["name"] != short {
			t.Fatalf("tools name = %#v", tools[0]["name"])
		}
		tc := body["tool_choice"].(map[string]any)
		if tc["name"] != short {
			t.Fatalf("tool_choice name = %#v", tc["name"])
		}
		msgs := body["messages"].([]map[string]any)
		content := msgs[0]["content"].([]map[string]any)
		if content[0]["name"] != short {
			t.Fatalf("history tool_use name = %#v", content[0]["name"])
		}
	})
	// []any 形状（json.Unmarshal 产物）+ 双向映射
	t.Run("json shapes", func(t *testing.T) {
		rw := newResponsesNameRewrites(false)
		raw := `{"tools":[{"type":"custom","name":"` + long68 + `"}],"messages":[{"role":"user","content":[{"type":"tool_use","name":"` + long68 + `"}]}]}`
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		if !rw.shortenAnthropicBodyNames(body) {
			t.Fatal("expected change")
		}
		short := rw.outbound[long68]
		tools := body["tools"].([]any)
		if tools[0].(map[string]any)["name"] != short {
			t.Fatalf("tools name = %#v", tools[0].(map[string]any)["name"])
		}
		if rw.inbound[short] != long68 {
			t.Fatalf("inbound = %q", rw.inbound[short])
		}
	})
	// 全合法 → 快路径（false 且字节不变）
	t.Run("fast path", func(t *testing.T) {
		rw := newResponsesNameRewrites(false)
		raw := `{"tools":[{"name":"ok_tool","input_schema":{}}],"messages":[{"role":"user","content":"hi"}]}`
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		if rw.shortenAnthropicBodyNames(body) {
			t.Fatal("valid names must not change")
		}
		if len(rw.outbound) != 0 || len(rw.inbound) != 0 {
			t.Fatalf("fast path must keep maps empty: %#v %#v", rw.outbound, rw.inbound)
		}
	})
	// 缺失 name 键跳过；空 name 占位
	t.Run("missing and empty names", func(t *testing.T) {
		rw := newResponsesNameRewrites(false)
		raw := `{"tools":[{"type":"web_search_20250305"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","name":""}]}]}`
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		rw.shortenAnthropicBodyNames(body)
		tools := body["tools"].([]any)
		if _, ok := tools[0].(map[string]any)["name"]; ok {
			t.Fatal("missing name key must not be injected")
		}
		msgs := body["messages"].([]any)
		content := msgs[0].(map[string]any)["content"].([]any)
		if content[0].(map[string]any)["name"] != unnamedAnthropicToolName {
			t.Fatalf("empty name = %#v", content[0].(map[string]any)["name"])
		}
	})
	// 跨请求确定性：同一 body 两次走不同 rw 结果一致
	t.Run("deterministic across requests", func(t *testing.T) {
		raw := `{"tools":[{"type":"custom","name":"` + long68 + `"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"` + long68 + `"}]}]}`
		short := func() string {
			var body map[string]any
			if err := json.Unmarshal([]byte(raw), &body); err != nil {
				t.Fatal(err)
			}
			rw := newResponsesNameRewrites(false)
			rw.shortenAnthropicBodyNames(body)
			return body["tools"].([]any)[0].(map[string]any)["name"].(string)
		}
		first, second := short(), short()
		if first != second {
			t.Fatal("shortened name must be stable across requests")
		}
	})
	// mcp_servers 缩短 + 复合名一致改写（点号 server 名折叠后可读前缀保留，
	// 复合名改写仍 ≤64 时用复合形；否则哈希兜底）
	t.Run("mcp servers and composed names", func(t *testing.T) {
		rw := newResponsesNameRewrites(false)
		dottedServer := "mcp.example.com"
		body := map[string]any{
			"mcp_servers": []map[string]any{{"name": dottedServer, "type": "url", "url": "https://" + dottedServer}},
			"tools":       []map[string]any{{"name": "mcp__" + dottedServer + "__do_thing", "input_schema": map[string]any{}}},
		}
		rw.shortenAnthropicBodyNames(body)
		short := rw.outbound[dottedServer]
		if short != "mcp_example_com" {
			t.Fatalf("server fold = %q", short)
		}
		servers := body["mcp_servers"].([]map[string]any)
		if servers[0]["name"] != short {
			t.Fatalf("server name = %#v", servers[0]["name"])
		}
		tools := body["tools"].([]map[string]any)
		if tools[0]["name"] != "mcp__"+short+"__do_thing" {
			t.Fatalf("composed name = %#v", tools[0]["name"])
		}
		// 还原侧：上游按缩短 server 名拼的复合名回原名
		if got := rw.restore("mcp__" + short + "__do_thing"); got != "mcp__"+dottedServer+"__do_thing" {
			t.Fatalf("composed restore = %q", got)
		}
		// 超长 server 名：复合名哈希兜底
		rw2 := newResponsesNameRewrites(false)
		longServer := "very-long-mcp-server-name-that-exceeds-the-anthropic-limit-xxxxx000"
		body2 := map[string]any{
			"mcp_servers": []map[string]any{{"name": longServer, "type": "url", "url": "https://mcp.example.com"}},
			"tools":       []map[string]any{{"name": "mcp__" + longServer + "__do_thing", "input_schema": map[string]any{}}},
		}
		rw2.shortenAnthropicBodyNames(body2)
		tools2 := body2["tools"].([]map[string]any)
		composed := tools2[0]["name"].(string)
		if !isAnthropicToolNameValid(composed) {
			t.Fatalf("composed name invalid: %q", composed)
		}
		if rw2.restore(composed) != "mcp__"+longServer+"__do_thing" {
			t.Fatalf("hashed composed restore = %q", rw2.restore(composed))
		}
	})
}

func TestSanitizeAnthropicUpstreamBody_FastPathBytes(t *testing.T) {
	raw := []byte(`{"model":"m","tools":[{"name":"ok","input_schema":{}}],"messages":[{"role":"user","content":"hi"}]}`)
	out, rw := sanitizeAnthropicUpstreamBody(raw, false)
	if string(out) != string(raw) {
		t.Fatalf("fast path must return original bytes, got %s", out)
	}
	if len(rw.outbound) != 0 || len(rw.inbound) != 0 {
		t.Fatal("fast path must keep maps empty")
	}
}

// ======================== 还原侧 ========================

func TestRestoreAnthropicResponseNames_MessageAndSSE(t *testing.T) {
	rw := newResponsesNameRewrites(false)
	long := strings.Repeat("z", 68)
	short := rw.shortenRecord(long)
	// message JSON 形：tool_use name 还原，文本（模型回声）永不改写。
	msg := []byte(`{"id":"m1","type":"message","content":[{"type":"tool_use","id":"t1","name":"` + short + `","input":{}},{"type":"text","text":"echo ` + short + `"}]}`)
	out := string(restoreAnthropicResponseNames(msg, rw))
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	content := got["content"].([]any)
	if name := content[0].(map[string]any)["name"].(string); name != long {
		t.Fatalf("tool_use name = %q, want original", name)
	}
	// 文本不被改写（仍含上游回显的短名）
	if text := content[1].(map[string]any)["text"].(string); text != "echo "+short {
		t.Fatalf("text must not be rewritten: %q", text)
	}
	// SSE 兜底形（非流式请求偶尔回 SSE）
	sse := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"" + short + "\"}}\n\n")
	out = string(restoreAnthropicResponseNames(sse, rw))
	if !strings.Contains(out, long) {
		t.Fatalf("SSE restore failed: %s", out)
	}
	var evt map[string]any
	line := strings.Split(strings.Split(out, "\n")[1], "data: ")[1]
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		t.Fatal(err)
	}
	if name := evt["content_block"].(map[string]any)["name"].(string); name != long {
		t.Fatalf("SSE tool_use name = %q", name)
	}
}

func TestRestoreAnthropicStreamLine_WithRewrites(t *testing.T) {
	rw := newResponsesNameRewrites(true) // 含占位名大小写还原
	long := strings.Repeat("q", 68)
	short := rw.shortenRecord(long)
	line := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"" + short + "\",\"input\":{}}}\n"
	got := restoreAnthropicStreamLine(line, rw)
	if !strings.Contains(got, long) || strings.Contains(got, short) {
		t.Fatalf("stream restore failed: %s", got)
	}
	// 未命中的行字节原样
	if got := restoreAnthropicStreamLine("event: ping\n", rw); got != "event: ping\n" {
		t.Fatalf("unmatched line changed: %q", got)
	}
	// 占位名大小写还原保持（restoreStubCase=true）
	got = restoreAnthropicStreamLine("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"bash\"}}\n", rw)
	if !strings.Contains(got, `"name":"Bash"`) {
		t.Fatalf("stub case restore lost: %s", got)
	}
	// 幂等
	if restoreAnthropicStreamLine(got, rw) != got {
		t.Fatal("restore must be idempotent")
	}
	// 旧包装行为不变：restoreCase=false → 原样；true → 占位名还原
	if got := restoreAnthropicStreamLineCase(line, false); got != line {
		t.Fatalf("compat wrapper restoreCase=false changed line: %s", got)
	}
	got = restoreAnthropicStreamLineCase("data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"bash\"}}\n", true)
	if !strings.Contains(got, `"name":"Bash"`) {
		t.Fatalf("compat wrapper stub case lost: %s", got)
	}
}

func TestRestoreAnthropicBodyToolCase_CompatWrapper(t *testing.T) {
	body := []byte(`{"content":[{"type":"tool_use","id":"t","name":"grep","input":{}}]}`)
	if got := restoreAnthropicBodyToolCase(body, false); string(got) != string(body) {
		t.Fatalf("restoreCase=false must passthrough: %s", got)
	}
	got := string(restoreAnthropicBodyToolCase(body, true))
	if !strings.Contains(got, `"name":"Grep"`) {
		t.Fatalf("stub case restore lost: %s", got)
	}
}

func TestMCPPrefixRestore(t *testing.T) {
	rw := newResponsesNameRewrites(false)
	origServer := "very-long-mcp-server-name-that-exceeds-the-anthropic-limit-xxxxx000"
	short := rw.shortenRecord(origServer)
	rw.mcpServers[short] = origServer
	if got := rw.restore("mcp__" + short + "__tool"); got != "mcp__"+origServer+"__tool" {
		t.Fatalf("mcp prefix restore = %q", got)
	}
	// 未知前缀原样
	if got := rw.restore("mcp__unknown__tool"); got != "mcp__unknown__tool" {
		t.Fatalf("unknown prefix changed: %q", got)
	}
}

func TestShortenResponsesName_CharsetFold(t *testing.T) {
	// 有意行为变化：≤64 但含非法字符的 muse-spark 透传名现在折叠并在中继还原。
	rw := newResponsesNameRewrites(false)
	dotted := "docs.search.api"
	short := rw.shortenRecord(dotted)
	if short != "docs_search_api" {
		t.Fatalf("dotted fold = %q", short)
	}
	if rw.restore(short) != dotted {
		t.Fatalf("restore = %q", rw.restore(short))
	}
}

func TestRestoreResponsesBodyNames(t *testing.T) {
	rw := newResponsesNameRewrites(false)
	long := strings.Repeat("w", 70)
	short := rw.shortenRecord(long)
	body := []byte(`{"output":[{"type":"function_call","name":"` + short + `","arguments":"{}"}]}`)
	out := string(restoreResponsesBodyNames(body, rw))
	if !strings.Contains(out, long) || strings.Contains(out, short) {
		t.Fatalf("responses restore failed: %s", out)
	}
}

// ======================== 集成回归（真实 handler + fake 上游） ========================

const nameCompatLongTool = "mcp__codex_apps__plugin_management___update_app_permissionsXYZi000" // 66 字符（>64）

// TestClaudeMessages_LongToolNames_MappedAndRestored_Stream 复现生产 bug：
// 68 字符工具名经 anthropic 直通不再 400，上游收到合法短名，客户端流中
// 看到原始长名。
func TestClaudeMessages_LongToolNames_MappedAndRestored_Stream(t *testing.T) {
	const model = "claude-namecompat-stream"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-namecompat-*", Protocol: "anthropic"}})
	// 上游回显 content_block_start：实际短名（模拟上游按收到的 tools 回放）。
	short := shortenAnthropicToolName(nameCompatLongTool)
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_nc\",\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":4}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_nc\",\"name\":\"" + short + "\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":6}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})
	reqBody := `{"model":"` + model + `","max_tokens":128,"stream":true,` +
		`"tools":[{"name":"` + nameCompatLongTool + `","description":"d","input_schema":{"type":"object","properties":{}}}],` +
		`"tool_choice":{"type":"tool","name":"` + nameCompatLongTool + `"},` +
		`"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_hist","name":"` + nameCompatLongTool + `","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_hist","content":"ok"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// 上游收到的 tools/tool_choice/历史三处同短名且合法，原始长名不再出现。
	payload := transport.requestPayloads[0]
	tools := payload["tools"].([]any)
	upName := tools[0].(map[string]any)["name"].(string)
	if upName == nameCompatLongTool || !isAnthropicToolNameValid(upName) {
		t.Fatalf("upstream tools name = %q", upName)
	}
	if tc := payload["tool_choice"].(map[string]any); tc["name"] != upName {
		t.Fatalf("tool_choice name = %#v, want %q", tc["name"], upName)
	}
	msgs := payload["messages"].([]any)
	assistantContent := msgs[1].(map[string]any)["content"].([]any)
	if histName := assistantContent[0].(map[string]any)["name"].(string); histName != upName {
		t.Fatalf("history name = %q, want %q", histName, upName)
	}
	if want := shortenAnthropicToolName(nameCompatLongTool); upName != want {
		t.Fatalf("upstream name = %q, want %q", upName, want)
	}

	// 客户端流中看到原始长名（还原自上游回显的短名）。
	clientBody := rec.Body.String()
	if !strings.Contains(clientBody, nameCompatLongTool) {
		t.Fatalf("client stream missing original name; body=%s", clientBody)
	}
	if strings.Contains(clientBody, upName) {
		t.Fatalf("client stream leaked shortened name %q", upName)
	}
	if !strings.Contains(clientBody, "message_stop") {
		t.Fatalf("stream must end with message_stop; body=%s", clientBody)
	}
}

// TestClaudeMessages_AllValidNames_BytePassthrough 全合法名 → 快路径，
// 上游 payload 与请求语义逐字段一致。
func TestClaudeMessages_AllValidNames_BytePassthrough(t *testing.T) {
	const model = "claude-namecompat-valid"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-namecompat-*", Protocol: "anthropic"}})
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"msg_v","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
	})
	reqBody := `{"model":"` + model + `","max_tokens":128,"tools":[{"name":"read_file","description":"d","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	payload := transport.requestPayloads[0]
	tools := payload["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "read_file" {
		t.Fatalf("valid name must passthrough: %#v", tools[0].(map[string]any)["name"])
	}
}

// TestClaudeMessages_NonStream_Restored 非流式 2xx：上游回短名，客户端得原名。
func TestClaudeMessages_NonStream_Restored(t *testing.T) {
	const model = "claude-namecompat-nonstream"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "claude-namecompat-*", Protocol: "anthropic"}})
	// 请求声明超长工具（登记 inbound 映射）；上游回显实际短名 → 客户端得原名。
	short := shortenAnthropicToolName(nameCompatLongTool)
	upstream := `{"id":"msg_ns","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"` + short + `","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: upstream}})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"`+model+`","max_tokens":128,"tools":[{"name":"`+nameCompatLongTool+`","description":"d","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), short) {
		t.Fatalf("client body leaked shortened name %q: %s", short, rec.Body.String())
	}
}

// TestChatViaAnthropic_LongToolName_Restored chat 入站：delta 与非流式
// tool_calls 均为原始长名。
func TestChatViaAnthropic_LongToolName_Restored(t *testing.T) {
	const pattern = "chat-namecompat-*"
	const model = "chat-namecompat-x"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: pattern, Protocol: "anthropic"}})
	short := shortenAnthropicToolName(nameCompatLongTool)
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu\",\"name\":\"" + short + "\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: sse, header: http.Header{"Content-Type": []string{"text/event-stream"}}},
	})
	reqBody := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"` + nameCompatLongTool + `","parameters":{"type":"object","properties":{}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	chatCompletionsHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), nameCompatLongTool) {
		t.Fatalf("client stream missing original name; body=%s", rec.Body.String())
	}
	payload := transport.requestPayloads[0]
	tools := payload["tools"].([]any)
	if got := tools[0].(map[string]any)["name"].(string); got != short {
		t.Fatalf("upstream tools name = %q, want %q", got, short)
	}
}

// TestResponsesViaAnthropic_LongToolName_Restored responses 入站非流式：
// output[].function_call.name 为原始长名。
func TestResponsesViaAnthropic_LongToolName_Restored(t *testing.T) {
	const model = "resp-namecompat-x"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "resp-namecompat-*", Protocol: "anthropic"}})
	short := shortenAnthropicToolName(nameCompatLongTool)
	upstream := `{"id":"msg_r","type":"message","role":"assistant","content":[{"type":"tool_use","id":"tu","name":"` + short + `","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
	transport := installFakeOpenCodeClient(t, []fakeUpstreamResponse{{status: http.StatusOK, body: upstream}})
	reqBody := `{"model":"` + model + `","input":[{"type":"message","role":"user","content":"hi"}],"tools":[{"type":"function","name":"` + nameCompatLongTool + `","parameters":{"type":"object","properties":{}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	responsesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), nameCompatLongTool) {
		t.Fatalf("client body missing original name: %s", rec.Body.String())
	}
	payload := transport.requestPayloads[0]
	tools := payload["tools"].([]any)
	if got := tools[0].(map[string]any)["name"].(string); got != short {
		t.Fatalf("upstream tools name = %q, want %q", got, short)
	}
}

// TestCountTokens_LongToolName_Shortened count_tokens：上游 payload name 合法。
func TestCountTokens_LongToolName_Shortened(t *testing.T) {
	const model = "count-namecompat-x"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "count-namecompat-*", Protocol: "anthropic"}})
	var captured map[string]any
	old := callCountTokensUpstream
	callCountTokensUpstream = func(ctx context.Context, body []byte, modelID string, auth UpstreamAuth) (io.ReadCloser, int, http.Header, error) {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		captured = m
		return io.NopCloser(strings.NewReader(`{"input_tokens":42}`)), http.StatusOK, http.Header{}, nil
	}
	t.Cleanup(func() { callCountTokensUpstream = old })
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"`+nameCompatLongTool+`","description":"d","input_schema":{"type":"object","properties":{}}}]}`))
	rec := httptest.NewRecorder()
	claudeCountTokensHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	tools := captured["tools"].([]any)
	if got := tools[0].(map[string]any)["name"].(string); got != shortenAnthropicToolName(nameCompatLongTool) {
		t.Fatalf("count_tokens upstream name = %q", got)
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens":42`) {
		t.Fatalf("count passthrough lost: %s", rec.Body.String())
	}
}

// TestClaudeViaResponses_LongToolName_Restored claude 入站 → responses 上游
// （Path E）：非流式 output[].function_call.name 为原始长名。
func TestClaudeViaResponses_LongToolName_Restored(t *testing.T) {
	const model = "cr-namecompat-x"
	setProtocolRulesForTest(t, []domain.ProtocolRule{{Pattern: "cr-namecompat-*", Protocol: "responses"}})
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		{status: http.StatusOK, body: `{"id":"resp_e","object":"response","status":"completed","model":"` + model + `","output":[{"type":"function_call","id":"fc1","call_id":"call_1","name":"` + shortenAnthropicToolName(nameCompatLongTool) + `","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`},
	})
	reqBody := `{"model":"` + model + `","max_tokens":128,"tools":[{"name":"` + nameCompatLongTool + `","description":"d","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), nameCompatLongTool) {
		t.Fatalf("client body missing original name: %s", rec.Body.String())
	}
}

var _ = stats.Snapshot

// TestClaudeProbeViaResponses_NonStream_Restored 复现评审发现的 probe 泄漏：
// chat 翻译路径 500 → probe 原生 responses 成功 → 上游回显短名 → 客户端
// 非流式响应必须还原原始长名。
func TestClaudeProbeViaResponses_NonStream_Restored(t *testing.T) {
	const model = "probe-namecompat-x"
	installFakeOpenCodeClient(t, []fakeUpstreamResponse{
		// chat 翻译路径 500（内部重试 3 次全消耗）→ probe 原生 responses。
		{status: http.StatusInternalServerError, body: `{"error":{"message":"model unsupported"}}`},
		{status: http.StatusInternalServerError, body: `{"error":{"message":"model unsupported"}}`},
		{status: http.StatusInternalServerError, body: `{"error":{"message":"model unsupported"}}`},
		{status: http.StatusOK, body: `{"id":"resp_p","object":"response","status":"completed","model":"` + model + `","output":[{"type":"function_call","id":"fc1","call_id":"call_1","name":"` + shortenAnthropicToolName(nameCompatLongTool) + `","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`},
	})
	reqBody := `{"model":"` + model + `","max_tokens":128,"tools":[{"name":"` + nameCompatLongTool + `","description":"d","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	rec := httptest.NewRecorder()
	claudeMessagesHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	short := shortenAnthropicToolName(nameCompatLongTool)
	if strings.Contains(rec.Body.String(), short) {
		t.Fatalf("probe response leaked shortened name %q: %s", short, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), nameCompatLongTool) {
		t.Fatalf("probe response missing original name: %s", rec.Body.String())
	}
}
