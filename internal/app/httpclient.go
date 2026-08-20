package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var httpClient = &http.Client{
	Timeout: 300 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	},
}

// ======================== SOCKS5 代理 ========================

func socks5Dial(proxy Socks5Proxy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		conn, err := net.DialTimeout("tcp", proxy.Addr, 10*time.Second)
		if err != nil {
			return nil, fmt.Errorf("socks5 connect to %s: %w", proxy.Addr, err)
		}
		deadline := time.Now().Add(15 * time.Second)
		conn.SetDeadline(deadline)

		// 认证方法协商
		auth := byte(0x00) // no auth
		if proxy.Username != "" {
			auth = 0x02 // username/password
		}
		if _, err := conn.Write([]byte{0x05, 0x01, auth}); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 handshake write: %w", err)
		}
		buf := make([]byte, 2)
		if _, err := io.ReadFull(conn, buf); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 handshake read: %w", err)
		}
		if buf[0] != 0x05 {
			conn.Close()
			return nil, fmt.Errorf("socks5: not socks5 protocol")
		}

		// 用户名/密码认证
		if buf[1] == 0x02 {
			if proxy.Username == "" {
				conn.Close()
				return nil, fmt.Errorf("socks5: server requires auth but no credentials")
			}
			ulen := len(proxy.Username)
			plen := len(proxy.Password)
			authBuf := make([]byte, 3+ulen+plen)
			authBuf[0] = 0x01
			authBuf[1] = byte(ulen)
			copy(authBuf[2:], proxy.Username)
			authBuf[2+ulen] = byte(plen)
			copy(authBuf[3+ulen:], proxy.Password)
			if _, err := conn.Write(authBuf); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth write: %w", err)
			}
			authResp := make([]byte, 2)
			if _, err := io.ReadFull(conn, authResp); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth read: %w", err)
			}
			if authResp[1] != 0x00 {
				conn.Close()
				return nil, fmt.Errorf("socks5: auth failed")
			}
		} else if buf[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5: unsupported auth method 0x%02x", buf[1])
		}

		// CONNECT 请求
		host, portStr, err := net.SplitHostPort(target)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5: invalid target %s: %w", target, err)
		}
		port := 0
		fmt.Sscanf(portStr, "%d", &port)

		req := []byte{0x05, 0x01, 0x00} // VER, CMD=CONNECT, RSV
		ip := net.ParseIP(host)
		if ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				req = append(req, 0x01) // IPv4
				req = append(req, ip4...)
			} else {
				req = append(req, 0x04) // IPv6
				req = append(req, ip.To16()...)
			}
		} else {
			if len(host) > 255 {
				conn.Close()
				return nil, fmt.Errorf("socks5: hostname too long")
			}
			req = append(req, 0x03) // Domain
			req = append(req, byte(len(host)))
			req = append(req, []byte(host)...)
		}
		req = append(req, byte(port>>8), byte(port))

		if _, err := conn.Write(req); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect write: %w", err)
		}

		// 读取响应
		resp := make([]byte, 4)
		if _, err := io.ReadFull(conn, resp); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect read: %w", err)
		}
		if resp[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5: connect failed, status 0x%02x", resp[1])
		}

		// 读取绑定地址
		switch resp[3] {
		case 0x01: // IPv4
			if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind ipv4: %w", err)
			}
		case 0x03: // Domain
			dlen := make([]byte, 1)
			if _, err := io.ReadFull(conn, dlen); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind domain len: %w", err)
			}
			if _, err := io.ReadFull(conn, make([]byte, int(dlen[0])+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind domain: %w", err)
			}
		case 0x04: // IPv6
			if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5: read bind ipv6: %w", err)
			}
		default:
			conn.Close()
			return nil, fmt.Errorf("socks5: unknown address type 0x%02x", resp[3])
		}

		conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

var (
	socks5Proxies    []Socks5Proxy
	activeSocks5     string // 启用的代理 Addr，空表示直连，__round_robin__ 表示轮询
	socks5PaidDirect bool   // true=带 key/付费直连；false/缺省=全部走代理
	socks5Mu         sync.RWMutex
)

const socks5RR = "__round_robin__"

var socks5RRIndex uint32

var (
	socks5Client     *http.Client // 缓存的 SOCKS5 客户端
	socks5ClientAddr string       // 缓存对应的代理地址
	socks5Sticky     bool         // 轮询模式下按会话固定出口（默认 true）
)

// upstreamBaseURLs holds the normalized upstream base URL list (e.g. reversed
// zen domains). Guarded by socks5Mu like the rest of the routing state.
var upstreamBaseURLs = normalizeBaseURLs(nil)

var baseURLRRIndex uint32

// getUpstreamBaseURLs returns a copy of the base URL list.
func getUpstreamBaseURLs() []string {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()
	return append([]string(nil), upstreamBaseURLs...)
}

