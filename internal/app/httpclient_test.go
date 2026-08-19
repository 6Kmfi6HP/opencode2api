package app

import (
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
		socks5Mu.Unlock()
		stickyMu.Lock()
		stickyEntries = map[string]*stickyProxyEntry{}
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
			if got := stickyKeyForRequest(c.auth, c.body); got != c.want {
				t.Fatalf("stickyKeyForRequest = %q, want %q", got, c.want)
			}
		})
	}
}

func TestGetHTTPClientStickyPinsSession(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"},
	}, socks5RR, true, false)

	c1 := getHTTPClientSticky(UpstreamAuth{Token: "tok-1"}, nil)
	c2 := getHTTPClientSticky(UpstreamAuth{Token: "tok-1"}, nil)
	if c1 != c2 {
		t.Fatalf("same session got different clients")
	}
	// 用户级会话也固定。
	u1 := getHTTPClientSticky(UpstreamAuth{}, map[string]any{"user": "sess-1"})
	u2 := getHTTPClientSticky(UpstreamAuth{}, map[string]any{"user": "sess-1"})
	if u1 != u2 {
		t.Fatalf("same user session got different clients")
	}
	// 无会话标识的 public 请求共享同一个兜底出口。
	p1 := getHTTPClientSticky(UpstreamAuth{}, map[string]any{})
	p2 := getHTTPClientSticky(UpstreamAuth{}, map[string]any{})
	if p1 != p2 {
		t.Fatalf("public fallback sessions should share one client")
	}
}

func TestInvalidateStickyProxyRebinds(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"},
	}, socks5RR, true, false)

	auth := UpstreamAuth{Token: "tok-1"}
	_ = getHTTPClientSticky(auth, nil)
	if got := len(stickyEntries); got != 1 {
		t.Fatalf("sticky entries = %d, want 1", got)
	}
	invalidateStickyProxy(auth, nil)
	if got := len(stickyEntries); got != 0 {
		t.Fatalf("sticky entries after invalidate = %d, want 0", got)
	}
	// 重新获取应重建绑定(可能哈希到同一代理,但绑定必须重新存在)。
	_ = getHTTPClientSticky(auth, nil)
	if _, ok := stickyEntries["tok:tok-1"]; !ok {
		t.Fatal("binding should be recreated after invalidate")
	}
}

// TestInvalidateStickyProxyRotatesEgress 验证切断绑定后重新分配会改变出口:
// 确定性哈希 + 递增序号保证同一会话的连续绑定轮换到不同代理,而不是
// 永远钉死在同一个哈希结果上(否则 429/错误后的"换 IP"永远不会发生)。
func TestInvalidateStickyProxyRotatesEgress(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"}, {Addr: "p3"},
	}, socks5RR, true, false)

	auth := UpstreamAuth{Token: "tok-2"}
	seen := map[int]bool{}
	for i := 0; i < 4; i++ {
		_ = getHTTPClientSticky(auth, nil)
		stickyMu.Lock()
		idx := stickyEntries["tok:tok-2"].proxyIdx
		stickyMu.Unlock()
		seen[idx] = true
		invalidateStickyProxy(auth, nil)
	}
	if len(seen) < 2 {
		t.Fatalf("egress did not rotate across rebinds: %v", seen)
	}
}

func TestGetHTTPClientStickySkipsWhenDisabled(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"},
	}, socks5RR, false, false)

	getHTTPClientSticky(UpstreamAuth{Token: "tok-1"}, nil)
	if got := len(stickyEntries); got != 0 {
		t.Fatalf("sticky disabled but entries = %d", got)
	}
}

func TestGetHTTPClientStickyPaidDirectBypasses(t *testing.T) {
	withStickyProxyEnv(t, []Socks5Proxy{
		{Addr: "p1"}, {Addr: "p2"},
	}, socks5RR, true, true)

	paid := UpstreamAuth{Token: "tok-1", Mode: AuthRouteAuto}
	c := getHTTPClientSticky(paid, nil)
	if c != httpClient {
		t.Fatalf("paid direct should return the direct client, got %p", c)
	}
	if got := len(stickyEntries); got != 0 {
		t.Fatalf("paid direct must not create sticky entries, got %d", got)
	}
}
