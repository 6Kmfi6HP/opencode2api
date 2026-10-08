# Claude Code 缓存测试法（网关强制指向）

> 任何"缓存命中率 / `prompt_cache_key` / instructions 变化"类测试，
> 一律按此法执行。合成 curl 只用于冒烟，结论必须来自真机 + 网关日志。

## 1. 铁律（违反则结论作废）

1. 模型必须用网关现存模型（如 `muse-spark-1.3-contributor`，以 `/v1/models` 为准）。
   网关没有的模型会触发 `[claude-code:unrecognized_model]`，流量可能旁路网关。
2. 必须用 `--settings <gw-settings.json>` 强制指向网关。
   `~/.claude/settings.json` 自带 `ANTHROPIC_BASE_URL` 与 `ANTHROPIC_AUTH_TOKEN`，
   进程 env 的优先级不稳定，裸 `export` 经常被覆盖，导致流量根本没进网关
   （现象：回了 `TURN_ONE_OK` 但网关 `cache_debug_key_parts` 计数不涨）。
   不要手配 `ANTHROPIC_AUTH_TOKEN=sk-123` 类 env（`launch_env.go` 已证明它会劫持
   Claude Code 网络栈）。一切以 `--settings` 文件为准。
   ⚠️ 2026-10-08 实测修正：CLI 2.1.293 下 `--settings` 的 env 会被全局
   `~/.claude/settings.json` 盖住（网关零流量）。此时改用 env 直驱：
   `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` + `ANTHROPIC_API_KEY` 三件套
   与 `--model` 显式指定，实测有效（计数增长可验证）。
   多轮 agents 测试还必须全程相同 `--allowedTools`，否则 tools 变化换缓存分片。
3. 两轮必须同会话：首轮 `--session-id $UUID`，次轮 `--resume $UUID`。
4. 网关必须带 `OPENCODE2API_CACHE_DEBUG=1` 启动，且是最新 `bin/opencode2api`
   （`make build` 后重启才生效）。

## 2. 标准步骤

```bash
# 0. 起网关（新二进制 + debug）
OPENCODE2API_CACHE_DEBUG=1 ./bin/opencode2api -port 8080 -log-stdout

# 1. 写强制网关的 settings（模型换成网关现存的）
python3 -c "
import json
m='muse-spark-1.3-contributor'
env={
 'ANTHROPIC_BASE_URL':'http://127.0.0.1:8080',
 'ANTHROPIC_AUTH_TOKEN':'sk-123',
 'ANTHROPIC_MODEL':m,
 'ANTHROPIC_DEFAULT_OPUS_MODEL':m,   'ANTHROPIC_DEFAULT_OPUS_MODEL_NAME':m,
 'ANTHROPIC_DEFAULT_SONNET_MODEL':m, 'ANTHROPIC_DEFAULT_SONNET_MODEL_NAME':m,
 'ANTHROPIC_DEFAULT_HAIKU_MODEL':m,  'ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME':m,
 'ANTHROPIC_DEFAULT_FABLE_MODEL':m,  'ANTHROPIC_DEFAULT_FABLE_MODEL_NAME':m,
 'CLAUDE_CODE_SUBAGENT_MODEL':m,
}
json.dump({'env':env}, open('/tmp/cc-cache-test/gw-settings.json','w'), indent=2)
"

# 2. 第一轮（记 BASE 计数）
UUID=$(uuidgen | tr 'A-Z' 'a-z'); echo "$UUID" > /tmp/cc-cache-test/session.id
echo "BASE=$(grep -a -c cache_debug_key_parts opencode2api.log)"
cd /tmp/cc-cache-test && timeout 280 claude -p --session-id "$UUID" \
  --settings /tmp/cc-cache-test/gw-settings.json \
  "Reply with exactly: TURN_ONE_OK and nothing else. Do not use any tools." \
  --max-turns 3 2>&1 | tail -4

# 3. 有效性检查：计数必须涨，否则流量没进网关，本轮作废
grep -a -c "cache_debug_key_parts" opencode2api.log  # 必须 > BASE

# 4. 同会话第二轮
UUID=$(cat /tmp/cc-cache-test/session.id)
cd /tmp/cc-cache-test && timeout 280 claude -p --resume "$UUID" \
  --settings /tmp/cc-cache-test/gw-settings.json \
  "Reply with exactly: TURN_TWO_OK and nothing else. Do not use any tools." \
  --max-turns 3 2>&1 | tail -4
```

