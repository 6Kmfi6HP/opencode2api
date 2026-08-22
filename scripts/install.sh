#!/usr/bin/env bash
set -euo pipefail

REPO="${OPENCODE2API_REPO:-6Kmfi6HP/opencode2api}"
VERSION="${OPENCODE2API_VERSION:-}"
INSTALL_DIR="${OPENCODE2API_INSTALL_DIR:-$HOME/.opencode2api/bin}"
MODIFY_PATH=1
CHECK_ONLY=0

usage() {
  cat <<'EOF'
One-click installer for opencode2api

Usage:
  curl -fsSL https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.sh | bash

Options:
  --repo <owner/repo>       GitHub repository to install from
  --version <version>       Release tag, for example v0.5.0
  --install-dir <path>      Binary install directory (default: ~/.opencode2api/bin)
  --no-modify-path          Do not append the install directory to shell startup files
  --check                   Print detected OS/arch/download URL without installing
  -h, --help                Show this help

Environment:
  OPENCODE2API_REPO         Same as --repo
  OPENCODE2API_VERSION      Same as --version
  OPENCODE2API_INSTALL_DIR  Same as --install-dir

After installation, run:
  opencode2api launch claude
  opencode2api launch codex
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      REPO="${2:-}"; shift 2 ;;
    --version)
      VERSION="${2:-}"; shift 2 ;;
    --install-dir)
      INSTALL_DIR="${2:-}"; shift 2 ;;
    --no-modify-path)
      MODIFY_PATH=0; shift ;;
    --check)
      CHECK_ONLY=1; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      echo "unsupported argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "${REPO:-}" ]] || { echo "missing repository" >&2; exit 2; }
[[ -n "${INSTALL_DIR:-}" ]] || { echo "missing install directory" >&2; exit 2; }

# --- utility functions -------------------------------------------------------

info() { printf '\033[1;34m[opencode2api]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[opencode2api]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[opencode2api]\033[0m %s\n' "$*" >&2; exit 1; }

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

require_cmd() {
  has_cmd "$1" || die "required command not found: $1"
}

# ---------------- platform detection ---------------------------------------

case "$(uname -s)" in
  Linux*)   GOOS="linux" ;;
  Darwin*)  GOOS="darwin" ;;
  FreeBSD*) GOOS="freebsd" ;;
  *) die "unsupported OS: $(uname -s). Supported: Linux, macOS, FreeBSD." ;;
esac

raw_arch="$(uname -m)"
case "$raw_arch" in
  x86_64|amd64|AMD64)
    GOARCH="amd64"; SUFFIX="" ;;
  aarch64|arm64|ARM64)
    case "$GOOS" in
      linux|darwin|freebsd) GOARCH="arm64"; SUFFIX="" ;;
      *) die "unsupported architecture on $GOOS: $raw_arch" ;;
    esac ;;
  armv7l|armv7|armhf)
    [[ "$GOOS" == "linux" ]] || die "unsupported architecture on $GOOS: $raw_arch"
    GOARCH="arm"; SUFFIX="_v7" ;;
  *)
    die "unsupported architecture: $raw_arch. Supported: x86_64/amd64, arm64/aarch64, armv7l." ;;
esac

if [[ "$SUFFIX" == "_v7" ]]; then
  TARGET="$GOOS/arm/v7"
else
  TARGET="$GOOS/$GOARCH"
fi
ASSET="opencode2api_${VERSION:-<version>}_${GOOS}_${GOARCH}${SUFFIX}.tar.gz"

if [[ "$CHECK_ONLY" == "1" ]]; then
  echo "os=$GOOS"
  echo "arch=$raw_arch"
  echo "target=$TARGET"
  if [[ -n "$VERSION" ]]; then
    echo "asset=$ASSET"
    echo "download=https://github.com/$REPO/releases/download/$VERSION/$ASSET"
    echo "checksums=https://github.com/$REPO/releases/download/$VERSION/checksums.txt"
  else
    echo "version=latest (will be resolved from GitHub API)"
    echo "asset=$ASSET (version will be filled at runtime)"
    echo "release_api=https://api.github.com/repos/$REPO/releases/latest"
  fi
  exit 0
