# API 兼容说明

服务默认监听 `http://127.0.0.1:8000`，所有接口都不要求客户端传入真实 OpenAI 或 Anthropic API key。

## 路由

| 路由 | 方法 | 说明 |
| --- | --- | --- |
| `/v1/models` | `GET` | 返回上游模型和本地别名模型 |
| `/v1/chat/completions` | `POST` | OpenAI Chat Completions 兼容入口 |
| `/v1/responses` | `POST` | OpenAI Responses 兼容入口 |
| `/v1/messages` | `POST` | Anthropic Messages 兼容入口 |
| `/health` | `GET` | 健康检查 |
| `/api/config` | `GET`/`POST` | 管理面板配置接口 |
| `/api/stats` | `GET`/`DELETE` | token 统计接口 |
| `/api/reload` | `POST` | 刷新 OpenCode 会话和模型列表 |

## Chat Completions

支持常见字段：

- `model`
- `messages`
- `stream`
- `temperature`
- `max_tokens`
- `top_p`
- `thinking`
- `reasoning_effort`
- `extra_body`
- `tools`
- `tool_choice`

`model` 会先经过 `model_alias` 解析。`reasoning_effort` 会按 `reasoning_effort_map` 转换。

## Responses API

支持从 `input`、`instructions` 或 `messages` 转换为 Chat Completions 形状。函数调用输出会尽量映射到 OpenAI tool message。

示例：

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "input": "Write one short sentence.",
    "stream": false
  }'
```

## Anthropic Messages

支持 `system`、文本消息、base64 image block、thinking block、tool_use 和 tool_result 的基础转换。

示例：

```bash
curl http://127.0.0.1:8000/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

## 流式响应

`stream: true` 时服务会使用 SSE 返回，并在内部清理空 delta、空 finish reason 和不需要的 reasoning 字段。Responses 和 Anthropic 流式接口会把上游 Chat Completions chunk 转换成对应事件。
