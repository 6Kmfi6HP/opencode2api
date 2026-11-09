package app

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ======================== 环境变量代理出口 ========================

// connectProxy 是一个只做 CONNECT 的本地代理：记下隧道数与 CONNECT 目标，
// 然后忽略目标、固定拨 dialAddr（测试域名不可解析）并双向转发。
type connectProxy struct {
	srv        *httptest.Server
	tunnels    int32
	lastTarget atomic.Value
}

func newConnectProxy(t *testing.T, dialAddr string) *connectProxy {
	t.Helper()
	p := &connectProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&p.tunnels, 1)
		p.lastTarget.Store(r.Host)

		up, err := net.DialTimeout("tcp", dialAddr, 5*time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			up.Close()
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		down, _, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		defer down.Close()
		defer up.Close()
		io.WriteString(down, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{}, 1)
		go func() { io.Copy(up, down); done <- struct{}{} }()
		io.Copy(down, up)
		<-done
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *connectProxy) count() int32 { return atomic.LoadInt32(&p.tunnels) }

// TestEgressProxy_CONNECTTunnel 走 HTTPS_PROXY 的 CONNECT 隧道拿到 200。
// 目标用非回环名字：httpproxy 对 127.0.0.1/localhost 会强制直连，本地测试
// 服务器若用回环地址根本不会经过代理，测不到隧道。
func TestEgressProxy_CONNECTTunnel(t *testing.T) {
	var seenPath atomic.Value
	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath.Store(r.URL.Path)
		io.WriteString(w, `{"ok":true}`)
	}))
	proxy := newConnectProxy(t, srv.Listener.Addr().String())

	t.Setenv("HTTPS_PROXY", proxy.srv.URL)
	t.Setenv("NO_PROXY", "")

	c := &http.Client{
		Transport: newBunFingerprintTransport(
			(&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			withHTTPProxy((&net.Dialer{Timeout: 5 * time.Second}).DialContext, fpTLSSettings{InsecureSkipVerify: true}),
			fpTLSSettings{InsecureSkipVerify: true}),
		Timeout: 20 * time.Second,
	}

	resp, err := c.Get("https://fp-test.invalid:443/v1/models")
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := proxy.count(); got != 1 {
		t.Fatalf("tunnels = %d, want 1", got)
	}
	if target, _ := proxy.lastTarget.Load().(string); target != "fp-test.invalid:443" {
		t.Fatalf("CONNECT target = %q", target)
	}
	if p, _ := seenPath.Load().(string); p != "/v1/models" {
		t.Fatalf("server saw path %q", p)
	}
}

