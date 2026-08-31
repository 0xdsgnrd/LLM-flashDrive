#!/bin/bash
# Publish the drive's binaries as a GitHub release and pin them in drive.lock.
#   ./scripts/release-binaries.sh <tag>
#   ./scripts/release-binaries.sh v1.0.0 --rebuild
#
# This is the half of the workflow that needs a toolchain: a Mac for the Metal
# build and the ad-hoc signature, Docker for the Linux and Windows cross-builds.
# Run it when the code changes — not when you make a drive.
#
# Making a drive is the other half (provision.sh) and needs none of it. That
# separation is the entire point: today a stick costs cmake, Docker and a Mac;
# afterwards it costs bandwidth. The binaries are pinned by sha256 exactly like
# the models, so a provisioned drive can prove what it is carrying without ever
# executing it — which is also how verify-drive.sh finally checks the two
# cross-compiled binaries it has never been able to run.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCK="$ROOT/drive.lock"
TAG="${1:-}"
REBUILD=0
[ "${2:-}" = "--rebuild" ] && REBUILD=1

[ -n "$TAG" ] || { echo "usage: release-binaries.sh <tag> [--rebuild]"; exit 1; }
command -v gh >/dev/null || { echo "✗ gh CLI not installed — brew install gh"; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "✗ gh is not logged in — run: gh auth login"; exit 1; }

# path on the drive  ->  build script that produces it
BINARIES=(
  "mac-arm64/llama-server    build-mac.sh"
  "mac-arm64/pocketd         build-helper.sh"
  "linux-x64/llama-server    build-linux.sh"
  "linux-x64/pocketd         build-helper.sh"
  "win-x64/llama-server.exe  build-windows.sh"
  "win-x64/pocketd.exe       build-helper.sh"
)

# GitHub release assets share one flat namespace, so three files all called
# llama-server cannot be uploaded as themselves. The asset name is the drive
# path with the separator swapped, which keeps it derivable in both directions
# and means the lock does not need a column to remember it.
asset_name () { echo "${1//\//-}"; }

echo "Release $TAG"
echo "─────────────────────────────────────────"

# ---- build whatever is missing ----------------------------------------
# Convergent rather than unconditional: a full llama.cpp build is tens of
# minutes per platform, and re-running this for a helper-only change should not
# cost an hour. --rebuild forces the lot.
missing=()
for entry in "${BINARIES[@]}"; do
  read -r path script <<<"$entry"
  [ $REBUILD -eq 1 ] || [ ! -f "$ROOT/dist/$path" ] || continue
  case " ${missing[*]-} " in *" $script "*) ;; *) missing+=("$script") ;; esac
done

if [ ${#missing[@]} -gt 0 ]; then
  echo "  building: ${missing[*]-}"
  for s in ${missing[@]+"${missing[@]}"}; do
    echo "  ── $s ──"
    "$ROOT/scripts/$s" || { echo "  ✗ $s failed"; exit 1; }
  done
else
  echo "  ✓ dist/ already complete (--rebuild to force)"
fi

for entry in "${BINARIES[@]}"; do
  read -r path _ <<<"$entry"
  [ -f "$ROOT/dist/$path" ] || { echo "  ✗ still missing after build: dist/$path"; exit 1; }
done

# ---- stage assets under their flat names ------------------------------
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
for entry in "${BINARIES[@]}"; do
  read -r path _ <<<"$entry"
  cp "$ROOT/dist/$path" "$STAGE/$(asset_name "$path")"
done
echo "  ✓ staged $(ls "$STAGE" | wc -l | tr -d ' ') assets ($(du -sh "$STAGE" | cut -f1))"

# ---- publish ----------------------------------------------------------
LLAMA_REF=$(awk '$1=="llama_ref"{print $2}' "$LOCK")
REPO_SLUG=$(git -C "$ROOT" remote get-url origin 2>/dev/null | sed -E 's#.*github.com[:/]##; s#\.git$##')
[ -n "$REPO_SLUG" ] || { echo "✗ cannot determine the GitHub repo from git remote origin"; exit 1; }
if gh release view "$TAG" >/dev/null 2>&1; then
  echo "  ! release $TAG exists — replacing its assets"
  gh release upload "$TAG" "$STAGE"/* --clobber
else
  gh release create "$TAG" "$STAGE"/* \
    --title "Pocket LLM $TAG" \
    --notes "Binaries for provisioning a drive with \`./scripts/provision.sh\`.

llama.cpp $LLAMA_REF · pocketd $(git -C "$ROOT" rev-parse --short HEAD)

Every asset is pinned by sha256 in \`drive.lock\`; provisioning verifies each one
before it is staged, so these do not have to be trusted on the strength of the
URL they arrived from."
fi
echo "  ✓ published"

# ---- pin into drive.lock ----------------------------------------------
# Rewritten wholesale rather than appended: a lock with two release stanzas in
# it would be ambiguous about which binaries the drive is supposed to carry.
sha256 () { shasum -a 256 "$1" | awk '{print $1}'; }
TMP="$(mktemp)"
awk '$1!="release" && $1!="binary"' "$LOCK" > "$TMP"
{
  echo
  echo "# ---------------------------------------------------------------- binaries"
  echo "# release  <tag>  <github-repo>"
  echo "# binary   <path-on-drive>  <bytes>  <sha256>"
  echo "#"
  echo "# The repo is recorded rather than read from git's origin, because a fork's"
  echo "# origin is the fork — which has no release — and provisioning would dead-end"
  echo "# on the most natural thing an interested person does. Forking and then"
  echo "# publishing your own re-pins this line to your repo, which is correct."
  echo "#"
  echo "# Published by release-binaries.sh, fetched by provision.sh. The asset name in"
  echo "# the GitHub release is this path with '/' swapped for '-', because release"
  echo "# assets share one flat namespace and three files are called llama-server."
  echo "#"
  echo "# Pinning these by hash is what lets verify-drive.sh check the Linux and"
  echo "# Windows binaries at all: it has only ever been able to run --version against"
  echo "# the host's own, and a binary you cannot execute you cannot interrogate."
  echo "release  $TAG  $REPO_SLUG"
  for entry in "${BINARIES[@]}"; do
    read -r path _ <<<"$entry"
    f="$ROOT/dist/$path"
    printf 'binary   %-24s %s  %s\n' "$path" "$(stat -f%z "$f")" "$(sha256 "$f")"
  done
} >> "$TMP"
mv "$TMP" "$LOCK"

echo "  ✓ pinned 6 binaries in drive.lock"
echo
echo "Next: ./scripts/provision.sh   (needs no toolchain — just a stick and bandwidth)"