## 3. 标准读数

```bash
# 跨轮 pck / 全长 / 入 key 长
grep -a "cache_debug_key_parts" opencode2api.log | tail -n 6 | python3 -c "
import sys,re
for line in sys.stdin:
    ts=line[:30]
    pck=re.search(r'pck=(\S+)',line).group(1)[-12:]
    fl=re.search(r'instr_full_len=(\d+)',line)
    fs=re.search(r'instr_full_segs=(\d+)',line)
    pl=re.search(r'part0_len=(\d+)',line)
    print(ts,'pck=..'+pck,'part0_len=',pl.group(1) if pl else '?','full_len=',fl.group(1) if fl else '?','full_segs=',fs.group(1) if fs else '?')
"
# 命中
grep -a "cache_debug_usage" opencode2api.log | tail -n 4 | grep -oE "input_tokens=[0-9]+ output_tokens=[0-9]+.*cached_tokens=[0-9]+"
# 池 key 是否钉住
grep -a "upstream_attempt" opencode2api.log | tail -n 4 | grep -oE "key_id=k[0-9]+"
```

判定：

- `part0_len` 同 + `pck` 同 + 池 key 同 + 轮 2 `cached>0` = 修好。
- `instr_full_len` 涨但 `part0_len` 不涨 = 稳定头截断生效。
- 计数不涨 = 流量没进网关，重查模型名 / settings / `--resume`。

## 4. 背景：2026-10-08 缓存命中率修复

- 现象：同会话 `part0_len 31397→36694`，`first_user`/tools 不变，两轮 `cached=0`。
- 定位：`instructions` = system 参数 + `role=system` 消息 join，
  Claude Code 每轮尾部追加约 18 段（`59→77` 段，`+6KB`），全量哈希导致每轮换 key。
- 修复：`contentPromptCacheKey` 只取 `instructions` 前 16 段
 （`stableInstructionsHead`，见 `internal/app/chat.go`）入 key；
  `config.json` 本地 `key_pool.strategy round_robin→sticky`、
  `prompt_cache_retention off→24h`。
- 验证（本测试法）：尾部 `+5KB` 时 `pck` 不变、池 key 钉住、
  轮 2 `7025/16984≈41%` 命中。回归测试
  `TestContentPromptCacheKey_StableHeadIgnoresAppendedTail`。

## 5. 背景：2026-10-08 稳态 80%+ 修复

- 现象：agents 多轮稳态上限 ~22%——`instructions` 每轮尾部追加让上游前缀
  缓存从增长点截断（cached 恒 ≈ instructions tokens，tools 之后全 miss）；
  tools 全量哈希跨轮漂移换缓存分片。
- 修复（`internal/app/instructions_stable.go`）：
  ① `clipInstructionsToStablePrefix` 按会话注册首轮 instructions 原文，
  后续请求钉住首轮前缀、尾部追加段挪到 input 末条 user 消息（前缀缓存
  只看公共前缀，尾部易变不影响头部命中）；
  ② pck tools 分支改哈希工具名集合（`toolsNamesKey`），schema 漂移不换分片。
- 验证（本测试法，muse-spark-1.3-contributor-free，4 轮复杂 agents 任务、
  同会话 resume、全程相同 `--allowedTools`）：总命中率 **87.4%**，
  稳态单请求 **91-99%**，`instr_full_len` 全程恒定，池 key 全程钉住。
  回归测试 `TestClipInstructionsToStablePrefix` 等。

