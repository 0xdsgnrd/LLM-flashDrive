#!/bin/bash
# Resolve an HF model file to a pinned drive.lock entry and append it.
#   ./scripts/lock-add.sh [--embed] <hf-repo> <filename>
#
# --embed pins a retrieval encoder rather than a chat model. It is the same
# resolution and the same fields; only the keyword differs, because the two live
# in different directories on the drive and only one of them is ever offered in
# the model picker.
#
# Everything recorded is read from the HF API: the repo's current revision SHA,
# the file's exact byte size, and its sha256 (HF stores the LFS oid, which IS
# the sha256). Nothing is inferred from the filename or size, because neither
# identifies a model — see the header of drive.lock.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCK="$ROOT/drive.lock"
KIND=model
if [ "${1:-}" = "--embed" ]; then KIND=embed; shift; fi
REPO="${1:?usage: lock-add.sh [--embed] <hf-repo> <filename>}"
FILE="${2:?usage: lock-add.sh [--embed] <hf-repo> <filename>}"

api () { curl -fsSL "https://huggingface.co/api/models/$1" 2>/dev/null; }

REV=$(api "$REPO" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("sha",""))') \
  || { echo "✗ cannot read repo $REPO (gated repos need a token; fetch-model.sh has none)"; exit 1; }
[ -n "$REV" ] || { echo "✗ no revision SHA for $REPO"; exit 1; }

read -r SIZE SHA SPLIT < <(api "$REPO/tree/main?recursive=true" | python3 -c "
import json,sys,re
d=json.load(sys.stdin)
if isinstance(d,dict): print('ERR ERR 0'); raise SystemExit
hit=[x for x in d if x.get('path')=='$FILE']
if not hit: print('MISSING MISSING 0'); raise SystemExit
lfs=hit[0].get('lfs') or {}
base=re.sub(r'\.gguf\$','','$FILE')
parts=[x for x in d if re.match(re.escape(base)+r'-\d{5}-of-\d{5}\.gguf\$', x.get('path',''))]
print(lfs.get('size') or hit[0].get('size'), lfs.get('oid') or 'NONE', len(parts))
")

case "$SIZE" in
  MISSING) echo "✗ $FILE not found in $REPO. List candidates with:"; echo "    ./scripts/fetch-model.sh $REPO"; exit 1 ;;
  ERR)     echo "✗ could not read the file tree for $REPO"; exit 1 ;;
esac
[ "$SHA" != "NONE" ] && [ ${#SHA} -eq 64 ] || { echo "✗ no sha256 for $FILE (not LFS-tracked?)"; exit 1; }

# Split sets are one model across many files and the launchers cannot group
# them — each part would count as its own model and wreck the --models-max math.
# Two distinct cases: the target names a base whose parts exist, OR the target
# IS a part. The second is the easy one to miss.
if [[ "$FILE" =~ -[0-9]{5}-of-[0-9]{5}\.gguf$ ]]; then
  echo "✗ $FILE is one part of a split set — split sets are not supported"; exit 1
fi
[ "$SPLIT" -eq 0 ] || { echo "✗ $FILE has $SPLIT sibling parts — split sets are not supported"; exit 1; }

if grep -q "  $FILE  " "$LOCK" 2>/dev/null; then
  echo "! $FILE is already in drive.lock — remove it first to re-pin."; exit 1
fi

printf '\n%s  %s  %s  %s  %s  %s\n' "$KIND" "$REPO" "$REV" "$FILE" "$SIZE" "$SHA" >> "$LOCK"
echo "✓ pinned $FILE ($KIND)"
echo "    repo      $REPO"
echo "    revision  $REV"
echo "    size      $SIZE bytes ($((SIZE/1024/1024/1024)) GiB)"
echo "    sha256    $SHA"
