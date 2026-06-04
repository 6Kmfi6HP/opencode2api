# 部署说明

## Docker Compose

项目提供三套 compose 模版：

- `deploy/compose/compose.yml`：单独运行，直连上游。
- `deploy/compose/compose.tor.yml`：通过 Tor SOCKS5 代理访问上游。
- `deploy/compose/compose.warp.yml`：通过 Cloudflare WARP SOCKS5 代理访问上游。

快速启动：

```bash
export OPENCODE2API_PASSWORD="change-me"
docker compose -f deploy/compose/compose.yml up -d
curl http://127.0.0.1:8000/health
```

使用 Tor：

```bash
docker compose -f deploy/compose/compose.tor.yml up -d
```

使用 WARP：

```bash
docker compose -f deploy/compose/compose.warp.yml up -d
```

默认镜像是 `ghcr.io/6kmfi6hp/opencode2api:latest`。如果使用 fork 或私有镜像，设置：

```bash
export OPENCODE2API_IMAGE="ghcr.io/OWNER/opencode2api:latest"
```

更多说明见 `deploy/compose/README.md`。

## 使用 release 二进制

从 GitHub Releases 下载对应系统的包：

```bash
tar -xzf opencode2api_v0.1.0_linux_amd64.tar.gz
cd opencode2api_v0.1.0_linux_amd64
cp config.example.json config.json
./opencode2api -port 8000 -config config.json -password "change-me"
```

## systemd 示例

创建运行目录：

```bash
sudo install -d -m 0755 /opt/opencode2api
sudo install -m 0755 opencode2api /opt/opencode2api/opencode2api
sudo install -m 0644 config.example.json /opt/opencode2api/config.json
```

创建 `/etc/systemd/system/opencode2api.service`：

```ini
[Unit]
Description=opencode2api proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/opencode2api
ExecStart=/opt/opencode2api/opencode2api -port 8000 -config /opt/opencode2api/config.json -password CHANGE_ME
Restart=on-failure
RestartSec=3
User=nobody
Group=nogroup
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

启动服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now opencode2api
sudo systemctl status opencode2api
```

## 反向代理建议

如果需要公网访问，建议：

- 只暴露 API 路由，管理面板放在 VPN 或内网后面
- 使用 HTTPS
- 在反向代理层加限流和访问控制
- 修改默认管理密码
- 定期备份 `config.json`，按需保留或清理 `stats.json`
