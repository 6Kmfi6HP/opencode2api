---
name: oc2api-admin
description: "管理 opencode2api 实例：探测状态、读写运行时配置、刷新会话与模型、查看用量、更新镜像"
disable-model-invocation: true
---

# oc2api-admin — opencode2api 实例管理

管理一组（fleet）或单个 opencode2api 实例。
- 运行时 API 操作：通过 Python 客户端 `.claude/skills/oc2api-admin/oc2api-admin.py` 执行。
- 容器镜像更新：通过宿主机部署脚本 `scripts/update-remote.sh` 或 Docker 命令执行。

实例地址（`-u`）与管理密码（`-p`）优先由用户指定，或从本地 `config.json` 中的 `admin_password` 获取。

```bash
python3 .claude/skills/oc2api-admin/oc2api-admin.py -u <base_url> -p <admin_password> <子命令>
# 支持子命令: probe | config | reload | stats
# 加 -j 输出 JSON
```

---

## probe

探测目标实例是否在线及响应状态（无需鉴权）。

```bash
python3 .claude/skills/oc2api-admin/oc2api-admin.py -u <base_url> probe
```

**完成标准**：已向用户明确汇报每个目标实例的连通状态（在线 HTTP 状态码 / 离线错误原因）。

---

## config

读取或增量修改运行时配置（变更立即生效，无需重启）。

**读取**：
```bash
python3 .claude/skills/oc2api-admin/oc2api-admin.py -u <base_url> -p <admin_password> config get
```

**写入**（只传需修改的字段，值自动按 JSON 解析；布尔/数字/数组/对象均可直接传）：
```bash
python3 .claude/skills/oc2api-admin/oc2api-admin.py -u <base_url> -p <admin_password> config set <KEY>=<VALUE> ...

# 常用示例：
#   config set force_free=true
#   config set log_level=debug max_tokens_cap=131072
#   config set upstream_base_urls='["https://opencode.ai"]'
```

> **字段定义参考**：完整可用字段、类型及默认值以项目根目录 [`config.example.json`](config.example.json) 为单一真实来源。

> ⚠️ **服务端 POST /api/config 是整体覆盖**：它会用提交内容覆盖整个配置文件并全量应用，
> 未提交的字段一律取零值——`active_socks5` 变空字符串等于改为直连，`key_pool` /
> `protocol_rules` 等会直接从磁盘消失，重启即丢。
> 客户端已内置保护：`config set` 会先 `GET` 全量运行时快照、合并本次变更后再整份 `POST`。
> 直接 `curl` 打这个端点时必须自己完成同样的合并，不要只发增量字段。
>
> 三个字段服务端不下发（`native_responses_models`、`stream_empty_retry_max`、
> `stream_first_byte_timeout_ms`），合并回写保不住它们的原值；需要修改请直接编辑宿主机
> 配置文件。客户端在检测到时会在 stderr 打印提示（`OC2API_QUIET=1` 可关闭）。

**完成标准**：
- `get`：已向用户展示当前生效配置及关注字段。
- `set`：确认命令输出 `✓ 配置已更新`，并向用户核对生效的键值对；涉及代理配置时
  另需回读 `active_socks5`（应为 `__round_robin__`）与宿主机 config.json 大小/键数是否完整。

---

## reload

刷新 OpenCode 会话和模型列表（并发请求共享同一次刷新，具备幂等性）。

```bash
python3 .claude/skills/oc2api-admin/oc2api-admin.py -u <base_url> -p <admin_password> reload
```

**完成标准**：确认输出 `✓ 刷新完成`，且向用户汇报新分配的 session ID、免费模型数及 Go 模型数。

---

## stats

查看或清除运行时 Token 用量统计。

**查看用量**：
```bash
python3 .claude/skills/oc2api-admin/oc2api-admin.py -u <base_url> -p <admin_password> stats
```

**清除统计**（破坏性操作）：
```bash
python3 .claude/skills/oc2api-admin/oc2api-admin.py -u <base_url> -p <admin_password> stats clear
```

**完成标准**：
- 查看：向用户呈现各模型及总计的请求数（req）、各阶段 token 及缓存读取量。
- 清除：确认命令输出 `✓ 统计数据已清除`，并告知用户统计已重置。

---

## update

更新远程服务器或本地实例的容器镜像并重启生效。

**远程集群多节点更新（推荐）**：
```bash
# 1. 预检探活（dry-run，只检查所有服务器容器状态与当前版本，不执行更新）
./scripts/update-remote.sh -n

# 2. 一键更新所有远程服务器（拉取最新镜像、同步 upstream_base_urls、重建容器）
./scripts/update-remote.sh
```

**单机 / 本地容器更新**：
```bash
docker compose pull && docker compose up -d
python3 .claude/skills/oc2api-admin/oc2api-admin.py -u <base_url> probe
```

**完成标准**：目标服务器容器均已拉取最新镜像并重启成功，`probe` 或 `/health` 均返回 200，且向用户确认当前运行版本。

---

## 参考：底层 API 协议

如需直接使用 `curl` 调试，管理端点采用 **Session Cookie + CSRF Token** 双重校验：
1. `POST <base_url>/login`（form 表单 `password=<admin_password>`）→ 捕获响应头 `Set-Cookie` 中的 `session` 与 `csrf_token`。
2. 后续请求携带 `Cookie: session=...; csrf_token=...`。
3. 非 GET 请求需额外附加请求头 `X-CSRF-Token: <csrf_token>`。