// roundRobinBaseURL picks a base URL for server-global requests (model
// refresh, catalog fetch) that are not tied to a session.
func roundRobinBaseURL() string {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()
	if len(upstreamBaseURLs) <= 1 {
		return upstreamBaseURLs[0]
	}
	idx := atomic.AddUint32(&baseURLRRIndex, 1) % uint32(len(upstreamBaseURLs))
	return upstreamBaseURLs[idx]
}

// ======================== 会话粘性出口（sticky egress） ========================
//
// 上游免费层（opencode.ai/zen -free 模型）的 prompt 缓存按出口 IP 隔离：
// 同一请求经不同出口会各自冷启动，缓存几乎无法命中。轮询模式下若每次请求
// 随机换出口，命中率会归零（实测 0% vs 固定出口 99.8%）。
//
// sticky 模式为同一会话（账号 token 或客户端会话 user）固定一个出口代理，
// 让缓存持续累积；不同会话之间仍轮询分散，保留多出口的意义。绑定 TTL
// 过期或代理连接失败时自动解除，重新分配出口。
//
// 多上游域名下，同一 (key, 域名, 代理) 三元组一起绑定，使会话在负载均衡
// 多个反代域名时也保持同一目标，避免在 domain 间漂移破坏缓存与限流状态。

type stickyProxyEntry struct {
	baseIdx  int // 上游域名下标（-1 表示未启用多域名 sticky）
	proxyIdx int // 代理下标（-1 表示直连）
	client   *http.Client
	lastUsed time.Time
}

var (
	stickyMu      sync.Mutex
	stickyEntries = map[string]*stickyProxyEntry{}
)

const (
	stickyEntryTTL       = 15 * time.Minute
	stickyMaxEntries     = 256
	stickyPublicFallback = "cli://public-shared" // 无会话标识的 public 请求共用同一出口
)

// stickyRebindSeq 每次重新绑定递增,参与哈希,保证同一会话在绑定被切断
// (上游 429/5xx/连接错误)后重新分配时换到不同出口,而不是永远钉死
// 在同一个确定性哈希结果上。
var stickyRebindSeq uint32

// stickyKeyForRequest 生成会话粘性键。优先级：账号 token > 会话 user
// （来自 Claude metadata 的 session_id 等）> 公共兜底。
func stickyKeyForRequest(auth UpstreamAuth, bodyMap map[string]any) string {
	if auth.Token != "" {
		return "tok:" + auth.Token
	}
	if u, ok := bodyMap["user"].(string); ok && u != "" {
		return "usr:" + u
	}
	return stickyPublicFallback
}