// TestEgressProxy_NoProxyBypasses 两级验证：NO_PROXY 命中时解析不返回代理；
// 回环目标即使配了 HTTPS_PROXY 也直连（httpproxy 的强制规则）。
func TestEgressProxy_NoProxyBypasses(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "fp-test.invalid")
	if pu, err := egressProxyFor("fp-test.invalid:443"); err != nil || pu != nil {
		t.Fatalf("NO_PROXY 命中仍返回代理 %v (err=%v)", pu, err)
	}

	tunnels := int32(0)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tunnels, 1)
		http.Error(w, "must not be used", http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "direct")
	}))
	c, _ := newTestBunClient(t)
	c.Transport = newBunFingerprintTransport(
		(&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		withHTTPProxy((&net.Dialer{Timeout: 5 * time.Second}).DialContext, fpTLSSettings{InsecureSkipVerify: true}),
		fpTLSSettings{InsecureSkipVerify: true})

	resp, err := c.Get(srv.URL + "/direct")
	if err != nil {
		t.Fatalf("loopback request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "direct" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&tunnels); got != 0 {
		t.Fatalf("loopback 走了代理: tunnels = %d", got)
	}
}

// TestEgressProxy_BrokenEnvFallsBackToDirect 环境变量写坏时退回直连。
func TestEgressProxy_BrokenEnvFallsBackToDirect(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "://not a url")
	t.Setenv("NO_PROXY", "")
	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "direct")
	}))
	c, _ := newTestBunClient(t)
	c.Transport = newBunFingerprintTransport(
		(&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		withHTTPProxy((&net.Dialer{Timeout: 5 * time.Second}).DialContext, fpTLSSettings{InsecureSkipVerify: true}),
		fpTLSSettings{InsecureSkipVerify: true})
	resp, err := c.Get(srv.URL + "/direct")
	if err != nil {
		t.Fatalf("broken env wedged egress: %v", err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "direct" {
		t.Fatalf("body = %q", b)
	}
}

// ======================== 空闲连接过期与重放 ========================

// halfCloseH1Server 对第一次请求回 200 后只关写方向（读方向留着），让客户端
// 读到干净的 EOF 而不是 RST；之后的新连接按普通 keep-alive 服务。
// 这样「第二条请求落在已失效的池化连接上」这条路径是确定的。
type halfCloseH1Server struct {
	ln         net.Listener
	mu         sync.Mutex
	reqs       int
	afterFirst func()
	afterOnce  sync.Once
}

func newHalfCloseH1Server(t *testing.T, afterFirst func()) *halfCloseH1Server {
	t.Helper()
	cert := generateSelfSignedTLS(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &halfCloseH1Server{ln: ln, afterFirst: afterFirst}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				tc := tls.Server(conn, cfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				br := bufio.NewReader(tc)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					io.Copy(io.Discard, req.Body)
					s.mu.Lock()
					s.reqs++
					n := s.reqs
					s.mu.Unlock()
					io.WriteString(tc, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
					if n == 1 {
						// 第一次响应后关闭写方向：连接看起来仍然可读，
						// 下一条复用它的请求会读到 EOF。
						if err := tc.CloseWrite(); err != nil {
							return
						}
						if s.afterFirst != nil {
							s.afterOnce.Do(s.afterFirst)
						}
						io.Copy(io.Discard, tc)
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *halfCloseH1Server) url() string { return "https://" + s.ln.Addr().String() }

func TestFP1Transport_RetriesStaleIdleConn(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	srv := newHalfCloseH1Server(t, func() { once.Do(func() { close(closed) }) })

	var dials int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	c := &http.Client{
		Transport: newBunFingerprintTransport(dial, dial, fpTLSSettings{InsecureSkipVerify: true}),
		Timeout:   20 * time.Second,
	}

	get := func() {
		resp, err := c.Get(srv.url() + "/ok")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if b, _ := io.ReadAll(resp.Body); string(b) != "ok" {
			t.Fatalf("body = %q", b)
		}
	}
	get()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not close its write side")
	}
	get()

	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Fatalf("dials = %d, want 2 (one stale conn replaced)", got)
	}
}

func TestFP1Transport_DoesNotRetryPOST(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	srv := newHalfCloseH1Server(t, func() { once.Do(func() { close(closed) }) })

	c, _ := newTestBunClient(t)
	c.Transport = newBunFingerprintTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		nil, fpTLSSettings{InsecureSkipVerify: true})

	post := func() error {
		resp, err := c.Post(srv.url()+"/post", "application/json", strings.NewReader(`{"a":1}`))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return nil
	}
	if err := post(); err != nil {
		t.Fatalf("first POST: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not close its write side")
	}
	if err := post(); err == nil {
		t.Fatal("second POST on a stale conn must fail, not be replayed")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.reqs != 1 {
		t.Fatalf("server saw %d requests, want 1", srv.reqs)
	}
}

// TestFP1Transport_ExpiresIdleConn 池里超过 fp1IdleTimeout 的条目必须被丢弃重建。
func TestFP1Transport_ExpiresIdleConn(t *testing.T) {
	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	var dials int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	tr := newBunFingerprintTransport(dial, dial, fpTLSSettings{InsecureSkipVerify: true}).(*fp1Transport)
	c := &http.Client{Transport: tr, Timeout: 20 * time.Second}

	get := func() {
		resp, err := c.Get(srv.URL + "/ok")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
	}
	get()
	// 把刚归还的连接改成年纪超过空闲上限。
	tr.mu.Lock()
	for _, list := range tr.idle {
		for _, conn := range list {
			conn.usedAt = time.Now().Add(-fp1IdleTimeout - time.Minute)
		}
	}
	tr.mu.Unlock()
	get()

	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Fatalf("dials = %d, want 2 (超龄连接未被丢弃)", got)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, list := range tr.idle {
		for _, conn := range list {
			if time.Since(conn.usedAt) > fp1IdleTimeout {
				t.Fatal("池中仍残留超龄连接")
			}
		}
	}
}

// ======================== deflate ========================

func TestFP1Transport_DeflateZlibAndRaw(t *testing.T) {
	payload := []byte(`{"object":"list","data":[{"id":"x"}]}`)
	cases := map[string]func(io.Writer) io.WriteCloser{
		"zlib": func(w io.Writer) io.WriteCloser { return zlib.NewWriter(w) },
		"raw": func(w io.Writer) io.WriteCloser {
			fw, err := flate.NewWriter(w, flate.DefaultCompression)
			if err != nil {
				t.Fatalf("flate writer: %v", err)
			}
			return fw
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			zw := mk(&buf)
			zw.Write(payload)
			zw.Close()
			srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "deflate")
				w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
				w.Write(buf.Bytes())
			}))
			c, _ := newTestBunClient(t)
			resp, err := c.Get(srv.URL + "/deflate")
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("decoded = %q", got)
			}
		})
	}
}

