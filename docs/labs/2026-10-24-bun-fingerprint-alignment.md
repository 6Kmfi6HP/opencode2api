# 2026-10-24 按真实 opencode CLI 抓包对齐 TLS / HTTP 指纹

## 结论

直接抓真实 opencode CLI v2.1.4 打到官方 base URL（`https://opencode.ai`，免费额度可用；
全程不用自定义 baseurl）的流量，得到两个事实：

1. **TLS**：CLI 的 ClientHello 是 BoringSSL 默认档 —— 17 个 cipher、13 个扩展、32 字节
   legacy session id、**ALPN 只有 `http/1.1`**，1502 字节（含 5 字节记录头），
   JA3 md5 `1523504b38f0fae0d881d4b6554aac1b`。
2. **HTTP**：CLI **全程 HTTP/1.1**，不协商 h2。请求头布局是「请求行 + 用户头按字节序 +
   固定尾块」，带 `b3` / `traceparent`。

因此前一版实现（`feat(httpclient)` 引入的 uTLS + 自研 h2）方向不对：

- `chrome` 档是 Chrome 133 hello（GREASE、ALPS、无 MLKEM→更长的 key_share、17 扩展、
  ALPN `h2,http/1.1`），与 CLI 毫无共同点；
- `bun` 档当时按「Bun `node:http2`」建模（10 扩展、单 x25519 key_share、ALPN `h2,http/1.1`、
  246B），同样不对 —— 真实 CLI 根本没走 h2；
- HTTP 层自研 h2 客户端虽然去掉了 Go 特征，但**协议版本本身**就是与真实客户端不一致的特征。

现在的 `tls_fingerprint=bun` 档（保留旧档名以兼容配置）改为：

- **TLS**：`utls.HelloCustom` + 逐字节复刻 CLI 抓包的 ClientHello（`internal/app/fp_bun_hello.go`），
  与真实抓包归一化后**逐字节相等**；
- **HTTP/1.1**：项目自写的 h1 客户端（`internal/app/fp_h1.go`），请求字节布局与 CLI 抓包
  **逐行相等**（头名大小写、头序、固定尾块、trace 头格式）；
- 原 `fp_h2.go` / `fp_h2_test.go` 已删除：真实客户端不发 h2，留着只会制造不一致特征。

默认档位仍是 `chrome`（历史行为），`bun` 为显式开关，`tls_fingerprint=off` 完全关闭 →
回滚只改一个配置项。

## 抓包方法

macOS arm64，两个本地代理：

1. **透明 CONNECT 代理**（`127.0.0.1:18099`，纯 TCP 转发，只记录客户端首段字节）——
   拿到未经改写的 ClientHello，用于 TLS 对比；
2. **MITM 代理**（自签 CA，`NODE_EXTRA_CA_CERTS` 注入信任）——TLS 终止后记录明文
   HTTP/1.1 请求字节，用于请求头对比。

被比较的客户端：

| 客户端 | 说明 |
| --- | --- |
| opencode CLI v2.1.4 | Bun 编译产物，`--standalone` 运行，代理只用于抓包 |
| 项目 `chrome` 档 | uTLS `HelloChrome_Auto` + `x/net/http2` |
| 项目 `off` 档 | 标准 `crypto/tls` + `net/http` |
| 项目 `bun` 档 | 本次实现（`HelloCustom` + 自写 h1） |

CLI 侧的坑：它会把内部请求发到自己起的本地 HTTP 服务，走代理会得到 `405` ——
必须 `NO_PROXY=127.0.0.1,localhost` 且用 `--standalone`，只有真正出网的请求才落进抓包。

## TLS ClientHello 对比（同机实测）

| 客户端 | 长度 | cipher | 扩展 | ALPN | JA3 md5 |
| --- | --- | --- | --- | --- | --- |
| **CLI v2.1.4** | **1502B** | **17（BoringSSL 顺序）** | **13（含 MLKEM/OCSP/SCT）** | **`http/1.1`** | **`1523504b38f0fae0d881d4b6554aac1b`** |
| 项目 `bun`（新） | 1502B | 17，逐项一致 | 13，顺序与长度一致 | `http/1.1` | `1523504b38f0fae0d881d4b6554aac1b` |
| 项目 `bun`（旧） | ≈250B | 17 | 10（单 x25519） | `h2,http/1.1` | `a676460b15977bc4b2e32dfbba906642` |
| 项目 `chrome` | 1797B | 15（含 GREASE） | 17（GREASE×2、ALPS、MLKEM） | `h2,http/1.1` | `db378ed19eca382a6cfe98c364106c07` |
| 项目 `off` | 1483B | 13（Go 顺序） | 10 | 无 | `725543c78edf669194c11dc7a039b56e` |

CLI hello 的结构（`models.opencode.ai` 与 `opencode.ai` 两次抓包一致，仅 SNI 长度不同）：

```
record: type=22(TLS 1.0 兼容版本号), len=1497
handshake: ClientHello, ver=0303, random(32), session_id_len=32
cipher_suites(17): 1301 1302 1303 c02b c02f c02c c030 cca9 cca8 c009 c013 c00a c014 009c 009d 002f 0035
compression: 00
extensions(13):
  server_name(0,16)  extended_master_secret(23,0)  renegotiation_info(65281,1)
  supported_groups(10,10)[x25519_mlkem768(0x11ec), x25519, p256, p384]
  ec_point_formats(11,2)[0]  session_ticket(35,0)  alpn(16,11)[http/1.1]
  status_request(5,5)  signature_algorithms(13,24)  signed_certificate_timestamp(18,0)
  key_share(51,1258)[mlkem768(1216), x25519(32)]  psk_key_exchange_modes(45,2)  supported_versions(43,5)
```

