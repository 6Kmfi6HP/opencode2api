# 发布流程

Release 由 `.github/workflows/release.yml` 管理。工作流分为三段：

- `verify`：只运行一次格式检查、测试和 vet。
- `build`：使用 GitHub Actions matrix 并发编译每个系统版本，每个目标独立上传 artifact。
- `publish`：下载全部目标产物，统一生成 `checksums.txt`，再创建或更新 GitHub Release。

## 触发方式

推荐通过 tag 触发：

```bash
git tag v0.7.0
git push origin v0.7.0
```

也可以在 GitHub Actions 页面手动运行 Release workflow，并输入版本号。

## 构建内容

`verify` job 会先执行：

```bash
gofmt -l .
go test ./...
go vet ./...
```

随后 `build` matrix 会并发调用 `scripts/build-release.sh` 交叉编译：

- `linux/amd64`
- `linux/arm64`
- `linux/arm/v7`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`
- `windows/arm64`
- `freebsd/amd64`
- `freebsd/arm64`

每个 matrix job 只构建一个目标，因此某个平台失败时可以直接在对应 job 里定位。每个包包含：

- `opencode2api` 或 `opencode2api.exe`
- `README.md`
- `config.example.json`
- `LICENSE`

`publish` job 会在所有平台构建完成后生成 `dist/checksums.txt`，其中包含所有压缩包的 SHA256。

## Docker 镜像

Docker 镜像由 `.github/workflows/docker.yml` 发布到 GHCR：

- `main` 分支推送：发布 `ghcr.io/<owner>/<repo>:main`、`latest` 和 `sha-<short_sha>`。
- `v*` tag 推送：发布 `ghcr.io/<owner>/<repo>:vX.Y.Z` 和 `sha-<short_sha>`。
- 手动运行 workflow：按当前 ref 发布对应 tag。

工作流会先执行：

```bash
go test ./...
```

然后通过 Buildx 构建并推送：

- `linux/amd64`
- `linux/arm64`

发布 GHCR 需要 workflow 权限：

```yaml
permissions:
  contents: read
  packages: write
```

工作流使用仓库自带的 `GITHUB_TOKEN` 登录 `ghcr.io`。第一次发布的 package 可能默认为 private；如果要公开拉取，需要在 GitHub Packages 页面把可见性改为 public。

## 本地预检

```bash
make fmt
make test
make vet
make release-snapshot VERSION=v0.7.0
```

确认 `dist/` 里有各平台 `.tar.gz` 和 `checksums.txt` 后再推 tag。

## 一键安装脚本检查

仓库提供两个安装脚本，分别覆盖 Unix-like 和 Windows：

- `scripts/install.sh`
- `scripts/install.ps1`

它们依赖 GitHub Release 资产命名：

```text
opencode2api_<version>_<goos>_<goarch>.tar.gz
opencode2api_<version>_<goos>_<goarch>_<variant>.tar.gz
checksums.txt
```

发布新版本前，在本地或目标系统执行 dry-run，确认安装脚本指向的资产名与 `scripts/build-release.sh` 一致：

```bash
OPENCODE2API_VERSION=<new-tag> bash scripts/install.sh --check
```

Windows：

```powershell
.\scripts\install.ps1 -Version <new-tag> -CheckOnly
```

安装说明和 launch 后续命令见 `docs/INSTALL.md`。

## 单目标构建

CI matrix 和本地调试共用同一个脚本。要只构建某个目标：

```bash
TARGETS="linux/amd64" CHECKSUMS=false VERSION=v0.7.0 ./scripts/build-release.sh
```

`TARGETS` 可以放多个以空格分隔的目标，例如：

```bash
TARGETS="linux/amd64 windows/amd64" ./scripts/build-release.sh
```
