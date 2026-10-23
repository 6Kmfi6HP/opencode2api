package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// upstreamTimeout 返回上游 http.Client 的墙钟超时。Go http.Client.Timeout
// 覆盖整个响应体流式读取——单出口代理下任一超上限的 agent 长输出会被恰好
// 在墙钟处掐断并显得像「正常结束」。env OPENCODE2API_UPSTREAM_TIMEOUT_SECS
// （默认 900）；<=0 关闭整体墙钟（交由流中断分支 + 客户端断开兜底）。
func upstreamTimeout() time.Duration {
	secs := envIntOr("OPENCODE2API_UPSTREAM_TIMEOUT_SECS", 900)
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// envIntOr 读取整型环境变量，缺失/非法时返回缺省值。
func envIntOr(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

var httpClient = &http.Client{
	Timeout: upstreamTimeout(),
	Transport: &http.Transport{
		// TCP 连接建立超时
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,  // TCP 三次握手超时
			KeepAlive: 30 * time.Second, // TCP keep-alive 探测间隔
		}).DialContext,
		// TLS 握手超时
		TLSHandshakeTimeout: 5 * time.Second,
		// 等待响应头超时（避免上游卡住）
		ResponseHeaderTimeout: 10 * time.Second,
		// 连接池优化：高并发下单 host 需要更多空闲连接
		MaxIdleConns:        500, // 全局空闲连接池（原 100）
		MaxIdleConnsPerHost: 100, // 单 host 空闲连接（原 20）
		MaxConnsPerHost:     200, // 单 host 最大连接数（新增）
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

// proxiesEqual 深度比较两个代理列表（含凭证），用于判断配置热重载时代理配置
// 是否真正发生变化，避免无关字段保存时误触发 sticky/连接池重置。
func proxiesEqual(a, b []Socks5Proxy) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Addr != b[i].Addr || a[i].Username != b[i].Username || a[i].Password != b[i].Password {
			return false
		}
	}
	return true
}

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
	lastUsed atomic.Int64 // unix nano；读路径可无锁更新
}

var (
	stickyMu      sync.RWMutex
	stickyEntries = map[string]*stickyProxyEntry{}
)

const (
	stickyEntryTTL       = 15 * time.Minute
	stickyMaxEntries     = 256
	stickyPublicFallback = "cli://public-shared" // 无会话标识的 public 请求共用同一出口

	headerClaudeSession = "X-Claude-Code-Session-Id"
	headerCodexSession1 = "Session-Id"
	headerCodexSession2 = "Session_id"
	headerCodexThread1  = "Thread-Id"
	headerCodexThread2  = "Thread_id"
)

// sessionHeaderValue returns the first non-empty session header value, matching
// header names case-insensitively.
func sessionHeaderValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := strings.TrimSpace(headers.Get(name)); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			for _, raw := range values {
				if value := strings.TrimSpace(raw); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

// hashSessionRouteKey is the routing-only view of a session identity. The
// downstream may send session IDs that embed account-ish data; local sticky
// keys and logs should never echo it verbatim.
func hashSessionRouteKey(session string) string {
	session = strings.TrimSpace(session)
	if session == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(session))
	return fmt.Sprintf("%x", sum[:8])
}

// stickyRebindSeq 每次重新绑定递增,参与哈希,保证同一会话在绑定被切断
// (上游 429/5xx/连接错误)后重新分配时换到不同出口,而不是永远钉死
// 在同一个确定性哈希结果上。
var stickyRebindSeq uint32

