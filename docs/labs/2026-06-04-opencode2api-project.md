# opencode2api Projectization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the provided single-file Go program into a GitHub-hosted project with local checks, documentation, and tag-driven multi-platform releases.

**Architecture:** Keep the existing proxy behavior in `main.go` for the first projectization pass to reduce behavior drift. Add small, tested build metadata helpers, project docs, examples, and a GitHub Actions release workflow that cross-compiles static Go binaries.

**Tech Stack:** Go standard library, GitHub Actions, GitHub CLI, POSIX shell.

---

### Task 1: Repository Skeleton

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `config.example.json`
- Create: `scripts/build-release.sh`
- Modify: `main.go`

- [ ] Create a Go module at `github.com/6Kmfi6HP/opencode2api`.
- [ ] Copy the provided source into `main.go`.
- [ ] Add ignored runtime files such as `config.json`, `stats.json`, and generated binaries.
- [ ] Add an example config file without secrets.
- [ ] Add a local release build script matching the CI target matrix.

### Task 2: Version Metadata

**Files:**
- Create: `main_test.go`
- Modify: `main.go`

- [ ] Write a failing test for `versionString()`.
- [ ] Run `go test ./...` and verify the test fails because `versionString` is missing.
- [ ] Add `version`, `commit`, and `date` variables plus `versionString()`.
- [ ] Add a `-version` flag that prints build metadata and exits.
- [ ] Re-run `go test ./...` and verify the tests pass.

### Task 3: Documentation

**Files:**
- Create: `README.md`
- Create: `docs/API.md`
- Create: `docs/CONFIGURATION.md`
- Create: `docs/DEPLOYMENT.md`
- Create: `docs/RELEASE.md`
- Create: `CONTRIBUTING.md`
- Create: `SECURITY.md`
- Create: `CHANGELOG.md`
- Create: `LICENSE`

- [ ] Document supported OpenAI-compatible, Anthropic-compatible, and Responses API routes.
- [ ] Document local build, binary usage, Docker-free service deployment, and config fields.
- [ ] Document release tagging and expected artifacts.
- [ ] Add maintenance and contribution guidance.

### Task 4: CI and Release Automation

**Files:**
- Create: `.github/workflows/ci.yml`
- Create: `.github/workflows/release.yml`

- [ ] Add CI for `gofmt`, `go test`, `go vet`, and a local build.
- [ ] Add tag-driven release workflow for Linux, macOS, Windows, and FreeBSD on amd64/arm64 where supported.
- [ ] Generate checksums and attach archives to GitHub Releases.
- [ ] Keep release creation and asset upload in the same workflow.

### Task 5: Verification and Publish

**Files:**
- All project files

- [ ] Run `gofmt`.
- [ ] Run `go test ./...`.
- [ ] Run `go vet ./...`.
- [ ] Run a local Linux build.
- [ ] Run the release build script.
- [ ] Initialize git, commit all intended files, create the GitHub repo, and push `main`.
