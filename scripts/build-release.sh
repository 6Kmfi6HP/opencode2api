#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-"$ROOT_DIR/dist"}"
VERSION="${VERSION:-$(git -C "$ROOT_DIR" describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || echo none)}"
DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

targets=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm/v7"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
  "freebsd/amd64"
  "freebsd/arm64"
)

rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

for target in "${targets[@]}"; do
  IFS=/ read -r goos goarch variant <<< "$target"
  suffix=""
  env_vars=("CGO_ENABLED=0" "GOOS=$goos" "GOARCH=$goarch")

  if [[ -n "${variant:-}" ]]; then
    case "$variant" in
      v*) env_vars+=("GOARM=${variant#v}"); suffix="_${variant}" ;;
      *) echo "unsupported target variant: $target" >&2; exit 1 ;;
    esac
  fi

  asset="opencode2api_${VERSION}_${goos}_${goarch}${suffix}"
  work_dir="$DIST_DIR/$asset"
  binary="opencode2api"
  if [[ "$goos" == "windows" ]]; then
    binary="opencode2api.exe"
  fi

  mkdir -p "$work_dir"
  echo "building $target"
  (
    cd "$ROOT_DIR"
    env "${env_vars[@]}" go build \
      -trimpath \
      -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
      -o "$work_dir/$binary" .
  )

  cp "$ROOT_DIR/README.md" "$ROOT_DIR/config.example.json" "$work_dir/"
  if [[ -f "$ROOT_DIR/LICENSE" ]]; then
    cp "$ROOT_DIR/LICENSE" "$work_dir/"
  fi
  tar -C "$DIST_DIR" -czf "$DIST_DIR/$asset.tar.gz" "$asset"
  rm -rf "$work_dir"
done

(
  cd "$DIST_DIR"
  sha256sum ./*.tar.gz > checksums.txt
)

echo "release artifacts written to $DIST_DIR"
