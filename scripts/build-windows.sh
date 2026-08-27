#!/bin/bash
# Cross-compiled win-x64 build via mingw-w64, inside Docker.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/dist/win-x64"; mkdir -p "$OUT"
docker build --platform linux/amd64 -f "$ROOT/docker/Dockerfile.windows-build" \
       --build-arg LLAMA_REF="${LLAMA_REF:-master}" -t llmusb/build-windows "$ROOT"
docker run --rm --platform linux/amd64 -v "$OUT:/out" llmusb/build-windows
file "$OUT/llama-server.exe"
echo "✓ win-x64 build complete → $OUT"
