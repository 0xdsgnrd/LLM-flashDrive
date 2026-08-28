#!/bin/bash
# Check the drive against drive.lock.
#   ./scripts/verify-drive.sh          presence + exact byte size (seconds)
#   ./scripts/verify-drive.sh --sha    also verify sha256 (minutes — reads every byte)
#
# Size alone does NOT prove identity: DeepSeek-R1-Distill-Llama-70B Q4_K_M and
# Llama-3.3-70B-Instruct Q4_K_M differ by 2,368 bytes out of 42.5GB. Use --sha
# before trusting a drive you did not fill yourself, or after a flaky transfer.
# exFAT has no journal, so an interrupted write can leave a plausible-looking
# file at the right length.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# When this script sits ON the drive (release.sh stages it there), its own
# directory IS the drive — whatever the stick happens to mount as. Only fall
# back to the hardcoded path when running from a repo checkout.
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -n "${DRIVE:-}" ]; then :
elif [ -d "$SELF_DIR/models" ] && [ -f "$SELF_DIR/drive.lock" ]; then DRIVE="$SELF_DIR"
else DRIVE="/Volumes/Pocket-LLM"; fi
# Prefer the repo's lock, but fall back to the one release.sh stages on the
# drive — on a machine with no checkout, that copy is the only lock there is.
if [ -n "${LOCK:-}" ];            then :
elif [ -f "$ROOT/drive.lock" ];   then LOCK="$ROOT/drive.lock"
else                                   LOCK="$DRIVE/drive.lock"; fi
CHECK_SHA=0
[ "${1:-}" = "--sha" ] && CHECK_SHA=1

[ -f "$LOCK" ]   || { echo "✗ no lock file at $LOCK"; exit 1; }
[ -d "$DRIVE" ]  || { echo "✗ drive not mounted at $DRIVE"; exit 1; }

fsize () { stat -f%z "$1" 2>/dev/null || stat -c%s "$1" 2>/dev/null; }
sha256 () {
  if command -v shasum >/dev/null;      then shasum -a 256 "$1" | awk '{print $1}'
  elif command -v sha256sum >/dev/null; then sha256sum "$1"     | awk '{print $1}'
  else echo UNAVAILABLE; fi
}

FAIL=0; OK=0; MISSING=0; BAD=0

echo "drive : $DRIVE"
echo "lock  : $(basename "$LOCK")"
[ $CHECK_SHA -eq 1 ] && echo "mode  : full (sha256 — reads every byte)" || echo "mode  : quick (size only; --sha for content)"
echo

# ---- models pinned in the lock ----------------------------------------
while read -r name want sha; do
  f="$DRIVE/models/$name"
  if [ ! -f "$f" ]; then
    printf '  ·  %-46s not on drive\n' "$name"; MISSING=$((MISSING+1)); continue
  fi
  have=$(fsize "$f")
  if [ "$have" != "$want" ]; then
    pct=$(( have * 100 / want ))
    if [ "$have" -lt "$want" ]; then
      printf '  ✗  %-46s INCOMPLETE  %s of %s bytes (%s%%)\n' "$name" "$have" "$want" "$pct"
    else
      printf '  ✗  %-46s TOO LARGE   %s vs %s bytes\n' "$name" "$have" "$want"
    fi
    BAD=$((BAD+1)); FAIL=1; continue
  fi
  if [ $CHECK_SHA -eq 1 ]; then
    # Progress only when attached to a terminal; padded so the \r-return does
    # not leave residue behind the final line. Redirected output stays clean.
    [ -t 1 ] && printf '  …  %-46s hashing %sGB…%-20s\r' "$name" "$((want/1024/1024/1024))" ""
    got=$(sha256 "$f")
    if [ "$got" = "UNAVAILABLE" ]; then
      printf '  ?  %-46s size ok — no sha256 tool available\n' "$name"
    elif [ "$got" != "$sha" ]; then
      printf '  ✗  %-46s SHA MISMATCH — right size, wrong bytes\n' "$name"
      printf '     expected %s\n     got      %s\n' "$sha" "$got"
      BAD=$((BAD+1)); FAIL=1; continue
    else
      printf '  ✓  %-46s size + sha256\n' "$name"; OK=$((OK+1)); continue
    fi
  else
    printf '  ✓  %-46s %s bytes\n' "$name" "$have"
  fi
  OK=$((OK+1))
done < <(awk '$1=="model"{print $4, $5, $6}' "$LOCK")

# ---- anything on the drive the lock does not know about ---------------
UNPINNED=0
for f in "$DRIVE"/models/*.gguf; do
  [ -e "$f" ] || continue
  n=$(basename "$f")
  awk -v n="$n" '$1=="model" && $4==n {found=1} END{exit !found}' "$LOCK" && continue
  printf '  ?  %-46s on drive but NOT pinned in the lock\n' "$n"
  UNPINNED=$((UNPINNED+1))
done

# ---- binaries ---------------------------------------------------------
echo
for p in mac-arm64 linux-x64 win-x64; do
  b="$DRIVE/bin/$p/llama-server"; [ "$p" = win-x64 ] && b="$b.exe"
  if [ -f "$b" ]; then printf '  ✓  bin/%-42s %s\n' "$p" "$(fsize "$b" | awk '{printf "%.0fMB", $1/1048576}')"
  else printf '  ·  bin/%-42s missing\n' "$p"; fi
done

# The host's own binary can be run, so check it was built from the pinned
# commit. The cross-compiled ones cannot be executed here.
WANT_COMMIT=$(awk '$1=="llama_commit"{print $2}' "$LOCK")
HOST_BIN=""
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) HOST_BIN="$DRIVE/bin/mac-arm64/llama-server" ;;
  Linux-x86_64) HOST_BIN="$DRIVE/bin/linux-x64/llama-server" ;;
esac
if [ -n "$WANT_COMMIT" ] && [ -n "$HOST_BIN" ] && [ -x "$HOST_BIN" ]; then
  got=$("$HOST_BIN" --version 2>&1 | grep -oE 'commit [0-9a-f]+' | awk '{print $2}')
  if [ -n "$got" ]; then
    case "$WANT_COMMIT" in
      "$got"*) printf '  ✓  host binary built from pinned commit %s\n' "$got" ;;
      *)       printf '  ✗  host binary commit %s != pinned %s\n' "$got" "${WANT_COMMIT:0:12}"; FAIL=1 ;;
    esac
  fi
fi

# ---- summary ----------------------------------------------------------
echo
echo "  $OK ok · $BAD corrupt · $MISSING not downloaded · $UNPINNED unpinned"
if [ $FAIL -ne 0 ]; then
  echo "  ✗ drive does NOT match the lock"
  [ $CHECK_SHA -eq 0 ] && echo "    (size-only check — run with --sha to catch same-size corruption)"
  exit 1
fi
# A partial drive is legitimate — you may deliberately carry only some models —
# so missing entries are not a failure. But do not let the summary read as
# "fully verified" when most of the lock is absent.
SCOPE="drive matches the lock"
[ $MISSING -gt 0 ] && SCOPE="the $OK model(s) present match the lock ($MISSING not downloaded)"
[ $CHECK_SHA -eq 1 ] && echo "  ✓ $SCOPE (content verified)" \
                     || echo "  ✓ $SCOPE (sizes only — run --sha to verify content)"
exit 0
