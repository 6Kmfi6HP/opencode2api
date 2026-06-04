# 贡献指南

## 本地检查

提交前请运行：

```bash
make fmt
make test
make vet
make build
```

如果改动 release 逻辑，再运行：

```bash
make release-snapshot VERSION=v0.0.0-dev
```

## 开发原则

- 优先保持现有 API 行为不变。
- 对转换逻辑、配置逻辑和 release 脚本增加小而明确的测试或验证步骤。
- 不要提交 `config.json`、`stats.json`、日志或 release 产物。
- 文档和代码行为一起更新。

## 提交信息

建议使用简短动词开头：

- `feat: add ...`
- `fix: handle ...`
- `docs: update ...`
- `ci: improve ...`
