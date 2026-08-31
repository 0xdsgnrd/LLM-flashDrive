#!/bin/bash
# Take a formatted stick to a finished Pocket LLM drive, unattended.
#   ./scripts/provision.sh                    find the drive, pick a profile, go
#   ./scripts/provision.sh /Volumes/NAME      name the drive explicitly
#   ./scripts/provision.sh --profile 128      override the profile
#   ./scripts/provision.sh --plan             say what would happen, change nothing
#
# Needs no toolchain: no cmake, no Docker, no Go, no Mac-only build. The
# binaries come from the GitHub release pinned in drive.lock and are checked by
# sha256 before they are staged. Building is the other half of the workflow
# (release-binaries.sh) and happens when the code changes, not per drive.
#
# THIS IS A CONVERGENCE LOOP, NOT A SCRIPT OF STEPS. Every run asks what the
# drive is still missing and fixes that; nothing assumes a clean start. So there
# is no resume logic to get wrong — interrupt it, unplug it, re-run it, and it
# picks up where it stopped, because "where it stopped" is derived from the
# drive rather than remembered. 110GB over a throttled connection is hours, and
# a process you cannot safely kill is a process you cannot walk away from.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCK="$ROOT/drive.lock"

DRIVE=""; WANT_PROFILE=""; PLAN_ONLY=0; NO_EJECT=0
while [ $# -gt 0 ]; do
  case "$1" in
    --profile) WANT_PROFILE="${2:-}"; shift 2 ;;
    --plan)    PLAN_ONLY=1; shift ;;
    --no-eject) NO_EJECT=1; shift ;;
    -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)         DRIVE="$1"; shift ;;
  esac
done

say  () { printf '  %s\n' "$*"; }
fail () { printf '  ✗ %s\n' "$*" >&2; exit 1; }
G=1073741824

[ -f "$LOCK" ] || fail "no drive.lock at $LOCK"

echo "Pocket LLM — provision"
echo "─────────────────────────────────────────"

# ---------------------------------------------------------------- the drive
# Detected by Device Location rather than "Removable Media", which reports
# Fixed for USB SSDs and would quietly skip exactly the drives most worth
# provisioning.
if [ -z "$DRIVE" ]; then
  found=()
  for v in /Volumes/*; do
    [ -d "$v" ] || continue
    info=$(diskutil info "$v" 2>/dev/null) || continue
    echo "$info" | grep -q "Device Location:.*External" || continue
    found+=("$v")
  done
  case ${#found[@]} in
    0) fail "no external drive mounted. Plug one in, or name it: provision.sh /Volumes/NAME" ;;
    1) DRIVE="${found[0]}" ;;
    *) echo "  ✗ more than one external drive is mounted:" >&2
       for v in ${found[@]+"${found[@]}"}; do echo "      $v" >&2; done
       echo "    Name the one you mean — nothing should guess which disk gets 100GB written to it." >&2
       exit 1 ;;
  esac
fi
[ -d "$DRIVE" ] || fail "no such volume: $DRIVE"

info=$(diskutil info "$DRIVE" 2>/dev/null) || fail "cannot read $DRIVE"
FS=$(echo "$info" | awk -F: '/File System Personality/{gsub(/^ +| +$/,"",$2);print $2}')
TOTAL=$(echo "$info" | sed -n 's/.*Volume Total Space: .*(\([0-9]*\) Bytes).*/\1/p')
[ -n "$TOTAL" ] || TOTAL=0

case "$FS" in
  ExFAT) ;;
  MS-DOS*|FAT*)
    # Worth naming rather than letting it fail later: every model on this drive
    # is larger than FAT32's 4GB per-file ceiling, and the error you get is
    # about a failed write, not about the filesystem.
    fail "$DRIVE is $FS. Every model here is larger than FAT32's 4GB file limit.
    Reformat as exFAT (Disk Utility → Erase → ExFAT). This script will not
    format a disk for you." ;;
  *)
    fail "$DRIVE is $FS, which is not readable on all three platforms.
    Reformat as exFAT — it is the only filesystem macOS, Windows and Linux
    all mount read-write without extra software." ;;
esac
[ -w "$DRIVE" ] || fail "$DRIVE is not writable"

