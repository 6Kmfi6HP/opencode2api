# 并发高负载首字时间优化

## 问题

并发 100+ 请求时首字时间从正常 200-500ms 飙升至 5-30s。

### 根因

**P0 — HTTP Transport 配置缺失（主要瓶颈）**

`internal/app/httpclient.go` 原配置缺少三个关键超时：

- `DialContext` 未设置 → TCP 握手使用系统默认（最长 ~30s）
- `TLSHandshakeTimeout` 未设置 → TLS 握手无限制（默认 ~10s）
- `ResponseHeaderTimeout` 未设置 → 等待上游响应头无限制
- `MaxIdleConnsPerHost: 20` 过小 → 高并发下连接池耗尽，每个请求都新建连接

**P1 — 日志同步写入（次要瓶颈）**

`lumberjack.Logger` 直接同步写盘，每个请求触发 2 次 fsync，高并发下磁盘 I/O 争抢点。

---

## 已实施变更

### httpclient.go

```diff
 var httpClient = &http.Client{
     Timeout: upstreamTimeout(),
     Transport: &http.Transport{
-        MaxIdleConns:        100,
-        MaxIdleConnsPerHost: 20,
+        DialContext: (&net.Dialer{
+            Timeout:   5 * time.Second,
+            KeepAlive: 30 * time.Second,
+        }).DialContext,
+        TLSHandshakeTimeout:   5 * time.Second,
+        ResponseHeaderTimeout: 10 * time.Second,
+        MaxIdleConns:          500,
+        MaxIdleConnsPerHost:   100,
+        MaxConnsPerHost:       200,
         IdleConnTimeout:     90 * time.Second,
     },
 }
```

`buildProxyClient` 同步追加了 `TLSHandshakeTimeout`、`ResponseHeaderTimeout`、`MaxConnsPerHost`。

### logging.go

用 64KB `bufio.Writer` 包装 `lumberjack.Logger`，启动 100ms 定时 flush goroutine；`CloseRotator()` 优雅关闭时确保 flush。

---

## 预期效果

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| P50 首字时间 | 5-10s | 200-500ms |
| P99 首字时间 | 20-30s | 1-2s |
| 磁盘 I/O | 2000/s | 20/s |
| 单 host 连接池 | 20 | 100 |

---

## 测试

```bash
# E2E 测试（启动 mock 上游 + 网关 + 压测，自动验证）
./scripts/e2e-test.sh

# 仅压测 TTFB（需先手动启动服务）
./scripts/benchmark-ttfb.sh

# 一键编译+启动+压测（使用 config.test.json）
./scripts/run-test.sh
```

脚本参数见各文件头部注释；`scripts/mock-upstream.go` 支持 `-min-delay`、`-max-delay`、`-error-rate`、`-slow-rate` 等参数。

---

## 回滚

```bash
git checkout HEAD -- internal/app/httpclient.go internal/logging/logging.go
make build
```

---

## 后续优化方向

1. **DNS 缓存**：Go 默认无 DNS 缓存，可引入 `github.com/rs/dnscache`
2. **HTTP/2**：启用多路复用
3. **Prometheus metrics**：按阶段（DNS/TCP/TLS/TTFB）埋点
4. **连接预热**：启动时预建连接池
