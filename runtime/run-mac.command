#!/bin/bash
# Portable LLM launcher — macOS (Apple Silicon)
# Picks the largest model that fits in RAM, serves UI + API on one origin.
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$DIR/bin/mac-arm64/llama-server"
MODELS="$DIR/models"
UI="$DIR/ui"
CTX=8192

cd "$DIR"
clear
echo "  Portable LLM"
echo "  ─────────────────────────────────────────"

[ -x "$BIN" ] || { echo "  ✗ Missing binary: $BIN"; echo; read -r -p "  Press Return to close."; exit 1; }

# Gatekeeper: clear quarantine on first run (harmless if already clear).
xattr -dr com.apple.quarantine "$DIR/bin" 2>/dev/null || true

# --- pick model by available RAM ---------------------------------------
RAM=$(sysctl -n hw.memsize)
RAM_GB=$((RAM / 1024 / 1024 / 1024))
BUDGET=$((RAM * 6 / 10))          # leave 40% for KV cache, OS, apps

BEST=""; BEST_SZ=0
shopt -s nullglob
for f in "$MODELS"/*.gguf; do
  sz=$(stat -f%z "$f")
  if [ "$sz" -le "$BUDGET" ] && [ "$sz" -gt "$BEST_SZ" ]; then BEST="$f"; BEST_SZ=$sz; fi
done
shopt -u nullglob

if [ -z "$BEST" ]; then
  echo "  ✗ No model fits in ${RAM_GB}GB of RAM (budget $((BUDGET/1024/1024/1024))GB)."
  echo "    Models present:"
  ls -1sh "$MODELS"/*.gguf 2>/dev/null | sed 's/^/      /' || echo "      (none)"
  echo; read -r -p "  Press Return to close."; exit 1
fi

# --- find a free port --------------------------------------------------
PORT=8080
while lsof -iTCP:$PORT -sTCP:LISTEN >/dev/null 2>&1; do PORT=$((PORT+1)); done

echo "  Machine : ${RAM_GB}GB RAM · Apple Silicon (Metal)"
echo "  Model   : $(basename "$BEST") ($((BEST_SZ/1024/1024/1024))GB)"
echo "  Address : http://127.0.0.1:$PORT"
echo "  ─────────────────────────────────────────"
echo "  Loading… first launch reads the model off the drive."
echo "  Close this window to shut down."
echo

"$BIN" -m "$BEST" --host 127.0.0.1 --port "$PORT" --path "$UI" \
       -c "$CTX" -ngl 999 --no-warmup --cors-origins localhost > "$DIR/logs/server.log" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null' EXIT INT TERM

# wait for readiness, then open the browser
for _ in $(seq 1 600); do
  if curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    echo "  ✓ Ready — opening browser."
    open "http://127.0.0.1:$PORT"
    break
  fi
  kill -0 $PID 2>/dev/null || { echo "  ✗ Server exited. Last lines:"; tail -15 "$DIR/logs/server.log"; echo; read -r -p "  Press Return to close."; exit 1; }
  sleep 1
done

wait $PID
