# KeyPool 多 Key 管理 / 负载均衡 / Failover 设计（P0 冻结）

日期：2026-09-27。参考 gpt-load（Channel→Group→AccessKey、原子轮询、冷却拉黑），按本项目做减法：
单上游（opencode zen/go）、单二进制单依赖、无 DB，全部基于 `config.json` + 内存。

## 1. 配置契约（`config.json`，面板 `key_pool` 全字段对等可设）

```jsonc
"key_pool": {
  "enabled": false,
  "strategy": "sticky",        // 默认 sticky（缓存亲和）；round_robin | weighted 仍然可显式选配
  "max_retries": 2,            // 换 key 重试上限；总 attempt = 1+max_retries
  "retry_on": [429, 500, 502, 503, 504],
  "cooldown_secs": 60,         // 429/5xx/传输错误后的冷却窗口
  "blacklist_after": 3,        // 连续失败 N 次 → 长冷却 15min（内存黑名单，重启清空）
  "keys": [
    {"id": "k1", "key": "sk-...", "group": "", "weight": 1, "enabled": true, "note": "账号A"}
  ]
}
```

* `keys[].group`：`""`（默认）= zen/go 双表面可用；`"zen"`/`"go"` 限制表面。
  按 `auth.shouldUseGoEndpoint(modelID)` 决定本次 surface，只选 group 匹配的 key。
* `keys[]` 允许字符串简写：`"sk-..."` ≡ `{"key":"sk-..."}`（仿 `ModelAliasList` 的宽容解析）。
* 缺 `id` → `apply` 时归一化 `k1,k2…`；重复 id → 面板 POST 400，文件加载宽容去重。
* 零值 = 现状：`enabled:false` 或 keys 为空 → 客户端 `Bearer` 直通，public 保持 public。

## 2. 生效规则（可预测，文档化）

* `key_pool.enabled && 有可用候选 && auth.Mode != Public` → 上游 key 强制走池选择，
  客户端 token 仅保留于日志 `source`，不再作为上游 `Authorization`。
  `go:`/`zen:` 前缀仍控制 surface（即 group 过滤），token 部分被池 key 代替。
* `auth.Mode == Public` → 永远 public，不动用池内付费 key（防匿名蹭付费额度）。
* 池启用但本次 surface 无候选 → 回退客户端直通（不断服），记 `pool_fallback` 日志。

## 3. 选择器（`internal/app/keypool.go`，内存状态，仿 `socks5RRIndex` 原子模式）

* `keyRRIndex atomic.Uint64`：`round_robin` 取 `Add % len`；`weighted` 取 `Add % totalWeight` 走权重区间；
  `sticky` 取 `fnv32a(完整 egress sticky 键) % len`：`stickySessionBase(auth, bodyMap, headers, scope)`
  = `stickyKeyForRequest` 全键（`tok:<客户端token>|cli:<客户端会话哈希>` 等，与 egress
  完全同源），同一会话同时粘定同一池 key 与同一出口路径（prompt cache 亲和），
  不同会话按哈希散开（会话级负载均衡）。`attempt>0` 的池 failover 重试混入常量
  后缀 `|pool-retry`，跳离刚失败的 key（重试之间仍粘同一备选 key，保持亲和）。
* 候选过滤：`enabled && group匹配 && now > cooldownUntil`；全冷却 → 放行最早过期的那把（不断服）。
* 状态表（`keypoolMu` 守卫）：`{cooldownUntil, consecutiveFails}`；成功清零。

## 4. Failover（改 `opencode.go:582` 重试循环内两点，不建新子系统）

* 每 attempt：`attemptAuth, keyID := selectPoolKey(auth, modelID, bodyMap, headers, scope, attempt)`，
  以值拷贝传入 `selectUpstreamTarget` + `buildOCRequestWithSubpath` + `invalidateUpstreamTarget`
 （三处都收 `auth` 值类型，race-free；sticky egress 按池 key 绑定）。
