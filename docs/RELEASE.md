# 发布流程

Release 由 `.github/workflows/release.yml` 管理。

## 触发方式

推荐通过 tag 触发：

```bash
git tag v0.1.0
git push origin v0.1.0
```

也可以在 GitHub Actions 页面手动运行 Release workflow，并输入版本号。

## 构建内容

CI 会先执行：

```bash
gofmt -l .
go test ./...
go vet ./...
```

随后 `scripts/build-release.sh` 会交叉编译：

- `linux/amd64`
- `linux/arm64`
- `linux/arm/v7`
- `darwin/amd64`
- `darwin/arm64`
- `windows/amd64`
- `windows/arm64`
- `freebsd/amd64`
- `freebsd/arm64`

每个包包含：

- `opencode2api` 或 `opencode2api.exe`
- `README.md`
- `config.example.json`
- `LICENSE`

`dist/checksums.txt` 包含所有压缩包的 SHA256。

## 本地预检

```bash
make fmt
make test
make vet
make release-snapshot VERSION=v0.1.0
```

确认 `dist/` 里有各平台 `.tar.gz` 和 `checksums.txt` 后再推 tag。
