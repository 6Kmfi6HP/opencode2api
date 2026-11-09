package app

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// ======================== 自控 HTTP/1.1 客户端（opencode CLI 档） ========================
//
// 真实 opencode CLI v2.1.4 抓包（docs/labs/2026-10-24-bun-fingerprint-alignment.md）：
// 它全程 HTTP/1.1（ALPN 只 offer http/1.1），请求头布局是
//
//	POST /inference/openai/v1/chat/completions HTTP/1.1
//	Authorization: Bearer public          ← 用户头按「字节序」排序，
//	Content-Type: application/json          大写开头的名字（A-Z = 0x41-0x5A）
//	User-Agent: opencode/latest/2.1.4/cli   排在小写名字（x-*、b3、traceparent）之前
//	b3: <trace>-<span>-1-<parent>
//	traceparent: 00-<trace>-<span>-01
//	x-opencode-client: cli
//	x-opencode-project: <40 hex>
//	x-opencode-session / -session-id / x-session-affinity / x-session-id: ses_…
//	Connection: keep-alive               ← 以下 5 行由 HTTP 客户端固定追加
//	Accept: */*
//	Host: opencode.ai
//	Accept-Encoding: gzip, deflate, br, zstd
//	Content-Length: N
//
// net/http 的 Transport 做不到这套顺序（Host 第一、User-Agent 第二、头按名字
// 排序后追加 Content-Length/Accept-Encoding），因此这里自带一个最小 h1 客户端：
// 请求字节完全由本文件拼装，响应读取复用 http.ReadResponse（含 chunked）。

// fp1AcceptEncoding 是真实 CLI 抓包里的固定值。上游对 br 会真的回 br，
// 因此本客户端自带 br/zstd/gzip/deflate 解压。
var fp1AcceptEncoding = "gzip, deflate, br, zstd"

// fp1HeaderCasing 是线上头名的大小写（Go 会规范化成 X-Opencode-Client，
// 真实客户端发的是全小写，必须按原样写回）。
var fp1HeaderCasing = map[string]string{
	"authorization": "Authorization",
	"content-type":  "Content-Type",
	"user-agent":    "User-Agent",
	"b3":            "b3",
	"traceparent":   "traceparent",
}

// fp1SkipHeaders 由本实现接管或 HTTP/1.1 不应重复发送的头。
var fp1SkipHeaders = map[string]bool{
	"host":              true,
	"connection":        true,
	"content-length":    true,
	"transfer-encoding": true,
	"accept":            true,
	"accept-encoding":   true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"upgrade":           true,
}

// fp1CanonicalAddr 把 URL host 补成 host:port（https 默认 443）。
func fp1CanonicalAddr(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	name := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return net.JoinHostPort(name, "443")
}

// fpTLSDial 建立一条 TLS 已就绪（ALPN 已按档位协商）的连接。
type fpTLSDial func(ctx context.Context, addr string) (net.Conn, error)

// fp1IdleTimeout 是空闲连接的存活上限：超龄条目在取出时直接关闭重建，
// 避免服务端已按 keep-alive 超时关掉连接、下一次请求却读到 EOF。
const fp1IdleTimeout = 90 * time.Second

type fp1Transport struct {
	dialTLS fpTLSDial
	// plain 只兜底 http:// 明文上游（正常配置都是 https）。该路径没有 TLS
	// 指纹可言，走标准库即可，头序也随之为 Go 的形态。
	plain *http.Transport

	mu   sync.Mutex
	idle map[string][]*fp1Conn
}

func newFP1Transport(dial fpDialFunc, dialTLS fpTLSDial) *fp1Transport {
	if dial == nil {
		dial = (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
	}
	return &fp1Transport{
		dialTLS: dialTLS,
		plain: &http.Transport{
			DialContext:           dial,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConns:          200,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
		},
		idle: map[string][]*fp1Conn{},
	}
}

func (t *fp1Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return t.plain.RoundTrip(req)
	}
	addr := fp1CanonicalAddr(req.URL.Host)
	// 请求体只读一次：重放时复用同一份字节，避免第二次发送空体。
	bodyBytes, err := fp1BodyBytes(req)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		conn, reused, err := t.getConn(req.Context(), addr)
		if err != nil {
			return nil, err
		}
		// 请求取消（http.Client.Timeout / ctx）时直接关掉连接，让阻塞在
		// 读响应头或读响应体上的调用立刻返回，而不是等到对端超时。
		stop := make(chan struct{})
		go func() {
			select {
			case <-req.Context().Done():
				conn.close()
			case <-stop:
			}
		}()

		resp, body, err := conn.roundTrip(req, bodyBytes)
		if err != nil {
			close(stop)
			conn.close()
			// 复用连接才可能是「服务端已关掉空闲 keep-alive」，且只对幂等方法
			// 重试一次：POST 交给上层的显式重试，避免重复提交。
			if attempt == 0 && reused && fp1RetryableStaleConn(req.Method, err) {
				continue
			}
			return nil, err
		}
		resp.Body = &fp1Body{conn: conn, t: t, dec: body, closable: resp.Close, req: req, stop: stop}
		return resp, nil
	}
}