// TestUTLSTransport_OffProfileProxyAndTrust off 档同样接环境代理与信任配置：
// 不走指纹时由标准库自己发 CONNECT，因此 Transport.Proxy 必须设置且
// DialContext 保持直连（否则会双重拨号）。
func TestUTLSTransport_OffProfileProxyAndTrust(t *testing.T) {
	oldProfile := getTLSFingerprintProfile()
	t.Cleanup(func() { setTLSFingerprintProfile(oldProfile) })
	setTLSFingerprintProfile("off")

	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "")
	t.Setenv("OPENCODE2API_TLS_INSECURE", "1")

	tr := newUTLSTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext, true)
	plain, ok := tr.(*http.Transport)
	if !ok {
		t.Fatalf("off 档应返回 *http.Transport, got %T", tr)
	}
	if plain.Proxy == nil {
		t.Fatal("off 档未接入环境代理")
	}
	pu, err := plain.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "opencode.ai"}})
	if err != nil || pu == nil || pu.Host != "proxy.example:3128" {
		t.Fatalf("Proxy = %v (err=%v)", pu, err)
	}
	if plain.TLSClientConfig == nil || !plain.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("off 档未接入 OPENCODE2API_TLS_INSECURE")
	}

	// useEnvProxy=false（SOCKS5 出口）时不应再叠加环境代理。
	tr2 := newUTLSTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext, false)
	if tr2.(*http.Transport).Proxy != nil {
		t.Fatal("useEnvProxy=false 时不应设置 Proxy")
	}
}

// TestUTLSTransport_ChromeProfileUsesEnvProxy chrome 档的 TLS 拨号必须走环境代理
// （h2 协商失败后回落到 h1，两次拨号都经 tlsDial）。
func TestUTLSTransport_ChromeProfileUsesEnvProxy(t *testing.T) {
	oldProfile := getTLSFingerprintProfile()
	t.Cleanup(func() { setTLSFingerprintProfile(oldProfile) })
	setTLSFingerprintProfile("chrome")
	t.Setenv("NO_PROXY", "")
	t.Setenv("OPENCODE2API_TLS_INSECURE", "1")

	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "chrome-proxied")
	}))
	proxy := newConnectProxy(t, srv.Listener.Addr().String())
	t.Setenv("HTTPS_PROXY", proxy.srv.URL)

	c := &http.Client{
		Transport: newUTLSTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext, true),
		Timeout:   20 * time.Second,
	}
	resp, err := c.Get("https://fp-chrome.invalid:443/hello")
	if err != nil {
		t.Fatalf("chrome request through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "chrome-proxied" {
		t.Fatalf("body = %q", body)
	}
	if got := proxy.count(); got == 0 {
		t.Fatal("chrome 档未走环境代理")
	}
}

// TestDialViaHTTPProxy_DefaultPort 代理 URL 省略端口时按 scheme 补（80/443）。
func TestDialViaHTTPProxy_DefaultPort(t *testing.T) {
	cases := map[string]string{"http": "80", "https": "443"}
	for scheme, wantPort := range cases {
		var seen string
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			seen = addr
			return nil, errors.New("stop after address check")
		}
		pu := &url.URL{Scheme: scheme, Host: "proxy.internal"}
		if _, err := dialViaHTTPProxy(context.Background(), dial, pu, "target.example:443", fpTLSSettings{}); err == nil {
			t.Fatalf("%s: expected dial error", scheme)
		}
		if want := net.JoinHostPort("proxy.internal", wantPort); seen != want {
			t.Fatalf("%s 代理省略端口时拨号地址 = %q, want %q", scheme, seen, want)
		}
	}
}

// TestDialViaHTTPProxy_Rejected CONNECT 被拒时报错且带上代理状态。
func TestDialViaHTTPProxy_Rejected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1024)
				c.Read(buf)
				c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
			}()
		}
	}()
	pu := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	_, err = dialViaHTTPProxy(context.Background(), (&net.Dialer{Timeout: 5 * time.Second}).DialContext, pu, "target.example:443", fpTLSSettings{})
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("err = %v, want CONNECT rejection with status", err)
	}
}

