# 发布流程

Release 由 `.github/workflows/release.yml` 管理。工作流分为三段：

- `verify`：只运行一次格式检查、测试和 vet。
- `build`：使用 GitHub Actions matrix 并发编译每个系统版本，每个目标独立上传 artifact。
- `publish`：下载全部目标产物，统一生成 `checksums.txt`，再创建或更新 GitHub Release。

## 触发方式

推荐通过 tag 触发：

```bash
git tag v0.1.0
git push origin v0.1.0
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

## 本地预检

```bash
make fmt
make test
make vet
make release-snapshot VERSION=v0.1.0
```

确认 `dist/` 里有各平台 `.tar.gz` 和 `checksums.txt` 后再推 tag。

## 单目标构建

CI matrix 和本地调试共用同一个脚本。要只构建某个目标：

```bash
TARGETS="linux/amd64" CHECKSUMS=false VERSION=v0.1.0 ./scripts/build-release.sh
```

`TARGETS` 可以放多个以空格分隔的目标，例如：

```bash
TARGETS="linux/amd64 windows/amd64" ./scripts/build-release.sh
```
