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
echo "→ $DRIVE/models/$FILE"
curl -fL --progress-bar -C - -o "$DRIVE/models/$FILE" "$URL"
ls -lh "$DRIVE/models/$FILE"