say "drive   : $DRIVE"
say "format  : $FS ✓"
say "capacity: $(python3 -c "print('%.1f GiB' % ($TOTAL/$G))")"

# ---------------------------------------------------------------- profile
# Reserve room for chats, documents and the vector cache. A drive filled to the
# brim is a drive that fails the first time somebody uses it.
RESERVE=$(python3 -c "print(max(8*$G, int($TOTAL*0.10)))")
USABLE=$((TOTAL - RESERVE))

profile_size () { awk -v p="$1" '($1=="model"||$1=="embed") && $7<=p {s+=$5} END{print s+0}' "$LOCK"; }
PROFILES=$(awk '($1=="model"||$1=="embed"){print $7}' "$LOCK" | sort -n | uniq)

if [ -z "$WANT_PROFILE" ]; then
  WANT_PROFILE=""
  for p in $PROFILES; do
    [ "$(profile_size "$p")" -le "$USABLE" ] && WANT_PROFILE="$p"
  done
  [ -n "$WANT_PROFILE" ] || fail "even the smallest profile ($(python3 -c "print('%.1f GiB' % ($(profile_size $(echo $PROFILES|awk '{print $1}'))/$G))")) does not fit with room to spare on this drive."
  auto=" (auto — largest that fits)"
else
  grep -q "^$WANT_PROFILE\$" <<<"$PROFILES" || fail "no profile '$WANT_PROFILE' in drive.lock. Known: $(echo $PROFILES | tr '\n' ' ')"
  auto=" (requested)"
fi

WANT_BYTES=$(profile_size "$WANT_PROFILE")
say "profile : $WANT_PROFILE$auto"
say "models  : $(awk -v p="$WANT_PROFILE" '($1=="model"||$1=="embed") && $7<=p' "$LOCK" | wc -l | tr -d ' ') files, $(python3 -c "print('%.1f GiB' % ($WANT_BYTES/$G))")"
if [ "$WANT_BYTES" -gt "$USABLE" ]; then
  fail "profile $WANT_PROFILE needs $(python3 -c "print('%.1f GiB' % ($WANT_BYTES/$G))") but only $(python3 -c "print('%.1f GiB' % ($USABLE/$G))") is usable after reserving room for your data."
