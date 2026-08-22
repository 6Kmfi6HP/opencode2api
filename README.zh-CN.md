# opencode2api

[English](README.md) · [简体中文](README.zh-CN.md)

`opencode2api` 是一个本地优先的 HTTP 代理，把 OpenAI Chat Completions、OpenAI Responses 和 Anthropic Messages 风格的请求转发到 OpenCode 上游接口，并提供模型别名、reasoning/thinking 兼容、SOCKS5 代理、token 用量统计和一个轻量管理面板——让任何 OpenAI/Anthropic 兼容客户端无需改动即可对接 OpenCode。

> 这个项目不是 OpenAI、Anthropic 或 OpenCode 的官方项目。请遵守上游服务条款，并只在你有权限的环境中使用。

## 功能

- **OpenAI 兼容**接口：`/v1/chat/completions`、`/v1/models`
- **OpenAI Responses** 兼容接口：`/v1/responses`
- **Anthropic Messages** 兼容接口：`/v1/messages`
- 流式 SSE 转换和 token 用量统计
- 模型别名、reasoning effort 映射、强制禁用 thinking
- 多层级鉴权路由：public / auto / `zen:` / `go:` 前缀
- SOCKS5 支持：直连、指定代理、轮询代理
- Web 管理面板：修改配置、查看统计、刷新上游会话
- GitHub Actions：多平台 release 二进制（Linux / macOS / Windows / FreeBSD）
- GitHub Actions：多架构 Docker 镜像发布到 GHCR（`linux/amd64`、`linux/arm64`）
- 仅一个 Go 依赖（`lumberjack`）；以单个静态二进制交付

## 开发结构

```text
cmd/opencode2api/         # 可执行入口
internal/app/             # 代理核心：handler、协议转换、上游调用、管理面板
internal/domain/          # 协议 DTO
internal/ids/             # 响应 ID 规范化
internal/random/          # 随机 ID 工具
```

本地构建：

```bash
go build ./cmd/opencode2api
```

## 一键安装

macOS、Linux、FreeBSD：

```bash
curl -fsSL https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.sh | bash
opencode2api launch claude
```

Windows PowerShell：

```powershell
irm https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.ps1 | iex
opencode2api launch claude
```

安装脚本会下载最新 Release、校验 SHA256、安装到 `~/.opencode2api/bin`（Windows 为 `~\.opencode2api\bin`）。`launch claude` / `launch codex` 仍然需要本机已安装对应的 CLI。版本固定、安装目录、支持平台和手动安装说明见 [docs/INSTALL.md](docs/INSTALL.md)。

## 快速开始

```bash
git clone https://github.com/6Kmfi6HP/opencode2api.git
cd opencode2api
cp config.example.json config.json
go run ./cmd/opencode2api -port 8000 -config config.json -password "change-me"
```

健康检查：

```bash
curl http://127.0.0.1:8000/health
```

查看模型：

```bash
curl http://127.0.0.1:8000/v1/models
```

### 认证模式

- 不带 `Authorization`，或 `Bearer public` → 走 OpenCode public，只可稳定访问 `-free` 结尾的免费 Zen 模型。
- `Bearer <api-key>` → 默认走 Zen；如果请求的是仅存在于 Go 目录中的模型，会自动切到 Go。
- `Bearer zen:<api-key>` → 强制走 Zen 按量计费目录。
- `Bearer go:<api-key>` → 优先走 Go 订阅目录；共享模型也会按 Go 路径请求。
- 无效或占位 key（如 `no-key-required`、Anthropic `sk-ant-*`）会自动回退到 public 模式。

Chat Completions 示例：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

Go 订阅示例：

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer go:YOUR_OPENCODE_KEY" \
  -d '{
    "model": "glm-5.2",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

## 命令行参数

```text
-port string
    服务端口，默认 8000
-config string
    配置文件路径，默认 config.json
-password string
    管理面板密码，默认 123456；留空表示不启用登录验证
-debug
    输出调试日志（在默认 info 级别时将其提升到 debug）
-log-level string
    日志级别: debug/info/warn/error，默认 info
-log-file string
    日志文件路径，默认 opencode2api.log；配合自动轮换
-log-stdout
    是否同时写 stdout，默认 true
-log-max-size int
    单日志文件最大 MB，默认 100
-log-max-backups int
    保留旧日志个数，默认 7
-log-max-age int
    旧日志保留天数，默认 14
-log-compress
    轮换后 gzip 压缩，默认 true
-log-bodies
    Debug 下记录截断的 body 形状摘要，默认 false
-version
    显示构建版本
```

第一次部署请务必修改 `-password`。如果把服务暴露到公网，建议只通过反向代理、访问控制或 VPN 暴露管理面板。

## launch 子命令

`opencode2api launch <tool>` 在本地回环端口启动代理，然后运行本地编码 CLI，并通过临时配置将请求重定向到 opencode2api。支持的工具是 `claude` 和 `codex`。launch 模式只读取代理配置文件、不写回，因此启动子 CLI 不会修改 `config.json`。

### 模型选择

