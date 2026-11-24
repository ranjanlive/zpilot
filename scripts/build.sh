#!/usr/bin/env sh
# Cross-compile zpilot for all supported platforms into dist/.
set -eu
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
cd "$(dirname "$0")/.."
mkdir -p dist
for target in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%/*}"; arch="${target#*/}"
  out="dist/zpilot-${VERSION}-${os}-${arch}"
  [ "$os" = windows ] && out="$out.exe"
  echo "building $out"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$out" ./cmd/zpilot
done
( cd dist && sha256sum zpilot-* > SHA256SUMS.txt )
echo "done: dist/"
