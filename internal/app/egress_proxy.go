package app

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// ======================== 环境变量 HTTP(S) 代理出口 ========================
//
// 与 SOCKS5 出口并存：显式配置的 active_socks5 优先，其次是标准的
// HTTPS_PROXY / HTTP_PROXY / NO_PROXY，最后才是直连（优先级见
// buildProxyClient 的注释）。
//
// 作用范围：只有 https 上游走环境代理。明文 http:// 上游仍在
// fp1Transport.plain / http.Transport 的直连路径上——真实上游全部是 https，
// 为它补一条「明文经代理」的分支只会多一条没人验证的路径。
//
// 这里用 x/net/http/httpproxy 而不是 net/http.ProxyFromEnvironment：后者把
// 环境变量缓存到进程级，测试与热更新都会读到旧值。

// egressProxyFor 解析 https 目标对应的环境代理；NO_PROXY 命中或无代理时返回 nil。
//
// httpproxy 对写坏的值不报错，而是自己拼出 `host="http:"` 这类垃圾 URL
// （`HTTPS_PROXY=http://[::bad` 就是这样），后面每次拨号都会 DNS 失败。
// 所以这里先按原值验一遍，坏值当「没有代理」处理。
func egressProxyFor(target string) (*url.URL, error) {
	cfg := httpproxy.FromEnvironment()
	for _, raw := range []string{cfg.HTTPSProxy, cfg.HTTPProxy} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if err := validateProxyEnv(raw); err != nil {
			return nil, err
		}
	}
	return cfg.ProxyFunc()(&url.URL{Scheme: "https", Host: target})
}

// validateProxyEnv 检查一条代理环境变量是否是可用端点。
// 允许省略 scheme（httpproxy 会给它补 http://）。
func validateProxyEnv(raw string) error {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid proxy url %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("proxy url %q has no host", raw)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("proxy url %q has bad port %q", raw, port)
		}
	}
	return nil
}

// withHTTPProxy 在直连 dialer 外包一层环境代理：命中原生 HTTP(S) 代理时先做
// CONNECT 隧道，否则原样直连。环境变量写坏也只记一条 debug 并退回直连——
// 一个笔误不该把整个出口打死。
//
// settings 同时作用于「TLS 到 https 代理」那一段：企业代理常带私有 CA，
// 信任配置若只覆盖上游就会让 `HTTPS_PROXY=https://…` 直接拨不通。
func withHTTPProxy(dial fpDialFunc, settings fpTLSSettings) fpDialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		pu, err := egressProxyFor(addr)
		if err != nil {
			slog.Debug("fp proxy env ignored", "error", err)
			return dial(ctx, network, addr)
		}
		if pu == nil {
			return dial(ctx, network, addr)
		}
		return dialViaHTTPProxy(ctx, dial, pu, addr, settings)
	}
}

// dialViaHTTPProxy 建立到目标的 CONNECT 隧道。https 代理（TLS 到代理本身）用
// 标准库的 tls.Client：代理那一段与上游看到的 TLS 口径无关，不必复刻指纹，
// 但信任根要跟着 settings 走（与上游同一套 CA/跳过校验开关）。
func dialViaHTTPProxy(ctx context.Context, dial fpDialFunc, pu *url.URL, target string, settings fpTLSSettings) (net.Conn, error) {
	if pu == nil || pu.Scheme == "" {
		return dial(ctx, "tcp", target)
	}
	proxyAddr := pu.Host
	if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
		port := "80"
		if strings.EqualFold(pu.Scheme, "https") {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(pu.Hostname(), port)
	}
	conn, err := dial(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("fp proxy: connect %s: %w", proxyAddr, err)
	}
	if strings.EqualFold(pu.Scheme, "https") {
		tc := tls.Client(conn, &tls.Config{
			ServerName:         pu.Hostname(),
			InsecureSkipVerify: settings.InsecureSkipVerify,
			RootCAs:            settings.RootCAs,
		})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("fp proxy: tls to %s: %w", proxyAddr, err)
		}
		conn = tc
	}

	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if pu.User != nil {
		pass, _ := pu.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(pu.User.Username() + ":" + pass))
		fmt.Fprintf(&req, "Proxy-Authorization: Basic %s\r\n", token)
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("fp proxy: CONNECT %s: %w", target, err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("fp proxy: CONNECT %s: %w", target, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("fp proxy: CONNECT %s rejected: %s", target, resp.Status)
	}
	// CONNECT 成功后的字节归上层（TLS 握手）；bufio 可能已经多读了一点。
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn 把 bufio.Reader 里预读的字节接回 Read。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