## 6. 背景：2026-10-08 二轮修复（计数/thinking 漂移）

- 现象：agents 多轮稳态只到 ~50%，且 pck/池 key 每请求漂移。
- 抓包定位（`OPENCODE2API_DUMP_UPSTREAM` 上游 body 落盘 + 字节级 diff）三个根因：
  ① CLI 每轮把 `<total_tokens>N tokens left</total_tokens>` 计数以
  role=system 消息注入（转后并入 instructions）并往 user 消息累积，值逐请求
  变化——instructions/input 都在漂，pck 与上游前缀缓存从源头断；
  ② 免费层指纹重做按客户端原文重建 body，任何前置剥离都会被覆盖；
  ③ CLI 按 Anthropic 惯例只回放最后一轮 thinking，reasoning item 的有无/
  位置逐请求漂移。
- 修复：`stripVolatileCountersInMap`（instructions_stable.go）在
  `applyFreeTierFingerprint` 之后、pck 计算之前剥离 instructions/input/
  messages 三形状的计数块（regex 连同周围空白，防逐轮累积空行）；
  pck first_user 改取非 system-reminder 的稳定 part；转换器跳过 thinking
  回放不生成 reasoning item。
- 验证（本测试法，muse-spark-1.3-contributor，4 轮真实 agents 任务、
  同会话 resume）：总命中率 **92.9%**，主对话稳态 **96-100%**，
  计数残留 0，池 key 钉住（仅 subagent 冷启动 3 个一次性 key），
  截断 0（全部 `truncated=false`）。
  回归测试 `TestStripVolatileTokenCounters`、`TestStableUserTextFromParts`。
- 读数注意：Responses 口径 `input_tokens` 已含缓存（=prompt+cached），
  命中率 = `input_cached_tokens / input_tokens`，不要按
  `cached/(cached+input)` 计算（会得 ~50% 的假读数）。
- env 直驱注意：title/子代理请求走 sonnet 别名，三件套之外必须加
  `ANTHROPIC_DEFAULT_SONNET_MODEL`（及 HAIKU/OPUS）与
  `CLAUDE_CODE_SUBAGENT_MODEL`，否则每轮 401×3 失败、子代理起不来。

## 7. 背景：2026-10-08 三轮修复（chat 形状 system 漂移）

- 模型：`ling-3.1-flash`（`/v1/models` 暴露非 `-free` 名,上游实际
  `ling-3.1-flash-free`——网关内部映射,不要被模型名迷惑）。
- 现象：同 fixture 4 轮 agents 任务命中率仅 11.7%（27/27 请求 <60%）,
  而 muse-spark 同法 92.9%。dump diff 定位 chat 形状两个新根因：
  ① ling 走 chat 形状（`buildUpstreamBodyFromClaude` → OpenAI 兼容
  `messages`,非 muse-spark 的 Responses `input`）,CLI 计数以 role=system
  join 进 messages[0] 尾部逐轮累积（实测 3 处计数）——
  `stripVolatileTokenCountersInPlace` 只剥 user,system 没剥;
  ② chat 形状无 `clipInstructionsToStablePrefix` 路径,CLI 每轮往 system
  尾部追加 hand-back 通知（剥计数后仍增长 ~3K）,上游前缀缓存从 system
  增长点截断、tools 与历史全 miss（命中率封顶 ~17%）。
- 修复：`stripVolatileTokenCountersInPlace` 扩展剥 system/developer 的
  字符串 content（assistant 输出稳定不动）;新增
  `clipChatSystemStable`（instructions_stable.go）把 chat 形状首条
  role=system 消息钉在会话首轮原文、增量 TrimSpace 后并入末条 user 消息
  （`appendChatDeltaToLastUser`,chat 字符串/ parts content 两形状）。
  接入点同 Responses 路径：`buildOCRequestWithSubpathAndState` 内
  strip 之后、pck 重算之前。
