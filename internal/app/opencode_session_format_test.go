package app

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// 2026-10-13 起(上游 83f2b6f67a zen 路由改薄代理、免费层门禁下沉私有
// inference)门禁校验 session ID 前 12 位 hex 为反转时间戳
// (~(ts_ms*0x1000+counter) 低 48 位,对齐上游 packages/schema/src/identifier.ts);
// 纯随机或升序均 403 FreeTierError(docs/labs/2026-10-13-free-tier-fingerprint-refresh.md)。
// 回归: ses_ 必须携带降序反转时间戳且贴近当前毫秒,msg_ 携带升序时间戳。
func TestOpenCode_SessionIDDescendingTimestamp(t *testing.T) {
	now := time.Now().UnixMilli()
	decodeTS36 := func(t *testing.T, body string, descending bool) int64 {
		t.Helper()
		var stored uint64
		for i := range 6 {
			n, err := strconv.ParseUint(body[2*i:2*i+2], 16, 8)
			if err != nil {
				t.Fatalf("hex prefix %q not lowercase hex: %v", body[:12], err)
			}
			stored = stored<<8 | n
		}
		if descending {
			stored = ^stored & 0xFFFFFFFFFFFF
		}
		return int64(stored >> 12)
	}
	want36 := now % (1 << 36)
	if got := decodeTS36(t, strings.TrimPrefix(newOCSessionID(), "ses_"), true); got < want36-5000 || got > want36+5000 {
		t.Fatalf("ses_ decodes to ts36 %d, want within 5s of now %d (upstream gate requires descending timestamp)", got, want36)
	}
	if got := decodeTS36(t, strings.TrimPrefix(newOCRequestID(), "msg_"), false); got < want36-5000 || got > want36+5000 {
		t.Fatalf("msg_ decodes to ts36 %d, want within 5s of now %d (ascending timestamp)", got, want36)
	}
}

// 26 字符主体: 前 12 位 hex 为反转时间戳、后 14 位 base62 由 crypto/rand 填充;
// 1000 次生成不得重复。
// 原 opencodeDescendingID 的时间戳编码曾随 2026-09-18 lite 实测(仅格式校验)
// 被移除;2026-10-13 上游门禁收紧后恢复(见 newOCShapeID)。
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
	state := &opencodeSessionState{clientVersion: ocMinFreeTierVersion, sessionID: newOCSessionID(), projectID: randomHex(40)}
	setOCSessionStateForTest(state)
	auth := UpstreamAuth{}
	body := map[string]any{"messages": []any{}}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", body, auth, false, "https://opencode.ai", "chat/completions", state.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	wantUAPrefix := "opencode/latest/"
	wantUASuffix := "/cli"
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, wantUAPrefix) || !strings.HasSuffix(got, wantUASuffix) {
		t.Fatalf("User-Agent = %q, want %s<ver>%s format", got, wantUAPrefix, wantUASuffix)
	}
	if got := req.Header.Get("x-session-id"); got != state.sessionID {
		t.Fatalf("x-session-id = %q, want %q", got, state.sessionID)
	}
	if got := req.Header.Get("x-session-affinity"); got != state.sessionID {
		t.Fatalf("x-session-affinity = %q, want %q", got, state.sessionID)
	}
	if got := req.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("x-opencode-client = %q, want cli", got)
	}
	if !regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(req.Header.Get("x-opencode-session")) {
		t.Fatalf("x-opencode-session = %q has wrong format", req.Header.Get("x-opencode-session"))
	}
	// V2 gate: the inference path requires x-opencode-project "global" and
	// rejects any x-opencode-request header (live probe 2026-10-14).
	if got := req.Header.Get("x-opencode-project"); got != "global" {
		t.Fatalf("x-opencode-project = %q, want global", got)
	}
	if got := req.Header.Get("x-opencode-request"); got != "" {
		t.Fatalf("x-opencode-request = %q, want absent on the zen surface", got)
	}
}

func TestOpenCode_SessionRefreshSingleflight(t *testing.T) {
	resetOCSessionForTest()
	oldHTTPClient, oldActiveSocks5 := httpClient, activeSocks5
	t.Cleanup(func() {
		httpClient = oldHTTPClient
		socks5Mu.Lock()
		activeSocks5 = oldActiveSocks5
		socks5Mu.Unlock()
	})

	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"version":"1.18.31"}`))}, nil
	})}
	socks5Mu.Lock()
	activeSocks5 = ""
	socks5Mu.Unlock()

	initResult := make(chan *opencodeSessionState, 1)
	go func() { initResult <- initOCSession() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial session fetch did not start")
	}

	refreshResult := make(chan *opencodeSessionState, 1)
	go func() { refreshResult <- refreshOCSession() }()
	select {
	case <-refreshResult:
		close(release)
		t.Fatal("refresh did not join the in-flight initialization")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	first, refreshed := <-initResult, <-refreshResult
	if calls.Load() != 1 || first != refreshed {
		t.Fatalf("concurrent refresh calls = %d, states equal = %t", calls.Load(), first == refreshed)
	}
	second := refreshOCSession()
	if calls.Load() != 2 || second == first {
		t.Fatalf("sequential refresh calls = %d, replaced state = %t", calls.Load(), second != first)
	}
}

func setDefaultOCSessionStateForTest() {
	setOCSessionStateForTest(&opencodeSessionState{clientVersion: "test", sessionID: "ses_test", projectID: "project_test"})
}

func setOCSessionStateForTest(state *opencodeSessionState) {
	ocSessionState.Store(state)
}

func resetOCSessionForTest() {
	ocSessionState.Store(nil)
}
