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

# ---- models and the encoder pinned in the lock ------------------------
# Both kinds carry identical fields and differ only in the directory they live
# in on the drive, so one loop verifies both.
while read -r kind name want sha; do
  case "$kind" in embed) f="$DRIVE/embed/$name" ;; *) f="$DRIVE/models/$name" ;; esac
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
done < <(awk '$1=="model" || $1=="embed"{print $1, $4, $5, $6}' "$LOCK")

# ---- anything on the drive the lock does not know about ---------------
UNPINNED=0
for f in "$DRIVE"/models/*.gguf "$DRIVE"/embed/*.gguf; do
  [ -e "$f" ] || continue
  n=$(basename "$f")
  awk -v n="$n" '($1=="model" || $1=="embed") && $4==n {found=1} END{exit !found}' "$LOCK" && continue
  printf '  ?  %-46s on drive but NOT pinned in the lock\n' "$n"
  UNPINNED=$((UNPINNED+1))
done

# ---- binaries ---------------------------------------------------------
# Verified by sha256 when release-binaries.sh has pinned them. That is the only
# way the Linux and Windows binaries can be checked at all: this script can run
# --version against the host's own, but a binary it cannot execute it cannot
# interrogate, and for most of this project's life those two were taken on
# faith. A hash needs no CPU that understands the file.
echo
PINNED_BINS=$(awk '$1=="binary"' "$LOCK" | wc -l | tr -d ' ')
if [ "$PINNED_BINS" -gt 0 ]; then
  while read -r path want sha; do
    f="$DRIVE/bin/$path"
    if [ ! -f "$f" ]; then
      printf '  ·  bin/%-42s missing\n' "$path"; MISSING=$((MISSING+1)); continue
    fi
    have=$(fsize "$f")
    if [ "$have" != "$want" ]; then
      printf '  ✗  bin/%-42s WRONG SIZE  %s vs %s\n' "$path" "$have" "$want"
      BAD=$((BAD+1)); FAIL=1; continue
    fi
    if [ $CHECK_SHA -eq 1 ]; then
      got=$(sha256 "$f")
      if [ "$got" = "UNAVAILABLE" ]; then printf '  ?  bin/%-42s size ok — no sha256 tool\n' "$path"
      elif [ "$got" != "$sha" ]; then
        printf '  ✗  bin/%-42s SHA MISMATCH — right size, wrong bytes\n' "$path"
        BAD=$((BAD+1)); FAIL=1; continue
      else printf '  ✓  bin/%-42s size + sha256\n' "$path"; fi
    else
      printf '  ✓  bin/%-42s %s\n' "$path" "$(echo "$have" | awk '{printf "%.0fMB", $1/1048576}')"
    fi
  done < <(awk '$1=="binary"{print $2, $3, $4}' "$LOCK")
else
  # No release pinned yet: fall back to presence only, which is all this could
  # ever report before binaries were published as artifacts.
  for p in mac-arm64 linux-x64 win-x64; do
    for n in llama-server pocketd; do
      f="$n"; [ "$p" = win-x64 ] && f="$n.exe"
      b="$DRIVE/bin/$p/$f"
      if [ -f "$b" ]; then printf '  ✓  bin/%-42s %s\n' "$p/$f" "$(fsize "$b" | awk '{printf "%.0fMB", $1/1048576}')"
      else printf '  ·  bin/%-42s missing\n' "$p/$f"; fi
    done
  done
  printf '  ·  binaries are not pinned — run scripts/release-binaries.sh to hash them\n'
fi

# Anyone about to lend this drive needs to know whether their conversations are
# still on it. Titles are never printed — the count is the whole point.
NCHAT=0
for f in "$DRIVE"/chats/*.jsonl; do [ -e "$f" ] && NCHAT=$((NCHAT+1)); done
echo
if [ "$NCHAT" -gt 0 ]; then
  printf '  !  chats/  %s saved conversation(s) — erase before handing the drive on\n' "$NCHAT"
else
  printf '  ✓  chats/  empty\n'
fi

NDOC=0
for f in "$DRIVE"/docs/*.doc; do [ -e "$f" ] && NDOC=$((NDOC+1)); done
if [ "$NDOC" -gt 0 ]; then
  printf '  !  docs/   %s indexed document(s) — erase before handing the drive on\n' "$NDOC"
else
  printf '  ✓  docs/   empty\n'
fi

# A .vec is a lossy but real representation of the text it came from, so it is
# the user's data too and it counts for the same reason the documents do.
NVEC=0
for f in "$DRIVE"/docs/*.vec; do [ -e "$f" ] && NVEC=$((NVEC+1)); done
[ "$NVEC" -eq 0 ] || printf '  !  docs/   %s embedding cache file(s) — erased with the documents\n' "$NVEC"

# Which half of retrieval this drive can actually do. The encoder's absence
# changes behaviour without any error, so it is worth stating outright.
NENC=0
for f in "$DRIVE"/embed/*.gguf; do [ -e "$f" ] && NENC=$((NENC+1)); done
if [ "$NENC" -gt 0 ]; then
  printf '  ✓  search   lexical + semantic (%s)\n' "$(basename "$(ls "$DRIVE"/embed/*.gguf | head -1)" .gguf)"
else
  printf '  ·  search   lexical only — no encoder in embed/\n'
fi

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

# ---- which profile is actually on this drive --------------------------
# A partial drive is legitimate — you may deliberately carry only the small set —
# so this reports what the drive IS rather than judging it against the largest
# profile the lock happens to describe.
PROFILES=$(awk '($1=="model"||$1=="embed"){print $7}' "$LOCK" | grep -E '^[0-9]+$' | sort -n | uniq)
if [ -n "$PROFILES" ]; then
  SATISFIED=""
  for p in $PROFILES; do
    ok=1
    while read -r kind name; do
      d=models; [ "$kind" = embed ] && d=embed
      [ -f "$DRIVE/$d/$name" ] || { ok=0; break; }
    done < <(awk -v p="$p" '($1=="model"||$1=="embed") && $7<=p {print $1, $4}' "$LOCK")
    [ "$ok" -eq 1 ] && SATISFIED="$p"
  done
  echo
  if [ -n "$SATISFIED" ]; then
    printf '  ✓  profile  %s — complete\n' "$SATISFIED"
  else
    printf '  ·  profile  none complete yet (smallest is %s)\n' "$(echo $PROFILES | awk '{print $1}')"
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
