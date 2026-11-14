#!/usr/bin/env bash
# 交叉编译所有目标平台，产物在 dist/ 下。
set -euo pipefail
cd "$(dirname "$0")/.."

VER="${VER:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.version=${VER}"
OUT=dist
rm -rf "$OUT"; mkdir -p "$OUT"

targets=(
  "linux   amd64"
  "linux   arm64"
  "linux   arm     6"
  "darwin  amd64"
  "darwin  arm64"
  "windows amd64"
)

for t in "${targets[@]}"; do
  set -- $t
  os=$1; arch=$2; arm=${3:-}
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  name="beian2api_${VER}_${os}_${arch}${arm:+v$arm}"
  echo "→ $name"
  env CGO_ENABLED=0 GOOS=$os GOARCH=$arch ${arm:+GOARM=$arm} \
    go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/$name$ext" .
done

echo
ls -lh "$OUT"
