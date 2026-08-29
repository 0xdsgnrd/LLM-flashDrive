#!/bin/bash
# Static linux-x64 build, inside Docker. Container is the compiler, not the runtime.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/dist/linux-x64"; mkdir -p "$OUT"
docker build --platform linux/amd64 -f "$ROOT/docker/Dockerfile.linux-build" \
       --build-arg LLAMA_REF="${LLAMA_REF:-master}" -t llmusb/build-linux "$ROOT"
docker run --rm --platform linux/amd64 -v "$OUT:/out" llmusb/build-linux
file "$OUT/llama-server"
echo "✓ linux-x64 build complete → $OUT"
