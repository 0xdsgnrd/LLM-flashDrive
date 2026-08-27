#!/bin/bash
# Stage built artifacts onto the USB drive. Models are NOT touched — they are
# downloaded straight to the drive by fetch-model.sh and never round-trip.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRIVE="${DRIVE:-/Volumes/ai-Drive}"

[ -d "$DRIVE" ] || { echo "✗ Drive not mounted at $DRIVE"; exit 1; }
[ -w "$DRIVE" ] || { echo "✗ $DRIVE is not writable"; exit 1; }

echo "Staging → $DRIVE"
mkdir -p "$DRIVE"/{bin/{mac-arm64,linux-x64,win-x64},models,ui,logs}

# --- binaries (only those that have been built) ------------------------
staged=0
for plat in mac-arm64 linux-x64 win-x64; do
  src="$ROOT/dist/$plat"
  if compgen -G "$src/*" >/dev/null 2>&1; then
    cp -f "$src"/* "$DRIVE/bin/$plat/"
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
chmod +x "$DRIVE/run-mac.command" "$DRIVE/run-linux.sh" 2>/dev/null || true
echo "  ✓ launchers"

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
