#!/bin/bash
# Stage built artifacts onto the USB drive. Models are NOT touched — they are
# downloaded straight to the drive by fetch-model.sh and never round-trip.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRIVE="${DRIVE:-/Volumes/Pocket-LLM}"

[ -d "$DRIVE" ] || { echo "✗ Drive not mounted at $DRIVE"; exit 1; }
[ -w "$DRIVE" ] || { echo "✗ $DRIVE is not writable"; exit 1; }

# Refuse to stage over a running app. Replacing a binary that a live process has
# mapped is what produced a staged pocketd that was byte-identical to dist/,
# passed `codesign -v`, and still died instantly with SIGKILL.
if pgrep -f "$DRIVE/bin/" >/dev/null 2>&1; then
  echo "✗ Pocket LLM is still running from $DRIVE."
  echo "  Close its window (or: pkill -f '$DRIVE/bin/') and run this again."
  exit 1
fi

echo "Staging → $DRIVE"
mkdir -p "$DRIVE"/{bin/{mac-arm64,linux-x64,win-x64},models,ui,logs,chats,docs}

# --- binaries (only those that have been built) ------------------------
staged=0
for plat in mac-arm64 linux-x64 win-x64; do
  src="$ROOT/dist/$plat"
  if compgen -G "$src/*" >/dev/null 2>&1; then
    # Remove before copying, rather than `cp -f` over the top. Overwriting a
    # Mach-O in place leaves the volume's cached pages for that inode
    # inconsistent with the code signature the kernel then checks, and macOS
    # answers that with SIGKILL at exec — from a file whose bytes and signature
    # are both perfectly valid on disk. A fresh file gets a fresh inode.
    for f in "$src"/*; do
      dst="$DRIVE/bin/$plat/$(basename "$f")"
      rm -f "$dst"
      cp "$f" "$dst"
    done
    echo "  ✓ bin/$plat"
    staged=$((staged+1))
  else
    case "$plat" in
      mac-arm64) hint=build-mac.sh ;;
      linux-x64) hint=build-linux.sh ;;
      win-x64)   hint=build-windows.sh ;;
    esac
    echo "  · bin/$plat  (not built — run scripts/$hint)"
  fi
done

# --- UI ----------------------------------------------------------------
cp -f "$ROOT/ui"/* "$DRIVE/ui/"
echo "  ✓ ui/"

# --- launchers (line endings matter: LF for unix, CRLF for windows) -----
cp -f "$ROOT/runtime/run-mac.command" "$ROOT/runtime/run-linux.sh" "$DRIVE/"
cp -f "$ROOT/runtime/run-windows.bat" "$ROOT/runtime/run-windows.ps1" "$DRIVE/"
cp -f "$ROOT/runtime/erase-chats.command" "$ROOT/runtime/erase-documents.command" "$DRIVE/"
chmod +x "$DRIVE/run-mac.command" "$DRIVE/run-linux.sh" \
         "$DRIVE/erase-chats.command" "$DRIVE/erase-documents.command" 2>/dev/null || true
echo "  ✓ launchers"

# --- provenance --------------------------------------------------------
# The drive must be self-describing. Without this, a stick holding seven .gguf
# files says nothing about where they came from — identifying the Llama-70B
# already on it once took byte-matching against three candidate repos. It also
# lets verify-drive.sh run against a drive with no repo checked out, which is
# the situation on any machine you carry the drive to.
if [ -f "$ROOT/drive.lock" ]; then
  {
    cat "$ROOT/drive.lock"
    echo
    echo "# ---- staged by release.sh ----"
    echo "# repo commit  $(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)$(git -C "$ROOT" diff --quiet 2>/dev/null || echo ' (dirty)')"
    echo "# staged from  $(hostname -s 2>/dev/null || echo unknown)"
  } > "$DRIVE/drive.lock"
  echo "  ✓ drive.lock ($(grep -c '^model' "$ROOT/drive.lock") models pinned)"
  # Stage the verifier too. A lock nobody can check is just a text file, and on
  # a machine with no checkout this is the only copy of either.
  if [ -f "$ROOT/scripts/verify-drive.sh" ]; then
    cp -f "$ROOT/scripts/verify-drive.sh" "$DRIVE/verify-drive.sh"
    chmod +x "$DRIVE/verify-drive.sh" 2>/dev/null || true
    echo "  ✓ verify-drive.sh"
  fi
else
  echo "  · drive.lock  (absent — drive will not record its own provenance)"
fi

# --- user data ---------------------------------------------------------
# chats/ and docs/ are created above and then left completely alone. They are
# the user's data, written only by pocketd on the drive; a release must never
# stage over them and never quietly erase them. Use the app, or the
# erase-chats / erase-documents scripts.
if compgen -G "$DRIVE/chats/*.jsonl" >/dev/null 2>&1; then
  echo "  · chats/ ($(ls "$DRIVE"/chats/*.jsonl | wc -l | tr -d ' ') conversation(s) — untouched)"
fi
if compgen -G "$DRIVE/docs/*.doc" >/dev/null 2>&1; then
  echo "  · docs/  ($(ls "$DRIVE"/docs/*.doc | wc -l | tr -d ' ') document(s) — untouched)"
fi

# --- hygiene -----------------------------------------------------------
xattr -dr com.apple.quarantine "$DRIVE" 2>/dev/null || true   # unblock Gatekeeper elsewhere
touch "$DRIVE/.metadata_never_index"
find "$DRIVE" -name '.DS_Store' -delete 2>/dev/null || true
command -v dot_clean >/dev/null && dot_clean -m "$DRIVE" 2>/dev/null || true

echo
echo "Drive contents:"
du -sh "$DRIVE"/* 2>/dev/null | sed 's/^/  /'
echo
if [ "$staged" -eq 0 ]; then
  echo "⚠ No binaries staged yet. Build at least one platform, then re-run."
else
  echo "✓ Release staged. Eject with: diskutil eject $DRIVE"
fi
