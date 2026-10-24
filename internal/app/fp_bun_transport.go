package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ======================== Bun/opencode CLI 档 Transport ========================
//
// 组合：uTLS HelloCustom（bunClientHelloSpec，逐字节对齐 opencode CLI v2.1.4
// 抓包）+ 自研 HTTP/1.1 客户端（fp1Transport，请求字节与头序自控）。
//
// 真实客户端只 offer ALPN http/1.1，因此这里也只 offer http/1.1：上游不会
// 协商到 h2，整条链路与 CLI 一致。

type fpDialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// bunTLSHandshake 在已建立的 TCP 连接上完成 CLI 档 uTLS 握手。
func bunTLSHandshake(ctx context.Context, dial fpDialFunc, addr string, alpn []string, insecure bool) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	raw, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	cfg := &utls.Config{ServerName: host, InsecureSkipVerify: insecure}
	uconn := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(bunClientHelloSpec(alpn)); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls(bun) preset: %w", err)
	}
	if net.ParseIP(host) != nil {
		// BoringSSL 对 IP 目标不发 SNI，也不发空 SNI 扩展。
		_ = uconn.RemoveSNIExtension()
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls(bun) handshake to %s: %w", addr, err)
	}
	return uconn, nil
}

// newBunFingerprintTransport 返回线上使用的 CLI 档 RoundTripper。
func newBunFingerprintTransport(dial fpDialFunc) http.RoundTripper {
	return newBunFingerprintTransportWithSkipVerify(dial, false)
}

func newBunFingerprintTransportWithSkipVerify(dial fpDialFunc, insecure bool) http.RoundTripper {
	if dial == nil {
		dial = (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}
	dialTLS := func(ctx context.Context, addr string) (net.Conn, error) {
		return bunTLSHandshake(ctx, dial, addr, bunFingerprintALPN, insecure)
	}
	return newFP1Transport(dial, dialTLS)
}
