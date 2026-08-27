#!/bin/bash
# Native Apple Silicon build — Metal enabled. This is the ONLY build that runs
# on the GPU; the Linux/Windows builds are CPU-only. See README "GPU wall".
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/dist/mac-arm64"; SRC="$ROOT/.cache/llama.cpp"
REF="${LLAMA_REF:-master}"

command -v cmake >/dev/null || { echo "cmake missing. Install with: brew install cmake"; exit 1; }

# --- Command Line Tools health check -----------------------------------
# A damaged CLT install leaves a stale, near-empty libc++ header dir at
# <CLT>/usr/include/c++/v1 while the real headers sit in the SDK. clang
# searches the former, so every C++ build fails with "'array' file not found".
# Detect it and point clang at the SDK copy.
EXTRA_CXX=""
probe="$(mktemp -t cxxprobe).cpp"
printf '#include <array>\nint main(){return std::array<int,1>{0}[0];}\n' > "$probe"
if ! clang++ -fsyntax-only "$probe" >/dev/null 2>&1; then
  SDK_CXX="$(xcrun --show-sdk-path)/usr/include/c++/v1"
  if [ -f "$SDK_CXX/array" ] && clang++ -cxx-isystem "$SDK_CXX" -fsyntax-only "$probe" >/dev/null 2>&1; then
    EXTRA_CXX="-cxx-isystem $SDK_CXX"
    echo "⚠  Command Line Tools libc++ headers are broken."
    echo "   Working around it with: $EXTRA_CXX"
    echo "   Permanent fix (needs your password, opens Apple's installer):"
    echo "     sudo rm -rf /Library/Developer/CommandLineTools"
    echo "     sudo xcode-select --install"
    echo
  else
    echo "✗ clang++ cannot compile C++ and the SDK fallback did not help."
    echo "  Reinstall the Command Line Tools:"
    echo "    sudo rm -rf /Library/Developer/CommandLineTools && sudo xcode-select --install"
    rm -f "$probe"; exit 1
  fi
fi
rm -f "$probe"

mkdir -p "$OUT" "$(dirname "$SRC")"
[ -d "$SRC" ] || git clone --depth 1 --branch "$REF" https://github.com/ggml-org/llama.cpp "$SRC"
cd "$SRC" && git fetch --depth 1 origin "$REF" && git checkout -q FETCH_HEAD

rm -rf build   # previous configure may have cached the broken toolchain state
cmake -B build -DCMAKE_BUILD_TYPE=Release -DGGML_METAL=ON -DGGML_METAL_EMBED_LIBRARY=ON \
      -DLLAMA_CURL=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF \
      -DBUILD_SHARED_LIBS=OFF \
      -DLLAMA_OPENSSL=OFF \
      ${EXTRA_CXX:+-DCMAKE_CXX_FLAGS="$EXTRA_CXX"}
cmake --build build --config Release -j"$(sysctl -n hw.ncpu)" --target llama-server

cp -v build/bin/llama-server "$OUT/"
codesign --force --sign - "$OUT/llama-server"       # ad-hoc sign; arm64 requires it
echo "✓ mac-arm64 build complete → $OUT"
