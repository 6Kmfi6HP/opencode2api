package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFreeTierFingerprint_PreambleAndManifest(t *testing.T) {
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "2.1.4"})
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	bodyMap := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":   false,
	}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_0123abcdef45ABCDEf01234567")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	msgs, _ := sent["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("missing messages")
	}
	sys, _ := msgs[0].(map[string]any)
	text, _ := sys["content"].(string)
	if sys["role"] != "system" || !strings.HasPrefix(text, opencodePromptMarker) {
		t.Fatalf("system = %#v, want harness preamble first", sys)
	}
	if !strings.Contains(text, "<env>") || !strings.Contains(text, "Working directory:") {
		t.Fatalf("preamble missing <env> block: %s", text[:min(80, len(text))])
	}
	names := toolNames(t, sent)
	want := []string{"edit", "execute", "glob", "grep", "question", "read", "shell", "skill", "subagent", "webfetch", "websearch", "write", "patch"}
	if len(names) < len(want) {
		t.Fatalf("tools = %v, want manifest prefix %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("tools = %v, want prefix %v", names, want)
		}
	}
	if sent["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %#v, want none for a caller with no tools", sent["tool_choice"])
	}
}

func TestAgentToolReply_RemapsManifestCallAndDropsUnknown(t *testing.T) {
	reply := newAgentToolReplyContext(map[string]bool{"Read": true, "weather": true}, true, true)
	name, args := reply.remapAgentToolCall("read", `{"path":"/tmp/a","limit":10}`)
	if name != "Read" || !strings.Contains(args, `"file_path":"/tmp/a"`) || strings.Contains(args, `"path"`) {
		t.Fatalf("remap = %s %s, want Read + file_path", name, args)
	}
	if reply.keepRawName("question") {
		t.Fatal("invented/unmapped manifest tool must be dropped")
	}
	if !reply.keepRawName("read") {
		t.Fatal("read must be kept when the caller declared Read")
	}
	if !reply.keepRawName("weather") {
		t.Fatal("caller tool must be kept")
	}
}

func TestAgentPreambleFor_UsesLiveEnvironment(t *testing.T) {
	got := agentPreambleFor("opencode/big-pickle-free")
	if !strings.HasPrefix(got, opencodePromptMarker) {
		t.Fatalf("preamble = %q, want marker first", got[:min(80, len(got))])
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Clean(wd); !strings.Contains(got, "Working directory: "+want) {
		t.Fatalf("preamble missing live working directory %q", want)
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	if !strings.Contains(got, "Platform: "+platform) {
		t.Fatalf("preamble platform differs from runtime: want %q", platform)
	}
	if want := "Today's date: " + time.Now().UTC().Format("Mon Jan 02 2006"); !strings.Contains(got, want) {
		t.Fatalf("preamble missing live date %q", want)
	}
	if !strings.Contains(got, "You are powered by the model named big-pickle-free. The exact model ID is opencode/big-pickle-free") {
		t.Fatal("preamble model id was not patched")
	}
	if strings.Contains(got, "Sun Oct 25 2026") {
		t.Fatal("preamble still carries the captured date")
	}
}

func TestFreeTierFingerprint_TitleRequestSkipsManifest(t *testing.T) {
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "2.1.4"})
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	bodyMap := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You generate a descriptive title for this conversation."},
			map[string]any{"role": "user", "content": "hello"},
		},
		"stream": false,
	}
	req, err := buildOCRequestWithSubpath("big-pickle-free", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_0123abcdef45ABCDEf01234567")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	if _, ok := sent["tools"]; ok {
		t.Fatalf("title request gained tools: %#v", sent["tools"])
	}
	if _, ok := sent["tool_choice"]; ok {
		t.Fatalf("title request gained tool_choice: %#v", sent["tool_choice"])
	}
	msgs, _ := sent["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	if text, _ := sys["content"].(string); !strings.HasPrefix(text, opencodePromptMarker) {
		t.Fatalf("title request system = %q, want preamble", text[:min(80, len(text))])
	}
}

