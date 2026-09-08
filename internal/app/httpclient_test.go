package app

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 保存并替换全局代理状态,结束后恢复。
func withStickyProxyEnv(t *testing.T, proxies []Socks5Proxy, active string, sticky bool, paidDirect bool) {
	t.Helper()
	socks5Mu.Lock()
	oldProxies := socks5Proxies
	oldActive := activeSocks5
	oldSticky := socks5Sticky
	oldPaidDirect := socks5PaidDirect
	oldBaseURLs := upstreamBaseURLs
	socks5Proxies = proxies
	activeSocks5 = active
	socks5Sticky = sticky
	socks5PaidDirect = paidDirect
	socks5Mu.Unlock()
	stickyMu.Lock()
	stickyEntries = map[string]*stickyProxyEntry{}
	stickyMu.Unlock()
	t.Cleanup(func() {
		socks5Mu.Lock()
		socks5Proxies = oldProxies
		activeSocks5 = oldActive
		socks5Sticky = oldSticky
		socks5PaidDirect = oldPaidDirect
		upstreamBaseURLs = oldBaseURLs
		socks5Mu.Unlock()
		stickyMu.Lock()
		stickyEntries = map[string]*stickyProxyEntry{}
		stickyMu.Unlock()
	})
}

// withBaseURLs 设置多个上游域名，结束后恢复。
func withBaseURLs(t *testing.T, baseURLs []string) {
	t.Helper()
	socks5Mu.Lock()
	old := upstreamBaseURLs
	upstreamBaseURLs = baseURLs
	socks5Mu.Unlock()
	// Multi-base routing depends on both the sticky entry cache and the rebind
	// sequence. Clear them after changing the configured base list so prior
	// tests cannot skew retry targets or sticky rotation.
	stickyMu.Lock()
	stickyEntries = map[string]*stickyProxyEntry{}
	stickyRebindSeq = 0
	oldSeq := stickyRebindSeq
	stickyMu.Unlock()
	t.Cleanup(func() {
		socks5Mu.Lock()
		upstreamBaseURLs = old
		socks5Mu.Unlock()
		stickyMu.Lock()
		stickyEntries = map[string]*stickyProxyEntry{}
		stickyRebindSeq = oldSeq
		stickyMu.Unlock()
	})
}

func TestStickyKeyForRequest(t *testing.T) {
	cases := []struct {
		name string
		auth UpstreamAuth
		body map[string]any
		want string
	}{
		{"paid token wins", UpstreamAuth{Token: "sk-abc"}, nil, "tok:sk-abc"},
		{"public session user", UpstreamAuth{}, map[string]any{"user": "sess-42"}, "usr:sess-42"},
		{"public no session falls back", UpstreamAuth{}, map[string]any{}, stickyPublicFallback},
		{"public no body falls back", UpstreamAuth{}, nil, stickyPublicFallback},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stickyKeyForRequest(c.auth, c.body, nil, ""); got != c.want {
				t.Fatalf("stickyKeyForRequest = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStickyKeyPrefersClaudeCodeAndCodexHeaders(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		wantHas string
	}{
		{"claude code session", http.Header{"X-Claude-Code-Session-Id": []string{"claude-sess"}}, "|cli:"},
		{"codex thread", http.Header{"Thread-Id": []string{"codex-thread"}}, "|cli:"},
		{"codex session", http.Header{"Session-Id": []string{"codex-sess"}}, "|cli:"},
		{"opencode session", http.Header{"x-opencode-session": []string{"openai-sess"}}, "|oc:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stickyKeyForRequest(UpstreamAuth{}, map[string]any{}, c.headers, hashSessionRouteKey("openai-sess"))
			if !strings.Contains(got, c.wantHas) {
				t.Fatalf("stickyKeyForRequest = %q, want contains %q", got, c.wantHas)
			}
		})
	}
}

func TestSelectUpstreamTargetFollowsClientHeaders(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"},
	}, socks5RR, true, false)

	h1 := http.Header{"X-Claude-Code-Session-Id": []string{"claude-sess"}}
	h2 := http.Header{"X-Claude-Code-Session-Id": []string{"claude-sess"}}
	b1, c1 := selectUpstreamTarget(UpstreamAuth{}, map[string]any{}, h1, "")
	b2, c2 := selectUpstreamTarget(UpstreamAuth{}, map[string]any{}, h2, "")
	if b1 != b2 || c1 != c2 {
		t.Fatalf("same Claude session should pin same (base, client), got %q/%p vs %q/%p", b1, c1, b2, c2)
	}

	ch := http.Header{"Thread-Id": []string{"codex-thread"}}
	cb1, cc1 := selectUpstreamTarget(UpstreamAuth{}, map[string]any{}, ch, "")
	cb2, cc2 := selectUpstreamTarget(UpstreamAuth{}, map[string]any{}, ch, "")
	if cb1 != cb2 || cc1 != cc2 {
		t.Fatalf("same Codex thread should pin same (base, client), got %q/%p vs %q/%p", cb1, cc1, cb2, cc2)
	}
}

func TestSelectUpstreamTargetPinsSession(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"},
	}, socks5RR, true, false)
	// 单域名 + RR 代理：base 固定默认，仅代理 sticky。
	b1, c1 := selectUpstreamTarget(UpstreamAuth{Token: "tok-1"}, nil, nil, "")
	b2, c2 := selectUpstreamTarget(UpstreamAuth{Token: "tok-1"}, nil, nil, "")
	if b1 != "https://opencode.ai" || b2 != "https://opencode.ai" {
		t.Fatalf("single base = %q, %q, want default", b1, b2)
	}
	if c1 != c2 {
		t.Fatalf("same session got different clients")
	}
	// 用户级会话也固定。
	u1, _ := selectUpstreamTarget(UpstreamAuth{}, map[string]any{"user": "sess-1"}, nil, "")
	u2, _ := selectUpstreamTarget(UpstreamAuth{}, map[string]any{"user": "sess-1"}, nil, "")
	if u1 != u2 {
		t.Fatalf("same user session got different bases")
	}
	// 无会话标识的 public 请求共享同一个兜底出口。
	p1, _ := selectUpstreamTarget(UpstreamAuth{}, map[string]any{}, nil, "")
	p2, _ := selectUpstreamTarget(UpstreamAuth{}, map[string]any{}, nil, "")
	if p1 != p2 {
		t.Fatalf("public fallback sessions should share one base")
	}
}

