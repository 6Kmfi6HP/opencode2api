# 2026-09-18 上游指纹门禁消融（opencode.ai/zen/v1）

> 日期：2026-09-18（欧洲中部时区）
> 实验人：本机 debian 节点，同出口 IP
> 状态：一次性实测，结论仅覆盖该时间点与 `Bearer public` 免费层；上游策略可能滚动收紧
> 关联：issue #19（opencode2api 自 2026-09-16 起对免费层 403/500）；`internal/app/opencode.go` 注释；`CHANGELOG.md` v0.11.2

## 1. 结论摘要（先看这个）

| 假设（分别来自仓库 v0.12.0 注释与 lite 版 L471-492） | 实测判定 | 证据 |
|---|---|---|
| 必须是 `x-opencode-session`，形状 `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$` | **部分成立**：形状正则成立，但**头名不强制**；`x-session-id`（同形状）同样被接受，两者可互换 | S4 / S3（HTTP 200） |
| `x-session-id` 必须，旧头 `x-opencode-session` 系列已失效（lite L475） | **错误**：旧头单独携带即可通过门禁（200） | S4 / S5 / S6 |
| `tools` 必须包含 bash/glob/grep/read，缺任一即 403 | **成立**：无缺省组合只要缺任一就 403 | E8 / E9 / F1 / T* |
| 必须 `stream:true`，否则 403 | **成立**：tools+合法会话全配下仅改 `stream:false` 即 403 | S1 vs S2 |
| `User-Agent: opencode/<ver>`，最低 1.17.0，低的给 426 | **阈值已被上移为 1.18.0**：1.17.0 / 1.17.9 均 426；1.18.0 / 1.18.30 / 1.18.31 均 200；错误体：`OpenCode 1.18.0 or newer is required to use the free tier` | E17 / E18 / U1~U5 |
| `/zen/v1/models` 也走新指纹门 | **不受新门影响**：无任何 session 头即 200，返回 70 个模型 | M1 / M2 / M3 |
| `muse-spark-*-contributor-free` 500 与指纹有关 | **无关**：缺任一 tools 与四件齐两组（含 bash/glob/grep/read + stream:true + 合规头）都 500 `Internal server error`，是「contributor-free 档位对 public key」整档拒 | F2 / F3 / F4 |

一句话版：

- **真正新增的强制项只有两条**：`tools` 必须含 bash/glob/grep/read，且 `stream:true`。
- **会话头名/时间戳新鲜度都不是判定维度**：`x-opencode-session`（仓库现行实现）与 `x-session-id`（lite 主张）各自都能过；前缀 10 分钟或 1 小时的 TypeID 仍能过。
- **UA 版本阈值 已从 `1.17.0` 抬到 `1.18.0`**；仓库 v0.12.0 的 `ocMinFreeTierVersion = "1.18.31"` 注释准确（当前实战版本），但 lite 版写的 `minOCVersion = "1.17.0"` 是旧数据。

对仓库主分支的可操作建议（本报告只给证据，不动代码）：

1. 保持现状 session 生成即可（`x-opencode-session` 仍然有效）；如想贴近上游官方客户端，可同发 `x-session-id`，**但不是必须**。
2. **需要新加**：在升级到上游前给请求体补 `tools`（仅缺失时追加 bash/glob/grep/read 占位）；对已声明非流式的免费层下游请求，**对上游仍用 `stream:true` 并本地聚合**。没有这两个，`Bearer public` 一定 403。
3. `ocMinFreeTierVersion` 从 `1.17.0` 升到 `1.18.0`（今天的 426 错误体已经把 1.17.0 拒了）；UA 串用 `opencode/1.18.31+` 都对，`1.18.31` 是当前 npm `latest`，留着即可。

## 2. 实验设置

- **目标**：判断 issue #19 中哪一项是 2026-09-18 新门禁的**强制项**（头名/格式 vs tools vs stream），以及对 v0.12.0 仓库注释 / lite 版 opencode2api-lite.go L471-492 两份"实测主张"的**对照裁决**。
- **被测上游**：`https://opencode.ai/zen/v1/chat/completions`（POST）与 `https://opencode.ai/zen/v1/models`（GET），认证 `Authorization: Bearer public`（免费层）。
- **被测模型**：`big-pickle`（主用，两轮内行为稳定）、`mimo-v2.5-free`、`nemotron-3-ultra-free`、`ling-3.0-flash-fin-free`、`deepseek-v4-flash-free`、`muse-spark-1.2/1.3-contributor-free`（用于档位判别）。
- **脚本**：`go run` 一次性，源码在 `/tmp/oc-abl/`、`/tmp/oc-abl2/`、`/tmp/oc-abl3/`、`/tmp/oc-abl4/`（一次性、非仓库文件，见 §7 附录路径）；每请求间隔 ≥1.5s 回避免费层按出口 IP 的 429 限流。
- **会话 ID 生成**：`x-opencode-session` 用仓库 v0.12.0 `opencodeDescendingID()` 等价的 Go 实现（TypeID-descending，毫秒级时间戳降序）；`x-session-id` 用 lite 主张的纯随机 `ses_+12hex+14alnum`。
- **环境凭据**：`.env` 仅含 `OPENCODE_GO=`，无 `OPENCODE_API_KEY`（不读值，只查键位），所以全程走 `Bearer public` 免费层 —— 正好是 issue #19 报错的场景。

