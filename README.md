# opencode2api

[English](README.md) · [简体中文](README.zh-CN.md)

`opencode2api` is a local-first HTTP proxy that forwards OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages–style requests to the OpenCode upstream. It adds model aliases, reasoning/thinking compatibility, SOCKS5 proxying, token usage accounting, and a lightweight admin panel — so any OpenAI/Anthropic-compatible client can talk to OpenCode without changes.

> This project is not affiliated with OpenAI, Anthropic, or OpenCode. Respect the upstream terms of service and only run it in environments you are authorized to use.

## Features

- **OpenAI-compatible** endpoints: `/v1/chat/completions`, `/v1/models`
- **OpenAI Responses** compatible endpoint: `/v1/responses`
- **Anthropic Messages** compatible endpoint: `/v1/messages`
- Streaming SSE conversion with token usage accounting
- Model aliases, reasoning-effort mapping, and force-disable-thinking
- Multi-tier auth routing: public / auto / `zen:` / `go:` prefixes
- SOCKS5 support: direct, fixed proxy, or round-robin
- Web admin panel: edit config, view stats, reload upstream sessions
- GitHub Actions: multi-platform release binaries (Linux / macOS / Windows / FreeBSD)
- GitHub Actions: multi-arch Docker image published to GHCR (`linux/amd64`, `linux/arm64`)
- Single Go dependency (`lumberjack`); ships as one static binary

## Project layout

```text
cmd/opencode2api/         # executable entrypoint
internal/app/             # proxy core: handlers, protocol conversion, upstream calls, admin panel
internal/domain/          # protocol DTOs
internal/ids/             # response ID normalization
internal/random/          # random ID helpers
```

Build locally:

```bash
go build ./cmd/opencode2api
```

## Quick start

```bash
git clone https://github.com/6Kmfi6HP/opencode2api.git
cd opencode2api
cp config.example.json config.json
go run ./cmd/opencode2api -port 8000 -config config.json -password "change-me"
```

Health check:

```bash
curl http://127.0.0.1:8000/health
```

List models:

```bash
curl http://127.0.0.1:8000/v1/models
```

### Authentication modes

- No `Authorization`, or `Bearer public` → OpenCode public tier; only the `-free` Zen models are reachable.
- `Bearer <api-key>` → defaults to Zen; auto-switches to Go if the requested model only exists in the Go catalog.
- `Bearer zen:<api-key>` → forces the Zen metered catalog.
- `Bearer go:<api-key>` → prefers the Go subscription catalog; shared models are also requested via the Go path.
- Invalid or placeholder keys (e.g. `no-key-required`, Anthropic `sk-ant-*`) fall back to public.

Chat Completions example:

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

Go subscription example:

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

## CLI flags

```text
-port string
    Service port, default 8000
-config string
    Config file path, default config.json
-password string
    Admin panel password, default 123456; empty disables login auth
-debug
    Emit debug logs (raises -log-level to debug when it is at the default info)
-log-level string
    Log level: debug/info/warn/error, default info
-log-file string
    Log file path, default opencode2api.log; auto-rotated
-log-stdout
    Also write to stdout, default true
-log-max-size int
    Max MB per log file, default 100
-log-max-backups int
    Number of old logs to keep, default 7
-log-max-age int
    Days to retain old logs, default 14
-log-compress
    gzip rotated logs, default true
-log-bodies
    Under debug, log truncated body-shape summaries, default false
-version
    Print build version
```

Change `-password` on first deploy. If you expose the service publicly, put the admin panel behind a reverse proxy, access control, or VPN.

### Logs and troubleshooting

By default logs go to both file and stdout. Every request carries a `request_id` (response header `X-Request-Id`) you can chain:

`request_started → request_plan → upstream_attempt* → upstream_result → stream_result|request_result → request_done`

Common queries:

```bash
rg 'empty_reply=true' opencode2api.log
rg 'request_id=XXXX' opencode2api.log
rg 'promoted_reasoning=true' opencode2api.log
```

In the container, the default log path is `/data/opencode2api.log` (persisted on the mounted volume). The entrypoint reads the following environment variables (all optional — CLI flags still win when passed explicitly):

