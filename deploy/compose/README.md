# Docker Compose 部署模版

这里提供三种部署方式：

- `compose.yml`：单独运行 `opencode2api`，上游请求直连。
- `compose.tor.yml`：同时启动 Tor，`opencode2api` 通过 `tor:9050` 走 SOCKS5。
- `compose.warp.yml`：同时启动 Cloudflare WARP，`opencode2api` 通过 `warp:1080` 走 SOCKS5。

默认镜像是 `ghcr.io/6kmfi6hp/opencode2api:latest`。Fork 或私有构建可以用 `OPENCODE2API_IMAGE` 覆盖。

## 通用变量

```bash
export OPENCODE2API_PASSWORD="change-me"
export OPENCODE2API_HTTP_PORT=8000
```

如果使用自己的镜像：

```bash
export OPENCODE2API_IMAGE="ghcr.io/OWNER/opencode2api:latest"
```

## 方式 1：单独运行

```bash
docker compose -f deploy/compose/compose.yml up -d
curl http://127.0.0.1:${OPENCODE2API_HTTP_PORT:-8000}/health
```

## 方式 2：使用 Tor 代理

```bash
docker compose -f deploy/compose/compose.tor.yml up -d
curl http://127.0.0.1:${OPENCODE2API_HTTP_PORT:-8000}/health
```

该模版会把首次生成的配置设为：

```json
{
  "socks5_proxies": [{"name": "tor", "addr": "tor:9050"}],
  "active_socks5": "tor:9050"
}
```

## 方式 3：使用 WARP 代理

```bash
docker compose -f deploy/compose/compose.warp.yml up -d
curl http://127.0.0.1:${OPENCODE2API_HTTP_PORT:-8000}/health
```

该模版使用 `caomingjun/warp` 镜像，并把首次生成的配置设为：

```json
{
  "socks5_proxies": [{"name": "warp", "addr": "warp:1080"}],
  "active_socks5": "warp:1080"
}
```

如果宿主机内核或 containerd 版本导致 WARP 容器无法打开 TUN 设备，先查看 WARP 容器日志：

```bash
docker logs opencode2api-warp
```

## 切换代理

容器只在 `/data/config.json` 不存在时根据环境变量生成初始配置。已经启动过的部署可以用两种方式切换：

- 打开管理面板修改 SOCKS5 代理和启用项。
- 删除 compose volume 后重新启动，让入口脚本重新生成配置。

```bash
docker compose -f deploy/compose/compose.tor.yml down -v
docker compose -f deploy/compose/compose.warp.yml up -d
```

生产环境请务必修改 `OPENCODE2API_PASSWORD`，并把服务放在 HTTPS 反向代理、VPN 或访问控制之后。