省略 `--model` 时弹出交互式 TUI，列出上游 catalog 中可用的免费模型。列表按上下文窗口降序排列，≥1M 上下文的模型标记 `[1m]`。

```bash
# 交互式 TUI 模型选择（仅免费模型）
opencode2api launch claude
opencode2api launch codex

# 直接指定模型（跳过 TUI）
opencode2api launch claude --model deepseek-v4-flash
opencode2api launch codex --model deepseek-v4-flash

# -- 之后的模型参数会被提取，不转发给子 CLI
opencode2api launch claude -- --dangerously-skip-permissions --model x-preview-f
opencode2api launch codex -- --ephemeral -m x-preview-f
```

### Claude Code 上下文窗口与自动压缩

`launch claude` 选定模型后，从 [models.dev](https://models.dev/catalog.json) 查询上下文窗口：

- **≥1M 上下文**：模型 ID 追加 `[1m]` 后缀（如 `deepseek-v4-flash[1m]`），并设置 `CLAUDE_CODE_AUTO_COMPACT_WINDOW = ctx × 0.9`。
- **<1M 上下文**：不追加后缀，设置 `CLAUDE_CODE_AUTO_COMPACT_WINDOW = ctx × 0.9`。
- **未知上下文**：不追加后缀，不设自动压缩。

`[1m]` 后缀原样转发给上游；代理的 `resolveModel` / `mapPublicToFreeModel` 在 catalog 查找时去掉后缀，在结果上重新加上，所以免费层映射（如 `deepseek-v4-flash[1m]` → `deepseek-v4-flash-free[1m]`）正确工作。

### Codex 启动

`opencode2api launch codex` 正常运行已安装的 `codex` 命令，因此仍会加载用户现有的 `~/.codex/config.toml`（插件、功能、沙箱设置等）。随后仅对本次进程添加 `-c` 覆盖，把新的 `opencode2api` 自定义 provider 指向本地代理：

- `model_provider = "opencode2api"`
- `model_providers.opencode2api.base_url = http://127.0.0.1:<port>/v1`
- `model_providers.opencode2api.wire_api = "responses"`
- `model_providers.opencode2api.requires_openai_auth = true`
- `model_providers.opencode2api.env_key = "OPENCODE2API_OPENAI_API_KEY"`

opencode2api 还会根据当前可用上游模型生成一个临时 Codex model catalog，并通过本次进程参数传入：

- `model_catalog_json = /tmp/opencode2api-codex-catalog-*/models.json`
- 默认 `public` key 时只包含免费模型，和交互式模型列表一致。
- 付费/分 tier key 时包含全部可用模型。
- 对上下文已知的模型，catalog 中设置 `context_window`、`max_context_window`、`auto_compact_token_limit = int(ctx × 0.9)`。

这样 Codex 后续切换模型和 per-model 上下文都能生效，同时不写入任何 `~/.codex` 文件。

选中的 OpenCode key 只通过子进程变量 `OPENCODE2API_OPENAI_API_KEY` 传入。不会写入任何 `~/.codex` 文件。

### 其他参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--model` | _(空)_ | 上游模型 ID。Claude 设置 `ANTHROPIC_*_MODEL`；Codex 前置 `--model`。空 = 交互式 TUI 选择。 |
| `--key` | `public` | OpenCode key。解析优先级：flag > `OPENCODE_API_KEY` 环境变量 > `public` |
| `--config` | `config.json` | 配置文件路径；launch 模式只读取该文件。 |
| `--port` | `0` | 绑定端口；`0` = 系统随机分配 |
| `--debug` | 关闭 | 启用调试日志 |
| `--version` | 关闭 | 显示构建版本后退出 |

`--` 之后的参数原样透传给选中的子 CLI，除了模型参数（Claude 的 `--model`；Codex 的 `--model` / `-m`）会被提取用于设置模型。

Claude 工作原理：`ANTHROPIC_API_KEY` 携带 OpenCode key（`public`、`sk-…`、`go:…` 或 `zen:…`）。五个环境变量（`ANTHROPIC_MODEL`、`ANTHROPIC_DEFAULT_OPUS_MODEL`、`ANTHROPIC_DEFAULT_SONNET_MODEL`、`ANTHROPIC_DEFAULT_HAIKU_MODEL`、`ANTHROPIC_SMALL_FAST_MODEL`）统一设为选中的模型 ID，避免 `[claude-code:unrecognized_model]` 警告。`CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` 告知 Claude Code 由宿主机管理认证（跳过 OAuth/订阅登录窗口）。

Codex 工作原理：代理服务地址为 `http://127.0.0.1:<port>/v1`，Codex 通过本次进程的临时 `model_providers.opencode2api` 配置使用 Responses wire API。仅子进程可见的 `OPENCODE2API_OPENAI_API_KEY` 携带 OpenCode key；代理读取 Authorization/x-api-key header 并按前缀路由——`go:` → go 层级、`zen:` → zen 层级、`sk-` → auto、`public` → free——自动选择正确的上游。

### 排障日志

默认同时写文件与 stdout。每个请求带 `request_id`（响应头 `X-Request-Id`），可串联：

`request_started → request_plan → upstream_attempt* → upstream_result → stream_result|request_result → request_done`

常见排查：

```bash
rg 'empty_reply=true' opencode2api.log
rg 'request_id=XXXX' opencode2api.log
rg 'promoted_reasoning=true' opencode2api.log
```

容器内默认日志路径是 `/data/opencode2api.log`（挂载卷持久化）。entrypoint 读取以下环境变量（均为可选——显式传入 CLI flag 时仍以 flag 为准）：

| 环境变量 | 默认值 | 对应 flag |
| --- | --- | --- |
| `OPENCODE2API_PORT` | `8000` | `-port` |
| `OPENCODE2API_CONFIG` | `/data/config.json` | `-config` |
| `OPENCODE2API_PASSWORD` | `123456` | `-password` |
| `OPENCODE2API_LOG_FILE` | `/data/opencode2api.log` | `-log-file` |
| `OPENCODE2API_LOG_LEVEL` | `info` | `-log-level` |
| `OPENCODE2API_LOG_STDOUT` | `true` | `-log-stdout` |
| `OPENCODE2API_SOCKS5_ADDR` | *(未设置)* | 设置时在 `config.json` 中引导一个 SOCKS5 条目 |
| `OPENCODE2API_SOCKS5_NAME` | `proxy` | 引导的 SOCKS5 条目名称 |

## 本地构建

```bash
make test
make vet
make build
./bin/opencode2api -version
```

生成本地多平台 release 包：

```bash
make release-snapshot VERSION=v0.1.0
ls dist/
```

## 自动 Release

推送 `v*` tag 后，GitHub Actions 会先运行一次格式、测试和 vet 检查，然后用 matrix 并发构建以下目标：

- `linux/amd64`
- `linux/arm64`
- `linux/arm/v7`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`
- `windows/arm64`
- `freebsd/amd64`
- `freebsd/arm64`

发布命令：

```bash
git tag v0.1.0
git push origin v0.1.0
```

Release 会包含每个平台的 `.tar.gz` 包和统一生成的 `checksums.txt`。

## Docker

Dockerfile 支持多架构并发布到 GHCR。拉取镜像：

```bash
docker pull ghcr.io/6kmfi6hp/opencode2api:latest
```

直接运行：

```bash
docker run -d \
  -p 8000:8000 \
  -v "$PWD/data:/data" \
  -e OPENCODE2API_PASSWORD="change-me" \
  ghcr.io/6kmfi6hp/opencode2api:latest
```

### Docker Compose

项目提供三套 compose 模版（单独运行、Tor、WARP）：

```bash
export OPENCODE2API_PASSWORD="change-me"
docker compose -f deploy/compose/compose.yml up -d
```

Tor 和 WARP 变体见 [Docker Compose 部署模版](deploy/compose/README.md)。

## 配置

配置文件是 `config.json`（从 `config.example.json` 复制）。主要字段：

| 字段 | 说明 |
| --- | --- |
| `model_alias` | 客户端模型名 → 上游模型名。显式 `go:`/`zen:` 路由优先于同名 `-free` 别名。 |
| `reasoning_effort_map` | 把客户端 `reasoning_effort` 映射到上游可接受的值。 |
| `force_disable_thinking` | 设为 `true` 时禁用 thinking/reasoning 并从返回中移除。 |
| `max_tokens_cap` | 全局 `max_tokens` 上限；`0` = 不限制。 |
| `max_tokens_cap_per_model` | 按模型覆盖；`0` = 该模型不限制。 |
| `socks5_proxies` | SOCKS5 代理列表。 |
| `active_socks5` | `""` 直连、某个 `addr` 固定代理、或 `__round_robin__` 轮询。 |
| `socks5_paid_direct` | `true` 时带 key/付费请求绕过 SOCKS5；仅 public/免费走代理。 |
| `upstream_base_urls` | opencode zen 上游域名列表（例如你的反代域名）。未设置/为空默认 `["https://opencode.ai"]`。配置多个时，同一会话会 sticky 固定到某个 (域名, 代理) 组合，实现负载均衡与缓存亲和。 |
| `text_only_models` | 只接受文本的上游模型前缀（默认 `deepseek`）；图片会被静默降级为 `[image attached]` 文本标注。 |

完整说明见 [配置说明](docs/CONFIGURATION.md)。

## 文档

- [API 兼容说明](docs/API.md)
- [配置说明](docs/CONFIGURATION.md)
- [部署说明](docs/DEPLOYMENT.md)
- [发布流程](docs/RELEASE.md)
- [Docker Compose 部署模版](deploy/compose/README.md)
- [贡献指南](CONTRIBUTING.md)
- [安全说明](SECURITY.md)

## 贡献

提交前请运行：

```bash
make fmt
make test
make vet
make build
```

开发原则和提交信息约定见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 许可证

当前仓库默认保留全部权利，避免在未确认授权策略前自动开源。需要公开开源时，可将 `LICENSE` 替换为 MIT、Apache-2.0 或其他许可证。