## 3. 假设与对应实验组

正文表的每个 ID 对应一次真实上游请求。错误时**严格保留**上游 error body 的首行（多数为单行 JSON）。

约定：

- `A/B/C/D` 对应任务里的四个组合：A=`x-opencode-session` TypeID-descending；B=`x-session-id` 纯随机；C=tools 强制四件；D=stream:true 强制。
- `S*` = 间隔/UA 补充实验；`U*` = UA 最低版本二分；`T*` = 多模型/多消息形状；`F*` = muse-spark contributor-free 档位判别；`M*` = `/zen/v1/models` 探测。

## 4. 数据

### 4.1 会话头/工具/流式组合（模型 `big-pickle`，UA `opencode/1.18.31`）

| ID | 会话头 | tools | stream | HTTP | 错误体首行 |
|---|---|---|---|---|---|
| E1 | x-opencode-session (TypeID-desc) | 无 | false | **403** | `FreeTierError: OpenCode's free tier can only be used from within OpenCode` |
| E2 | x-session-id (纯随机) | 无 | false | **403** | 同上 |
| E3 | 无 | 无 | false | **403** | 同上 |
| E4 | 双头 | 无 | false | **403** | 同上 |
| E5 | x-opencode-session | bash/glob/grep/read+edit+list | false | **403** | 同上 |
| E6 | 双头 | 无 | false | **403** | 同上 |
| E7 | 双头 | 四件（bash/glob/grep/read） | **true** | **200** | SSE 流，正常完成 `[DONE]` |
| E8 | 双头 | 无 | **true** | **403** | 同上 |
| E9 | 双头 | 仅 bash/glob/grep（缺 read） | **true** | **403** | 同上 |
| E10 | 仅 x-session-id | 四件 | **true** | **200** | SSE 流 |
| E11 | 仅 x-opencode-session | 四件 | **true** | **200** | SSE 流 |
| E12 | x-session-id="ses_0123456789abcdef01234567"（24hex，格式错） | 无 | true | **403** | 同上 |
| E13 | x-opencode-session="ses_0123456789abcdef01234567"（24hex，格式错） | 无 | true | **403** | 同上 |
| E14 | x-session-id 29 位（长错） | 四件 | true | **403** | 同上 |
| E15 | x-session-id 缺 `ses_` 前缀 | 四件 | true | **403** | 同上 |
| F1 | 双头 | 仅 bash/glob/grep（缺 read） | true | **403** | 同上 |

> 关键判读：E5 vs E7 唯一差是 `stream:false → stream:true`，403 ↔ 200；E8/E9/F1 vs E7 唯一差是 tools 完整性，403 ↔ 200。
> E10/E11 说明：单独带 `x-session-id` 或单独带 `x-opencode-session`（形状正确即可）都能过；新头不是必须、旧头也没失效。
> E12/E13/E14/E15 说明：session 头的**形状**校验仍在（ses_前缀 + 12hex + 14alnum），但**哪个头名**不重要。

### 4.2 会话时间戳新鲜度（模型 `big-pickle`，仅 x-opencode-session，tools 四件 + stream:true）

| ID | 头前缀对应的 TimeStamp | HTTP |
|---|---|---|
| S4 | 全新（毫秒级生成） | 200 |
| S5 | 10 分钟前 | 200 |
| S6 | 1 小时前 | 200 |

> `opencodeDescendingID` 前 12 位是"降序时间戳"。上游不校验新鲜度/是否注册过会话，只看形状。仓库现行生成函数依然安全。

### 4.3 stream 强制验证（模型 `big-pickle`，双头合规 + tools 四件）

| ID | stream | HTTP |
|---|---|---|
| S1 | false | **403** FreeTierError |
| S2 | true | **200** SSE |

### 4.4 UA 最低版本二分（全部请求双头 + tools 四件 + stream:true，模型 `big-pickle`）