func TestFreeTierFingerprint_IdempotentAcrossRetries(t *testing.T) {
	bodyMap := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":       "read",
				"parameters": map[string]any{"type": "object"},
			},
		}},
	}
	applyFreeTierBodyFingerprint(bodyMap, "chat/completions", "big-pickle-free")
	first := toolNames(t, bodyMap)
	applyFreeTierBodyFingerprint(bodyMap, "chat/completions", "big-pickle-free")
	second := toolNames(t, bodyMap)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second shaping changed tools:\nfirst  %v\nsecond %v", first, second)
	}
	alias := callerToolAlias("read")
	counts := map[string]int{}
	for _, name := range second {
		counts[name]++
	}
	if counts[alias] != 1 {
		t.Fatalf("alias count = %d, want 1 in %v", counts[alias], second)
	}
	if counts["read"] != 1 {
		t.Fatalf("manifest read count = %d, want 1 in %v", counts["read"], second)
	}
}

func TestFreeTierFingerprint_CompletionLimit(t *testing.T) {
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "2.1.4"})
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	bodyMap := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	req, err := buildOCRequestWithSubpath("big-pickle-free", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_0123abcdef45ABCDEf01234567")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	if got := sent["max_completion_tokens"]; got != float64(freeTierCompletionLimit) {
		t.Fatalf("max_completion_tokens = %#v, want %d", got, freeTierCompletionLimit)
	}

	bodyMap = map[string]any{
		"max_tokens": float64(1024),
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}
	req, err = buildOCRequestWithSubpath("big-pickle-free", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_0123abcdef45ABCDEf01234567")
	if err != nil {
		t.Fatal(err)
	}
	sent = sentBody(t, req)
	if _, ok := sent["max_completion_tokens"]; ok {
		t.Fatal("explicit max_tokens must not gain max_completion_tokens")
	}
	if got := sent["max_tokens"]; got != float64(1024) {
		t.Fatalf("max_tokens = %#v, want 1024", got)
	}
}

func TestFreeTierRequestProjectID(t *testing.T) {
	if got := freeTierRequestProjectID("paid-model"); got != "" {
		t.Fatalf("paid model project = %q, want empty", got)
	}
	got := freeTierRequestProjectID("big-pickle-free")
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(got) {
		t.Fatalf("free model project = %q, want 40 hex", got)
	}
}

func TestBuildOCRequestWithSubpathAndProject_Override(t *testing.T) {
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "2.1.4", projectID: strings.Repeat("b", 40)})
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	override := strings.Repeat("a", 40)
	req, err := buildOCRequestWithSubpathAndProject("big-pickle-free", map[string]any{"messages": []any{}}, auth, false, "https://opencode.ai", "chat/completions", "ses_0123abcdef45ABCDEf01234567", override, initOCSession())
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("x-opencode-project"); got != override {
		t.Fatalf("x-opencode-project = %q, want explicit %q", got, override)
	}
}

func TestFreeTierFingerprint_CollidingCallerToolGetsAlias(t *testing.T) {
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "2.1.4"})
	auth := UpstreamAuth{Mode: AuthRoutePublic}
	callerRead := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "caller-specific reader",
			"parameters":  map[string]any{"type": "object"},
		},
	}
	bodyMap := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
			}},
		},
		"tools":       []any{callerRead},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
	}
	req, err := buildOCRequestWithSubpath("big-pickle-free", bodyMap, auth, false, "https://opencode.ai", "chat/completions", "ses_0123abcdef45ABCDEf01234567")
	if err != nil {
		t.Fatal(err)
	}
	sent := sentBody(t, req)
	names := toolNames(t, sent)
	alias := callerToolAlias("read")
	if alias == "" {
		t.Fatal("callerToolAlias returned empty for read")
	}
	if firstIndex(names, alias) < 0 {
		t.Fatalf("tools = %v, want aliased caller tool %q", names, alias)
	}
	choice := sent["tool_choice"].(map[string]any)
	fn := choice["function"].(map[string]any)
	if fn["name"] != alias {
		t.Fatalf("tool_choice name = %#v, want %q", fn["name"], alias)
	}
	msgs := sent["messages"].([]any)
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		calls, _ := msg["tool_calls"].([]any)
		if len(calls) == 0 {
			continue
		}
		call := calls[0].(map[string]any)
		callFn := call["function"].(map[string]any)
		if callFn["name"] != alias {
			t.Fatalf("history tool call name = %#v, want %q", callFn["name"], alias)
		}
	}
}

