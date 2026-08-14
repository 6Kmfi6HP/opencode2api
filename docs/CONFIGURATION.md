# 配置说明

默认配置文件是 `config.json`。首次运行可以从示例复制：

```bash
cp config.example.json config.json
```

## 字段

### `model_alias`

模型别名映射。键是客户端请求的模型名，值是实际传给上游的模型名。

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
