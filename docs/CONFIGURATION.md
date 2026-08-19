# 配置说明

默认配置文件是 `config.json`。首次运行可以从示例复制：

```bash
cp config.example.json config.json
```

## 字段

### `model_alias`

模型别名映射。键是客户端请求的模型名，值是实际传给上游的模型名。

显式使用 `go:` 或 `zen:` 认证时，如果客户端模型名本身存在于对应目录，且别名仅将它映射到同名的 `-free` 版本，则优先使用对应目录中的原模型。其他别名仍照常解析。

```json
{
  "model_alias": {
    "deepseek-v4-flash": "deepseek-v4-flash-free",
    "mimo-v2.5": "mimo-v2.5-free",
    "ling-3.0-flash": "ling-3.0-flash-free",
    "nemotron-3-ultra": "nemotron-3-ultra-free",
    "north-mini-code": "north-mini-code-free",
    "laguna-s-2.1": "laguna-s-2.1-free"
  }
}
```

### `reasoning_effort_map`

把客户端传入的 `reasoning_effort` 映射到上游可接受的值。

```json
{
  "reasoning_effort_map": {
    "minimal": "low",
    "medium": "medium",
    "high": "high"
  }
}
```

### `force_disable_thinking`

设为 `true` 时，服务会尽量禁用 thinking/reasoning，并从返回中移除 reasoning 内容。

### `max_tokens_cap`

全局默认 `max_tokens` 上限。客户端传入的 `max_tokens` 超过此值时，会被截断到此值。设为 `0` 或不填则不限制。

```json
{
  "max_tokens_cap": 131072
}
```

### `max_tokens_cap_per_model`

按模型覆盖全局上限。键是上游模型名，值是该模型的上限。值为 `0` 表示对该模型不限制。

```json
{
  "max_tokens_cap_per_model": {
    "deepseek-v4-flash-free": 131072,
    "laguna-s-2.1-free": 262144,
    "mimo-v2.5-free": 1048576
  }
}
```

上游对不同模型的 `max_tokens` 限制不同，实测值如下：

| 模型 | 限制类型 | 上限 |
|------|---------|------|
| `deepseek-v4-flash-free` | completion tokens | 131,072 |
| `laguna-s-2.1-free` | context length | 262,144 |
| `mimo-v2.5-free` | context length | 1,048,576 |
| `nemotron-3-ultra-free` | context length | 1,000,000 |
| `nemotron-3.5-lightning-free` | context length | 1,000,000 |

### `socks5_proxies`

SOCKS5 代理列表。

```json
{
  "socks5_proxies": [
    {
      "name": "local",
      "addr": "127.0.0.1:1080",
      "username": "",
      "password": ""
    }
  ]
}
```

### `active_socks5`

启用的代理。

- 空字符串：直连
- 某个 `addr`：固定使用该代理
- `__round_robin__`：在多个代理之间轮询

### `socks5_paid_direct`

控制**带 key / 付费**上游请求是否绕过 SOCKS5。

- 不填或 `false`（默认）：只要配置了 `active_socks5`，public 与带 key 请求都走代理
- `true`：带 key 请求直连；仅 public / 免费层走代理（旧行为）

```json
{
  "active_socks5": "127.0.0.1:1080",
  "socks5_paid_direct": false
}
```

### `socks5_sticky`

轮询模式（`active_socks5: "__round_robin__"`）下的会话粘性出口。

实测（Claude Code 真实会话）：上游免费层 prompt 缓存按出口 IP 隔离，随机轮换出口时相同请求两次都全 miss；固定出口时相同请求命中 99.8%。`socks5_sticky` 让同一会话（付费按账号 token，public 按 Claude metadata 的 session_id，缺省按公共兜底）固定走同一出口代理，缓存持续累积；不同会话之间仍然轮询分散。

- 缺省或 `true`：轮询时按会话固定出口（推荐）
- `false`：恢复纯轮询（每次请求随机换出口）

以下情况会自动切断当前会话的 sticky 绑定，重试/下次请求换到下一个出口：

- 传输层连接错误（代理不可达）
- 上游 HTTP 429（免费层按出口 IP 限流，换出口可绕过）与 5xx

```json
{
  "active_socks5": "__round_robin__",
  "socks5_sticky": true
}
```

### `prompt_cache_retention`

向上游 zen 网关显式声明 prompt 前缀缓存的保留时长。上游默认约 5 分钟（`in_memory`），agent 任务间歇过长时缓存容易过期导致命中率低。

- 不填或 `"24h"`：注入 `prompt_cache_retention: "24h"`，缓存保留一天
- `"in_memory"`：显式维持上游默认（约 5 分钟）
- `"off"`：完全不注入该字段

```json
{
  "prompt_cache_retention": "24h"
}
```

### `cache_control_breakpoints`

是否向上游请求附加 Anthropic 风格缓存断点 `cache_control: {"type":"ephemeral","ttl":"1h"}`。

- 缺省或 `true`：注入（对支持的上游提升缓存命中；GLM/Zhipu 模型会拒绝该字段，自动跳过）
- `false`：不注入

运行时观测：`stats.json` 中每个模型新增 `cache_read_tokens` / `cache_created_tokens` 聚合（来自上游 `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` 或 `cached_tokens` 等），可用 `cache_read / (cache_read + cache_created)` 评估命中率。

```json
{
  "cache_control_breakpoints": true
}
```

## 管理面板

打开 `http://127.0.0.1:8000/` 可进入管理面板。面板可以修改配置、刷新模型和查看 token 统计。

默认管理密码是 `123456`，生产部署必须修改：

```bash
./opencode2api -password "your-strong-password"
```

`GET/POST /api/config` 额外返回/接受运行时日志字段（不写入 `config.json`）：

- `log_level`：`debug` / `info` / `warn` / `error`
- `log_bodies`：是否在 Debug 下记录 body 形状摘要

## 日志与排障

默认写入 `opencode2api.log` 并由 lumberjack 按大小轮换；同时写 stdout。

关键字段：

| 事件 | 用途 |
|------|------|
| `request_plan` | 协议决策：模型、auth_mode、thinking、reasoning_effort、stream |
| `upstream_attempt` / `upstream_result` | 上游重试与回退链 |
| `stream_result` | 流式结果摘要；`empty_reply=true` 时为 Warn |
| `request_result` | 非流式结果摘要 |

密钥字段（`authorization` / `token` / `sk-…`）会被脱敏，永不落完整密钥。