// TestUTLSTransport_OffProfileProxySkipsPlainHTTP off 档的 Proxy 只对 https 生效：
// 明文 http:// 目标保持直连（与 bun/chrome 档取舍一致）。
func TestUTLSTransport_OffProfileProxySkipsPlainHTTP(t *testing.T) {
	oldProfile := getTLSFingerprintProfile()
	t.Cleanup(func() { setTLSFingerprintProfile(oldProfile) })
	setTLSFingerprintProfile("off")
	t.Setenv("HTTP_PROXY", "http://proxy.example:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "")

	tr := newUTLSTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext, true).(*http.Transport)
	if pu, _ := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "http", Host: "plain.example"}}); pu != nil {
		t.Fatalf("明文 http 目标不应走代理: %v", pu)
	}
	if pu, _ := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "secure.example"}}); pu == nil {
		t.Fatal("https 目标应走代理")
	}

	plainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain-ok")
	}))
	defer plainSrv.Close()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := c.Get(plainSrv.URL + "/x")
	if err != nil {
		t.Fatalf("plain http through off transport: %v", err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "plain-ok" {
		t.Fatalf("body = %q", b)
	}
}

// TestEgressProxy_TLSProxyUsesTrustSettings https 代理那一段的握手必须跟着
// 同一套信任配置走：企业代理用私有 CA 时，只信任上游不够。
//
// 隧道另一头用明文 httptest（本测试只关心「到代理」的 TLS 段），
// 因此 CONNECT 目标指向它的监听地址。
func TestEgressProxy_TLSProxyUsesTrustSettings(t *testing.T) {
	cert := generateSelfSignedTLS(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "tls-proxy-ok")
	}))
	defer upstream.Close()

	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", upstream.Listener.Addr().String())
		if err != nil {
			client.Close()
			return
		}
		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { io.Copy(up, client); up.Close() }()
		io.Copy(client, up)
		client.Close()
	}))
	proxy.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	proxy.StartTLS()
	defer proxy.Close()

	pool := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(leaf)

	target := upstream.Listener.Addr().String()
	dial := (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	// 前提：不信任该 CA 时必须失败，否则后面的成功说明不了配置生效。
	if _, err := dialViaHTTPProxy(context.Background(), dial, mustURL(t, proxy.URL), target, fpTLSSettings{}); err == nil {
		t.Fatal("未提供 CA 时不该信任自签 https 代理")
	}

	conn, err := dialViaHTTPProxy(context.Background(), dial, mustURL(t, proxy.URL), target, fpTLSSettings{RootCAs: pool})
	if err != nil {
		t.Fatalf("带 CA 的 https 代理握手失败: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET /x HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("隧道内响应读取失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "tls-proxy-ok" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestEgressProxy_InvalidEnvValuesIgnored 写坏的代理环境变量必须当「没有代理」，
// 不能让所有出站跟着 DNS 失败（httpproxy 自己会把坏值拼成 host="http:"）。
func TestEgressProxy_InvalidEnvValuesIgnored(t *testing.T) {
	cases := []string{
		"http://[::bad",
		"://not a url",
		"http://",
		"127.0.0.1:99999",
		"socks5://127.0.0.1:1080",
	}
	for _, v := range cases {
		t.Setenv("HTTPS_PROXY", v)
		t.Setenv("HTTP_PROXY", "")
		pu, err := egressProxyFor("opencode.ai:443")
		if err == nil {
			t.Fatalf("%q: 期望报错（当坏值处理），得到代理 %v", v, pu)
		}

		direct := int32(0)
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			atomic.AddInt32(&direct, 1)
			return nil, errors.New("stop after direct dial")
		}
		if _, err := withHTTPProxy(dial, fpTLSSettings{})(context.Background(), "tcp", "opencode.ai:443"); err == nil {
			t.Fatalf("%q: 期望拨号错误", v)
		}
		if got := atomic.LoadInt32(&direct); got != 1 {
			t.Fatalf("%q: 坏值未退回直连（direct dials=%d）", v, got)
		}
	}

	// 合法值必须照常解析，别把校验写成过度拒绝。
	t.Setenv("HTTPS_PROXY", "127.0.0.1:18081")
	t.Setenv("HTTP_PROXY", "")
	pu, err := egressProxyFor("opencode.ai:443")
	if err != nil || pu == nil {
		t.Fatalf("省略 scheme 的合法值被拒: pu=%v err=%v", pu, err)
	}
	if pu.Host != "127.0.0.1:18081" {
		t.Fatalf("pu.Host = %q", pu.Host)
	}
}
