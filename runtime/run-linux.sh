#!/bin/bash
# Portable LLM launcher — Linux (x86_64, CPU build), router mode.
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
B_PCT=$((RAM * 7 / 10)); B_ABS=$((RAM - 4 * 1024 * 1024 * 1024))
BUDGET=$(( B_PCT < B_ABS ? B_PCT : B_ABS ))

shopt -s nullglob
FILES=("$MODELS"/*.gguf)
shopt -u nullglob
[ ${#FILES[@]} -gt 0 ] || { echo "  ✗ No .gguf files in $MODELS"; echo
  read -r -p "  Press Return to close."; exit 1; }

# See run-mac.command: --models-max default of 4 is fatal on small machines.
FITTING=0; SUM=0; MAXN=0
while read -r sz; do
  [ "$sz" -le "$BUDGET" ] || continue
  FITTING=$((FITTING + 1))
  if [ $((SUM + sz)) -le "$BUDGET" ]; then SUM=$((SUM + sz)); MAXN=$((MAXN + 1)); fi
done < <(for f in "${FILES[@]}"; do stat -c%s "$f"; done | sort -nr)

[ "$FITTING" -gt 0 ] || { echo "  ✗ No model fits in ${RAM_GB}GB of RAM."; echo
  read -r -p "  Press Return to close."; exit 1; }
[ "$MAXN" -ge 1 ] || MAXN=1

{
  printf '{"ramBytes":%s,"budgetBytes":%s,"modelsMax":%s,"models":{' "$RAM" "$BUDGET" "$MAXN"
  sep=""
  for f in "${FILES[@]}"; do
    n=$(basename "$f" .gguf); sz=$(stat -c%s "$f")
    fits=$([ "$sz" -le "$BUDGET" ] && echo true || echo false)
    printf '%s"%s":{"bytes":%s,"fits":%s}' "$sep" "$n" "$sz" "$fits"; sep=","
  done
  printf '}}\n'
} > "$UI/machine.json" 2>/dev/null || echo "  (note: manifest not writable — UI will offer all models)"

PORT=8080
while ss -ltn 2>/dev/null | grep -q ":$PORT " ; do PORT=$((PORT+1)); done

echo "  Machine : ${RAM_GB}GB RAM · CPU inference"
echo "  Models  : ${FITTING} of ${#FILES[@]} usable, up to ${MAXN} loaded at once"
echo "  Address : http://127.0.0.1:$PORT"
echo "  ─────────────────────────────────────────"; echo

"$BIN" --models-dir "$MODELS" --host 127.0.0.1 --port "$PORT" --path "$UI" \
       -c "$CTX" -t "$(nproc)" --models-max "$MAXN" \
       --cors-origins localhost --no-warmup > "$DIR/logs/server.log" 2>&1 &
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