- 验证（ling-3.1-flash-free,4 轮 agents、同会话 resume）：主对话
  **7/8 请求 ≥60%（87-100%,唯一 low 是 probe 冷启动）**,计数残留
  0/208 dumps,截断 0（28/28 `truncated=false`）。低命中余量全部为
  固有冷启动：子代理 11 个（每个新子代理首轮 0%,pck 按 first_user
  隔离防串污染）+ CLI 监控代理 2 个。
- 回归测试 `TestStripVolatileTokenCountersSystemRole`、
  `TestClipChatSystemStable`（fixture 须 >stableInstrHeadSegs 段,
  不足 16 段时稳定头全量取值、键随尾部增量漂移）。

### 测试环境陷阱（2026-10-08 实测）

- **8080 可能被用户日常网关占用**（launchd 管理,kill 后自动重启,
  bind 失败但 CLI 仍 200）——日志只有 9 行 `failed to start server`。
  测试网关用 `-port 8081 -config <repo>/config.json`（显式指定,否则
  回退应用目录 config,其 `key_pool.enabled=false` → 429 无
  pool_failover）。
- **CLI 429 后 fallback 直连官方**：网关全 429（`FreeUsageLimitError`,
  免费层用量上限,V1 测试 283 个请求耗尽配额）时 CLI 用桌面 3P OAuth
  直连 api.anthropic.com 返回正确结果——**流量绕过网关,命中率数据全
  作废**。probe 验证法：每轮后 `KEYPARTS=$(grep -c cache_debug_key_parts)`
  必须增长,网关日志必须有 `cache_debug_usage`（成功请求）;网关进程
  停掉重跑 probe,CLI 失败 = 流量真在网关。免配额耗尽:
  网关带项目 config（1811 keys 池）时 429 走 pool_failover 换 key。
- **CLI 自动更新改变行为**：2.1.293.754/907 把 hand-back 通知作为
  system 增量注入;2.1.293.45a 改为独立 user 消息插历史中段（上一轮
  assistant 后）——每次通知在消息流中段插入,前缀从插入点断一次
  （该轮 0%）,断后即恢复。通知含子代理最终报告,不可删（丢了丢上下文）;
  挪到尾部实测更差（见 §6 V5 实验,挡住下一请求的历史延续）——留原位,
  单次断点是固有代价。

## 7. 背景：2026-10-08 三轮修复（system 消息计数剥离）

- 现象（mimo-v2.6-flash-free 抓包）：首轮修复后多轮 agents 稳态仍封顶
  ~41K tokens（`prompt_cached_tokens` 恒 40960），总命中 62.5%。
- 抓包定位：剥离函数只处理 `role=user`，而 CLI 每轮往 **role=system 消息**
  累积追加新 `<total_tokens>N tokens left</total_tokens>` 块（b1 有 3 个、
  b2 有 4 个，逐轮 +1），system 消息 17/17 残留——每轮在固定文本中间插入
  新计数，前缀从插入点断。
- 修复：`stripVolatileTokenCountersInPlace` 扩展到 system/developer 的
  string content 同样剥离。
- 验证（本测试法，mimo-v2.6-flash-free，7 轮真实 agents 任务、同会话
  resume、全程相同 `--allowedTools`，18 个上游请求）：
  system 计数残留 **18/18 clean**；`prompt_cached_tokens` 突破 40960
  封顶随 prompt 增长（55360/57792/58112），单请求 **99%+ 命中 3 次**
  （99.4%/99.2%/99.0%），稳态 85-99%；截断 **0/18**；
  字节 diff：轮 3+ 请求公共前缀稳定在 ~70-75KB（轮 1-2 的 ~32KB 漂移
  是 PreToolUse hook 注入建立期——用户全局 hooks 的 context_guidance
  块逐工具类型累积，文本固定，非计数漂移）。