// fp1RetryableStaleConn 报告连接级失败是否值得在新连接上重放一次。
func fp1RetryableStaleConn(method string, err error) bool {
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// getConn 从池尾取一条连接；超龄条目就地关闭并丢弃，直到拿到可用连接或池空。
// reused 表示返回的连接来自池（重试判定只对复用的连接放开）。
func (t *fp1Transport) getConn(ctx context.Context, addr string) (*fp1Conn, bool, error) {
	var expired []*fp1Conn
	t.mu.Lock()
	for len(t.idle[addr]) > 0 {
		list := t.idle[addr]
		c := list[len(list)-1]
		t.idle[addr] = list[:len(list)-1]
		if time.Since(c.usedAt) > fp1IdleTimeout {
			expired = append(expired, c)
			continue
		}
		t.mu.Unlock()
		for _, e := range expired {
			e.close()
		}
		return c, true, nil
	}
	t.mu.Unlock()
	for _, e := range expired {
		e.close()
	}

	nc, err := t.dialTLS(ctx, addr)
	if err != nil {
		return nil, false, err
	}
	return &fp1Conn{
		nc:   nc,
		br:   bufio.NewReaderSize(nc, 32*1024),
		bw:   bufio.NewWriterSize(nc, 32*1024),
		addr: addr,
	}, false, nil
}

func (t *fp1Transport) putConn(c *fp1Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.idle[c.addr]) >= 32 {
		go c.close()
		return
	}
	c.usedAt = time.Now()
	t.idle[c.addr] = append(t.idle[c.addr], c)
}

func (t *fp1Transport) CloseIdleConnections() {
	if t.plain != nil {
		t.plain.CloseIdleConnections()
	}
	t.mu.Lock()
	conns := t.idle
	t.idle = map[string][]*fp1Conn{}
	t.mu.Unlock()
	for _, list := range conns {
		for _, c := range list {
			c.close()
		}
	}
}

type fp1Conn struct {
	nc     net.Conn
	br     *bufio.Reader
	bw     *bufio.Writer
	addr   string
	usedAt time.Time
}

func (c *fp1Conn) close() {
	_ = c.nc.Close()
}

// roundTrip 拼装并写出请求，读回响应头。返回的 *http.Response.Body 由调用方接管。
// bodyBytes 由调用方一次性读出，重放时复用。
func (c *fp1Conn) roundTrip(req *http.Request, bodyBytes []byte) (*http.Response, io.ReadCloser, error) {
	if err := c.writeRequest(req, bodyBytes); err != nil {
		return nil, nil, err
	}

	resp, err := http.ReadResponse(c.br, req)
	if err != nil {
		return nil, nil, err
	}
	dec, err := fp1DecodeBody(resp)
	if err != nil {
		resp.Body.Close()
		return nil, nil, err
	}
	return resp, dec, nil
}

func fp1BodyBytes(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	if req.GetBody != nil && req.ContentLength == 0 {
		return nil, nil
	}
	b, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	return b, nil
}

func fp1EmitName(lower string) string {
	if c, ok := fp1HeaderCasing[lower]; ok {
		return c
	}
	return lower
}

// fp1HeaderLines 生成用户头行，按线上名字的字节序排序（与 CLI 抓包一致：
// 大写开头在前、小写在后），并补 CLI 必带的 b3 / traceparent。
func fp1HeaderLines(req *http.Request) []string {
	values := map[string][]string{}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if fp1SkipHeaders[lk] {
			continue
		}
		values[lk] = append(values[lk], v...)
	}
	if _, ok := values["b3"]; !ok {
		trace, span, parent := fp1NewTraceIDs()
		values["b3"] = []string{trace + "-" + span + "-1-" + parent}
		values["traceparent"] = []string{"00-" + trace + "-" + span + "-01"}
	}

	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		return fp1EmitName(names[i]) < fp1EmitName(names[j])
	})

	lines := make([]string, 0, len(names))
	for _, n := range names {
		emit := fp1EmitName(n)
		for _, v := range values[n] {
			lines = append(lines, emit+": "+strings.TrimSpace(v))
		}
	}
	return lines
}