// stickyKeyForRequest 生成会话粘性键。优先级：账号 token > 客户端会话 route
// scope > 会话 user（Claude metadata 的 session_id 等） > 公共兜底。
func stickyKeyForRequest(auth UpstreamAuth, bodyMap map[string]any, upstreamHeaders http.Header, ocScope string) string {
	base := stickyPublicFallback
	if auth.Token != "" {
		base = "tok:" + auth.Token
	} else if u, ok := bodyMap["user"].(string); ok && u != "" {
		base = "usr:" + u
	}

	// 匿名（免费公共）请求优先以 inject 的 prompt_cache_key 作为 sticky 键：
	// 该 key 跨 launch 由内容派生时,命中缓存的稳定性显著优于每-launch 新会话
	// 头。OPENCODE2API_STICKY_BY_BODY=off 显式回退。付费层有上游自身的账号
	// 路由,不再覆盖。
	if stickyByBodyEnabled() && auth.Token == "" {
		if pck, ok := bodyMap["prompt_cache_key"].(string); ok && strings.TrimSpace(pck) != "" {
			return "pck:" + pck
		}
	}

	for _, raw := range []string{
		hashSessionRouteKey(sessionHeaderValue(upstreamHeaders, headerClaudeSession)),
		hashSessionRouteKey(sessionHeaderValue(upstreamHeaders, headerCodexThread1)),
		hashSessionRouteKey(sessionHeaderValue(upstreamHeaders, headerCodexThread2)),
		hashSessionRouteKey(sessionHeaderValue(upstreamHeaders, headerCodexSession1)),
		hashSessionRouteKey(sessionHeaderValue(upstreamHeaders, headerCodexSession2)),
	} {
		if raw != "" {
			return base + "|cli:" + raw
		}
	}

	if strings.TrimSpace(ocScope) != "" {
		return base + "|oc:" + strings.TrimSpace(ocScope)
	}
	return base
}

// stickyByBodyEnabled 决定是否按 prompt_cache_key 内容键做 sticky 出口。
// 默认开启（对无 token 的免费流量收益最大：内容 key 跨 launch 稳定 → 粘到
// 同一缓存出口）; OPENCODE2API_STICKY_BY_BODY=off 回退到原 session-头绑定。
func stickyByBodyEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("OPENCODE2API_STICKY_BY_BODY")))
	return v != "0" && v != "false" && v != "no" && v != "off"
}

// ======================== 代理 Client 单例池 ========================
//
// 之前每次请求（轮询模式）或每个会话（sticky 模式）都 buildProxyClient 新建
// http.Client + http.Transport，连接池配置形同虚设，且并发请求各自重建
// TCP+SOCKS5+TLS 握手。改为按 proxy.Addr 缓存单例 Client，全局复用同一连接池。
// 配置变更时由 resetProxyClientPool 清空并 CloseIdleConnections 旧实例。

var (
	proxyClientMu      sync.RWMutex
	proxyClientPool    = map[string]*http.Client{} // key: proxyClientKey(proxy)
	proxyClientBuilder = buildProxyClient
)

// proxyClientKey 生成代理单例池的键，包含 Addr 与凭证，避免同地址不同账号
// 的代理共用同一 Client 导致认证串号。
func proxyClientKey(proxy Socks5Proxy) string {
	return proxy.Addr + "\x00" + proxy.Username + "\x00" + proxy.Password
}

// isTemplateProxy 报告代理凭证中是否含有模板变量（目前仅 {{session-id}}）。
// 含模板的代理不进单例池，每次请求展开后独立构建 Client。
func isTemplateProxy(proxy Socks5Proxy) bool {
	return strings.Contains(proxy.Username, "{{session-id}}") ||
		strings.Contains(proxy.Password, "{{session-id}}")
}

// expandProxyCredentials 将代理凭证中的模板变量替换为实际值。
// ocScope 是 normalizedTransportScope(ocSessionID) 的哈希形式（16 hex chars），
// 用于唯一标识会话出口，长度固定且已脱敏，适合直接嵌入用户名。
func expandProxyCredentials(proxy Socks5Proxy, ocScope string) Socks5Proxy {
	if ocScope == "" || !isTemplateProxy(proxy) {
		return proxy
	}
	proxy.Username = strings.ReplaceAll(proxy.Username, "{{session-id}}", ocScope)
	proxy.Password = strings.ReplaceAll(proxy.Password, "{{session-id}}", ocScope)
	return proxy
}

// getOrBuildProxyClient 返回 proxy 对应的单例 HTTP 客户端，不存在则构建。
// 模板代理（含 {{session-id}}）不进单例池，由调用方负责传入已展开凭证的 proxy。
func getOrBuildProxyClient(proxy Socks5Proxy) *http.Client {
	k := proxyClientKey(proxy)
	proxyClientMu.RLock()
	if c, ok := proxyClientPool[k]; ok {
		proxyClientMu.RUnlock()
		return c
	}
	proxyClientMu.RUnlock()

	proxyClientMu.Lock()
	defer proxyClientMu.Unlock()
	// 双重检查
	if c, ok := proxyClientPool[k]; ok {
		return c
	}
	c := proxyClientBuilder(proxy)
	proxyClientPool[k] = c
	return c
}

