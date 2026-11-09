package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func generateSelfSignedTLS(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fp-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// newTestBunClient 构造连到测试服务器的 CLI 档客户端（跳过证书校验，
// 其余字节与线上一致）。
func newTestBunClient(t *testing.T) (*http.Client, *int32) {
	t.Helper()
	var dials int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt32(&dials, 1)
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
	tr := newBunFingerprintTransport(dial, dial, fpTLSSettings{InsecureSkipVerify: true})
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}, &dials
}

func newH1TestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestFP1Transport_BasicRequest(t *testing.T) {
	type seen struct {
		Method, Path, Query, UA, Auth, Body, Proto string
	}
	var got seen
	var mu sync.Mutex
	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("User-Agent"),
			r.Header.Get("Authorization"), string(b), r.Proto}
		mu.Unlock()
		w.Header().Set("X-Test", "ok")
		fmt.Fprint(w, `{"ok":true}`)
	}))

	c, _ := newTestBunClient(t)
	resp, err := c.Post(srv.URL+"/v1/chat/completions?x=1", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Method != "POST" || got.Path != "/v1/chat/completions" || got.Query != "x=1" {
		t.Fatalf("server saw %+v", got)
	}
	if got.Body != `{"a":1}` || got.Proto != "HTTP/1.1" {
		t.Fatalf("server saw %+v", got)
	}
}

func TestFP1Transport_StreamingSSE(t *testing.T) {
	const chunks = 20
	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
		}
	}))

	c, _ := newTestBunClient(t)
	resp, err := c.Get(srv.URL + "/stream")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 32)
	n, err := resp.Body.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("first read: n=%d err=%v", n, err)
	}
	if !strings.Contains(string(buf[:n]), "data: 0") {
		t.Fatalf("unexpected first chunk %q", buf[:n])
	}
	rest, _ := io.ReadAll(resp.Body)
	all := string(buf[:n]) + string(rest)
	for i := 0; i < chunks; i++ {
		if !strings.Contains(all, fmt.Sprintf("data: %d\n\n", i)) {
			t.Fatalf("missing chunk %d", i)
		}
	}
}

func TestFP1Transport_LargeBodyBothWays(t *testing.T) {
	const reqSize = 512 * 1024
	const respSize = 2 * 1024 * 1024
	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "%08d", n)
		w.Write(bytes.Repeat([]byte("x"), respSize-8))
	}))

	c, _ := newTestBunClient(t)
	resp, err := c.Post(srv.URL+"/big", "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte("y"), reqSize)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(body) != respSize || !bytes.HasPrefix(body, []byte(fmt.Sprintf("%08d", reqSize))) {
		t.Fatalf("len=%d prefix=%q", len(body), body[:8])
	}
}

func TestFP1Transport_ContextCancel(t *testing.T) {
	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	c, _ := newTestBunClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/slow", nil)
	start := time.Now()
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected error on cancelled request")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cancel took too long: %v", d)
	}
}

// TestFP1Transport_BrotliDecode 上游对 br 会真的回 br（实测 opencode.ai）。
func TestFP1Transport_BrotliDecode(t *testing.T) {
	payload := []byte(`{"object":"list","data":[]}`)
	srv := newH1TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		bw := brotli.NewWriter(&buf)
		bw.Write(payload)
		bw.Close()
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Length", fmt.Sprint(buf.Len()))
		w.Write(buf.Bytes())
	}))

	c, _ := newTestBunClient(t)
	resp, err := c.Get(srv.URL + "/br")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, payload) {
		t.Fatalf("decoded body = %q", got)
	}
}

// ======================== 线上字节格式断言 ========================

// recordH1Server 记录客户端请求的原始字节（含头顺序），并回 200。
type recordH1Server struct {
	ln   net.Listener
	mu   sync.Mutex
	raws [][]byte
	n    int32
}

