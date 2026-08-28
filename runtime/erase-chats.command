#!/bin/bash
# Erase every saved conversation from this drive.
#
# The app has a button for this. The script exists so the drive can be cleared
# by someone who is not going to open the app — handing it on, or checking it
# is clean before they do.
#
# This is a plain delete. exFAT does not overwrite the bytes, so a determined
# person with recovery tools could still get them back; it is not a defence
# against a stranger, only against the next person to plug the drive in.
set -uo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHATS="$DIR/chats"

cd "$DIR"; clear
echo "  Pocket LLM — erase conversations"
echo "  ─────────────────────────────────────────"

shopt -s nullglob
FILES=("$CHATS"/*.jsonl)
shopt -u nullglob

if [ ${#FILES[@]} -eq 0 ]; then
  echo "  ✓ Nothing to erase — no conversations on this drive."
  echo; read -r -p "  Press Return to close."; exit 0
fi

echo "  ${#FILES[@]} conversation(s) are stored on this drive."
echo
read -r -p "  Type ERASE to delete them permanently: " reply
if [ "$reply" != "ERASE" ]; then
  echo "  Cancelled — nothing was deleted."
  echo; read -r -p "  Press Return to close."; exit 0
fi

n=0
for f in "${FILES[@]}"; do rm -f "$f" && n=$((n+1)); done
# settings.json is deliberately kept: it holds a model preference, not anything
# that was said.
echo "  ✓ Erased $n conversation(s)."
echo; read -r -p "  Press Return to close."