// resetProxyClientPool 清空代理 Client 单例池并关闭旧实例的空闲连接。
// 在代理配置（socks5Proxies / activeSocks5）变更时调用，避免残留指向已删除
// 代理的连接池。sticky 条目由调用方另行清空。
func resetProxyClientPool() {
	proxyClientMu.Lock()
	defer proxyClientMu.Unlock()
	for addr, c := range proxyClientPool {
		if t, ok := c.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
		delete(proxyClientPool, addr)
	}
}

// buildProxyClient 为指定代理构建带 SOCKS5 dial 的 HTTP 客户端。
func buildProxyClient(proxy Socks5Proxy) *http.Client {
	return &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			DialContext: socks5Dial(proxy),
			// TLS 握手超时
			TLSHandshakeTimeout: 5 * time.Second,
			// 等待响应头超时
			ResponseHeaderTimeout: 10 * time.Second,
			// 连接池优化：高并发下单 host 需要更多空闲连接
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 100,
			MaxConnsPerHost:     200,
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
func selectUpstreamTarget(auth UpstreamAuth, bodyMap map[string]any, upstreamHeaders http.Header, ocScope string) (string, *http.Client) {
	paidDirect := auth.tier() == TierPaid && getSocks5PaidDirect()
	sticky := getSocks5Sticky()
	key := stickyKeyForRequest(auth, bodyMap, upstreamHeaders, ocScope)

	socks5Mu.RLock()
	rr := activeSocks5 == socks5RR
	proxies := socks5Proxies
	baseURLs := upstreamBaseURLs
	socks5Mu.RUnlock()

	// 快路径：单域名且代理维也不需要 sticky 绑定。跟随下游 OpenCode 会话
	// 时，即使只有一个上游域名也要保留 sticky entry：这样本地重试能基于
	// 会话 key 改绑，同时把 x-opencode-session 原样交给上游自行做后端亲和。
	// OPENCODE2API_STICKY_BY_BODY=1 时若 key 已被提升为 pck:，也强制启用域名
	// 维 sticky（pck 是内容键，跨 launch 稳定，理应锁定同一出口）。
	followOCSession := strings.TrimSpace(ocScope) != "" || strings.HasPrefix(key, "pck:")
	domainSticky := len(baseURLs) > 1 || followOCSession
	proxySticky := sticky && rr && len(proxies) > 0 && !paidDirect
	if !domainSticky && !proxySticky {
		if paidDirect {
			return baseURLs[0], httpClient
		}
		return baseURLs[0], getHTTPClient()
	}

	entry := lookupOrCreateSticky(key, domainSticky, proxySticky, len(baseURLs), len(proxies), proxies, ocScope)
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
	// 模板代理：client 字段为 nil，每次请求按 ocScope 展开凭证后新建（不池化）。
	if entry.client == nil && entry.proxyIdx >= 0 && entry.proxyIdx < len(proxies) {
		expanded := expandProxyCredentials(proxies[entry.proxyIdx], ocScope)
		return base, buildProxyClient(expanded)
	}
	return base, entry.client
}

// lookupOrCreateSticky 查找或创建会话的 (域名, 代理) 绑定。同一 key 的连续
// 绑定因 stickyRebindSeq 递增而轮换，使失效后的重绑真正换到不同目标。
// proxySticky=false 时只缓存域名维（baseIdx），client 字段不使用。
// 模板代理（含 {{session-id}}）不在 entry 中缓存 client，由调用方按 ocScope
// 展开凭证后直接构建，避免每会话独立 Transport 无限增长。
func lookupOrCreateSticky(key string, domainSticky, proxySticky bool, nBase, nProxy int, proxies []Socks5Proxy, _ string) *stickyProxyEntry {
	nowNano := time.Now().UnixNano()
	ttlNano := stickyEntryTTL.Nanoseconds()

	// 快路径：持读锁检查已有条目，命中时仅原子更新 lastUsed。
	// 若条目已过期则直接落入慢路径重建，不刷新 lastUsed。
	stickyMu.RLock()
	if e, ok := stickyEntries[key]; ok {
		needRebuild := (domainSticky && e.baseIdx < 0) || (proxySticky && e.proxyIdx < 0)
		expired := nowNano-e.lastUsed.Load() > ttlNano
		if !needRebuild && !expired {
			e.lastUsed.Store(nowNano)
			stickyMu.RUnlock()
			slog.Debug("sticky target reuse", "key", key, "baseIdx", e.baseIdx, "proxyIdx", e.proxyIdx)
			return e
		}
	}
	stickyMu.RUnlock()

	// 慢路径：写锁 — 创建或重建条目，并做懒清理。
	stickyMu.Lock()
	defer stickyMu.Unlock()

	// 双重检查：其他 goroutine 可能已在我们拿到写锁前完成了创建。
	if e, ok := stickyEntries[key]; ok {
		needRebuild := (domainSticky && e.baseIdx < 0) || (proxySticky && e.proxyIdx < 0)
		if !needRebuild {
			e.lastUsed.Store(nowNano)
			return e
		}
		delete(stickyEntries, key)
	}

	// 懒清理：清过期条目，超出上限时清最旧的。
	for k, e := range stickyEntries {
		if nowNano-e.lastUsed.Load() > ttlNano {
			delete(stickyEntries, k)
		}
	}
	if len(stickyEntries) > stickyMaxEntries {
		var oldestKey string
		var oldestNano int64 = 1<<63 - 1
		for k, e := range stickyEntries {
			if n := e.lastUsed.Load(); oldestKey == "" || n < oldestNano {
				oldestKey, oldestNano = k, n
			}
		}
		delete(stickyEntries, oldestKey)
	}

	// 哈希 + 递增序号：同一 key 的连续绑定会落在不同目标。
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

	entry := &stickyProxyEntry{baseIdx: baseIdx, proxyIdx: proxyIdx}
	entry.lastUsed.Store(nowNano)
	if proxySticky {
		p := proxies[proxyIdx]
		if isTemplateProxy(p) {
			// 模板代理：凭证含 {{session-id}}，每次请求须按 ocScope 展开，
			// 不同会话凭证不同，不能共用同一 Transport。entry.client 留 nil，
			// 由 selectUpstreamTarget 调用方负责按 ocScope 展开后直接构建。
			entry.client = nil
		} else {
			entry.client = getOrBuildProxyClient(p)
		}
	} else {
		entry.client = httpClient
	}
	stickyEntries[key] = entry
	slog.Debug("sticky target bind", "key", key, "baseIdx", baseIdx, "proxyIdx", proxyIdx, "template", proxyIdx >= 0 && isTemplateProxy(proxies[proxyIdx]))
	return entry
}

// invalidateUpstreamTarget 在连接失败/上游 429/5xx 时解除会话的 sticky 绑定，
// 让下一次请求重新分配 (base, 出口)，避免持续使用故障目标。
func invalidateUpstreamTarget(auth UpstreamAuth, bodyMap map[string]any, upstreamHeaders http.Header, ocSession string) {
	key := stickyKeyForRequest(auth, bodyMap, upstreamHeaders, normalizedTransportScope(ocSession))
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

	if activeSocks5 == socks5RR {
		if len(socks5Proxies) == 0 {
			return httpClient
		}
		idx := atomic.AddUint32(&socks5RRIndex, 1) % uint32(len(socks5Proxies))
		// 复用单例池：轮询只选代理，不重新建连接池。
		return getOrBuildProxyClient(socks5Proxies[idx])
	}

	// 单固定代理：优先查单例池；池中无则按 Addr 查找并缓存。
	if socks5Client != nil && socks5ClientAddr == activeSocks5 {
		return socks5Client
	}
	for i := range socks5Proxies {
		if socks5Proxies[i].Addr == activeSocks5 {
			c := getOrBuildProxyClient(socks5Proxies[i])
			socks5Client = c
			socks5ClientAddr = activeSocks5
			return c
		}
	}
	return httpClient
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
