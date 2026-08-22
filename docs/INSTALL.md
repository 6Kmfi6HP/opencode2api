# opencode2api 一键安装与 launch 使用

`opencode2api` 的 GitHub Release 已经提供多个系统的静态二进制。安装脚本会根据当前系统自动选择对应的 `.tar.gz`，下载、校验 SHA256，并安装到用户目录，然后给出 `launch claude` / `launch codex` 的下一步命令。

## 支持范围

| 系统 | 架构 | 安装方式 |
| --- | --- | --- |
| Linux | amd64 / arm64 / armv7l | `scripts/install.sh` |
| macOS | amd64 / arm64 | `scripts/install.sh` |
| FreeBSD | amd64 / arm64 | `scripts/install.sh` |
| Windows | amd64 / arm64 | `scripts/install.ps1` |

Windows 安装脚本要求 Windows 10 或更高版本自带的 `tar` 和 PowerShell 5.1+。

## 一键安装

### Linux / macOS / FreeBSD

```bash
curl -fsSL https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.sh | bash
```

也可以下载脚本后执行：

```bash
curl -fsSL https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.sh -o install.sh
bash install.sh
```

默认安装到 `~/.opencode2api/bin`，并把该目录追加到当前 shell 配置（`~/.zshrc`、`~/.bashrc` 或 `~/.profile`）。

### Windows PowerShell

以管理员还是普通用户权限运行都行，脚本只写当前用户目录和用户级 `Path`。

```powershell
irm https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.ps1 | iex
```

如果脚本执行被 PowerShell 执行策略挡住，可以在当前窗口临时放开：

```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
irm https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.ps1 | iex
```

默认安装到 `~\.opencode2api\bin\opencode2api.exe`，并把该目录加入当前用户的 `Path`。

## 固定版本、自定义仓库和目录

安装脚本默认读取 `6Kmfi6HP/opencode2api` 的最新 Release。发布新版本时，用户可以直接固定到某个 tag。

### Unix 参数 / 环境变量

| 参数 | 环境变量 | 说明 |
| --- | --- | --- |
| `--repo <owner/repo>` | `OPENCODE2API_REPO` | GitHub 仓库 |
| `--version <tag>` | `OPENCODE2API_VERSION` | Release tag，例如 `v0.6.0` |
| `--install-dir <path>` | `OPENCODE2API_INSTALL_DIR` | 安装目录 |
| `--no-modify-path` | | 不修改 shell 启动文件 |
| `--check` | | 只打印检测结果和下载地址，不安装 |

示例：

```bash
curl -fsSL https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.sh | bash -s -- --version v0.6.0

OPENCODE2API_VERSION=v0.6.0 OPENCODE2API_INSTALL_DIR="$HOME/bin" bash install.sh --no-modify-path
```

发布前用于检查资产名和下载地址：

```bash
OPENCODE2API_VERSION=v0.6.0 bash scripts/install.sh --check
```

### Windows 参数

```powershell
# 只打印检测结果和下载地址
irm https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.ps1 | iex -Version v0.6.0 -CheckOnly

# 固定版本，不修改用户 PATH
& C:\path\to\install.ps1 -Version v0.6.0 -InstallDir "$HOME\.opencode2api\bin" -NoModifyPath
```

支持的参数：

- `-Version <tag>`
- `-Repo <owner/repo>`
- `-InstallDir <path>`
- `-NoModifyPath`
- `-CheckOnly`
- `-Help`

对应环境变量为 `OPENCODE2API_VERSION`、`OPENCODE2API_REPO`、`OPENCODE2API_INSTALL_DIR`。

## Release 资产格式

安装脚本依赖以下 Release 资产命名，发布版本时不要改变 `scripts/build-release.sh` 生成的格式：

```text
opencode2api_<version>_<goos>_<goarch>.tar.gz
opencode2api_<version>_<goos>_<goarch>_<variant>.tar.gz
checksums.txt
```

当前发布目标：

- `opencode2api_v0.6.0_linux_amd64.tar.gz`
- `opencode2api_v0.6.0_linux_arm64.tar.gz`
- `opencode2api_v0.6.0_linux_arm_v7.tar.gz`
- `opencode2api_v0.6.0_darwin_amd64.tar.gz`
- `opencode2api_v0.6.0_darwin_arm64.tar.gz`
- `opencode2api_v0.6.0_windows_amd64.tar.gz`
- `opencode2api_v0.6.0_windows_arm64.tar.gz`
- `opencode2api_v0.6.0_freebsd_amd64.tar.gz`
- `opencode2api_v0.6.0_freebsd_arm64.tar.gz`

## 安装后启动 launch

安装脚本安装的是 `opencode2api` 代理和 `launch` 子命令。`launch claude` / `launch codex` 仍会启动本机已经安装的 CLI，因此还需要对应 CLI 可用：

- `claude`：`npm install -g @anthropic-ai/claude-code`
- `codex`：从 OpenAI Codex 官方安装，或确保 `codex` 在 `PATH` 中

Windows 下 `opencode2api launch` 已支持 npm 全局安装产生的 `.cmd/.bat` shim，也会查找常见用户目录和 `%APPDATA%\npm` 下的 `claude.cmd` / `codex.cmd`。具体平台行为见 `README` 的 launch 章节和 `internal/app/launch_*.go`。

安装成功且 PATH 生效后：

```bash
# 交互式选择免费模型
opencode2api launch claude
opencode2api launch codex

# 直接指定模型
opencode2api launch claude --model hy3
opencode2api launch codex --model hy3

# 透传子 CLI 参数
opencode2api launch claude -- --dangerously-skip-permissions
opencode2api launch codex -- --skip-git-repo-check
```

## 验证安装

```bash
opencode2api -version
```

Windows：

```powershell
opencode2api.exe -version
```

如果命令不存在，请先确认安装目录已在 `PATH` 中，或重启当前 shell / PowerShell。

## 手动安装

如果不想使用脚本，也可以从 [Releases](https://github.com/6Kmfi6HP/opencode2api/releases) 下载当前平台对应的 `.tar.gz`，解压后自行放置 `opencode2api` / `opencode2api.exe` 到已在 `PATH` 中的目录。

```bash
tar -xzf opencode2api_v0.6.0_linux_amd64.tar.gz
./opencode2api_v0.6.0_linux_amd64/opencode2api -version
```

Windows：

```powershell
tar -xzf .\opencode2api_v0.6.0_windows_amd64.tar.gz
.\opencode2api_v0.6.0_windows_amd64\opencode2api.exe -version
```

## 发布时检查清单

发布新版本前，建议执行：

```bash
OPENCODE2API_VERSION=<new-tag> bash scripts/install.sh --check
TARGETS="linux/amd64 windows/amd64" CHECKSUMS=false ./scripts/build-release.sh
```

Windows 上执行：

```powershell
.\scripts\install.ps1 -Version <new-tag> -CheckOnly
```

目标都是确认资产名与 `scripts/build-release.sh` 输出一致，避免安装脚本指向不存在的 Release 包。