fi

require_cmd uname
if command -v curl >/dev/null 2>&1; then
  GET="curl -fsSL"
elif command -v wget >/dev/null 2>&1; then
  GET="wget -qO-"
else
  die "required downloader not found: curl or wget"
fi
require_cmd tar
require_cmd mktemp
if ! has_cmd sha256sum && ! has_cmd shasum; then
  die "required checksum command not found: sha256sum or shasum"
fi

# ---------------- latest version -------------------------------------------

if [[ -z "${VERSION:-}" ]]; then
  info "resolving latest release from $REPO"
  api_json="$($GET "https://api.github.com/repos/$REPO/releases/latest")"
  VERSION="$(printf '%s' "$api_json" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  [[ -n "$VERSION" ]] || die "could not resolve latest release; use --version <tag>"
fi

ASSET="opencode2api_${VERSION}_${GOOS}_${GOARCH}${SUFFIX}.tar.gz"
TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/opencode2api-install.XXXXXX")"
trap 'rm -rf "$TMPDIR"' EXIT

download() {
  local url="$1" out="$2"
  info "downloading $url"
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --retry-delay 1 -o "$out" "$url"
  else
    wget -q --tries=3 --timeout=30 -O "$out" "$url"
  fi
}

download "https://github.com/$REPO/releases/download/$VERSION/$ASSET" "$TMPDIR/$ASSET"
download "https://github.com/$REPO/releases/download/$VERSION/checksums.txt" "$TMPDIR/checksums.txt"

expected="$(grep -F "$ASSET" "$TMPDIR/checksums.txt" | awk '{print $1}' | head -n1)"
[[ -n "$expected" ]] || die "checksums.txt does not contain $ASSET"

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$TMPDIR/$ASSET" | awk '{print $1}')"
else
  actual="$(shasum -a 256 "$TMPDIR/$ASSET" | awk '{print $1}')"
fi

[[ "$expected" == "$actual" ]] || die "checksum mismatch for $ASSET"
info "checksum verified"

tar -xzf "$TMPDIR/$ASSET" -C "$TMPDIR"
SRC_DIR="$TMPDIR/opencode2api_${VERSION}_${GOOS}_${GOARCH}${SUFFIX}"
BIN_SRC="$SRC_DIR/opencode2api"
[[ -f "$BIN_SRC" ]] || die "release archive did not contain opencode2api"

mkdir -p "$INSTALL_DIR"
cp "$BIN_SRC" "$INSTALL_DIR/opencode2api"
chmod 0755 "$INSTALL_DIR/opencode2api"

# ---------------- PATH hook -------------------------------------------------

if [[ "$MODIFY_PATH" == "1" ]]; then
  PROFILE=""
  case "$SHELL" in
    */zsh) PROFILE="$HOME/.zshrc" ;;
    */bash) PROFILE="$HOME/.bashrc" ;;
  esac
  if [[ -z "$PROFILE" && -f "$HOME/.profile" ]]; then
    PROFILE="$HOME/.profile"
  elif [[ -z "$PROFILE" ]]; then
    PROFILE="$HOME/.bashrc"
  fi

  if ! grep -Fq "opencode2api installer" "$PROFILE" 2>/dev/null; then
    mkdir -p "$(dirname "$PROFILE")"
    {
      printf '\n# opencode2api installer\n'
      printf 'export PATH="%s:$PATH"\n' "$INSTALL_DIR"
    } >> "$PROFILE"
    info "PATH hook added to $PROFILE"
  else
    info "PATH hook already present in $PROFILE"
  fi
fi

info "installed opencode2api $VERSION to $INSTALL_DIR/opencode2api"

if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
  warn "start a new shell, or add this directory to PATH: $INSTALL_DIR"
fi

cat <<EOF

Next steps:

  opencode2api launch claude
  opencode2api launch codex

For Windows, use the PowerShell installer:

  irm https://raw.githubusercontent.com/$REPO/main/scripts/install.ps1 | iex
EOF