* 记账（`ReportKeyResult`）：
  | 结果 | 动作 |
  |---|---|
  | 2xx | `consecutiveFails=0` |
  | 429/5xx/`retry_on`/传输错误 | `fails++`，冷却 `cooldown_secs`；`fails>=blacklist_after` → 15min |
  | 401 非账单 | 长冷却 15min（token 疑似失效） |
  | 账单错误（`isNonRetryableUpstreamError`=true：402/403/401+credits 类型） | 长冷却 15min **+ 换下一把 key 继续**（原来直接 break，现在池内 failover；池耗尽才返回） |
* attempt 上限：`1+max_retries`（默认 3，与现有 `maxUpstreamRetries=3` 对齐）。
* 流式：首字节写出后不再换 key，只记账（零拷贝透传不动）。

## 5. Per-key 统计（`stats.json` 复用异步 delta 落盘模式）

* `stats.TokenStatsData.Keys map[keyID]*KeyStats{requests, errors, last_status, last_error, last_used_unix}`；
  冷却/失败计数为纯内存视图，随 `GET /api/config` 的 `key_pool_status` 下发，不持久化。
* 面板 Keys Tab 展示：id/note/group/weight/启用/冷却剩余/requests/errors/last_status。

## 6. 管理面板（与配置文件 1:1）

* 新 Tab「API Keys」：策略表单（enabled/strategy/max_retries/retry_on/cooldown/blacklist_after）+
  单条添加行 + **批量添加 textarea**（每行 `key | 名称|key | 名称|key|group|weight`，`#` 注释，容错跳过并报告计数）+
  keys 表（启用开关/权重/分组/备注/删除）+ 状态表。
* `saveConfig()` 提交 `key_pool`；`loadConfig()` 回填；`Ctrl+S` 沿用。
* `GET /api/config` 返回池 key **明文**（与 `socks5_proxies` 密码同先例，管理员已鉴权）；
  日志/状态一律只出现 `id` 或掩码，永不打完整 key。
* POST 校验失败 → 400 且不落盘不生效：strategy 非法、空 key、重复 id、`weight<1`。

## 7. 明确不做（P5 条件触发：key>100 或多机共享）

DB/SQLite、密钥加密落盘（现阶段靠文件权限，见 §8）、下游 AccessKey 签发（本项目无下游凭证体系，
加签发 = 改信任模型）、主从同步、per-key 模型路由（已有 `protocol_rules`）。

## 8. 安全备注

* `saveConfig` 写权限从 `0644` 收紧为 `0600`（单运维者工具；key 明文落盘的最低补偿）。
* 运维备份 `config.json` 即备份全部 key；`config.example.json` 只放占位示例。


## 9. 管理员密码兼任 pool 触发 API key（v0.15.x 起）

- 客户端 `Authorization: Bearer <adminPassword>`（或 `x-api-key`）→ 由
  `extractUpstreamAuth` 常量时间比对命中，归为 `AuthRouteAdmin`，跳过 sk-
  前缀校验与池的 `Mode != Public` gate。`Authentication` 不再回传给上游。
- `adminPassword == ""` 时该路径恒不命中；launch 模式默认就是空。
- `go:` / `zen:` 前缀优先于密码判定（`<prefix>password` 仍按池接管且锁定那
  个 surface 的 group）。
- 池为空 / 全部冷却时按现状 `selectPoolKey` 返回 `pooled=false`，请求仍用
  原密码直接转发给上游 —— 上游会 401。这与"客户端 sk-..."直传的兜底一致。
- 安全约定：
  - 比较走 `crypto/subtle.ConstantTimeCompare`，拒绝时序侧信道。
  - `Source="admin"` 写日志，以便审计；密码本身永不落日志。
  - `config.json` 已收紧 0600 权限（key_pool 明文 + adminPassword 落盘）。