- 池 key：sticky 按 pck 分片，主对话连续请求同 key；subagent/title
  独立 pck 各自一次性 key，属预期。

## 8. pi agent 测试法（第三方客户端接入）

> pi（`@earendil-works/pi-coding-agent`，npm 全局）走 anthropic-messages API
> 直连网关，用于验证网关缓存对非 Claude Code 客户端同样生效。

### 接入配置

`~/.pi/agent/models.json` 加 provider（模型用网关名；anthropic-messages 的
baseUrl 不带 `/v1` 后缀——pi 的 anthropic SDK 自己拼 `/v1/messages`；
openai 两族的 baseUrl 带 `/v1`）。三协议各一个 provider：

```json
{
  "providers": {
    "opencode2api": {
      "baseUrl": "http://127.0.0.1:8080",
      "api": "anthropic-messages",
      "apiKey": "sk-123",
      "models": [
        { "id": "mimo-v2.6-flash", "name": "mimo v2.6 flash (gateway)",
          "contextWindow": 200000, "maxTokens": 64000, "reasoning": false }
      ]
    },
    "opencode2api-chat": {
      "baseUrl": "http://127.0.0.1:8080/v1",
      "api": "openai-completions",
      "apiKey": "sk-123",
      "models": [
        { "id": "mimo-v2.6-flash", "name": "mimo v2.6 flash (gw chat)",
          "contextWindow": 200000, "maxTokens": 64000, "reasoning": false }
      ]
    },
    "opencode2api-resp": {
      "baseUrl": "http://127.0.0.1:8080/v1",
      "api": "openai-responses",
      "apiKey": "sk-123",
      "models": [
        { "id": "mimo-v2.6-flash", "name": "mimo v2.6 flash (gw responses)",
          "contextWindow": 200000, "maxTokens": 64000, "reasoning": false }
      ]
    }
  }
}
```

### 步骤

```bash
# 0. 起网关（同铁律 4：新二进制 + OPENCODE2API_CACHE_DEBUG=1）

# 1. 干净目录建测试会话（避免项目 AGENTS.md 噪声）
mkdir -p /tmp/pi-cache-test/sessions && cd /tmp/pi-cache-test
UUID=$(uuidgen | tr 'A-Z' 'a-z')

# 2. 首轮（agent loop 内含多次 model call）
pi -p --provider opencode2api --model mimo-v2.6-flash \
  --session-id "$UUID" --session-dir /tmp/pi-cache-test/sessions \
  --thinking off "Create a file named hello.py containing a print statement, then run it."

# 3. 跨轮：同一 --session-id 即续会话
pi -p --provider opencode2api --model mimo-v2.6-flash \
  --session-id "$UUID" --session-dir /tmp/pi-cache-test/sessions \
  --thinking off "<second task>"
```

### 读数

客户端口径（Anthropic 语义）命中率 = `cacheRead/(input+cacheRead)`。
pi session JSONL（`--session-dir` 下）每条 assistant 带 usage，是首选读数
（网关日志混有并发客户端流量，需按时间戳+模型过滤）：

```bash
python3 -c "
import json,sys
for line in open('/tmp/pi-cache-test/sessions/<file>.jsonl'):
    r=json.loads(line); m=r.get('message',r)
    if m.get('role')=='assistant' and m.get('usage'):
        u=m['usage']; tot=u['input']+u.get('cacheRead',0)
        print(r.get('timestamp','')[:19], 'input=',u['input'],
              'cached=',u.get('cacheRead',0),
              'hit=%.1f%%'%(u.get('cacheRead',0)/tot*100 if tot else 0))
"
```

网关侧 `cache_debug_usage` 的 `prompt_tokens` 已含缓存
（= input+cacheRead），可与 session usage 逐 token 对账验证归属。

### 实测基线（2026-10-08，mimo-v2.6-flash，每协议 3 次 pi -p 续会话）