// fp1NewTraceIDs 生成与 CLI 同宽的 W3C/B3 trace 标识：trace 16 字节、
// span/parent 各 8 字节（抓包值均为 32/16/16 个 hex 字符）。
func fp1NewTraceIDs() (trace, span, parent string) {
	return randomHex(32), randomHex(16), randomHex(16)
}

func (c *fp1Conn) writeRequest(req *http.Request, body []byte) error {
	var sb strings.Builder
	path := req.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	sb.WriteString(req.Method + " " + path + " HTTP/1.1\r\n")
	for _, line := range fp1HeaderLines(req) {
		sb.WriteString(line + "\r\n")
	}
	// 以下 5 行固定追加，顺序与真实客户端抓包一致。
	sb.WriteString("Connection: keep-alive\r\n")
	sb.WriteString("Accept: */*\r\n")
	sb.WriteString("Host: " + req.URL.Host + "\r\n")
	sb.WriteString("Accept-Encoding: " + fp1AcceptEncoding + "\r\n")
	if body != nil {
		sb.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	sb.WriteString("\r\n")

	// 调试钩子：只在设了环境变量时生效，线上路径不产生额外开销。
	if path := os.Getenv("OPENCODE2API_FP_DUMP"); path != "" {
		_ = os.WriteFile(path, append([]byte(sb.String()), body...), 0o600)
	}
	if os.Getenv("OPENCODE2API_FP_TRACE") != "" {
		log.Printf("[fp] %s", strings.ReplaceAll(strings.TrimSuffix(sb.String(), "\r\n\r\n"), "\r\n", " | "))
	}

	if _, err := c.bw.WriteString(sb.String()); err != nil {
		return err
	}
	if len(body) > 0 {
		if _, err := c.bw.Write(body); err != nil {
			return err
		}
	}
	return c.bw.Flush()
}

// fp1DecodeBody 按 content-encoding 解压响应体（上游在收到 br 时会回 br）。
func fp1DecodeBody(resp *http.Response) (io.ReadCloser, error) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return resp.Body, nil
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return &fp1WrapCloser{Reader: zr, closers: []io.Closer{zr, resp.Body}}, nil
	case "deflate":
		// 上游可能发 zlib 包装（RFC 1950）也可能发裸 deflate（RFC 1951）。
		// 看头两字节是否为合法的 zlib 头（CM=8 且 (CMF<<8|FLG)%31==0），
		// 是则用 zlib，否则交给 flate。只 Peek 不消费，两种 reader 都从
		// 同一个 bufio 读起。
		br := bufio.NewReaderSize(resp.Body, 2)
		if p, _ := br.Peek(2); len(p) == 2 && p[0]&0x0f == 8 && (uint16(p[0])<<8|uint16(p[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return nil, err
			}
			return &fp1WrapCloser{Reader: zr, closers: []io.Closer{resp.Body}}, nil
		}
		return &fp1WrapCloser{Reader: flate.NewReader(br), closers: []io.Closer{resp.Body}}, nil
	case "br":
		return &fp1WrapCloser{Reader: brotli.NewReader(resp.Body), closers: []io.Closer{resp.Body}}, nil
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		return &fp1WrapCloser{Reader: zr, closers: []io.Closer{zr.IOReadCloser(), resp.Body}}, nil
	default:
		return resp.Body, nil
	}
}

type fp1WrapCloser struct {
	io.Reader
	closers []io.Closer
}

func (w *fp1WrapCloser) Close() error {
	var err error
	for _, c := range w.closers {
		if e := c.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// fp1Body 把连接生命周期绑定到响应体：读完或 Close 后归还连接池；
// 请求被取消（http.Client.Timeout / ctx）时直接关连接，避免读挂住。
type fp1Body struct {
	conn     *fp1Conn
	t        *fp1Transport
	dec      io.ReadCloser
	closable bool
	req      *http.Request
	once     sync.Once
	eof      bool
	stop     chan struct{}
}

func (b *fp1Body) Read(p []byte) (int, error) {
	n, err := b.dec.Read(p)
	if err == io.EOF {
		b.eof = true
	}
	if err != nil && err != io.EOF {
		b.closable = true
	}
	return n, err
}

func (b *fp1Body) Close() error {
	b.once.Do(func() {
		if b.stop != nil {
			close(b.stop)
		}
		_ = b.dec.Close()
		if b.eof && !b.closable {
			b.t.putConn(b.conn)
			return
		}
		b.conn.close()
	})
	return nil
}
