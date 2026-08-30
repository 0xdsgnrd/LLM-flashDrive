#!/bin/bash
# Pocket LLM launcher — macOS (Apple Silicon), router mode.
# Serves EVERY model in models/ and lets the UI pick per request.
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$DIR/bin/mac-arm64/llama-server"
MODELS="$DIR/models"
EMBED_DIR="$DIR/embed"
UI="$DIR/ui"
CTX=8192
# The encoder's own context. Chunks are ~1000 characters, so a few hundred
# tokens; 2048 is slack, not a target, and every batch has to fit inside it
# because an embedding pass is non-causal and cannot be split across ubatches.
EMBED_CTX=2048

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

# ---- the retrieval encoder, if the drive carries one ------------------
# Separate from models/ on purpose. It is not a model anybody picks in the
# sidebar, and putting it in models/ would put it in the router's enumeration,
# in the picker, and in the --models-max arithmetic below — where it would
# compete with the chat models for exactly the residency it must never lose.
shopt -s nullglob
EMBED_ALL=("$EMBED_DIR"/*.gguf)
shopt -u nullglob
EMBED=""; EMBED_NAME=""
if [ ${#EMBED_ALL[@]} -gt 0 ]; then
  # Sorted, so a drive with two encoders on it picks the same one every launch
  # rather than re-encoding the whole corpus on alternate days.
  IFS=$'\n' read -r -d '' -a EMBED_SORTED < <(printf '%s\n' "${EMBED_ALL[@]}" | sort && printf '\0')
  EMBED="${EMBED_SORTED[0]}"
  EMBED_NAME=$(basename "$EMBED" .gguf)
  [ ${#EMBED_ALL[@]} -eq 1 ] || echo "  ! ${#EMBED_ALL[@]} encoders in embed/ — using $EMBED_NAME"
  EMBED_SZ=$(stat -f%z "$EMBED")
  # It stays resident for the whole session, so it is spent out of the same
  # budget the chat models are packed against. Counting it twice is how a
  # machine ends up swapping.
  if [ $((BUDGET - EMBED_SZ)) -gt 0 ]; then
    BUDGET=$((BUDGET - EMBED_SZ))
  else
    echo "  ! Not enough RAM for the encoder as well — search will be lexical."
    EMBED=""; EMBED_NAME=""
  fi
fi

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

EMBED_PORT=""
if [ -x "$HELPER" ]; then
  LLAMA_PORT=$(free_port $((PORT + 1)))
  HIST="on — saved to chats/ on the drive"
  # Semantic search lives inside pocketd. Without the helper there is no /api
  # at all, so an encoder would have nothing to talk to.
  [ -z "$EMBED" ] || EMBED_PORT=$(free_port $((LLAMA_PORT + 1)))
else
  LLAMA_PORT="$PORT"
  HIST="OFF — helper missing, nothing will be saved"
  EMBED=""; EMBED_NAME=""
fi

if [ -n "$EMBED_PORT" ]; then SEARCH="lexical + semantic ($EMBED_NAME)"
else SEARCH="lexical (BM25) — add an encoder to embed/ for semantic search"; fi

echo "  Machine : ${RAM_GB}GB RAM · Apple Silicon (Metal)"
echo "  Models  : ${FITTING} of ${#FILES[@]} usable, up to ${MAXN} loaded at once"
echo "  History : $HIST"
echo "  Search  : $SEARCH"
echo "  Address : http://127.0.0.1:$PORT"
echo "  ─────────────────────────────────────────"
echo "  Pick a model in the sidebar. Close this window to shut down."
echo

ARGS=(--models-dir "$MODELS" --host 127.0.0.1 --port "$LLAMA_PORT"
      -c "$CTX" -ngl 999 --models-max "$MAXN" --cors-origins localhost --no-warmup)
[ -x "$HELPER" ] || ARGS+=(--path "$UI")      # only serve the UI when pocketd cannot

"$BIN" "${ARGS[@]}" > "$DIR/logs/server.log" 2>&1 &
PID=$!

# A dedicated server for the encoder, not a second model in the router. The
# router is started with --models-max derived from RAM, and on a small machine
# that is 1 — every search would evict the chat model and every answer would
# reload it. This process holds a few hundred megabytes and never contends.
EPID=""
if [ -n "$EMBED_PORT" ]; then
  "$BIN" --embeddings -m "$EMBED" --host 127.0.0.1 --port "$EMBED_PORT" \
      -c "$EMBED_CTX" -b "$EMBED_CTX" -ub "$EMBED_CTX" -ngl 999 \
      > "$DIR/logs/embed.log" 2>&1 &
  EPID=$!
fi

HPID=""
if [ -x "$HELPER" ]; then
  HARGS=(-port "$PORT" -ui "$UI" -chats "$CHATS" -docs "$DOCS"
         -upstream "127.0.0.1:$LLAMA_PORT")
  [ -z "$EMBED_PORT" ] || HARGS+=(-embed "127.0.0.1:$EMBED_PORT")
  "$HELPER" "${HARGS[@]}" > "$DIR/logs/pocketd.log" 2>&1 &
  HPID=$!
fi
trap 'kill $PID $HPID $EPID 2>/dev/null; wait $PID $HPID $EPID 2>/dev/null' EXIT INT TERM

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
  # The encoder is the one process whose death is survivable: pocketd notices,
  # stops embedding, and search carries on lexically. Say so and keep going.
  if [ -n "$EPID" ] && ! kill -0 $EPID 2>/dev/null; then
    echo "  ! Encoder exited — search is lexical only. See logs/embed.log"
    EPID=""
  fi
  sleep 1
done
wait $PID