func newRecordH1Server(t *testing.T) *recordH1Server {
	t.Helper()
	cert := generateSelfSignedTLS(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &recordH1Server{ln: ln}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&s.n, 1)
			go func() {
				defer conn.Close()
				tc := tls.Server(conn, cfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				br := bufio.NewReader(tc)
				for {
					tee := &recordingReader{r: br}
					req, err := http.ReadRequest(bufio.NewReader(tee))
					if err != nil {
						return
					}
					io.Copy(io.Discard, req.Body)
					s.mu.Lock()
					s.raws = append(s.raws, tee.buf.Bytes())
					s.mu.Unlock()
					fmt.Fprint(tc, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

type recordingReader struct {
	r   io.Reader
	buf bytes.Buffer
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.buf.Write(p[:n])
	}
	return n, err
}

func (s *recordH1Server) addr() string { return s.ln.Addr().String() }

func parts2(b3 string) string { return strings.Split(b3, "-")[0] }

func TestFP1Transport_WireFormat(t *testing.T) {
	s := newRecordH1Server(t)
	c, _ := newTestBunClient(t)

	do := func() {
		req, _ := http.NewRequest("POST", "https://"+s.addr()+"/inference/openai/v1/chat/completions", strings.NewReader(`{"x":1}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer public")
		req.Header.Set("User-Agent", "opencode/latest/2.1.4/cli")
		req.Header.Set("x-opencode-client", "cli")
		req.Header.Set("x-opencode-project", "52cfe0c675663a9e3533190da2b27f7d61c3edad")
		req.Header.Set("Accept", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	do()
	do()

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.raws) != 2 {
		t.Fatalf("captured %d requests", len(s.raws))
	}
	if n := atomic.LoadInt32(&s.n); n != 1 {
		t.Fatalf("connections = %d, want keep-alive reuse", n)
	}
	raw := string(s.raws[0])
	lines := strings.Split(raw, "\r\n")
	if lines[0] != "POST /inference/openai/v1/chat/completions HTTP/1.1" {
		t.Fatalf("request line = %q", lines[0])
	}
	// 用户头：大写名字在前、小写名字在后（字节序），随后是固定 5 行。
	wantPrefix := []string{
		"Authorization: Bearer public",
		"Content-Type: application/json",
		"User-Agent: opencode/latest/2.1.4/cli",
		"b3: ",
		"traceparent: 00-",
		"x-opencode-client: cli",
		"x-opencode-project: 52cfe0c675663a9e3533190da2b27f7d61c3edad",
	}
	for i, want := range wantPrefix {
		if !strings.HasPrefix(lines[i+1], want) {
			t.Fatalf("header line %d = %q, want prefix %q", i+1, lines[i+1], want)
		}
	}
	// trace 头宽度必须与 CLI 一致：b3=<32hex>-<16hex>-1-<16hex>，traceparent=00-<32hex>-<16hex>-01
	b3 := strings.TrimPrefix(lines[4], "b3: ")
	if parts := strings.Split(b3, "-"); len(parts) != 4 ||
		len(parts[0]) != 32 || len(parts[1]) != 16 || parts[2] != "1" || len(parts[3]) != 16 {
		t.Fatalf("b3 format = %q", b3)
	}
	tp := strings.TrimPrefix(lines[5], "traceparent: ")
	if parts := strings.Split(tp, "-"); len(parts) != 4 ||
		parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || parts[3] != "01" {
		t.Fatalf("traceparent format = %q", tp)
	}
	if !strings.Contains(tp, parts2(b3)) {
		t.Fatalf("traceparent/b3 trace id mismatch: %q vs %q", tp, b3)
	}
	if lines[8] != "Connection: keep-alive" {
		t.Fatalf("line 8 = %q", lines[8])
	}
	if lines[9] != "Accept: */*" {
		t.Fatalf("line 9 = %q", lines[9])
	}
	if !strings.HasPrefix(lines[10], "Host: ") {
		t.Fatalf("line 10 = %q", lines[10])
	}
	if lines[11] != "Accept-Encoding: gzip, deflate, br, zstd" {
		t.Fatalf("line 11 = %q", lines[11])
	}
	if lines[12] != "Content-Length: 7" {
		t.Fatalf("line 12 = %q", lines[12])
	}

	// 两次请求的头顺序必须完全一致（trace 值不同，只比较名字序列）。
	nameOf := func(raw string) []string {
		var out []string
		for _, l := range strings.Split(raw, "\r\n")[1:] {
			if l == "" {
				break
			}
			out = append(out, strings.SplitN(l, ":", 2)[0])
		}
		return out
	}
	if a, b := nameOf(string(s.raws[0])), nameOf(string(s.raws[1])); strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("header order unstable:\n1: %v\n2: %v", a, b)
	}
}

// ======================== TLS 信任配置 ========================

func TestFingerprintTLSSettings_Env(t *testing.T) {
	t.Setenv("OPENCODE2API_TLS_INSECURE", "")
	t.Setenv("OPENCODE2API_TLS_CA_FILE", "")
	if s := fingerprintTLSSettings(); s.InsecureSkipVerify || s.RootCAs != nil {
		t.Fatalf("no env should mean default settings, got %+v", s)
	}

	t.Setenv("OPENCODE2API_TLS_INSECURE", "1")
	if s := fingerprintTLSSettings(); !s.InsecureSkipVerify {
		t.Fatal("OPENCODE2API_TLS_INSECURE=1 未生效")
	}
	t.Setenv("OPENCODE2API_TLS_INSECURE", "0")
	if s := fingerprintTLSSettings(); s.InsecureSkipVerify {
		t.Fatal("OPENCODE2API_TLS_INSECURE=0 不应跳过校验")
	}

	cert := generateSelfSignedTLS(t)
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_TLS_CA_FILE", path)
	s := fingerprintTLSSettings()
	if s.RootCAs == nil {
		t.Fatal("OPENCODE2API_TLS_CA_FILE 未装载根池")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: s.RootCAs, DNSName: "localhost"}); err != nil {
		t.Fatalf("CA 未进入返回的根池: %v", err)
	}
}

// TestFingerprintTLSSettings_CAFileEnablesHandshake 用 CA 文件（不跳过校验）
// 连自签服务器，验证配置真的接到了握手路径上。
func TestFingerprintTLSSettings_CAFileEnablesHandshake(t *testing.T) {
	cert := generateSelfSignedTLS(t)
	path := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE2API_TLS_INSECURE", "")
	t.Setenv("OPENCODE2API_TLS_CA_FILE", path)

	// httptest 默认自签证书与我们写入 CA 文件的那张不同，必须显式装上。
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "trusted")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	tr := newBunFingerprintTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext, nil, fingerprintTLSSettings())
	c := &http.Client{Transport: tr, Timeout: 20 * time.Second}
	resp, err := c.Get(srv.URL + "/ca")
	if err != nil {
		t.Fatalf("CA 文件未生效，握手失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "trusted" {
		t.Fatalf("body = %q", body)
	}

	// 同一台服务器，不配置 CA 时必须失败（证明上面的成功来自 RootCAs）。
	t.Setenv("OPENCODE2API_TLS_CA_FILE", "")
	tr2 := newBunFingerprintTransport((&net.Dialer{Timeout: 5 * time.Second}).DialContext, nil, fingerprintTLSSettings())
	c2 := &http.Client{Transport: tr2, Timeout: 20 * time.Second}
	if _, err := c2.Get(srv.URL + "/ca"); err == nil {
		t.Fatal("未配置 CA 时自签证书不应通过校验")
	}
}

// TestFP1Transport_DumpHook OPENCODE2API_FP_DUMP 落盘的就是线上字节（头 + 体）。
func TestFP1Transport_DumpHook(t *testing.T) {
	s := newRecordH1Server(t)
	path := filepath.Join(t.TempDir(), "req.bin")
	t.Setenv("OPENCODE2API_FP_DUMP", path)

	c, _ := newTestBunClient(t)
	req, _ := http.NewRequest("POST", "https://"+s.addr()+"/dump", strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer public")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dump file: %v", err)
	}
	s.mu.Lock()
	raw := s.raws[0]
	s.mu.Unlock()
	if !bytes.Equal(got, raw) {
		t.Fatalf("dump 与线上字节不一致:\ndump=%q\nwire=%q", got, raw)
	}
}
