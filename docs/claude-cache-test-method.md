# Claude Code 缓存测试法（网关强制指向）

> 任何"缓存命中率 / `prompt_cache_key` / instructions 变化"类测试，
> 一律按此法执行。合成 curl 只用于冒烟，结论必须来自真机 + 网关日志。

## 1. 铁律（违反则结论作废）

1. 模型必须用网关现存模型（如 `muse-spark-1.3-contributor-free`）。
   网关没有的模型会触发 `[claude-code:unrecognized_model]`，流量可能旁路网关。
2. 必须用 `--settings <gw-settings.json>` 强制指向网关。
   `~/.claude/settings.json` 自带 `ANTHROPIC_BASE_URL` 与 `ANTHROPIC_AUTH_TOKEN`，
   进程 env 的优先级不稳定，裸 `export` 经常被覆盖，导致流量根本没进网关
   （现象：回了 `TURN_ONE_OK` 但网关 `cache_debug_key_parts` 计数不涨）。
   不要手配 `ANTHROPIC_AUTH_TOKEN=sk-123` 类 env（`launch_env.go` 已证明它会劫持
   Claude Code 网络栈）。一切以 `--settings` 文件为准。
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
m='muse-spark-1.3-contributor-free'
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
