# 2026-10-13 免费层指纹修复:session ID 反转时间戳门禁

## 现象

全部免费模型(mimo-v2.6-flash-free 等 9 个)对上游 `/zen/v1/chat/completions` / `/responses` / `/messages` 返回:

```
403 {"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode. Download it at https://opencode.ai and keep building."}}
```

`/zen/v1/models` 用 `Bearer public` 裸头也 403(2026-09-19 起该端点无需指纹头)。

## 上游变更考证(anomalyco/opencode)

- `83f2b6f67a`(2026-10-11,"make Zen routes a thin inference proxy"):zen handler 1314 行 → 62 行薄代理,旧门禁(error.ts / ipRateLimiter / stickyProviderTracker 等)全部删除;`b9ca4d21`(2026-10-12)删除 legacy database。免费层决策下沉到私有 inference 服务(`Resource.ConsoleMigration.inferenceUrl`),`FreeTierError` 字符串在公开仓库中不存在——新门禁不可审计。
- 真实客户端(dev HEAD,v2 线)每请求发 9 个头(`packages/core/src/session/model-request.ts`),关键差异:`x-opencode-session-id`(网关未发)、UA 三段式 `opencode/<channel>/<version>/<name>`(v2,`@opencode/cli` 2.0.27;npm `opencode-ai` 1.19.0 只是安装器 shim)。
- 事件 #52907(2026-10-03):官方 CLI 1.18.34 也全体 FreeTierError,官方结论 "upgrade to 2.0"。

## 消融矩阵(Bearer public + mimo-v2.6-flash-free + bash/glob/grep/read 四件工具 + stream:true)

| 探针 | 变量 | 结果 |
|---|---|---|
| A | 网关 2026-10-08 基线指纹(session 前 12 hex 纯随机) | 403 |
| B–F | 新三段式 UA / +x-opencode-session-id / project=global / 去 x-opencode-request / 旧式 UA+新头组合 | 均 403 |
| E/F/G | 无工具 / 全量 opencode 工具集 / 仅 bash | 均 403(tools 不参与) |
| I–L | 无 max_tokens / 最小 body / 去 stream_options / nanoid21 session | 均 403 |
| 会话预热 | 先 GET /models 再同 session POST | 403 |
| **J/K** | **session 前 12 hex = ~(ts_ms*0x1000+counter) 低 48 位(降序)** | **200** |
| L–O | 降序 session,ts=now / −1h / −24h / −7d | 均 200(时效容忍 ≥7 天) |
| **P** | **升序(非反转)session** | **403(必须反转)** |
| Q | msg_ 升序请求头 | 200(msg_ 不参与) |
| R/S | 旧式 UA + 降序 session / 40hex project + 降序 | 均 200(UA/project 不参与) |

## 结论

新门禁唯一新增校验:session ID 前 12 位 hex 必须是反转时间戳 `~(ts_ms*0x1000+counter) & 0xFFFFFFFFFFFF`(对齐上游 `packages/schema/src/identifier.ts`;session 降序、message 升序;时效容忍 ≥7 天,纯随机即拒)。2026-09-18 移除 descending 编码的前提(仅格式正则校验)已失效。

## 修复

- `internal/app/opencode.go` `newOCShapeID`:恢复反转时间戳编码(每毫秒计数器,ses_ 降序 / msg_ 升序)。
- `fetchModels`/`fetchGoModels`:补全量指纹头(裸头 403,全量头 200)——否则模型目录刷新整体失败(修复后 84 模型)。
- UA/project/x-opencode-request 头实测不参与门禁,保持现状。

## 验证

- 修复后直连上游:全部免费模型 chat/responses/messages 均 200。
- 真实网关(修复后二进制):`/v1/models` 返回 11 个免费模型;`/v1/chat/completions` tool_calls 回合(18× status=200,0 错误);`/v1/messages`、`/v1/responses` 均 200。
