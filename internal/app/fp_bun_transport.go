package app

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
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

// fpTLSSettings 是出站 TLS 的信任配置（三个指纹档共用）。
type fpTLSSettings struct {
	InsecureSkipVerify bool
	RootCAs            *x509.CertPool
}

// fingerprintTLSSettings 从环境变量读取 TLS 信任配置：
//   - OPENCODE2API_TLS_INSECURE=1|true|yes|on 跳过证书校验；
//   - OPENCODE2API_TLS_CA_FILE=<PEM> 在系统根池之上追加私有 CA。
//
// 只走环境变量、不加 config.json 键：与既有 OPENCODE2API_* 开关一致，
// 也避免引入需要管理后台支撑的配置项。
func fingerprintTLSSettings() fpTLSSettings {
	var s fpTLSSettings
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENCODE2API_TLS_INSECURE"))) {
	case "1", "true", "yes", "on":
		s.InsecureSkipVerify = true
	}

	path := strings.TrimSpace(os.Getenv("OPENCODE2API_TLS_CA_FILE"))
	if path == "" {
		return s
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("fp tls ca file ignored", "path", path, "error", err)
		return s
	}
	if !pool.AppendCertsFromPEM(pem) {
		slog.Warn("fp tls ca file ignored", "path", path, "error", "no certificates appended")
		return s
	}
	s.RootCAs = pool
	return s
}

// bunTLSHandshake 在已建立的 TCP 连接上完成 CLI 档 uTLS 握手。
func bunTLSHandshake(ctx context.Context, dial fpDialFunc, addr string, alpn []string, settings fpTLSSettings) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	raw, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	cfg := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: settings.InsecureSkipVerify,
		RootCAs:            settings.RootCAs,
	}
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
//
// dial 只用于明文 http:// 兜底路径，dialTLS 用于 https 握手（可能已包了
// 环境代理）。两者分开传：环境代理只接管 https，明文不该被 CONNECT 隧道带走。
func newBunFingerprintTransport(dial fpDialFunc, dialTLS fpDialFunc, settings fpTLSSettings) http.RoundTripper {
	if dial == nil {
		dial = (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}
	if dialTLS == nil {
		dialTLS = dial
	}
	tlsHandshake := func(ctx context.Context, addr string) (net.Conn, error) {
		return bunTLSHandshake(ctx, dialTLS, addr, bunFingerprintALPN, settings)
	}
	return newFP1Transport(dial, tlsHandshake)
}