// buildProxyClient 为指定代理构建带 SOCKS5 dial 的 HTTP 客户端。
func buildProxyClient(proxy Socks5Proxy) *http.Client {
	return &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			DialContext:         socks5Dial(proxy),
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// selectUpstreamTarget 为一次上游请求选择 (baseURL, client)：
//   - 单域名(默认)时 base 固定为默认域名；多域名时按会话 key 哈希 sticky 到一个 base；
//   - socks5 轮询+sticky 开启时同一会话同时固定一个代理出口；
//   - paid 直连(socks5_paid_direct=true 且付费层)时 client=httpClient，但域名仍 sticky。
//
// 快路径(单域名 + 代理维不需要 sticky 绑定)不产生条目。
func selectUpstreamTarget(auth UpstreamAuth, bodyMap map[string]any) (string, *http.Client) {
	paidDirect := auth.tier() == TierPaid && getSocks5PaidDirect()
	sticky := getSocks5Sticky()
	key := stickyKeyForRequest(auth, bodyMap)

	socks5Mu.RLock()
	rr := activeSocks5 == socks5RR
	proxies := socks5Proxies
	baseURLs := upstreamBaseURLs
	socks5Mu.RUnlock()

	// 快路径：单域名且代理维也不需要 sticky 绑定。
	domainSticky := len(baseURLs) > 1
	proxySticky := sticky && rr && len(proxies) > 0 && !paidDirect
	if !domainSticky && !proxySticky {
		if paidDirect {
			return baseURLs[0], httpClient
		}
		return baseURLs[0], getHTTPClient()
	}

	entry := lookupOrCreateSticky(key, domainSticky, proxySticky, len(baseURLs), len(proxies), proxies)
	base := baseURLs[0]
	if entry.baseIdx >= 0 {
		base = baseURLs[entry.baseIdx]
	}
	// 代理维未启用 sticky 时，client 每次按当前配置轮询/固定，不缓存。
	if !proxySticky {
		if paidDirect {
			return base, httpClient
		}
		return base, getHTTPClient()
	}
	return base, entry.client
}

// lookupOrCreateSticky 查找或创建会话的 (域名, 代理) 绑定。同一 key 的连续
// 绑定因 stickyRebindSeq 递增而轮换，使失效后的重绑真正换到不同目标。
// proxySticky=false 时只缓存域名维（baseIdx），client 字段不使用。
func lookupOrCreateSticky(key string, domainSticky, proxySticky bool, nBase, nProxy int, proxies []Socks5Proxy) *stickyProxyEntry {
	stickyMu.Lock()
	defer stickyMu.Unlock()
	now := time.Now()
	// 懒清理：先清过期条目，超出上限时再清最旧的。
	if len(stickyEntries) > stickyMaxEntries || len(stickyEntries) > 0 {
		for k, e := range stickyEntries {
			if now.Sub(e.lastUsed) > stickyEntryTTL {
				delete(stickyEntries, k)
			}
		}
	}
	if len(stickyEntries) > stickyMaxEntries {
		var oldestKey string
		var oldest time.Time
		for k, e := range stickyEntries {
			if oldestKey == "" || e.lastUsed.Before(oldest) {
				oldestKey, oldest = k, e.lastUsed
			}
		}
		delete(stickyEntries, oldestKey)
	}
	if e, ok := stickyEntries[key]; ok {
		if (domainSticky && e.baseIdx < 0) || (proxySticky && e.proxyIdx < 0) {
			// 维度配置变化导致旧条目不满足当前需求，重建。
			delete(stickyEntries, key)
		} else {
			e.lastUsed = now
			slog.Debug("sticky target reuse", "key", key, "baseIdx", e.baseIdx, "proxyIdx", e.proxyIdx)
			return e
		}
	}

	// 哈希 + 递增序号：同一 key 的连续绑定会落在不同目标。
	// base 与 proxy 使用不同哈希位独立分布，避免组合取模导致的偏斜。
	seq := atomic.AddUint32(&stickyRebindSeq, 1)
	h := fnv32a(key) + seq

	baseIdx := -1
	if domainSticky {
		if proxySticky {
			baseIdx = int((h / uint32(nProxy)) % uint32(nBase))
		} else {
			baseIdx = int(h % uint32(nBase))
		}
	}
	proxyIdx := -1
	if proxySticky {
		proxyIdx = int(h % uint32(nProxy))
	}

	entry := &stickyProxyEntry{baseIdx: baseIdx, proxyIdx: proxyIdx, lastUsed: now}
	if proxySticky {
		entry.client = buildProxyClient(proxies[proxyIdx])
	} else {
		entry.client = httpClient
	}
	stickyEntries[key] = entry
	slog.Debug("sticky target bind", "key", key, "baseIdx", baseIdx, "proxyIdx", proxyIdx)
	return entry
}

// invalidateUpstreamTarget 在连接失败/上游 429/5xx 时解除会话的 sticky 绑定，
// 让下一次请求重新分配 (base, 出口)，避免持续使用故障目标。
func invalidateUpstreamTarget(auth UpstreamAuth, bodyMap map[string]any) {
	key := stickyKeyForRequest(auth, bodyMap)
	stickyMu.Lock()
	delete(stickyEntries, key)
	stickyMu.Unlock()
}

func fnv32a(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func getSocks5Sticky() bool {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()
	return socks5Sticky
}

func getHTTPClient() *http.Client {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()

	if activeSocks5 == "" {
		return httpClient
	}

	var proxy Socks5Proxy
	var useRR bool

	if activeSocks5 == socks5RR {
		if len(socks5Proxies) == 0 {
			return httpClient
		}
		idx := atomic.AddUint32(&socks5RRIndex, 1) % uint32(len(socks5Proxies))
		proxy = socks5Proxies[idx]
		useRR = true
	} else {
		if socks5Client != nil && socks5ClientAddr == activeSocks5 {
			return socks5Client
		}

		var found bool
		for i := range socks5Proxies {
			if socks5Proxies[i].Addr == activeSocks5 {
				proxy = socks5Proxies[i]
				found = true
				break
			}
		}
		if !found {
			return httpClient
		}
	}

	client := buildProxyClient(proxy)

	if !useRR {
		socks5Client = client
		socks5ClientAddr = activeSocks5
	}
	return client
}

// getHTTPClientForTier 按认证层级选择 HTTP 客户端。
// 默认（socks5_paid_direct 未填或 false）：只要配置了 active_socks5，付费/带 key 与 public 都走代理。
// socks5_paid_direct=true 时恢复旧行为：付费层直连，仅免费层走代理。
func getHTTPClientForTier(tier TierType) *http.Client {
	if tier == TierPaid && getSocks5PaidDirect() {
		return httpClient
	}
	return getHTTPClient()
}

func getSocks5PaidDirect() bool {
	socks5Mu.RLock()
	defer socks5Mu.RUnlock()
	return socks5PaidDirect
}