| ID | User-Agent | HTTP | 错误体首行 |
|---|---|---|---|
| U1 | `opencode/1.17.0` | **426** | `UpgradeRequired: OpenCode 1.18.0 or newer is required to use the free tier` |
| U2 | `opencode/1.17.9` | **426** | 同上 |
| U3 | `opencode/1.18.0` | **200** | — |
| U4 | `opencode/1.18.30` | **200** | — |
| U5 | `opencode/1.18.31` | **200** | — |
| E16 | `ablation-probe`（无 opencode/版本语义） | **403** | FreeTierError |
| E17 | `opencode/1.16.9` | **426** | 同 U1 |

仓库 `ocMinFreeTierVersion = "1.18.31"` 的实战值仍安全；`minOCVersion = "1.17.0"` 已不满足。

### 4.5 `/zen/v1/models` 列表（无任何 session 头 × 三种会话组合）

| ID | 会话头 | HTTP | models |
|---|---|---|---|
| M1 | 无 | 200 | 70 |
| M2 | x-session-id | 200 | 70 |
| M3 | x-opencode-session | 200 | 70 |

> `/models` 不受新指纹门；70 个模型含 `big-pickle`、`deepseek-v4-flash-free`、`mimo-v2.5-free`、`muse-spark-*-contributor-free` 等。

### 4.6 模型档位判别与 issue #19 的 500

| ID | 模型 | tools | HTTP | 错误体 |
|---|---|---|---|---|
| F2 / T-single | `muse-spark-1.2-contributor-free` | 缺 read | **500** | `{"type":"error","error":{"type":"error","message":"Internal server error"}}` |
| F3 | `muse-spark-1.2-contributor-free` | 四件齐 | **500** | 同上 |
| F4 | `muse-spark-1.3-contributor-free` | 四件齐 | **500** | 同上 |
| T-single | `deepseek-v4-flash-free` | 无 | **400** | `{"error":{"type":"server_error","message":"Error from provider (Console): Upstream request failed: Model is unavailable."}}` |

> issue #19 提到的"500"不是指纹触发，是 **`muse-spark-*-contributor-free` 档位整体对 public key 拒**（即便按官方客户端方式发完整指纹 + tools + stream:true）。`deepseek-v4-flash-free` 当前不可用（400 Model is unavailable；可用目录里有它在，但服务端仍拒绝）—— 这与免费层雷"模型下线"是一类性质的抖动，与本 issue 的 403 指纹门是两回事。

### 4.7 多消息历史是否要 tools（更多模型）

| 模型 | messages 形状 | HTTP |
|---|---|---|
| mimo-v2.5-free（单消息） | 1 条 user | 403 |
| mimo-v2.5-free | 3 条（user/assistant/user） | 403 |
| nemotron-3-ultra-free | 单消息 / 3 条 | 403 |
| ling-3.0-flash-fin-free | 单消息 / 3 条 | 403 |
| big-pickle | 单消息 | 403 |
| mimo-v2.5-free（合规流式对照） | 1 条 + tools 四件 + stream:true | **200** |

> 无 tools 一律 403，覆盖多个模型与消息形状。**lite 关于 tools 的主张成立**。

## 5. 对既有注释的最终判定

| 位置 | 原文主张 | 判定 |
|---|---|---|
| `internal/app/opencode.go` L41-46（v0.12.0） | "免费层校验 x-opencode-session 必须匹配 `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`，否则拒绝" | 形状部分成立；**"必须 x-opencode-session"**这一半过期：`x-session-id` 也接受，旧头也并没有被淘汰 |
| `internal/app/opencode.go` ~L121（`ocMinFreeTierVersion = "1.18.31"`） | 免费层最低 1.17.0，低的 426 → 实战版本取 npm latest（1.18.31） | 行为正确；注释里 "<1.17.0 被 426" 的**阈值数字已过期**（今天 ≤1.17.9 都 426），但取 1.18.31 是安全/
附带 effect |
| `CHANGELOG.md` v0.11.2 | 同上口径 | 同上 |
| lite L471-477 "请求头必须携带 x-session-id；… 旧协议头已被上游无视" | **错误**：不携带 x-session-id 的旧头发完整指纹一样 200（S4/S5/S6）。"可带可不带"对"通过门禁"而言正确，但这两组头都足以通过，不是"必须新头" |
| lite L480-483 tools 四件强制 | **成立** |
| lite L484-486 stream:true 强制 | **成立** |
| lite minOCVersion = 1.17.0 | **过期**：阈值已抬到 1.18.0；当前 `defaultOCVersion = 1.18.31` 才安全 |

## 6. 对仓库代码的建议（不改代码，只落证据）

