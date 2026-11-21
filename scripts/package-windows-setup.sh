#!/usr/bin/env bash
# Build SeshSwap-setup.exe: one download that installs Rusty (Session Router
# as a Windows service) if needed, then the wallet.
#
#   dist/release/SeshSwap-setup.exe (+ .sha256)
#
# It embeds the Windows zip from scripts/package-windows.sh (built first if it
# isn't there) and Rusty's rusty-setup.exe, pinned by SHA-256 below.
#
# Usage:
#   scripts/package-windows-setup.sh VERSION
#
# rusty-setup.exe comes from $RUSTY_SETUP if set (a local file), else from the
# rusty-windows release. Either way it must match RUSTY_SHA256. A test build
# of Rusty can be used by setting RUSTY_SETUP, RUSTY_VERSION, RUSTY_SHA256,
# RUSTY_COMMIT and RUSTY_SOURCE (where its source is, for the notices).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"
VER="${1:?usage: $0 VERSION}"
RUSTY_VERSION="${RUSTY_VERSION:-v0.1.0-test}"
RUSTY_SHA256="${RUSTY_SHA256:-2bbc19b439c045d72b05edc313d3b45f2a813a6f20975472e593c3fd641f49fe}"
RUSTY_COMMIT="${RUSTY_COMMIT:-c8a3a0f8f6e2211a8451a392fbde9e4c91ec9a37}"
RUSTY_SOURCE="${RUSTY_SOURCE:-https://github.com/carlcrainellc/rusty-windows/tree/$RUSTY_COMMIT}"
RUSTY_URL="https://github.com/carlcrainellc/rusty-windows/releases/download/$RUSTY_VERSION/rusty-setup.exe"
OUTDIR="${OUTDIR:-$HERE/dist/release}"
CACHE="${SESHSWAP_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/seshswap-build}"
NAME="seshswap-$VER-windows-x86_64"
ZIP="$OUTDIR/$NAME.zip"
PAYLOAD="$HERE/swapd/cmd/seshswap-setup/payload"
mkdir -p "$OUTDIR" "$CACHE"

[[ -f "$ZIP" ]] || OUTDIR="$OUTDIR" "$HERE/scripts/package-windows.sh" "$VER"

echo "== rusty-setup.exe $RUSTY_VERSION"
RS="${RUSTY_SETUP:-$CACHE/rusty-setup-$RUSTY_VERSION.exe}"
if [[ ! -f "$RS" ]]; then
  curl -fsSL --retry 3 -o "$RS.part" "$RUSTY_URL"
  mv "$RS.part" "$RS"
fi
echo "$RUSTY_SHA256  $RS" | sha256sum -c --status || { echo "rusty-setup.exe: SHA-256 mismatch ($RS)" >&2; exit 1; }

echo "== payload"
WORK="$(mktemp -d)"
cleanup() { rm -rf --one-file-system "$WORK" "$PAYLOAD"; }
trap cleanup EXIT
rm -rf --one-file-system "$PAYLOAD"
mkdir -p "$PAYLOAD"
unzip -q "$ZIP" -d "$WORK"
mv "$WORK/$NAME" "$PAYLOAD/wallet"
cp "$RS" "$PAYLOAD/rusty-setup.exe"
printf '\nInstalled by SeshSwap-setup.exe: Rusty %s (rusty-setup.exe, sha256\n%s), Session Router and a Windows service, GPL-3.0.\nSource: %s\n' \
  "$RUSTY_VERSION" "$RUSTY_SHA256" "$RUSTY_SOURCE" >> "$PAYLOAD/wallet/THIRD-PARTY-NOTICES.txt"
(cd "$PAYLOAD/wallet" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)

echo "== SeshSwap-setup.exe ($VER)"
OUT="$OUTDIR/SeshSwap-setup.exe"
(cd swapd && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags seshswap_setup -trimpath \
  -ldflags "-s -w -buildid= -X main.version=$VER -X main.rustyVersion=$RUSTY_VERSION -X main.rustySHA256=$RUSTY_SHA256" \
  -o "$OUT" ./cmd/seshswap-setup)
(cd "$OUTDIR" && sha256sum "$(basename "$OUT")" > "$(basename "$OUT").sha256")
echo
echo "built $OUT ($(du -h "$OUT" | cut -f1))"
cat "$OUT.sha256"
