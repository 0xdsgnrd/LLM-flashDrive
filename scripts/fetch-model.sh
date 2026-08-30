#!/bin/bash
# Download a GGUF from Hugging Face into the drive's models/ folder.
#   ./fetch-model.sh <hf-repo>            → lists available GGUF files
#   ./fetch-model.sh <hf-repo> <filename> → downloads that file
#   ./fetch-model.sh --embed <repo> <file> → ...into embed/ instead
#
# --embed is for the retrieval encoder. It goes in its own directory because
# models/ is what the router enumerates and the picker offers, and an encoder
# is neither: it answers no questions and must never be evicted to load one.
#
# If the file is pinned in drive.lock, the pinned REVISION is used instead of
# main. main is a moving branch: a repo that re-quantises or re-tags upstream
# would silently hand you different bytes under the same filename, which is
# exactly what the lock exists to prevent. Filenames are still resolved live
# from the HF API, never hardcoded, so this does not rot when repos re-tag.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRIVE="${DRIVE:-/Volumes/Pocket-LLM}"
LOCK="${LOCK:-$ROOT/drive.lock}"
KIND=model; SUBDIR=models
if [ "${1:-}" = "--embed" ]; then KIND=embed; SUBDIR=embed; shift; fi
REPO="${1:?usage: fetch-model.sh [--embed] <hf-repo> [filename]}"
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

# --- resolve the revision from drive.lock ------------------------------
# Match on repo AND filename: the same filename appears in several repos.
REV=""
if [ -f "$LOCK" ]; then
  REV=$(awk -v k="$KIND" -v r="$REPO" -v f="$FILE" '$1==k && $2==r && $4==f {print $3; exit}' "$LOCK")
fi
if [ -n "$REV" ]; then
  echo "  pinned revision ${REV:0:12} (from $(basename "$LOCK"))"
else
  REV=main
  echo "  ! $FILE is not pinned in drive.lock — falling back to main."
  echo "    Pin it first so this download is reproducible:"
  [ "$KIND" = embed ] && echo "      ./scripts/lock-add.sh --embed $REPO $FILE" \
                      || echo "      ./scripts/lock-add.sh $REPO $FILE"
fi

mkdir -p "$DRIVE/$SUBDIR"
URL="https://huggingface.co/$REPO/resolve/$REV/$FILE?download=true"
OUT="$DRIVE/$SUBDIR/$FILE"
echo "→ $OUT"

# HuggingFace throttles sustained single-stream transfers: a connection starts
# near 24 MB/s and decays to ~2 MB/s within minutes, while a FRESH connection to
# the same file still measures ~9 MB/s. Waiting for a hard stall does not help —
# it never drops below a stall threshold, it just crawls. So cycle the connection
# on a timer and resume, which keeps every segment on a fast fresh socket.
HEAD=$(curl -sIL -o /dev/null -w '%{http_code}' "$URL" || echo 000)
case "$HEAD" in
  200) : ;;
  404) echo "  ✗ $FILE does not exist at revision ${REV:0:12} of $REPO."
       echo "    List what is there:  $0 $REPO"; exit 1 ;;
  401|403) echo "  ✗ HTTP $HEAD — $REPO is gated or private."
       echo "    This script sends no auth token, so gated repos cannot be fetched."; exit 1 ;;
  *)   echo "  ✗ HTTP $HEAD from HuggingFace — not attempting a download."; exit 1 ;;
esac

SIZE=$(curl -sIL "$URL" | awk 'BEGIN{IGNORECASE=1}/^content-length:/{n=$2}END{print n+0}' | tr -d '\r')
[ "$SIZE" -gt 0 ] 2>/dev/null || SIZE=0
[ "$SIZE" -gt 0 ] && echo "  remote size: $((SIZE/1024/1024))MB"

# Cross-check against the lock before spending an hour on the wrong bytes.
if [ "$REV" != main ] && [ "$SIZE" -gt 0 ]; then
  WANT=$(awk -v k="$KIND" -v r="$REPO" -v f="$FILE" '$1==k && $2==r && $4==f {print $5; exit}' "$LOCK")
  if [ -n "$WANT" ] && [ "$WANT" != "$SIZE" ]; then
    echo "  ✗ size disagrees with drive.lock: lock=$WANT remote=$SIZE"
    echo "    The pinned revision should be immutable — re-pin deliberately."
    exit 1
  fi
fi

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
