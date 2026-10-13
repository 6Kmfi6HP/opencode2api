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

// 上游除 UA 外还校验 session 前 12 位可解码为降序时间戳（2026-10-13 实测：
// 随机前缀 403 FreeTierError，时间戳前缀 200）。正则断言覆盖不到这一层——
// 随机实现同样满足 ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$，故反解前缀比对时间窗。
// 前缀只有 48 位，容不下完整毫秒时间戳，故只比对低 36 位。
func TestOpenCode_SessionIDCarriesDescendingTimestamp(t *testing.T) {
	const (
		bodyMask = uint64(0xffffffffffff)
		tsMask   = uint64(0xfffffffff)
	)
	before := uint64(time.Now().UnixMilli()) & tsMask
	for i := 0; i < 1000; i++ {
		id := newOCSessionID()
		v, err := strconv.ParseUint(id[4:16], 16, 64)
		if err != nil {
			t.Fatalf("session id %q has non-hex prefix", id)
		}
		got := ((bodyMask - v) >> 12) & tsMask
		if got != before && got != uint64(time.Now().UnixMilli())&tsMask {
			t.Fatalf("session id %q prefix decodes to %d, want current ms low 36 bits %d", id, got, before)
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
	state := &opencodeSessionState{clientVersion: ocMinFreeTierVersion, sessionID: newOCSessionID(), projectID: randomHex(40)}
	setOCSessionStateForTest(state)
	auth := UpstreamAuth{}
	body := map[string]any{"messages": []any{}}
	req, err := buildOCRequestWithSubpath("mimo-v2.5-free", body, auth, false, "https://opencode.ai", "chat/completions", state.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	wantUA := "opencode/" + ocMinFreeTierVersion + " ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
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
	if !regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`).MatchString(req.Header.Get("x-opencode-request")) {
		t.Fatalf("x-opencode-request = %q has wrong format", req.Header.Get("x-opencode-request"))
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