与我们实现的差分：**只剩 key_share 里的公钥随机字节**（每次连接必变），长度、顺序、取值全等。
BoringSSL 的 512 字节 padding 规则（未填充长度落在 `(0xff,0x200)` 时补到 512）由
`utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle}` 覆盖；真实 CLI 这几次
hello 都超过 512 字节，因此抓包里没有 `padding(21)` 扩展，我们的实现同样不加。

## HTTP/1.1 请求对比

CLI（`POST /inference/openai/v1/chat/completions`，免费额度）：

```
POST /inference/openai/v1/chat/completions HTTP/1.1
Authorization: Bearer public
Content-Type: application/json
User-Agent: opencode/latest/2.1.4/cli
b3: <32hex>-<16hex>-1-<16hex>
traceparent: 00-<32hex>-<16hex>-01
x-opencode-client: cli
x-opencode-project: <40hex>
x-opencode-session: ses_…
x-opencode-session-id: ses_…
x-session-affinity: ses_…
x-session-id: ses_…
Connection: keep-alive
Accept: */*
Host: opencode.ai
Accept-Encoding: gzip, deflate, br, zstd
Content-Length: N
```

本轮修的两处差异：

1. **`b3` / `traceparent` 宽度**：原先我们发 16/8/8 位 hex，CLI 是 32/16/16；已改为
   `b3: <32>-<16>-1-<16>`、`traceparent: 00-<32>-<16>-01`。
2. **`Host` / `Accept` / `Accept-Encoding` / `Content-Length` 的位置与取值**：`net/http`
   会把 `Host` 放第一行之后、头名规范化成 `X-Opencode-*`、头序随机 —— 三者都是明显特征。
   现在由 `fp_h1.go` 自己拼字节：用户头按**字节序**排序（`Authorization`/`Content-Type`/
   `User-Agent` 这类大写开头排在 `b3`/`traceparent`/`x-*` 前），再固定追加尾块五连。

用 MITM 抓我们改造后的同一条请求，与 CLI 抓包逐行 diff：**头名顺序完全一致**，
`b3`/`traceparent` 格式一致，body 仅差项目有意注入的缓存字段（`prompt_cache_key` /
`prompt_cache_retention` / `cache_control` 断点）与不同 prompt 内容。

## 实现

| 文件 | 职责 |
| --- | --- |
| `internal/app/fp_bun_hello.go` | `bunClientHelloSpec()`：17 cipher、13 扩展固定顺序、MLKEM key_share、ALPN `http/1.1`、`BoringPaddingStyle` |
| `internal/app/fp_h1.go` | 自写 HTTP/1.1 客户端：请求行 + 字节序用户头 + 固定尾块；`b3`/`traceparent` 生成；按 host 的 keep-alive 空闲池（90s）；ctx 取消即关连接；gzip/deflate/br/zstd 响应解压；EOF 归还连接、异常即丢弃 |
| `internal/app/fp_bun_transport.go` | 组装：`fpDialFunc` → `bunTLSHandshake`（`HelloCustom`，IP 直连时 `RemoveSNIExtension`）→ `newFP1Transport` |
| `internal/app/httpclient.go` | `tls_fingerprint=bun\|boringssl\|node` 路由到上面这套；其余档位行为不变 |
| ~~`internal/app/fp_h2.go`~~ | 已删除：真实客户端不发 h2 |

`fp1SkipHeaders` 挡掉 `host`/`connection`/`content-length`/`accept`/`accept-encoding`/`keep-alive`
等由本实现接管的头；`fp1HeaderCasing` 负责把 Go 规范化后的头名还原成线上大小写
（`b3`、`traceparent` 本身是全小写）。

## 回归与复测

```bash
go test ./internal/app -run 'TestBunClientHelloSpec|TestFP1Transport'
go test ./...            # 全量
```

- `TestBunClientHelloSpec_MatchesCapture`：与 CLI 1502 字节抓包归一化后逐字节相等
- `TestBunClientHelloSpec_SNI`：带域名时 `server_name(0)` 在首位且值为域名
- `TestFP1Transport_WireFormat`：请求行、头序（含大小写）、固定尾块、`b3`/`traceparent`
  宽度、两次请求头序稳定且复用同一连接
- `TestFP1Transport_{BasicRequest,StreamingSSE,LargeBodyBothWays,ContextCancel,BrotliDecode}`：功能面

线上实测：

- `bun` 档直连 `https://opencode.ai/inference/v1/models` → `200`，`HTTP/1.1`；
- 免费额度 chat/completions：改造前与 CLI 同一出口都拿到 `429 FreeUsageLimitError`
  （额度耗尽），说明请求形态已被上游正常受理，不是指纹层面被拦；
- 经透明代理抓我们**自己**发出的 hello：1497+5 字节、13 扩展、JA3 `1523504b…`，与 CLI 全等，
  仅 key_share 公钥随机字节不同。

## 已知差异 / 未做

- **无会话恢复**：不带 PSK / 0-RTT / session ticket 复用，每次全握手（真实 CLI 多次连接
  也未见 `pre_shared_key`，此处等价）。
- **无 h2**：真实客户端也不发 h2；若上游将来强制 h2，本档需要重做（`chrome` 档仍走 h2）。
- **HTTP/1.1 连接复用策略**：按 host 单池、空闲 90s，未做 pipeline；CLI 侧同样串行复用。
- **body 字段差异是有意的**：项目会注入缓存相关字段（`prompt_cache_key` 等），属于功能而非指纹。

## 回滚

```json
{ "tls_fingerprint": "chrome" }
```

或环境变量 `OPENCODE2API_TLS_FINGERPRINT=off`（完全退回标准库）。切换即时生效，无需重启。