func TestSelectUpstreamTargetMultiBaseSticky(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"},
	}, socks5RR, true, false)
	withBaseURLs(t, []string{"https://opencode.ai", "https://zen1.example.com"})

	b1, c1 := selectUpstreamTarget(UpstreamAuth{Token: "tok-1"}, nil, nil, "")
	b2, c2 := selectUpstreamTarget(UpstreamAuth{Token: "tok-1"}, nil, nil, "")
	if b1 != b2 || c1 != c2 {
		t.Fatalf("same session should pin same (base, client), got %q/%p vs %q/%p", b1, c1, b2, c2)
	}
	if b1 != "https://opencode.ai" && b1 != "https://zen1.example.com" {
		t.Fatalf("base %q not in configured set", b1)
	}
	// 不同会话（不同 token）会分散到不同 base。
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		b, _ := selectUpstreamTarget(UpstreamAuth{Token: fmt.Sprintf("tok-sess-%d", i)}, nil, nil, "")
		seen[b] = true
	}
	if len(seen) < 2 {
		t.Fatalf("different sessions should distribute across bases, got %v", seen)
	}
}

func TestInvalidateUpstreamTargetRebindsChannel(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"},
	}, socks5RR, true, false)
	withBaseURLs(t, []string{"https://opencode.ai", "https://zen2.example.com"})

	auth := UpstreamAuth{Token: "tok-2"}
	seenCombos := map[[2]int]bool{}
	for i := 0; i < 6; i++ {
		_, _ = selectUpstreamTarget(auth, nil, nil, "")
		stickyMu.Lock()
		e := stickyEntries["tok:tok-2"]
		stickyMu.Unlock()
		if e == nil {
			t.Fatal("binding should exist after select")
		}
		seenCombos[[2]int{e.baseIdx, e.proxyIdx}] = true
		invalidateUpstreamTarget(auth, nil, nil, "")
	}
	if len(seenCombos) < 3 {
		t.Fatalf("channel did not rotate across rebinds: %v", seenCombos)
	}
}

func TestSelectUpstreamTargetSkipsWhenDisabled(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"},
	}, socks5RR, false, false)
	withBaseURLs(t, []string{"https://opencode.ai", "https://zen3.example.com"})

	// socks5_sticky=false 只关闭代理维绑定：域名维仍按会话哈希 sticky，
	// 客户端每次按当前配置轮询/固定（不缓存）。
	b1, c1 := selectUpstreamTarget(UpstreamAuth{Token: "tok-1"}, nil, nil, "")
	b2, c2 := selectUpstreamTarget(UpstreamAuth{Token: "tok-1"}, nil, nil, "")
	if b1 != b2 {
		t.Fatalf("domain sticky should stay when proxy sticky disabled, got %q vs %q", b1, b2)
	}
	// 代理维不缓存在条目里：两次返回的 client 应来自 getHTTPClient() 轮询，
	// 且不能是 httpClient（有代理配置时）。
	if c1 == httpClient || c2 == httpClient {
		t.Fatalf("proxy should still route through socks5 when sticky disabled")
	}
	// 域名维仍有条目（用于固定 base）。
	if got := len(stickyEntries); got != 1 {
		t.Fatalf("domain-only sticky should keep 1 entry, got %d", got)
	}
}

func TestSelectUpstreamTargetPaidDirectBypasses(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"},
	}, socks5RR, true, true)

	paid := UpstreamAuth{Token: "tok-1", Mode: AuthRouteAuto}
	b, c := selectUpstreamTarget(paid, nil, nil, "")
	if c != httpClient {
		t.Fatalf("paid direct should return the direct client, got %p", c)
	}
	if b != "https://opencode.ai" {
		t.Fatalf("paid direct base = %q, want default", b)
	}
	if got := len(stickyEntries); got != 0 {
		t.Fatalf("paid direct must not create sticky entries, got %d", got)
	}
}

func TestRoundRobinBaseURLDistributes(t *testing.T) {
	withBaseURLs(t, []string{"https://one.example.com", "https://two.example.com"})
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		seen[roundRobinBaseURL()] = true
	}
	if len(seen) < 2 {
		t.Fatalf("round robin should distribute across bases, got %v", seen)
	}
}

func TestRoundRobinBaseURLSingleDefault(t *testing.T) {
	withBaseURLs(t, defaultBaseURLs)
	if got := roundRobinBaseURL(); got != "https://opencode.ai" {
		t.Fatalf("single base round robin = %q, want default", got)
	}
}

func TestNormalizeBaseURLs(t *testing.T) {
	got := normalizeBaseURLs([]string{"https://a.com/", "https://a.com", "", "  ", "https://b.com", "https://a.com"})
	if len(got) != 2 || got[0] != "https://a.com" || got[1] != "https://b.com" {
		t.Fatalf("normalizeBaseURLs = %#v, want [https://a.com https://b.com]", got)
	}
	if got := normalizeBaseURLs(nil); strings.Join(got, ",") != strings.Join(defaultBaseURLs, ",") {
		t.Fatalf("empty normalize = %#v, want default", got)
	}
}