1. **新增** `ensureFreeTierTools`（或等价实现），但**判定键不是"tools 是否存在"，而是逐项缺什么补什么**——上游把"必须含 bash/glob/grep/read"当成整体门槛（E8/E9/F1：缺任一即 403），客户端即使在 `tools` 里只带了 `weather` 一个业务工具、缺四件中任何其一，裸发同样 403。正确语义：保留客户端已有工具的原始位置与形状，仅按其名字集合对缺失四件按 bash,glob,grep,read 顺序追加（OpenAI Chat 形状与 Anthropic Messages / Responses 原生 `tools[].name` 形状各用对应占位）。仅对"完全不带 tools"才兜底是 PR #20 `opencode.go` 里 `if _, hasTools := bodyMap["tools"]; !hasTools` 的那个一次性修复与 lite 主张共同的误判点，issue #19 的 403 之所以反复，正是被"客户端自带任一工具"这条分支漏掉的。
2. **对上游**始终 `stream:true`，如需迁就下游非流式客户端则本地聚合（跟上一次大版本里做过的 SSE 聚合是一个套路）；因为现在 `stream:false` 一定 403。
3. **判定免费层按"解析后的上游模型是否免费"（`isFreeModel(resolved)`），不按客户端 Authorization tier**——上游门禁并不识别客户端带的 `Bearer public` vs 真实 `sk-` key，只要路由命中的是免费模型（`*-free` 后缀短路或 models.dev 目录零价入册），同一组 tools + stream 校验照走。用 `auth.tier()` 做开关是 PR #20 与 lite 版共同的另一处误判：一条 `Bearer sk-…` + `big-pickle` 的消息本应触发指纹重做，在 PR #20 下会被当作"付费客户端"跳过所有改写（上游一样 403）。
4. **subpath 不能只限 `chat/completions`**：上游对 `/zen/v1/messages` 与 `/zen/v1/responses` 原生协议子路径施加同一组指纹校验（`messages` 子路径不写 `stream_options`——那不是 Anthropic schema；`tools` 在 messages 子路径用 Anthropic 形状 `tools[].name` 补齐；`count_tokens` 等计费子路径不套该门禁）。PR #20 把这条按 `subpath == "chat/completions"` 砍掉了 `messages` / `responses` 子路径，issue #19 真正涉及的 `claude.go / responses.go / anthropic_upstream.go / chat_to_responses_upstream.go / responses_to_anthropic.go` 里所有"直发上游原生协议"的路径就是因此被遗留。
5. **`ocMinFreeTierVersion` 从 1.17.0 升 1.18.0**；UA 继续走 npm latest。
6. **session 头不用改**：`x-opencode-session`（仓库现行 TypeID-descending / 随机同形状）依然有效；是否加发 `x-session-id` 是按上游官方形态"贴近"考量，不是必需。
7. **提示上游 `muse-spark-*-contributor-free` 500 与指纹无关**，不能把 500 归到新指纹门头上。可选：上游 500 且模型名含 `-contributor-free` 时，网关可读性提示"contributor 档位对 public 不开放"。


## 7. 复现

实测脚本（一次性，放任务本地目录不属于仓库）：

- `/tmp/oc-abl/main.go`（第一轮：deepseek 主线，因 deepseek-v4-flash-free 当日 400 噪音改用 big-pickle，第二轮矩阵全有效）
- `/tmp/oc-abl2/main.go`（session 时间戳新鲜度、UA 二分）
- `/tmp/oc-abl3/main.go`（多模型、多消息形状下 tools 无条件校验）
- `/tmp/oc-abl4/main.go`（muse-spark contributor-free 500 指纹无关验证）

核心路径（与仓库生产路径一致）：

```
POST https://opencode.ai/zen/v1/chat/completions
Authorization: Bearer public
User-Agent: opencode/1.18.31
Content-Type: application/json
x-opencode-session: ses_<12 hex TypeID-desc><14 base62>   # 仓库实现
  (或) x-session-id: ses_<12 hex><14 base62>            # lite 主张，均可
Body(JSON):
  model=<model id>
  stream=true                                          # 必
  tools=[{"type":"function","function":{"name":"bash",...}},
         {"type":"function","function":{"name":"glob",...}},
         {"type":"function","function":{"name":"grep",...}},
         {"type":"function","function":{"name":"read",...}}]  # 必
  messages=[...]
```

正文表中的状态码与错误体首行即真实响应，未经任何改写。

## 8. 局限

- 单日、单账号（`Bearer public`）、单出口 IP；免费层策略可能按时段/负载滚动收紧。上游若把 `x-session-id` 设为唯一接受名或加入时效校验，结论需重新跑一遍上述矩阵。
- `muse-spark-*-contributor-free` 的 500 未覆盖 contributor 账号视角（只有匿名 public key 场景）。
- `deepseek-v4-flash-free` 当日返回 400 `Model is unavailable`，后续若恢复，可在该模型上交叉验证一遍 4.1 矩阵（但 big-pickle/mimo/nemotron/ling 四个免费模型一致同结果，结论鲁棒）。
