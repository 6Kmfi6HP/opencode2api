# opencode2api

`opencode2api` 是一个本地 HTTP 代理，把 OpenAI Chat Completions、OpenAI Responses 和 Anthropic Messages 风格的请求转发到 OpenCode 上游接口，并提供模型别名、reasoning/thinking 兼容、SOCKS5 代理和一个轻量管理面板。

> 这个项目不是 OpenAI、Anthropic 或 OpenCode 的官方项目。请遵守上游服务条款，并只在你有权限的环境中使用。

## 功能

- OpenAI 兼容接口：`/v1/chat/completions`、`/v1/models`
- OpenAI Responses 兼容接口：`/v1/responses`
- Anthropic Messages 兼容接口：`/v1/messages`
- 流式 SSE 转换和 token 用量统计
- 模型别名、reasoning effort 映射、强制禁用 thinking
- SOCKS5 直连、指定代理和轮询代理
- Web 管理面板：配置、统计、刷新上游会话
- GitHub Actions 自动构建 Linux、macOS、Windows、FreeBSD 多平台 release
- GitHub Actions 自动发布 Docker 镜像到 GHCR

## 快速开始

```bash
git clone https://github.com/6Kmfi6HP/opencode2api.git
cd opencode2api
cp config.example.json config.json
go run . -port 8000 -config config.json -password "change-me"
```

健康检查：

```bash
curl http://127.0.0.1:8000/health
```

查看模型：

```bash
curl http://127.0.0.1:8000/v1/models
```

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

## 命令行参数

```text
-port string
    服务端口，默认 8000
-config string
    配置文件路径，默认 config.json
-password string
    管理面板密码，默认 123456；留空表示不启用登录验证
-debug
    输出调试日志
-version
    显示构建版本
```

第一次部署请务必修改 `-password`。如果把服务暴露到公网，建议只通过反向代理、访问控制或 VPN 暴露管理面板。

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

## Docker Compose 部署

项目提供单独运行、Tor 代理、WARP 代理三套 compose 模版：

```bash
export OPENCODE2API_PASSWORD="change-me"
docker compose -f deploy/compose/compose.yml up -d
```

代理部署见 [Docker Compose 部署模版](deploy/compose/README.md)。

## 文档

- [API 兼容说明](docs/API.md)
- [配置说明](docs/CONFIGURATION.md)
- [部署说明](docs/DEPLOYMENT.md)
- [发布流程](docs/RELEASE.md)
- [Docker Compose 部署模版](deploy/compose/README.md)
- [贡献指南](CONTRIBUTING.md)
- [安全说明](SECURITY.md)

## 许可证

当前仓库默认保留全部权利，避免在未确认授权策略前自动开源。需要公开开源时，可将 `LICENSE` 替换为 MIT、Apache-2.0 或其他许可证。
