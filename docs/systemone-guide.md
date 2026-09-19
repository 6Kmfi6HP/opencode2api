# System One 调用指南 (`POST /v1/systemone`)

TypeSafe System One 模型 (`jev-1.13*`) 不做文本生成，吃 `state` + typed `questions` → 返回结构化 `answers`，
无法用 Chat / Responses / Messages 任一协议驱动。本网关为它开了一个专用透传入站端点。

行为定义见 `internal/app/systemone.go`，路由见 `server.go` (`/v1/systemone`)，
端点总览见 `docs/API.md`。

```
client ── POST /v1/systemone ──> opencode2api ── POST /zen/v1/systemone ──> zen
         {model, state, questions}   (除 model 外原样透传)               {answers, usage, cost}
```

注意：网关对外只暴露 `/v1/systemone`。直接请求 `/zen/v1/systemone` 会 `404`，
`GET /v1/systemone` 会 `405`（只收 POST），这都是预期的。

## 1. 请求格式

- `model` (string, 必填)：缺失或空 → 网关直接 `400 model is required`
  （上游对空 model 只回 `500 Model  is not supported`，且空 model 无法做别名/免费档判定）。
- `state` (`string | object | array`)：原样透传，网关不校验语义。
- `questions` (object)：`{ 名字: {type, instructions, ...} }`。
  question protocol 的类型细节由上游校验，网关不复制、不转换。
- Body 上限 `10 MiB`（对齐 chat 路径）。
- 免费层指纹重写（补 bash/glob/grep/read 工具 + 强制上游流式那套）只作用于
  `chat/completions` / `messages` / `responses` 三个上游子路径，**不作用于 `systemone`**。

## 2. 最小可用示例（已在 live 网关验证）

```bash
curl -s http://100.74.21.88:8001/v1/systemone \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "jev-1.13-free",
    "state": {"chat": "hello"},
    "questions": {
      "is_spam": {"type": "noul", "instructions": "spam?"}
    }
  }'
```

返回 (`200`)：

```json
{"model":"jev-1.13-free","answers":{"is_spam":{"type":"noul","noul":0.07}},"usage":{"input_tokens":275,"output_tokens":22},"cost":"0"}
```

- `answers.<名字>` 与提问名一一对应，内含 `type` 与评分/结论。
- `usage` 计费信息；免费模型 `cost` 为 `"0"`。

## 3. 一次问多题

```bash
curl -s http://100.74.21.88:8001/v1/systemone \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "jev-1.13-free",
    "state": {"transcript": "user: 退款怎么做？ agent: 把卡号发我"},
    "questions": {
      "is_spam":  {"type": "noul", "instructions": "Is this spam?"},
      "pii_leak": {"type": "noul", "instructions": "Does the agent request sensitive PII?"},
      "helpful":  {"type": "score", "instructions": "How helpful is the agent?"}
    }
  }'
```

## 4. TypeSafe 官方推荐玩法（摘要）

详见 https://docs.typesafe.ai/introduction 及其 Quickstart / Primitives / API Reference：

- System One 是常开的生产遥测层，区别于开发期才跑的 eval。
- Question 区分 measurement vs action：先定义“要测什么”，问题是可复用的量尺。
- State 是静态证据窗口：一次 run 开始时快照冻结，问题之间靠问答传递上下文。
- 好问题要 publish（命名 + 版本化），让线下 eval 和线上 monitor 复用同一套问题；
  monitor 是“把生产流量全扫一遍找问题”（map 语义），eval 是“固定数据集上对比版本”（group-by 语义）。
- 在本网关链路上，TypeSafe API key 由网关侧的上游 auth 接管，客户端打网关无需自带。

## 5. 错误速查

| 现象 | 含义 |
|---|---|
| `GET /v1/systemone` → `405` | 路由存在，只收 POST |
| `GET /zen/v1/systemone` → `404` | 正常，该路径只存在于网关 → 上游方向 |
| `400 model is required` | `model` 缺失/空，网关提前拦截 |
| `400 invalid JSON body` | body 不是合法 JSON |
| `500 Rate-limited Zen models require a workspace` | 付费 `jev-1.13` 被上游限流/要求 workspace，换 `-free` 模型或绑定 workspace |
| 其它非 2xx + JSON | 网关原样透传上游错误（pydantic `detail` 数组 / `type:error` 包裹），按上游协议解读 |

## 6. 回归测试

见 `internal/app/systemone_test.go`：

- 免费模型直通：断言打到 `/zen/v1/systemone`，`state`/`questions` 原样，
  `tools` / `stream` / `stream_options` 未泄漏进 body。
- 缺 `model` → `400`；非 JSON → `400`。
- 上游非 2xx → 原样回写状态码 + body。
- 付费 model 原样透传，不改写。
