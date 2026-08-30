#!/bin/bash
# Erase every document indexed for search on this drive.
#
# Separate from erase-chats.command on purpose: a reference set you are happy to
# pass on is a different thing from your conversations, and you may well want to
# clear one and keep the other.
#
# This is a plain delete. exFAT does not overwrite the bytes, so it stops the
# next person to plug the drive in, not someone with recovery tools.
set -uo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DOCS="$DIR/docs"

cd "$DIR"; clear
echo "  Pocket LLM — erase documents"
echo "  ─────────────────────────────────────────"

shopt -s nullglob
FILES=("$DOCS"/*.doc)
shopt -u nullglob

if [ ${#FILES[@]} -eq 0 ]; then
  echo "  ✓ Nothing to erase — no documents on this drive."
  echo; read -r -p "  Press Return to close."; exit 0
fi

echo "  ${#FILES[@]} document(s) are indexed on this drive:"
for f in "${FILES[@]}"; do
  # The name lives in the header line, not the filename.
  n=$(head -1 "$f" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p')
  echo "      ${n:-$(basename "$f")}"
done
echo
read -r -p "  Type ERASE to delete them permanently: " reply
if [ "$reply" != "ERASE" ]; then
  echo "  Cancelled — nothing was deleted."
  echo; read -r -p "  Press Return to close."; exit 0
fi

n=0
for f in "${FILES[@]}"; do rm -f "$f" && n=$((n+1)); done

# The lexical index lives only in memory and is rebuilt from these files, so
# deleting them is the whole operation there. The embedding cache is different:
# it is on the drive, and a vector is a lossy but real representation of the
# passage it was made from. Leaving it behind would leave part of the document
# behind, which is precisely what this script exists to prevent.
shopt -s nullglob
VECS=("$DOCS"/*.vec "$DOCS"/*.vec.tmp)
shopt -u nullglob
v=0
for f in "${VECS[@]}"; do rm -f "$f" && v=$((v+1)); done

echo "  ✓ Erased $n document(s)."
[ "$v" -eq 0 ] || echo "  ✓ Erased $v embedding cache file(s)."
echo; read -r -p "  Press Return to close."
