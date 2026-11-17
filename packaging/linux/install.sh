#!/bin/sh
# Copy SeshSwap to ~/.local/share/seshswap, add `seshswap` to ~/.local/bin and
# a menu entry. No sudo. ./install.sh --uninstall removes those three things
# (your wallet in ~/.seshswap is not touched).
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DATA="${XDG_DATA_HOME:-$HOME/.local/share}"
DEST="$DATA/seshswap"
BIN="$HOME/.local/bin"
APPS="$DATA/applications"
ICONS="$DATA/icons/hicolor/scalable/apps"

if [ "${1:-}" = "--uninstall" ]; then
  [ -x "$DEST/seshswap" ] && "$DEST/seshswap" stop >/dev/null 2>&1 || true
  rm -f "$APPS/seshswap.desktop" "$ICONS/seshswap.svg"
  [ -L "$BIN/seshswap" ] && rm -f "$BIN/seshswap"
  [ -f "$DEST/seshswap" ] && rm -rf "$DEST"
  echo "SeshSwap removed. Your wallet is still in ~/.seshswap."
  exit 0
fi

[ -x "$HERE/seshswap" ] || { echo "Run this from the unpacked SeshSwap folder." >&2; exit 1; }
if [ "$HERE" != "$DEST" ]; then
  [ -x "$DEST/seshswap" ] && "$DEST/seshswap" stop >/dev/null 2>&1 || true
  rm -rf "$DEST.new"
  mkdir -p "$DATA"
  cp -a "$HERE" "$DEST.new"
  rm -rf "$DEST"
  mv "$DEST.new" "$DEST"
fi
mkdir -p "$BIN" "$APPS" "$ICONS"
ln -sfn "$DEST/seshswap" "$BIN/seshswap"
cp "$DEST/share/seshswap.svg" "$ICONS/seshswap.svg"
sed -e "s|@BIN@|$DEST/seshswap|" -e "s|@ICON@|seshswap|" "$DEST/share/seshswap.desktop" > "$APPS/seshswap.desktop"
chmod 644 "$APPS/seshswap.desktop"
command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database "$APPS" >/dev/null 2>&1 || true

echo "Installed in $DEST"
echo "SeshSwap is in your app menu. From a terminal: seshswap"
case ":$PATH:" in *":$BIN:"*) ;; *) echo "(~/.local/bin isn't on your PATH yet; log out and back in, or run $DEST/seshswap)";; esac
echo
if "$DEST/seshswap" check 2>/dev/null | grep -q "private swaps are set up"; then
  echo "Private swaps are set up."
else
  echo "One more step for private swaps (once, asks for your password):"
  echo "  $DEST/seshswap setup-router"
fi
