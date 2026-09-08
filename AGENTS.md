# Repository Guidelines

Go API gateway that proxies OpenAI Chat, Responses, and Anthropic Messages to OpenCode / Claude backends.

## Project Structure & Module Organization

- `cmd/opencode2api/main.go`: binary entrypoint; keep thin.
- `internal/app/`: all logic — `server.go`, `chat.go`, `responses*.go`, `claude*.go`, `anthropic*.go`, `config.go`, `admin.go`, `launch*.go`, `stats.go`.
- `internal/domain/`, `internal/ids/`, `internal/random/`: shared types and helpers.
- `docs/`: `API.md`, `CONFIGURATION.md`, `DEPLOYMENT.md`, `RELEASE.md` — update with behavior changes.
- `scripts/`: `release.sh`, `build-release.sh`, `install.sh`; `Dockerfile`, `config.example.json` at root.

## Build, Test, and Development Commands

- `make build`: builds version-stamped binary to `bin/opencode2api`.
- `make test` / `go test ./...`: full test suite.
- `go test ./internal/app -run TestName -v`: single focused test.
- `make vet` / `make fmt`: `go vet` and `gofmt -w ./cmd ./internal`.
- `./bin/opencode2api`: run locally; copies `config.example.json` to `config.json` as starting point.

## Coding Style & Naming Conventions

- Go 1.22, tabs, `gofmt`-clean; run `make fmt` before pushing.
- Follow existing file layout: `*_protocol.go` for wire types, `*.go` for handlers, `*_test.go` beside code.
- Exported symbols use `CamelCase`, locals short but clear; no single-letter globals.
- Protocol fixes go in the matching converter (e.g. `responses_passthrough.go`), not `server.go`.

## Testing Guidelines

- Standard `go test` with table-driven cases; see `protocol_regression_test.go`, `request_compatibility_test.go`.
- Name tests `Test<Area>_<Case>`, e.g. `TestResponses_PassthroughRelay`.
- Add or update a regression test for any Chat/Responses/SSE mapping change.
- Verify streaming with `[DONE]` sentinel and event-order assertions where applicable.

## Commit & Pull Request Guidelines

- Use Conventional Commits: `feat(scope):`, `fix(scope):`, `chore:`, `docs:`, `style(admin):` — e.g. `fix(responses): append missing [DONE] sentinel`.
- Keep commits scoped; release chores (`chore: prepare vX.Y.Z`) stay separate.
- PRs: describe behavior change, link issue, list tests run (`make test`, `make vet`), include SSE/JSON samples or admin screenshots for protocol/UI changes.

## Security & Configuration Tips

- Never commit `config.json`, API keys, or `*.log`; use `config.example.json` as template.
- Validate auth, admin, and proxy-key paths after touching `auth.go` or `config.go`.
- Keep `Dockerfile` / `deploy/` in sync when changing ports, paths, or env vars.
