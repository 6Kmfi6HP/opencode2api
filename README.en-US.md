# opencode2api

`opencode2api` is a local HTTP proxy that forwards OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages style requests to the OpenCode upstream API. It provides model aliases, reasoning/thinking compatibility, SOCKS5 proxy support, and a lightweight management panel.

> This project is not an official project of OpenAI, Anthropic, or OpenCode. Please comply with the upstream terms of service and use it only in environments where you have authorization.

## Features

- OpenAI compatible interfaces: `/v1/chat/completions`, `/v1/models`
- OpenAI Responses compatible interface: `/v1/responses`
- Anthropic Messages compatible interface: `/v1/messages`
- Streaming SSE conversion and token usage statistics
- Model aliases, reasoning effort mapping, and forced disabling of thinking
- SOCKS5 direct connection, specified proxies, and round-robin proxies
- Web management panel: Configuration, statistics, and upstream session refresh
- GitHub Actions automated builds for multi-platform releases: Linux, macOS, Windows, FreeBSD
- GitHub Actions automated Docker image publishing to GHCR

## Quick Start

```bash
git clone https://github.com/6Kmfi6HP/opencode2api.git
cd opencode2api
cp config.example.json config.json
go run . -port 8000 -config config.json -password "change-me"
```

Health Check:

```bash
curl http://127.0.0.1:8000/health
```

List Models:

```bash
curl http://127.0.0.1:8000/v1/models
```

Authentication Modes:

- No `Authorization` header, or using `Bearer public`: Uses OpenCode public; only stable access to free Zen models ending in `-free` is available.
- Using `Bearer <api-key>`: Defaults to Zen; if the requested model exists only in the Go directory, it will automatically switch to Go.
- Using `Bearer zen:<api-key>`: Forces Zen usage; suitable for when you specifically want to use the Zen pay-as-you-go directory.
- Using `Bearer go:<api-key>`: Prioritizes the Go subscription directory; shared models will also be requested via the Go path.
- Invalid or placeholder keys (e.g., `no-key-required`) will automatically fall back to public mode.

Chat Completions Example:

```bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": false
  }'
```

Go Subscription Example:

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

## Command Line Arguments

```text
-port string
    Service port, default 8000
-config string
    Path to configuration file, default config.json
-password string
    Management panel password, default 123456; leave empty to disable login authentication
-debug
    Output debug logs
-version
    Display build version
```

Please be sure to change `-password` during your first deployment. If exposing the service to the public internet, it is recommended to expose the management panel only via reverse proxy, access control, or VPN.

## Local Build

```bash
make test
make vet
make build
./bin/opencode2api -version
```

Generate local multi-platform release packages:

```bash
make release-snapshot VERSION=v0.1.0
ls dist/
```

## Automated Release

After pushing a `v*` tag, GitHub Actions will first run formatting, test, and vet checks, then use a matrix to concurrently build the following targets:

- `linux/amd64`
- `linux/arm64`
- `linux/arm/v7`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`
- `windows/arm64`
- `freebsd/amd64`
- `freebsd/arm64`

Release Command:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The release will include `.tar.gz` packages for each platform and a unified `checksums.txt`.

## Docker Compose Deployment

The project provides three sets of compose templates: standalone, Tor proxy, and WARP proxy:

```bash
export OPENCODE2API_PASSWORD="change-me"
docker compose -f deploy/compose/compose.yml up -d
```

For proxy deployment, see [Docker Compose Deployment Templates](deploy/compose/README.md).

## Documentation

- [API Compatibility Notes](docs/API.md)
- [Configuration Guide](docs/CONFIGURATION.md)
- [Deployment Guide](docs/DEPLOYMENT.md)
- [Release Process](docs/RELEASE.md)
- [Docker Compose Deployment Templates](deploy/compose/README.md)
- [Contributing Guide](CONTRIBUTING.md)
- [Security Policy](SECURITY.md)

## License

This repository currently reserves all rights by default to avoid automatic open-sourcing before authorization policies are confirmed. If public open-sourcing is required, `LICENSE` can be replaced with MIT, Apache-2.0, or other licenses.
