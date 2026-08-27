#!/bin/bash
# Download a GGUF from Hugging Face into the drive's models/ folder.
#   ./fetch-model.sh <hf-repo>            → lists available GGUF files
#   ./fetch-model.sh <hf-repo> <filename> → downloads that file
# Filenames are resolved live from the HF API, never hardcoded, so this does
# not rot when repos re-tag their quantisations.
set -euo pipefail
DRIVE="${DRIVE:-/Volumes/ai-Drive}"
REPO="${1:?usage: fetch-model.sh <hf-repo> [filename]}"
FILE="${2:-}"

if [ -z "$FILE" ]; then
  echo "GGUF files in $REPO:"
  curl -fsSL "https://huggingface.co/api/models/$REPO" \
    | python3 -c '
import json,sys
d=json.load(sys.stdin)
for s in d.get("siblings",[]):
    n=s.get("rfilename","")
    if n.lower().endswith(".gguf"): print("   ", n)
' || { echo "Could not read repo $REPO — check the name."; exit 1; }
  echo
  echo "Then: $0 $REPO <filename>"
  exit 0
fi

mkdir -p "$DRIVE/models"
URL="https://huggingface.co/$REPO/resolve/main/$FILE?download=true"
OUT="$DRIVE/models/$FILE"
echo "→ $OUT"

# Long HF transfers degrade: an established connection can decay to ~1 MB/s while
# a fresh one still gets ~9 MB/s. --speed-limit/--speed-time abort a stalled
# transfer so the retry loop can reconnect; -C - resumes from the bytes on disk.
for attempt in $(seq 1 40); do
  if curl -fL --progress-bar -C - \
          --speed-limit 500000 --speed-time 30 \
          --connect-timeout 20 \
          -o "$OUT" "$URL"; then
    echo "  complete"
    break
  fi
  rc=$?
  # 22 = HTTP error (fatal); 33 = range not supported; anything else: reconnect.
  if [ $rc -eq 22 ] || [ $rc -eq 33 ]; then
    echo "  ✗ curl exit $rc — not retryable"; exit $rc
  fi
  echo "  connection stalled (curl $rc) — reconnecting, attempt $attempt"
  sleep 3
done
ls -lh "$OUT"
