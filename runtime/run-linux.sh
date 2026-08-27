#!/bin/bash
# Portable LLM launcher — Linux (x86_64, CPU build)
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$DIR/bin/linux-x64/llama-server"
MODELS="$DIR/models"; UI="$DIR/ui"; CTX=8192

cd "$DIR"; clear
echo "  Portable LLM"
echo "  ─────────────────────────────────────────"

if [ ! -x "$BIN" ]; then
  chmod +x "$BIN" 2>/dev/null
  [ -x "$BIN" ] || { echo "  ✗ Cannot execute $BIN"
    echo "    The drive may be mounted noexec. Remount with:"
    echo "      sudo mount -o remount,exec \"$(df --output=target "$DIR" | tail -1)\""
    echo; read -r -p "  Press Return to close."; exit 1; }
fi

RAM=$(( $(awk '/MemTotal/{print $2}' /proc/meminfo) * 1024 ))
RAM_GB=$((RAM / 1024 / 1024 / 1024))
BUDGET=$((RAM * 6 / 10))

BEST=""; BEST_SZ=0
shopt -s nullglob
for f in "$MODELS"/*.gguf; do
  sz=$(stat -c%s "$f")
  if [ "$sz" -le "$BUDGET" ] && [ "$sz" -gt "$BEST_SZ" ]; then BEST="$f"; BEST_SZ=$sz; fi
done
shopt -u nullglob

[ -n "$BEST" ] || { echo "  ✗ No model fits in ${RAM_GB}GB of RAM."; echo
  read -r -p "  Press Return to close."; exit 1; }

PORT=8080
while ss -ltn 2>/dev/null | grep -q ":$PORT " ; do PORT=$((PORT+1)); done

echo "  Machine : ${RAM_GB}GB RAM · CPU inference"
echo "  Model   : $(basename "$BEST") ($((BEST_SZ/1024/1024/1024))GB)"
echo "  Address : http://127.0.0.1:$PORT"
echo "  ─────────────────────────────────────────"; echo

"$BIN" -m "$BEST" --host 127.0.0.1 --port "$PORT" --path "$UI" \
       -c "$CTX" -t "$(nproc)" --no-warmup --cors-origins localhost > "$DIR/logs/server.log" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null' EXIT INT TERM

for _ in $(seq 1 600); do
  if curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    echo "  ✓ Ready — opening browser."
    (xdg-open "http://127.0.0.1:$PORT" >/dev/null 2>&1 &) ; break
  fi
  kill -0 $PID 2>/dev/null || { echo "  ✗ Server exited:"; tail -15 "$DIR/logs/server.log"
    echo; read -r -p "  Press Return to close."; exit 1; }
  sleep 1
done
wait $PID
