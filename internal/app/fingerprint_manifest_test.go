package app

import (
	"strings"
	"testing"
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
	if sys["role"] != "system" || !strings.HasPrefix(text, agentPreambleMarker) {
		t.Fatalf("system = %#v, want harness preamble first", sys)
	}
	if !strings.Contains(text, "<env>") || !strings.Contains(text, "Working directory:") {
		t.Fatalf("preamble missing <env> block: %s", text[:min(80, len(text))])
	}
	names := toolNames(t, sent)
	want := []string{"edit", "glob", "grep", "read", "shell", "write"}
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
	name, args := reply.remapAgentToolCall("read", `{"filePath":"/tmp/a","limit":10}`)
	if name != "Read" || !strings.Contains(args, `"file_path"`) || strings.Contains(args, `"filePath"`) {
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