fi
echo
# ---------------------------------------------------------------- the plan
# Derived from the drive every run, never remembered. This is what makes the
# script safe to interrupt: there is no state to be stale.
sha256 () { shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'; }
fsize  () { stat -f%z "$1" 2>/dev/null || echo 0; }

# The repo the binaries live in comes from the lock, NOT from git's origin.
# A fork's origin is the fork, which has no release attached, so reading origin
# would dead-end anyone who forked before provisioning — the most natural thing
# an interested person does. Origin is only the fallback for a lock written
# before this was recorded.
TAG=$(awk '$1=="release"{print $2}' "$LOCK")
REPO=$(awk '$1=="release"{print $3}' "$LOCK")
if [ -z "$REPO" ]; then
  REPO=$(git -C "$ROOT" remote get-url origin 2>/dev/null | sed -E 's#.*github.com[:/]##; s#\.git$##')
fi

need_binaries=(); need_models=()
while read -r path bytes sha; do
  [ -n "$path" ] || continue
  f="$DRIVE/bin/$path"
  [ "$(fsize "$f")" = "$bytes" ] && [ "$(sha256 "$f")" = "$sha" ] || need_binaries+=("$path $bytes $sha")
done < <(awk '$1=="binary"{print $2, $3, $4}' "$LOCK")

while read -r kind repo rev file bytes sha; do
  [ -n "$file" ] || continue
  dir=models; [ "$kind" = embed ] && dir=embed
  # Size only here. Hashing 110GB to build a plan would cost more than the
  # download it is trying to avoid; the --sha pass at the end is where content
  # is actually proven.
  [ "$(fsize "$DRIVE/$dir/$file")" = "$bytes" ] || need_models+=("$kind $repo $file $bytes")
done < <(awk -v p="$WANT_PROFILE" '($1=="model"||$1=="embed") && $7<=p {print $1,$2,$3,$4,$5,$6}' "$LOCK")

todo_bytes=0
for m in ${need_models[@]+"${need_models[@]}"}; do todo_bytes=$((todo_bytes + $(echo "$m" | awk '{print $4}'))); done

echo "Plan"
echo "─────────────────────────────────────────"
if [ -z "$TAG" ]; then
  say "! no release pinned in drive.lock — run ./scripts/release-binaries.sh <tag> first"
  say "  (binaries will be skipped; models and the UI can still be staged)"
else
  say "binaries: ${#need_binaries[@]} of 6 to fetch from $REPO release $TAG"
fi
say "models  : ${#need_models[@]} to download ($(python3 -c "print('%.1f GiB' % ($todo_bytes/$G))"))"
say "staging : ui/, launchers, drive.lock, verify-drive.sh (always refreshed)"

# Present but unpinned files are reported, never deleted. Removing 40GB on a
# script's own judgement is not a thing this should ever do.
extra=0
for f in "$DRIVE"/models/*.gguf "$DRIVE"/embed/*.gguf; do
  [ -e "$f" ] || continue
  n=$(basename "$f")
  awk -v n="$n" -v p="$WANT_PROFILE" '($1=="model"||$1=="embed") && $4==n && $7<=p {found=1} END{exit !found}' "$LOCK" && continue
  [ $extra -eq 0 ] && say "" && say "on the drive but not in profile $WANT_PROFILE — delete by hand if you want the space:"
  say "    $n  ($(python3 -c "print('%.1f GiB' % ($(fsize "$f")/$G))"))"
  extra=1
done
echo

if [ $PLAN_ONLY -eq 1 ]; then echo "  (--plan: nothing was changed)"; exit 0; fi
if [ ${#need_binaries[@]} -eq 0 ] && [ ${#need_models[@]} -eq 0 ]; then
  say "✓ drive already matches the lock — refreshing UI and launchers only"
fi

# ---------------------------------------------------------------- act
mkdir -p "$DRIVE"/{bin/{mac-arm64,linux-x64,win-x64},models,embed,ui,logs,chats,docs} || fail "cannot write to $DRIVE"

echo "Binaries"
echo "─────────────────────────────────────────"
if [ ${#need_binaries[@]} -eq 0 ]; then say "✓ all six present and verified"; fi
for entry in ${need_binaries[@]+"${need_binaries[@]}"}; do
  read -r path bytes sha <<<"$entry"
  [ -n "$TAG" ] || { say "· $path — skipped, no release pinned"; continue; }
  asset="${path//\//-}"
  url="https://github.com/$REPO/releases/download/$TAG/$asset"
  tmp="$DRIVE/bin/$path.part"
  printf '  ↓ %-26s ' "$path"
  if curl -fL --progress-bar -o "$tmp" "$url" 2>/dev/null; then
    got=$(sha256 "$tmp")
    if [ "$got" = "$sha" ]; then
      # Replace rather than overwrite: macOS keeps cached pages for an inode and
      # will SIGKILL a Mach-O whose signature no longer matches them, from a
      # file that is byte-perfect on disk. A fresh inode avoids it.
      rm -f "$DRIVE/bin/$path"; mv "$tmp" "$DRIVE/bin/$path"
      chmod +x "$DRIVE/bin/$path" 2>/dev/null
      echo "✓ sha256 ok"
    else
      rm -f "$tmp"; echo "✗ SHA MISMATCH — not staged"
      fail "release asset $asset does not match the lock. Re-publish, or re-pin deliberately."
    fi
  else
    rm -f "$tmp"; echo "✗ download failed"
    fail "could not fetch $url — is release $TAG published?"
  fi
done
echo

echo "Models"
echo "─────────────────────────────────────────"
if [ ${#need_models[@]} -eq 0 ]; then say "✓ every model in profile $WANT_PROFILE is already on the drive"; fi
i=0
for entry in ${need_models[@]+"${need_models[@]}"}; do
  read -r kind repo file _ <<<"$entry"
  i=$((i+1))
  say "[$i/${#need_models[@]}] $file"
  flag=""; [ "$kind" = embed ] && flag="--embed"
  # fetch-model.sh already resolves the pinned revision, cross-checks the size
  # before spending an hour on the wrong bytes, and cycles throttled HuggingFace
  # connections rather than waiting out a stall that never becomes one.
  DRIVE="$DRIVE" LOCK="$LOCK" "$ROOT/scripts/fetch-model.sh" $flag "$repo" "$file" 2>&1 \
    | sed 's/^/      /' | grep -vE '^\s*$' | tail -3
done
echo
# ---------------------------------------------------------------- stage
# The UI, launchers and lock come from this checkout, not from the release —
# they are text, they are in git, and you are already holding them.
echo "Staging"
echo "─────────────────────────────────────────"
if pgrep -f "$DRIVE/bin/" >/dev/null 2>&1; then
  fail "Pocket LLM is running from this drive. Close its window (or: pkill -f '$DRIVE/bin/') and re-run."
fi

cp -f "$ROOT/ui"/* "$DRIVE/ui/" && say "✓ ui/"
cp -f "$ROOT/runtime/run-mac.command" "$ROOT/runtime/run-linux.sh" "$DRIVE/"
cp -f "$ROOT/runtime/run-windows.bat" "$ROOT/runtime/run-windows.ps1" "$DRIVE/"
cp -f "$ROOT/runtime/erase-chats.command" "$ROOT/runtime/erase-documents.command" "$DRIVE/"
chmod +x "$DRIVE"/run-mac.command "$DRIVE"/run-linux.sh \
         "$DRIVE"/erase-chats.command "$DRIVE"/erase-documents.command 2>/dev/null
say "✓ launchers"

{
  cat "$LOCK"
  echo
  echo "# ---- provisioned by provision.sh ----"
  echo "# profile      $WANT_PROFILE"
  echo "# repo commit  $(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)$(git -C "$ROOT" diff --quiet 2>/dev/null || echo ' (dirty)')"
  echo "# provisioned  $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
} > "$DRIVE/drive.lock"
cp -f "$ROOT/scripts/verify-drive.sh" "$DRIVE/verify-drive.sh"
chmod +x "$DRIVE/verify-drive.sh" 2>/dev/null
say "✓ drive.lock + verify-drive.sh"

# Gatekeeper blocks a quarantined binary on the next Mac this is plugged into,
# and Spotlight has no business indexing a stick full of weights.
xattr -dr com.apple.quarantine "$DRIVE" 2>/dev/null
touch "$DRIVE/.metadata_never_index" 2>/dev/null
find "$DRIVE" -name '.DS_Store' -delete 2>/dev/null
command -v dot_clean >/dev/null && dot_clean -m "$DRIVE" 2>/dev/null
say "✓ quarantine stripped, indexing disabled"
echo

# ---------------------------------------------------------------- converge
# Ask the drive what is still wrong. If the answer is "nothing", we are done;
# if not, say so plainly rather than reporting success over a partial drive.
echo "Verify"
echo "─────────────────────────────────────────"
DRIVE="$DRIVE" LOCK="$LOCK" "$ROOT/scripts/verify-drive.sh" 2>&1 | sed 's/^/  /'
rc=${PIPESTATUS[0]}
echo

if [ "$rc" -ne 0 ]; then
  fail "the drive does not match the lock. Re-run this script — it resumes."
fi

still=0
for entry in ${need_models[@]+"${need_models[@]}"}; do
  read -r kind _ file bytes <<<"$entry"
  dir=models; [ "$kind" = embed ] && dir=embed
  [ "$(fsize "$DRIVE/$dir/$file")" = "$bytes" ] || still=$((still+1))
done
if [ "$still" -gt 0 ]; then
  say "! $still model(s) still incomplete — re-run to resume."
  exit 1
fi

echo "Done"
echo "─────────────────────────────────────────"
say "profile $WANT_PROFILE staged to $DRIVE"
say "free: $(df -h "$DRIVE" | tail -1 | awk '{print $4}')"
say ""
say "Content is proven by size here. Before handing this drive to anyone, run"
say "the full check — an interrupted write on exFAT can leave a file at exactly"
say "the right length:"
say "    $DRIVE/verify-drive.sh --sha"

if [ $NO_EJECT -eq 0 ]; then
  echo
  if diskutil eject "$DRIVE" >/dev/null 2>&1; then
    say "✓ ejected — safe to unplug"
  else
    say "! could not eject (something is still using it) — eject before unplugging"
  fi
fi