| Env var | Default | Maps to flag |
| --- | --- | --- |
| `OPENCODE2API_PORT` | `8000` | `-port` |
| `OPENCODE2API_CONFIG` | `/data/config.json` | `-config` |
| `OPENCODE2API_PASSWORD` | `123456` | `-password` |
| `OPENCODE2API_LOG_FILE` | `/data/opencode2api.log` | `-log-file` |
| `OPENCODE2API_LOG_LEVEL` | `info` | `-log-level` |
| `OPENCODE2API_LOG_STDOUT` | `true` | `-log-stdout` |
| `OPENCODE2API_SOCKS5_ADDR` | *(unset)* | bootstraps a SOCKS5 entry in `config.json` when set |
| `OPENCODE2API_SOCKS5_NAME` | `proxy` | name of the bootstrapped SOCKS5 entry |

## Local build

```bash
make test
make vet
make build
./bin/opencode2api -version
```

Generate local multi-platform release archives:

```bash
make release-snapshot VERSION=v0.1.0
ls dist/
```

## Releases

Pushing a `v*` tag triggers GitHub Actions, which first runs formatting, tests, and vet, then builds the following targets in a matrix:

- `linux/amd64`
- `linux/arm64`
- `linux/arm/v7`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`
- `windows/arm64`
- `freebsd/amd64`
- `freebsd/arm64`

Publish a release:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Each release includes a per-platform `.tar.gz` and a generated `checksums.txt`.

## Docker

The Dockerfile is multi-arch and publishes to GHCR. Pull the image:

```bash
docker pull ghcr.io/6kmfi6hp/opencode2api:latest
```

Run directly:

```bash
docker run -d \
  -p 8000:8000 \
  -v "$PWD/data:/data" \
  -e OPENCODE2API_PASSWORD="change-me" \
  ghcr.io/6kmfi6hp/opencode2api:latest
```

### Docker Compose

Three compose templates are provided (standalone, Tor, WARP):

```bash
export OPENCODE2API_PASSWORD="change-me"
docker compose -f deploy/compose/compose.yml up -d
```

See [Docker Compose templates](deploy/compose/README.md) for Tor and WARP variants.

## Configuration

Config lives in `config.json` (copy from `config.example.json`). Key fields:

| Field | Description |
| --- | --- |
| `model_alias` | Client model name → upstream model name. Explicit `go:`/`zen:` routing wins over a same-name `-free` alias. |
| `reasoning_effort_map` | Maps client `reasoning_effort` to upstream-accepted values. |
| `force_disable_thinking` | When `true`, disables thinking/reasoning and strips it from responses. |
| `max_tokens_cap` | Global `max_tokens` ceiling; `0` = unlimited. |
| `max_tokens_cap_per_model` | Per-model override; `0` = unlimited for that model. |
| `prompt_cache_retention` | Asks the upstream zen gateway to keep prompt-prefix caches for `"24h"` (default) or `"in_memory"` (~5 min); `"off"` disables injection. |
| `cache_control_breakpoints` | When `true` (default), adds an Anthropic-style `cache_control` breakpoint (`ttl: 1h`) to upstream requests for models that accept it. GLM/Zhipu models are always skipped. |
| `socks5_sticky` | When `true` (default) and `active_socks5` is `__round_robin__`, each session/account sticks to one egress proxy so upstream per-egress prompt caches keep hitting (measured 99.8% on a pinned egress vs ~0% on random rotation). |
| `socks5_proxies` | SOCKS5 proxy list. |
| `active_socks5` | `""` direct, an `addr` for a fixed proxy, or `__round_robin__`. |
| `socks5_paid_direct` | `true` makes keyed/paid requests bypass SOCKS5; only public/free goes through proxy. |
| `upstream_base_urls` | Upstream opencode zen base URLs (e.g. your reversed domains). Unset/empty defaults to `["https://opencode.ai"]`. When multiple are configured, sessions stick to one (base URL, proxy) pair for load balancing with cache affinity. |
| `text_only_models` | Model prefixes accepting text only (default `deepseek`); images are silently downgraded to an `[image attached]` text annotation. |

Full details: [Configuration](docs/CONFIGURATION.md).

## Documentation

- [API compatibility](docs/API.md)
- [Configuration](docs/CONFIGURATION.md)
- [Deployment](docs/DEPLOYMENT.md)
- [Release process](docs/RELEASE.md)
- [Docker Compose templates](deploy/compose/README.md)
- [Contributing](CONTRIBUTING.md)
- [Security](SECURITY.md)

## Contributing

Before submitting, run:

```bash
make fmt
make test
make vet
make build
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for principles and commit message conventions.

## License

All rights reserved by default until an open-source license is chosen. To open-source, replace `LICENSE` with MIT, Apache-2.0, or another license.
