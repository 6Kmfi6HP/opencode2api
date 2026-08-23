#!/usr/bin/env bash
# 自动化版本发布脚本
#
# 用法:
#   ./scripts/release.sh patch          # 自动升级补丁版本 (例如 v0.7.0 -> v0.7.1)
#   ./scripts/release.sh minor          # 自动升级次版本 (例如 v0.7.0 -> v0.8.0)
#   ./scripts/release.sh major          # 自动升级主版本 (例如 v0.7.0 -> v1.0.0)
#   ./scripts/release.sh v0.8.0         # 发布指定版本号
#   ./scripts/release.sh minor -n       # 干跑预览 (Dry-run), 只进行校验不创建或推送 tag
#   ./scripts/release.sh --help         # 查看帮助说明

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

DRY_RUN=0
ALLOW_DIRTY=0
SKIP_TESTS=0
TARGET_VERSION=""

# 获取最新 tag
LATEST_TAG="$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")"

# 解析 SemVer
parse_semver() {
  local tag="$1"
  local raw="${tag#v}"
  local major minor patch
  IFS='.' read -r major minor patch <<< "$raw"
  patch="${patch%%-*}"
  patch="${patch%%+*}"
  major="${major:-0}"
  minor="${minor:-0}"
  patch="${patch:-0}"
  echo "$major $minor $patch"
}

read -r CUR_MAJOR CUR_MINOR CUR_PATCH <<< "$(parse_semver "$LATEST_TAG")"
NEXT_PATCH="v${CUR_MAJOR}.${CUR_MINOR}.$((CUR_PATCH + 1))"
NEXT_MINOR="v${CUR_MAJOR}.$((CUR_MINOR + 1)).0"
NEXT_MAJOR="v$((CUR_MAJOR + 1)).0.0"

show_help() {
  echo "opencode2api 自动化版本发布工具"
  echo ""
  echo "当前最新版本 Tag: $LATEST_TAG"
  echo "候选版本建议:"
  echo "  patch : $NEXT_PATCH"
  echo "  minor : $NEXT_MINOR"
  echo "  major : $NEXT_MAJOR"
  echo ""
  echo "用法:"
  echo "  $0 <patch|minor|major|vX.Y.Z> [选项]"
  echo ""
  echo "选项:"
  echo "  -n, --dry-run      仅执行预检并打印操作，不实际打 tag 或 push"
  echo "  --allow-dirty      允许工作区存在未提交的更改"
  echo "  --skip-tests       跳过 go test / go vet 预检"
  echo "  -h, --help         显示此帮助信息"
  exit 0
}

# 解析命令行参数
while [[ $# -gt 0 ]]; do
  case "$1" in
    patch)
      TARGET_VERSION="$NEXT_PATCH"
      shift
      ;;
    minor)
      TARGET_VERSION="$NEXT_MINOR"
      shift
      ;;
    major)
      TARGET_VERSION="$NEXT_MAJOR"
      shift
      ;;
    v[0-9]*)
      TARGET_VERSION="$1"
      shift
      ;;
    [0-9]*)
      TARGET_VERSION="v$1"
      shift
      ;;
    -n|--dry-run)
      DRY_RUN=1
      shift
      ;;
    --allow-dirty)
      ALLOW_DIRTY=1
      shift
      ;;
    --skip-tests)
      SKIP_TESTS=1
      shift
      ;;
    -h|--help)
      show_help
      ;;
    *)
      echo "错误: 未知参数 '$1'" >&2
      show_help
      ;;
  esac
done

if [[ -z "$TARGET_VERSION" ]]; then
  echo "错误: 未指定发布版本号。" >&2
  show_help
fi

# 检查 tag 是否已存在
if git rev-parse "$TARGET_VERSION" >/dev/null 2>&1; then
  echo "错误: Git tag '$TARGET_VERSION' 已经存在！" >&2
  exit 1
fi

echo "=================================================="
echo "准备发布版本: $TARGET_VERSION (当前最新: $LATEST_TAG)"
if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "模式: [DRY-RUN 干跑预览 (不会修改 Git 状态)]"
fi
echo "=================================================="

# 1. 检查未提交修改
DIRTY_FILES="$(git status --porcelain | grep -v 'agent-browser.json' | grep -v 'opencode2api.log' | grep -v 'modelsdev_cache.json' | grep -v 'stats.json' || true)"
if [[ -n "$DIRTY_FILES" ]] && [[ "$ALLOW_DIRTY" -eq 0 ]]; then
  echo "警告: 检测到工作区有未提交的文件:"
  echo "$DIRTY_FILES"
  if [[ "$DRY_RUN" -eq 0 ]]; then
    echo "请先提交更改或使用 --allow-dirty 标志继续。" >&2
    exit 1
  fi
fi

# 2. 检查 CHANGELOG.md 是否记录了该版本
if [[ -f "CHANGELOG.md" ]]; then
  if ! grep -q "## ${TARGET_VERSION}" CHANGELOG.md; then
    echo "提示: CHANGELOG.md 中未找到 '## ${TARGET_VERSION}' 章节，建议在发布前补充更新日志。"
  fi
fi

# 3. 预检测试
if [[ "$SKIP_TESTS" -eq 0 ]]; then
  echo "==> 执行预检: gofmt / go vet / go test ..."
  FMT_ISSUES="$(gofmt -l ./cmd ./internal)"
  if [[ -n "$FMT_ISSUES" ]]; then
    echo "错误: 代码格式不符合 gofmt 规范:"
    echo "$FMT_ISSUES"
    exit 1
  fi
  go vet ./...
  go test ./...
  echo "==> 预检全部通过！"
fi

if [[ "$DRY_RUN" -eq 1 ]]; then
  echo ""
  echo "=================================================="
  echo "DRY-RUN 检查完成！若正式执行，将会执行以下操作:"
  echo "  1. git tag -a \"$TARGET_VERSION\" -m \"Release $TARGET_VERSION\""
  echo "  2. git push origin main"
  echo "  3. git push origin \"$TARGET_VERSION\""
  echo "=================================================="
  exit 0
fi

echo ""
echo "==> 正在创建 Git Tag: $TARGET_VERSION ..."
git tag -a "$TARGET_VERSION" -m "Release $TARGET_VERSION"

echo "==> 正在推送分支与 Tag 到远程仓库 ..."
CURRENT_BRANCH="$(git branch --show-current 2>/dev/null || echo "main")"
git push origin "$CURRENT_BRANCH"
git push origin "$TARGET_VERSION"

echo ""
echo "=================================================="
echo "发布成功！Tag $TARGET_VERSION 已推送至远程仓库。"
echo "GitHub Actions 将自动开始编译并创建 GitHub Release。"
echo "=================================================="
