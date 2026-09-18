package app

import (
	"regexp"
	"testing"
)

// The upstream free tier only accepts requests whose x-opencode-session
// matches the OpenCode client ID shape: "ses_" + 12 hex chars + 14 base62
// chars. Anything else is rejected with 403 FreeTierError ("OpenCode's free
// tier can only be used from within OpenCode"). The request ID uses the same
// 26-char body with a "msg_" prefix.
func TestOpenCode_SessionIDMatchesClientFormat(t *testing.T) {
	var re = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	for i := 0; i < 1000; i++ {
		id := newOCSessionID()
		if !re.MatchString(id) {
			t.Fatalf("session id %q does not match client format", id)
		}
	}
}

func TestOpenCode_RequestIDMatchesClientFormat(t *testing.T) {
	var re = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	for i := 0; i < 1000; i++ {
		id := newOCRequestID()
		if !re.MatchString(id) {
			t.Fatalf("request id %q does not match client format", id)
		}
	}
}

// 全随机 26 字符主体由 crypto/rand 填充;1000 次生成不得重复。
// 原 opencodeDescendingID 的时间戳回环/排序语义随上游格式正则化被移除。
func TestSession_IDUniqueAcrossGenerations(t *testing.T) {
	seen := make(map[string]bool, 2000)
	for i := 0; i < 1000; i++ {
		id := newOCSessionID()
		if seen[id] {
			t.Fatalf("duplicate session id %q generated", id)
		}
		seen[id] = true
	}
	for i := 0; i < 1000; i++ {
		id := newOCRequestID()
		if seen[id] {
			t.Fatalf("duplicate request id %q generated", id)
		}
		seen[id] = true
	}
}

// buildOCRequestWithSubpath must send the client-shaped headers: UA version,
// client tag, session and request IDs.
func TestOpenCode_RequestHeadersMirrorClient(t *testing.T) {
	ocClientVer = ocMinFreeTierVersion
	ocSessionID = newOCSessionID()
	ocProjectID = randomHex(40)
	auth := UpstreamAuth{}
	body := map[string]any{"messages": []any{}}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", body, auth, false, "https://opencode.ai", "chat/completions", ocSessionID)
	if err != nil {
		t.Fatal(err)
	}
	wantUA := "opencode/" + ocMinFreeTierVersion + " ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := req.Header.Get("x-session-id"); got != ocSessionID {
		t.Fatalf("x-session-id = %q, want %q", got, ocSessionID)
	}
	if got := req.Header.Get("x-session-affinity"); got != ocSessionID {
		t.Fatalf("x-session-affinity = %q, want %q", got, ocSessionID)
	}
	if got := req.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("x-opencode-client = %q, want cli", got)
	}
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(req.Header.Get("x-opencode-session")) {
		t.Fatalf("x-opencode-session = %q has wrong format", req.Header.Get("x-opencode-session"))
	}
	if !regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(req.Header.Get("x-opencode-request")) {
		t.Fatalf("x-opencode-request = %q has wrong format", req.Header.Get("x-opencode-request"))
	}
}
