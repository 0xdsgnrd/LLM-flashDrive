#!/bin/bash
# pocketd for all three platforms, inside Docker. Container is the compiler, not
# the runtime — same rule as the llama.cpp builds, and the reason none of this
# needs Go installed on the machine you are sitting at.
#
# Go makes portability cheap here in a way C++ did not: CGO_ENABLED=0 produces a
# binary with no libc dependency at all, so the glibc-version and libgomp traps
# that shaped the llama.cpp builds simply do not apply.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_IMAGE="${GO_IMAGE:-golang:1.24-alpine}"
VERSION="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)"
git -C "$ROOT" diff --quiet 2>/dev/null || VERSION="$VERSION-dirty"

docker run --rm -v "$ROOT/helper:/src" -v "$ROOT/dist:/out" -w /src \
  -e CGO_ENABLED=0 -e GOFLAGS=-trimpath -e GOFLAGS_NOSUMDB=1 "$GO_IMAGE" sh -euc '
    echo "── fmt + vet + test ──"
    unformatted=$(gofmt -l *.go); [ -z "$unformatted" ] || { echo "gofmt needed: $unformatted"; exit 1; }
    go vet -mod=vendor ./...
    go test -mod=vendor ./...
    echo "── build ──"
    for t in "darwin arm64 mac-arm64 pocketd" \
             "linux  amd64 linux-x64 pocketd" \
             "windows amd64 win-x64  pocketd.exe"; do
      set -- $t
      mkdir -p "/out/$3"
      GOOS=$1 GOARCH=$2 go build -mod=vendor -ldflags "-s -w -X main.version='"$VERSION"'" -o "/out/$3/$4" .
      echo "  ✓ $3/$4"
    done
  '

# arm64 macOS refuses to run an unsigned binary. Same ad-hoc signature the
# llama-server build applies, and it survives the copy to exFAT.
if [ -f "$ROOT/dist/mac-arm64/pocketd" ]; then
  codesign --force --sign - "$ROOT/dist/mac-arm64/pocketd"
  echo "  ✓ ad-hoc signed mac-arm64/pocketd"
fi

echo
for p in mac-arm64/pocketd linux-x64/pocketd win-x64/pocketd.exe; do
  [ -f "$ROOT/dist/$p" ] && printf '  %-24s %s\n' "$p" "$(du -h "$ROOT/dist/$p" | cut -f1)"
done
echo "✓ pocketd $VERSION built → dist/"
