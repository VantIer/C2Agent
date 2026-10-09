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

# The remote agent is an independent module; `go vet ./...` from the repo root
# does not cover it, so lint it explicitly.
echo ">> vet $REMOTE_DIR"
( cd "$REMOTE_DIR" && go vet ./... )

# ---- tests ---------------------------------------------------------------
echo ">> test control end"
go test ./...

echo ">> test $REMOTE_DIR"
( cd "$REMOTE_DIR" && go test ./... )

if command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1; then
  CC_BIN="$(command -v cc || command -v gcc)"
  echo ">> test remote-c (self-tests)"
  (
    cd remote/remote-c
    "$CC_BIN" -O2 -Wall -Wextra -o /tmp/c2a_crypto_selftest tests/crypto_selftest.c -lpthread
    "$CC_BIN" -O2 -Wall -Wextra -o /tmp/c2a_edit_selftest tests/edit_selftest.c protocol.c -lpthread
    /tmp/c2a_crypto_selftest
    /tmp/c2a_edit_selftest
    rm -f /tmp/c2a_crypto_selftest /tmp/c2a_edit_selftest
  )
else
  echo ">> remote-c self-tests skipped (no C compiler)"
fi

if command -v python3 >/dev/null 2>&1; then
  echo ">> test remote-py"
  python3 remote/common/test_crypto.py
  python3 remote/remote-py/test_local_executor.py
else
  echo ">> remote-py tests skipped (no python3)"
fi

build() {
  local goos=$1 goarch=$2 ext=$3
  echo ">> control    $goos/$goarch"
  GOOS="$goos" GOARCH="$goarch" go build -trimpath -tags "$TAGS" -ldflags "$LDFLAGS" \
    -o "$ROOT/$OUT/control/c2agent_${goos}_${goarch}${ext}" "$CONTROL_PKG"

  echo ">> remote-go  $goos/$goarch"
  ( cd "$ROOT/$REMOTE_DIR" && GOOS="$goos" GOARCH="$goarch" go build -trimpath -tags "$TAGS" -ldflags "$LDFLAGS" \
      -o "$ROOT/$OUT/remote-go/c2agent_remote_${goos}_${goarch}${ext}" . )
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
