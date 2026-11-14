package app

import (
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
