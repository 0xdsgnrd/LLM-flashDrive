#!/bin/bash
# Pocket LLM launcher — macOS (Apple Silicon), router mode.
# Serves EVERY model in models/ and lets the UI pick per request.
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$DIR/bin/mac-arm64/llama-server"
MODELS="$DIR/models"
UI="$DIR/ui"
CTX=8192

cd "$DIR"
clear
echo "  Pocket LLM"
echo "  ─────────────────────────────────────────"

[ -x "$BIN" ] || { echo "  ✗ Missing binary: $BIN"; echo; read -r -p "  Press Return to close."; exit 1; }
xattr -dr com.apple.quarantine "$DIR/bin" 2>/dev/null || true

RAM=$(sysctl -n hw.memsize)
RAM_GB=$((RAM / 1024 / 1024 / 1024))
B_PCT=$((RAM * 7 / 10))
B_ABS=$((RAM - 4 * 1024 * 1024 * 1024))
BUDGET=$(( B_PCT < B_ABS ? B_PCT : B_ABS ))

# ---- enumerate models -------------------------------------------------
# Multi-part models (foo-00001-of-00003.gguf) are ONE model split across files,
# but nothing here groups them: each part would count as its own model, wrecking
# the size math below. Skip them rather than silently miscount. The UI applies
# the same rule, because router mode lists them in /v1/models regardless.
shopt -s nullglob
ALL=("$MODELS"/*.gguf)
shopt -u nullglob
FILES=(); SPLIT=()
for f in "${ALL[@]}"; do
  if [[ "$(basename "$f")" =~ -[0-9]{5}-of-[0-9]{5}\.gguf$ ]]; then SPLIT+=("$f")
  else FILES+=("$f"); fi
done
[ ${#SPLIT[@]} -eq 0 ] || echo "  ! Skipping ${#SPLIT[@]} multi-part file(s) — split models are not supported yet."

[ ${#FILES[@]} -gt 0 ] || { echo "  ✗ No usable .gguf files in $MODELS"
  [ ${#SPLIT[@]} -eq 0 ] || echo "    (only multi-part files found)"
  echo; read -r -p "  Press Return to close."; exit 1; }

# ---- models-max: worst case is the N LARGEST all resident at once ------
# Default is 4, which is fatal on a small machine (four 20GB models = 80GB).
# Derive N from real file sizes so concurrent loads always fit the budget.
FITTING=0; SUM=0; MAXN=0; PACKING=1
while read -r sz; do
  [ "$sz" -le "$BUDGET" ] || continue          # skip models too big to load at all
  FITTING=$((FITTING + 1))
  # Stop growing MAXN at the first model that does not fit. --models-max is a
  # COUNT, not a set: the router may load ANY N models, so the only safe N is
  # one where the N LARGEST fit together. Continuing to pack smaller models
  # after an overflow inflates N past that guarantee.
  if [ "$PACKING" -eq 1 ] && [ $((SUM + sz)) -le "$BUDGET" ]; then
    SUM=$((SUM + sz)); MAXN=$((MAXN + 1))
  else
    PACKING=0                                  # keep looping: FITTING still counts
  fi
done < <(for f in "${FILES[@]}"; do stat -f%z "$f"; done | sort -nr)

[ "$FITTING" -gt 0 ] || { echo "  ✗ No model fits in ${RAM_GB}GB of RAM."
  for f in "${FILES[@]}"; do echo "      $(basename "$f")  $(( $(stat -f%z "$f") /1024/1024 ))MB"; done
  echo; read -r -p "  Press Return to close."; exit 1; }
[ "$MAXN" -ge 1 ] || MAXN=1

# ---- manifest so the UI can grey out models this machine cannot run ----
# Written into the served UI dir; harmless if the drive is read-only.
{
  printf '{"ramBytes":%s,"budgetBytes":%s,"modelsMax":%s,"models":{' "$RAM" "$BUDGET" "$MAXN"
  sep=""
  for f in "${FILES[@]}"; do
    n=$(basename "$f" .gguf); sz=$(stat -f%z "$f")
    fits=$([ "$sz" -le "$BUDGET" ] && echo true || echo false)
    printf '%s"%s":{"bytes":%s,"fits":%s}' "$sep" "$n" "$sz" "$fits"; sep=","
  done
  printf '}}\n'
} > "$UI/machine.json" 2>/dev/null || echo "  (note: could not write manifest — UI will offer all models)"

# ---- ports --------------------------------------------------------------
# pocketd takes the public port and llama-server moves to a private one behind
# it. The browser still sees a single origin, so there is still nothing to
# configure for CORS, and pocketd can own /api/* to write chats to the drive.
# Without the helper, llama-server serves the UI itself exactly as before.
free_port() { local p=$1; while lsof -iTCP:$p -sTCP:LISTEN >/dev/null 2>&1; do p=$((p+1)); done; echo "$p"; }

PORT=$(free_port 8080)
HELPER="$DIR/bin/mac-arm64/pocketd"
CHATS="$DIR/chats"
DOCS="$DIR/docs"

if [ -x "$HELPER" ]; then
  LLAMA_PORT=$(free_port $((PORT + 1)))
  HIST="on — saved to chats/ on the drive"
else
  LLAMA_PORT="$PORT"
  HIST="OFF — helper missing, nothing will be saved"
fi

echo "  Machine : ${RAM_GB}GB RAM · Apple Silicon (Metal)"
echo "  Models  : ${FITTING} of ${#FILES[@]} usable, up to ${MAXN} loaded at once"
echo "  History : $HIST"
echo "  Address : http://127.0.0.1:$PORT"
echo "  ─────────────────────────────────────────"
echo "  Pick a model in the sidebar. Close this window to shut down."
echo

ARGS=(--models-dir "$MODELS" --host 127.0.0.1 --port "$LLAMA_PORT"
      -c "$CTX" -ngl 999 --models-max "$MAXN" --cors-origins localhost --no-warmup)
[ -x "$HELPER" ] || ARGS+=(--path "$UI")      # only serve the UI when pocketd cannot

"$BIN" "${ARGS[@]}" > "$DIR/logs/server.log" 2>&1 &
PID=$!
HPID=""
if [ -x "$HELPER" ]; then
  "$HELPER" -port "$PORT" -ui "$UI" -chats "$CHATS" -docs "$DOCS" \
      -upstream "127.0.0.1:$LLAMA_PORT" \
      > "$DIR/logs/pocketd.log" 2>&1 &
  HPID=$!
fi
trap 'kill $PID $HPID 2>/dev/null; wait $PID $HPID 2>/dev/null' EXIT INT TERM

for _ in $(seq 1 600); do
  if curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    echo "  ✓ Ready — opening browser."
    open "http://127.0.0.1:$PORT"
    break
  fi
  kill -0 $PID 2>/dev/null || { echo "  ✗ Server exited. Last lines:"; tail -15 "$DIR/logs/server.log"
    echo; read -r -p "  Press Return to close."; exit 1; }
  if [ -n "$HPID" ] && ! kill -0 $HPID 2>/dev/null; then
    echo "  ✗ Helper exited. Last lines:"; tail -15 "$DIR/logs/pocketd.log"
    echo; read -r -p "  Press Return to close."; exit 1
  fi
  sleep 1
done
wait $PID