func TestAgentToolReply_ChatCallerKeepsItsOwnSchema(t *testing.T) {
	reply := newAgentToolReplyContext(map[string]bool{"read": true}, false, true)
	name, args := reply.remapAgentToolCall("read", `{"path":"/tmp/a"}`)
	if name != "read" || args != `{"path":"/tmp/a"}` {
		t.Fatalf("remap = %s %s, want read with chat caller args", name, args)
	}
}

func TestAgentToolReply_ChatStreamKeepsOwnArgSchema(t *testing.T) {
	argLine := func(args string) string {
		raw := map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index":    0,
				"function": map[string]any{"arguments": args},
			}}},
		}}}
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(b)
	}
	nameLine := func(name string) string {
		raw := map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index":    0,
				"function": map[string]any{"name": name, "arguments": ""},
			}}},
		}}}
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(b)
	}

	tests := []struct {
		name    string
		offered map[string]bool
		viaMap  bool
		rawName string
		wantKey string
		dropKey string
	}{
		{"chat caller", map[string]bool{"write": true}, false, "write", `\"path\"`, `\"file_path\"`},
		{"anthropic caller", map[string]bool{"Write": true}, true, "write", `\"file_path\"`, `\"path\"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply := newAgentToolReplyContext(tt.offered, tt.viaMap, true)
			rewriteChatSSELine(nameLine(tt.rawName), reply)
			out := rewriteChatSSELine(argLine(`,"path":"/tmp/a"`), reply)
			if !strings.Contains(out, tt.wantKey) || strings.Contains(out, tt.dropKey) {
				t.Fatalf("rewritten fragment = %s, want %s and no %s", out, tt.wantKey, tt.dropKey)
			}
		})
	}
}

func TestAgentToolReply_AliasMapsBackToCallerName(t *testing.T) {
	alias := callerToolAlias("read")
	reply := newAgentToolReplyContext(map[string]bool{"read": true}, false, true)
	if !reply.keepRawName(alias) {
		t.Fatal("alias for a declared caller tool must be kept")
	}
	name, args := reply.remapAgentToolCall(alias, `{"filePath":"/tmp/a"}`)
	if name != "read" || args != `{"filePath":"/tmp/a"}` {
		t.Fatalf("remap alias = %s %s, want read with original args", name, args)
	}
	name, input := reply.remapAgentToolInput(alias, map[string]any{"filePath": "/tmp/a"})
	if name != "read" {
		t.Fatalf("remap alias input name = %s, want read", name)
	}
	if input.(map[string]any)["filePath"] != "/tmp/a" {
		t.Fatalf("remap alias input = %#v, want original args", input)
	}
}

func TestAgentToolReply_SchemaAwareRemap(t *testing.T) {
	claudeTools := []ClaudeTool{
		{Name: "Read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "number"}}}},
		{Name: "Edit", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "old_string": map[string]any{"type": "string"}, "new_string": map[string]any{"type": "string"}, "replace_all": map[string]any{"type": "boolean"}}}},
		{Name: "Write", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}},
		{Name: "Bash", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}},
		{Name: "Skill", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"skill": map[string]any{"type": "string"}}}},
		{Name: "Task", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"subagent_type": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"}}}},
	}
	piClaudeTools := []ClaudeTool{
		{Name: "read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "number"}}}},
		{Name: "write", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}},
		{Name: "bash", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "timeout": map[string]any{"type": "number"}}}},
	}
	chatTools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
		{Type: "function", Function: ToolFunction{Name: "edit", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "oldString": map[string]any{"type": "string"}, "newString": map[string]any{"type": "string"}}}}},
		{Type: "function", Function: ToolFunction{Name: "shell", Parameters: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "workdir": map[string]any{"type": "string"}}}}},
	}

	t.Run("claude code gets pascal name and snake_case args", func(t *testing.T) {
		reply := newAgentToolReplyContextWithSchemas(offeredClaudeToolNames(claudeTools), offeredClaudeToolSchemas(claudeTools), true, true)
		name, args := reply.remapAgentToolCall("edit", `{"path":"/tmp/a","oldString":"a","newString":"b","replaceAll":true}`)
		if name != "Edit" {
			t.Fatalf("name = %q, want Edit", name)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(args), &got); err != nil {
			t.Fatal(err)
		}
		if got["file_path"] != "/tmp/a" || got["old_string"] != "a" || got["new_string"] != "b" || got["replace_all"] != true {
			t.Fatalf("args = %#v, want file_path/old_string/new_string/replace_all", got)
		}
		if _, has := got["path"]; has {
			t.Fatalf("unexpected unmapped path key in %#v", got)
		}
		bName, bArgs := reply.remapAgentToolCall("shell", `{"command":"pwd","workdir":"/tmp"}`)
		if bName != "Bash" {
			t.Fatalf("shell name = %q, want Bash", bName)
		}
		var bGot map[string]any
		_ = json.Unmarshal([]byte(bArgs), &bGot)
		if _, has := bGot["workdir"]; has || bGot["command"] != "pwd" {
			t.Fatalf("bash args = %#v, want only command", bGot)
		}
		sName, sArgs := reply.remapAgentToolCall("skill", `{"id":"plan"}`)
		var sGot map[string]any
		_ = json.Unmarshal([]byte(sArgs), &sGot)
		if sName != "Skill" || sGot["skill"] != "plan" {
			t.Fatalf("skill remap = %s %#v", sName, sGot)
		}
		tName, tArgs := reply.remapAgentToolCall("subagent", `{"agent":"explore","prompt":"hi"}`)
		var tGot map[string]any
		_ = json.Unmarshal([]byte(tArgs), &tGot)
		if tName != "Task" || tGot["subagent_type"] != "explore" || tGot["prompt"] != "hi" {
			t.Fatalf("subagent remap = %s %#v", tName, tGot)
		}
	})

	t.Run("pi over /v1/messages preserves path and lowercase tool names", func(t *testing.T) {
		reply := newAgentToolReplyContextWithSchemas(offeredClaudeToolNames(piClaudeTools), offeredClaudeToolSchemas(piClaudeTools), true, true)
		name, args := reply.remapAgentToolCall("write", `{"path":"/tmp/p.txt","content":"hello"}`)
		if name != "write" {
			t.Fatalf("name = %q, want write", name)
		}
		var got map[string]any
		_ = json.Unmarshal([]byte(args), &got)
		if got["path"] != "/tmp/p.txt" || got["content"] != "hello" {
			t.Fatalf("pi write args = %#v, want path preserved", got)
		}
		if _, has := got["file_path"]; has {
			t.Fatalf("pi write must not rewrite path to file_path: %#v", got)
		}
		bName, bArgs := reply.remapAgentToolCall("shell", `{"command":"ls","workdir":"/tmp"}`)
		if bName != "bash" {
			t.Fatalf("shell name = %q, want bash", bName)
		}
		var bGot map[string]any
		_ = json.Unmarshal([]byte(bArgs), &bGot)
		if _, has := bGot["workdir"]; has || bGot["command"] != "ls" {
			t.Fatalf("pi bash args = %#v, want command without unsupported workdir", bGot)
		}
	})

	t.Run("chat caller keeps manifest names and schema untouched", func(t *testing.T) {
		reply := newAgentToolReplyContextWithSchemas(offeredToolNames(chatTools), offeredToolSchemas(chatTools), false, true)
		name, args := reply.remapAgentToolCall("edit", `{"path":"/tmp/a","oldString":"a","newString":"b"}`)
		if name != "edit" || args != `{"path":"/tmp/a","oldString":"a","newString":"b"}` {
			t.Fatalf("chat edit = %s %s, want untouched", name, args)
		}
		sName, sArgs := reply.remapAgentToolCall("shell", `{"command":"ls","workdir":"/tmp"}`)
		if sName != "shell" || sArgs != `{"command":"ls","workdir":"/tmp"}` {
			t.Fatalf("chat shell = %s %s, want untouched", sName, sArgs)
		}
	})
}

func TestAgentToolReply_StreamSplitChunksAndValues(t *testing.T) {
	claudeTools := []ClaudeTool{
		{Name: "Edit", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"file_path": map[string]any{"type": "string"}, "old_string": map[string]any{"type": "string"},
			"new_string": map[string]any{"type": "string"}, "replace_all": map[string]any{"type": "boolean"},
		}}},
		{Name: "Write", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"file_path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"},
		}}},
	}

	t.Run("chat SSE splits JSON keys across chunks without corrupting values", func(t *testing.T) {
		reply := newAgentToolReplyContextWithSchemas(offeredClaudeToolNames(claudeTools), offeredClaudeToolSchemas(claudeTools), true, true)
		makeDelta := func(tc map[string]any) string {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{tc}}}}})
			return "data: " + string(b)
		}
		collectArgs := func(chunks []string) string {
			var sb strings.Builder
			for _, line := range chunks {
				out := rewriteChatSSELine(line, reply)
				payload := strings.TrimPrefix(out, "data: ")
				var m map[string]any
				_ = json.Unmarshal([]byte(payload), &m)
				tcs, _ := m["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)
				if len(tcs) > 0 {
					sb.WriteString(tcs[0].(map[string]any)["function"].(map[string]any)["arguments"].(string))
				}
			}
			return sb.String()
		}
		lines := []string{
			makeDelta(map[string]any{"index": 0, "function": map[string]any{"name": "edit", "arguments": `{"old`}}),
			makeDelta(map[string]any{"index": 0, "function": map[string]any{"arguments": `String":"val \"path\":1","new`}}),
			makeDelta(map[string]any{"index": 0, "function": map[string]any{"arguments": `String":"path","replace`}}),
			makeDelta(map[string]any{"index": 0, "function": map[string]any{"arguments": `All":true,"pa`}}),
			makeDelta(map[string]any{"index": 0, "function": map[string]any{"arguments": `th":"/tmp/a"}`}}),
		}
		assembled := collectArgs(lines)
		var parsed map[string]any
		if err := json.Unmarshal([]byte(assembled), &parsed); err != nil {
			t.Fatalf("assembled invalid JSON %q: %v", assembled, err)
		}
		if parsed["file_path"] != "/tmp/a" || parsed["old_string"] != `val "path":1` || parsed["new_string"] != "path" || parsed["replace_all"] != true {
			t.Fatalf("parsed args = %#v, want exact remapped keys and preserved string values", parsed)
		}
	})

	t.Run("anthropic SSE and body remap without map-compare panic", func(t *testing.T) {
		reply := newAgentToolReplyContextWithSchemas(offeredClaudeToolNames(claudeTools), offeredClaudeToolSchemas(claudeTools), true, true)
		body := `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"write","input":{"path":"/tmp/x","content":"path"}},{"type":"tool_use","id":"t2","name":"unknown_tool","input":{"x":1}}]}`
		out := presentAnthropicBody([]byte(body), nil, reply)
		var resp map[string]any
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatal(err)
		}
		blocks := resp["content"].([]any)
		if len(blocks) != 1 {
			t.Fatalf("expected unknown tool dropped leaving 1 block, got %d (%s)", len(blocks), out)
		}
		b0 := blocks[0].(map[string]any)
		inp := b0["input"].(map[string]any)
		if b0["name"] != "Write" || inp["file_path"] != "/tmp/x" || inp["content"] != "path" {
			t.Fatalf("presentAnthropicBody = %#v", b0)
		}

		replyStream := newAgentToolReplyContextWithSchemas(offeredClaudeToolNames(claudeTools), offeredClaudeToolSchemas(claudeTools), true, true)
		chunks := []string{
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"write","input":{}}}` + "\n",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}` + "\n",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"th\":\"/tmp/x\",\"content\":\"path\"}"}}` + "\n",
			`data: {"type":"content_block_stop","index":0}` + "\n",
		}
		var partialSB strings.Builder
		var blockName string
		for _, line := range chunks {
			rewritten := presentAnthropicStreamLine(line, nil, replyStream)
			if strings.HasPrefix(rewritten, "data: ") {
				var evt map[string]any
				_ = json.Unmarshal([]byte(strings.TrimPrefix(rewritten, "data: ")), &evt)
				switch evt["type"] {
				case "content_block_start":
					blockName = evt["content_block"].(map[string]any)["name"].(string)
				case "content_block_delta":
					partialSB.WriteString(evt["delta"].(map[string]any)["partial_json"].(string))
				}
			}
		}
		var streamedInp map[string]any
		if err := json.Unmarshal([]byte(partialSB.String()), &streamedInp); err != nil {
			t.Fatalf("streamed JSON invalid %q: %v", partialSB.String(), err)
		}
		if blockName != "Write" || streamedInp["file_path"] != "/tmp/x" || streamedInp["content"] != "path" {
			t.Fatalf("streamed = name:%q input:%#v", blockName, streamedInp)
		}
	})
}

func TestBridgeFreeTierToolReply_ResponsesAndAnthropicBridges(t *testing.T) {
	claudeTools := []ClaudeTool{
		{Name: "Read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}},
		{Name: "CustomTool", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"foo": map[string]any{"type": "string"}}}},
	}
	alias := callerToolAlias("CustomTool")

	t.Run("claude_responses stream restores alias, maps manifest, splits keys, drops unknown", func(t *testing.T) {
		reply := newAgentToolReplyContextWithSchemas(offeredClaudeToolNames(claudeTools), offeredClaudeToolSchemas(claudeTools), true, true)
		ctx := withAgentToolReply(context.Background(), reply)
		sse := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_drop","call_id":"call_drop","name":"question"}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_drop","delta":"{}"}`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"read"}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_read","delta":"{\"pa"}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_read","delta":"th\":\"/tmp/r\"}"}`,
			`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_alias","call_id":"call_alias","name":"` + alias + `"}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_alias","delta":"{\"foo\":\"bar\"}"}`,
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"call_drop","name":"question","arguments":"{}"},{"type":"function_call","call_id":"call_read","name":"read","arguments":"{\"path\":\"/tmp/r\"}"},{"type":"function_call","call_id":"call_alias","name":"` + alias + `","arguments":"{\"foo\":\"bar\"}"}]}}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n"

		w2 := httptest.NewRecorder()
		committed, cerr := claudeResponsesStreamWithRetry(ctx, w2, UpstreamAuth{Mode: AuthRoutePublic}, "muse-spark-free", ClaudeRequest{Model: "muse-spark-free"}, false, readCloserFromString(sse))
		if !committed || cerr != nil {
			t.Fatalf("claudeResponsesStreamWithRetry = (%v, %v), want (true, nil)", committed, cerr)
		}
		out := w2.Body.String()
		if strings.Contains(out, alias) || strings.Contains(out, `"question"`) {
			t.Fatalf("leaked alias or dropped tool in Claude Responses SSE:\n%s", out)
		}
		if !strings.Contains(out, `"name":"Read"`) || !strings.Contains(out, `"name":"CustomTool"`) {
			t.Fatalf("missing remapped Read / restored CustomTool in SSE:\n%s", out)
		}
		if !strings.Contains(out, `file_path`) || strings.Contains(out, `\"path\"`) {
			t.Fatalf("split path key not remapped in SSE:\n%s", out)
		}
	})

	t.Run("native responses stream and non-stream restore alias and drop unknown", func(t *testing.T) {
		respTools := []ResponsesTool{
			{Type: "function", Name: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}},
			{Type: "function", Name: "CustomTool", Parameters: map[string]any{"type": "object", "properties": map[string]any{"foo": map[string]any{"type": "string"}}}},
		}
		reply := newAgentToolReplyContextWithSchemas(offeredResponsesToolNames(respTools), offeredResponsesToolSchemas(respTools), false, true)
		body := `{"id":"resp_1","output":[{"type":"function_call","call_id":"c1","name":"question","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"` + alias + `","arguments":"{\"foo\":\"bar\"}"}]}`
		got := rewriteResponsesOutputBody([]byte(body), reply)
		if strings.Contains(string(got), alias) || strings.Contains(string(got), "question") || !strings.Contains(string(got), `"name":"CustomTool"`) {
			t.Fatalf("rewriteResponsesOutputBody = %s", got)
		}

		replyStream := newAgentToolReplyContextWithSchemas(offeredResponsesToolNames(respTools), offeredResponsesToolSchemas(respTools), false, true)
		if _, drop := rewriteResponsesSSELine([]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc0","call_id":"c0","name":"question"}}`+"\n"), replyStream); !drop {
			t.Fatal("expected unknown tool output_item.added to be dropped")
		}
		if _, drop := rewriteResponsesSSELine([]byte(`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc0","delta":"{}"}`+"\n"), replyStream); !drop {
			t.Fatal("expected dropped tool delta to be dropped")
		}
		rw, drop := rewriteResponsesSSELine([]byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc1","call_id":"c1","name":"`+alias+`"}}`+"\n"), replyStream)
		if drop || !bytes.Contains(rw, []byte(`"name":"CustomTool"`)) {
			t.Fatalf("alias item rewrite = (%s, drop=%v)", rw, drop)
		}
	})

	t.Run("chat via responses stream filters unknown with dense index and keeps schema", func(t *testing.T) {
		chatTools := []Tool{
			{Type: "function", Function: ToolFunction{Name: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
			{Type: "function", Function: ToolFunction{Name: "CustomTool", Parameters: map[string]any{"type": "object", "properties": map[string]any{"foo": map[string]any{"type": "string"}}}}},
		}
		reply := newAgentToolReplyContextWithSchemas(offeredToolNames(chatTools), offeredToolSchemas(chatTools), false, true)
		ctx := withAgentToolReply(context.Background(), reply)
		sse := strings.Join([]string{
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc0","call_id":"c0","name":"question"}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc0","delta":"{}"}`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc1","call_id":"c1","name":"` + alias + `"}}`,
			`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc1","delta":"{\"foo\":\"bar\"}"}`,
			`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n"
		w := httptest.NewRecorder()
		committed, err := responsesSSEToChatStream(ctx, w, strings.NewReader(sse), "m", false, false, nil, nil)
		if !committed || err != nil {
			t.Fatalf("responsesSSEToChatStream = (%v, %v)", committed, err)
		}
		out := w.Body.String()
		if strings.Contains(out, alias) || strings.Contains(out, "question") {
			t.Fatalf("leaked alias or unknown tool:\n%s", out)
		}
		if !strings.Contains(out, `"index":0`) || !strings.Contains(out, `"name":"CustomTool"`) {
			t.Fatalf("expected dense index 0 for restored CustomTool:\n%s", out)
		}
	})

	t.Run("chat via anthropic stream restores alias and downgrades dropped tool stop", func(t *testing.T) {
		chatTools := []Tool{
			{Type: "function", Function: ToolFunction{Name: "CustomTool", Parameters: map[string]any{"type": "object", "properties": map[string]any{"foo": map[string]any{"type": "string"}}}}},
		}
		reply := newAgentToolReplyContextWithSchemas(offeredToolNames(chatTools), offeredToolSchemas(chatTools), false, true)
		ctx := withAgentToolReply(context.Background(), reply)
		sse := strings.Join([]string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"m1","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t0","name":"question","input":{}}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}, "\n\n") + "\n\n"
		w := httptest.NewRecorder()
		committed, err := anthropicSSEToChatStream(ctx, w, strings.NewReader(sse), "m", false, false, nil, nil)
		if !committed || err != nil {
			t.Fatalf("anthropicSSEToChatStream = (%v, %v)", committed, err)
		}
		out := w.Body.String()
		if strings.Contains(out, `"question"`) || strings.Contains(out, `"finish_reason":"tool_calls"`) {
			t.Fatalf("expected unknown tool dropped and finish_reason downgraded:\n%s", out)
		}
		if !strings.Contains(out, `"finish_reason":"stop"`) {
			t.Fatalf("expected finish_reason=stop after dropping all tool calls:\n%s", out)
		}
	})
}

type strReadCloser struct{ *strings.Reader }

func (strReadCloser) Close() error { return nil }

func readCloserFromString(s string) strReadCloser {
	return strReadCloser{strings.NewReader(s)}
}

func TestAgentToolReply_NonStreamClaudeAggregateRemapsArgs(t *testing.T) {
	claudeTools := []ClaudeTool{
		{Name: "Read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}},
	}
	reply := newAgentToolReplyContextWithSchemas(offeredClaudeToolNames(claudeTools), offeredClaudeToolSchemas(claudeTools), true, true)
	req := withClientHeadersFromRequest(&http.Request{Header: http.Header{"User-Agent": {"claude-code/2.1.0"}}})
	ctx := withAgentToolReply(req.Context(), reply)
	if shouldRestoreToolCase(ctx) {
		t.Fatal("shouldRestoreToolCase must defer to active agentToolReplyContext so manifest keys remain remappable")
	}
	sse := strings.Join([]string{
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"path\":\"/tmp/x\"}"}}]}}]}`,
		`data: {"id":"c","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	agg := aggregateOpenAIStream([]byte(sse), "deepseek-v4.1-flash-free", shouldRestoreToolCase(ctx))
	got := rewriteChatCompletionBody(agg, agentToolReplyFromContext(ctx))
	if !strings.Contains(string(got), `"name":"Read"`) || !strings.Contains(string(got), `file_path`) || strings.Contains(string(got), `\"path\"`) {
		t.Fatalf("non-stream claude aggregate = %s, want Read with file_path", got)
	}
}
