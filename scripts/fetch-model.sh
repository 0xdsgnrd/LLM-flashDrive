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

# HuggingFace throttles sustained single-stream transfers: a connection starts
# near 24 MB/s and decays to ~2 MB/s within minutes, while a FRESH connection to
# the same file still measures ~9 MB/s. Waiting for a hard stall does not help —
# it never drops below a stall threshold, it just crawls. So cycle the connection
# on a timer and resume, which keeps every segment on a fast fresh socket.
SIZE=$(curl -sIL "$URL" | awk 'BEGIN{IGNORECASE=1}/^content-length:/{n=$2}END{print n+0}' | tr -d '\r')
[ "$SIZE" -gt 0 ] 2>/dev/null || SIZE=0
[ "$SIZE" -gt 0 ] && echo "  remote size: $((SIZE/1024/1024))MB"

for attempt in $(seq 1 400); do
  HAVE=$(stat -f%z "$OUT" 2>/dev/null || stat -c%s "$OUT" 2>/dev/null || echo 0)
  if [ "$SIZE" -gt 0 ] && [ "$HAVE" -ge "$SIZE" ]; then
    echo "  complete ($((HAVE/1024/1024))MB)"
    break
  fi
  # --max-time cycles the socket even while data is flowing; --speed-limit
  # catches a genuine stall sooner. Both exit non-zero and resume via -C -.
  curl -fL --progress-bar -C - \
       --speed-limit 2000000 --speed-time 20 \
       --max-time 90 --connect-timeout 20 \
       -o "$OUT" "$URL" && { echo "  complete"; break; }
  rc=$?
  if [ $rc -eq 22 ]; then echo "  ✗ HTTP error — not retryable"; exit 22; fi
  NOW=$(stat -f%z "$OUT" 2>/dev/null || stat -c%s "$OUT" 2>/dev/null || echo 0)
  echo "  cycling connection (curl $rc) at $((NOW/1024/1024))MB — attempt $attempt"
done

ls -lh "$OUT"
