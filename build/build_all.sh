#!/usr/bin/env bash
# Build the C2 control end and the Go controlled end for multiple platforms.
# Fully static, CGO disabled, zero external runtime dependencies.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

OUT="dist"
mkdir -p "$OUT/control" "$OUT/remote-go"

export CGO_ENABLED=0
TAGS="netgo,osusergo"
LDFLAGS="-s -w"
CONTROL_PKG="./cmd/c2agent"
REMOTE_DIR="remote/remote-go"

build() {
  local goos=$1 goarch=$2 ext=$3
  echo ">> control    $goos/$goarch"
  GOOS="$goos" GOARCH="$goarch" go build -trimpath -tags "$TAGS" -ldflags "$LDFLAGS" \
    -o "$ROOT/$OUT/control/c2agent_${goos}_${goarch}${ext}" "$CONTROL_PKG"

  echo ">> remote-go  $goos/$goarch"
  ( cd "$ROOT/$REMOTE_DIR" && GOOS="$goos" GOARCH="$goarch" go build -trimpath -tags "$TAGS" -ldflags "$LDFLAGS" \
      -o "$ROOT/$OUT/remote-go/irudo_remote_${goos}_${goarch}${ext}" . )
}

# Requested targets
build windows amd64 ".exe"   # win x86-64
build linux   amd64 ""       # linux amd64
build linux   386   ""       # linux x86 (32-bit)
build linux   arm64 ""       # linux arm64
# Extra common target
build windows 386   ".exe"   # win x86 (32-bit)

echo
echo "done -> $ROOT/$OUT"
ls -l "$OUT/control" "$OUT/remote-go"