| 协议 | 调用数 | 稳态命中 | 总命中 | 备注 |
|---|---|---|---|---|
| anthropic-messages | 9 | 98-99.6% | 88.5% | 首调用冷启动 0%（input 22619） |
| openai-completions | 8 | 98.8-99.7% | 74.5% | 冷启动 2 次（call1 + 轮3 首调） |
| openai-responses | 8 | 98.2-99.8% | 99.5% | 全部命中，含每次首调 |

- 口径同上 `cacheRead/(input+cacheRead)`；pi session JSONL 每 assistant 带 usage
- **跨协议共享缓存**：pi 公共前缀（system+tools ≈ 22.5-23.5K tokens）在
  三协议命中同一上游分片——cached 值逐 token 相同（22592/22656/22784/23040），
  responses 会话在网关重启后首调即 99.8% 命中前一会话留下的暖前缀
- openai-completions 的偶发冷启动（轮 3 首调 0%，次调 98.8%）：与免费层 429
  重试/池 key 轮换的已知行为一致（并发压测时出现），非协议缺陷
- 读数注意：`cache_debug_usage` 只在 claude（anthropic 入口）与 responses
  路径打日志；openai-completions 路径无此日志，读数只能走 pi session 文件
- 结论：pi 无逐轮动态注入（无 `<total_tokens>` 计数、无 instructions
  尾部追加），`instructions_stable.go` 修复对其透明，网关无需额外处理

## 9. 背景：2026-10-08 四轮修复（claude 路径 body 级剥离 + pck 稳定 first_user）验证

- 修改：`buildUpstreamBodyFromClaude` / `claudeToResponsesBody` /
  `chatToResponsesBodyWithRaw` 发送前 body 级剥离计数；
  pck first_user 改 `stableUserTextFromParts`（跳过整段 system-reminder
  注入块与计数块，取稳定请求文本）；转换器跳过 thinking 回放；
  新增 `OPENCODE2API_DUMP_UPSTREAM=<dir>` 调试落盘开关（发送侧逐请求
  body 落盘，用于字节级前缀 diff，默认关闭）。
- 验证（本测试法，mimo-v2.6-flash-free，3 遍独立 × 7 轮真实 agents 任务、
  同会话 resume、全程相同 `--allowedTools Task Agent Read Glob Grep Bash`、
  env 直驱，网关 sticky+24h+CACHE_DEBUG）：

| 遍 | 上游请求 | 总命中 | cached>0 | max_cached | >40960 | 截断 | 计数残留 |
|---|---|---|---|---|---|---|---|
| 1 | 47 | 81.5% | 45/47 | 56000 | 14 | 0 | 抽样 0 |
| 2 | 44 | 81.1% | 40/43 | 58816 | 16 | 0 | — |
| 3 | 35 | 84.2% | 33/35 | 53696 | 12 | 0 | 0/36（字节级） |

- 三遍一致：稳态（轮内 / subagent 连续请求）**96-100%**，
  `prompt_cached_tokens` 突破 40960 封顶随 prompt 增长；cached=0 仅
  subagent/title 冷启动（每遍 2-3 个一次性 key），属预期。
- 轮间转场请求 72-90%：字节 diff 定位为**客户端**首轮 user 消息 content
  parts 逐请求重建（MCP Server Instructions 部分随 MCP server 连接状态
  出现/消失，如 lcu 连上后新增段）——非网关计数漂移（发送侧 body
  `<total_tokens>` 残留 0），网关无法也无需修复；同类已知项：PreToolUse
  hook 注入建立期（第 7 节）。
- 中途网关重启（pck 注册表清空）后命中不塌：上游缓存跨重启保留
  （retention 24h），恢复后首调即 95-100%。
- 模型名注意：`/v1/models` 列 `mimo-v2.6-flash`（无 -free 后缀），但
  `mimo-v2.6-flash-free` 实测可过网关（三遍 126 请求全部有
  `cache_debug` 记录，流量未旁路）。
- 回归测试：`go test ./internal/app -count=1` 全过。

