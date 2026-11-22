#!/usr/bin/env bash
# Build the Windows wallet zip:
#   dist/release/seshswap-<version>-windows-x86_64.zip (+ .sha256)
#
# Inside: seshswap.exe (wallet and launcher, static Go, cross-built here) and
# the official monero-wallet-rpc.exe checked against binaryFate's signed
# hashes. No Session Router: on Windows the wallet uses Rusty's service
# (github.com/carlcrainellc/rusty-windows), which SeshSwap-setup.exe installs
# (scripts/package-windows-setup.sh).
#
# Usage:
#   scripts/package-windows.sh [VERSION]
#
# Needs: go, curl, gpg, sha256sum, unzip, python3. Runs on Linux.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"
VER="${1:-${SESHSWAP_VERSION:-$(git describe --tags --always 2>/dev/null | sed 's/^v//')}}"
VER="${VER:-0.0.0-dev}"
MONERO_V=v0.18.5.4
MONERO_FPR=81AC591FE9C4B65C5806AFC3F0AF4D462A0BDF92   # binaryFate's signing key
NAME="seshswap-$VER-windows-x86_64"
OUTDIR="${OUTDIR:-$HERE/dist/release}"
CACHE="${SESHSWAP_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/seshswap-build}"
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}"

for t in go curl gpg sha256sum unzip python3; do
  command -v "$t" >/dev/null || { echo "missing tool: $t" >&2; exit 1; }
done
mkdir -p "$CACHE" "$OUTDIR"
WORK="$(mktemp -d)"
trap 'rm -rf --one-file-system "$WORK"' EXIT
STAGE="$WORK/$NAME"
mkdir -p "$STAGE/monero"

fetch() { # sha256 url -> path in cache
  local sum="$1" url="$2" f
  f="$CACHE/$(basename "$url")"
  if [[ ! -f "$f" ]] || ! echo "$sum  $f" | sha256sum -c --status; then
    curl -fsSL --retry 3 -o "$f.part" "$url"
    mv "$f.part" "$f"
  fi
  echo "$sum  $f" | sha256sum -c --status || { echo "CHECKSUM MISMATCH: $url" >&2; rm -f "$f"; exit 1; }
  echo "$f"
}
crlf() { sed 's/\r*$/\r/' "$1"; } # Notepad-friendly line ends

echo "== wallet ($VER)"
SYSO="$HERE/swapd/cmd/seshswap/rsrc_windows_amd64.syso"
trap 'rm -rf --one-file-system "$WORK"; rm -f "$SYSO"' EXIT
"$HERE/scripts/winres.sh" "$HERE/swapd/cmd/seshswap" "$VER" "SeshSwap wallet (testnet)" seshswap.exe
(cd swapd && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -buildid= -X main.version=$VER" -o "$STAGE/seshswap.exe" ./cmd/seshswap)
rm -f "$SYSO"

echo "== monero-wallet-rpc $MONERO_V"
MC="$CACHE/monero-$MONERO_V"
mkdir -p "$MC"
curl -fsSL https://www.getmonero.org/downloads/hashes.txt -o "$MC/hashes.txt"
curl -fsSL https://raw.githubusercontent.com/monero-project/monero/master/utils/gpg_keys/binaryfate.asc -o "$MC/binaryfate.asc"
GNUPGHOME="$WORK/gnupg"; mkdir -m 700 "$GNUPGHOME"; export GNUPGHOME
gpg -q --import "$MC/binaryfate.asc" 2>/dev/null
status=$(gpg --status-fd 1 --verify "$MC/hashes.txt" 2>/dev/null || true)
if ! printf '%s\n' "$status" | grep -Eq "^\[GNUPG:\] VALIDSIG $MONERO_FPR "; then
  echo "hashes.txt is not validly signed by $MONERO_FPR" >&2; exit 1
fi
unset GNUPGHOME
MZ="monero-win-x64-$MONERO_V.zip"
want=$(awk -v f="$MZ" '$2 == f {print $1}' "$MC/hashes.txt")
[[ -n "$want" ]] || { echo "no hash for $MZ" >&2; exit 1; }
MF=$(fetch "$want" "https://downloads.getmonero.org/cli/$MZ")
unzip -q "$MF" -d "$WORK/monero"
MD="$WORK/monero/monero-x86_64-w64-mingw32-$MONERO_V"
cp "$MD/monero-wallet-rpc.exe" "$STAGE/monero/"
cp "$MD/LICENSE" "$STAGE/monero/LICENSE"
echo "$MONERO_V" > "$STAGE/monero/VERSION"

echo "== docs"
P="$HERE/packaging/windows"
crlf "$P/README.txt" | sed "s/@VERSION@/$VER/g" > "$STAGE/README.txt"
crlf "$P/THIRD-PARTY-NOTICES.txt" | sed -e "s/@VERSION@/$VER/g" -e "s/@MONERO_VERSION@/$MONERO_V/g" \
  > "$STAGE/THIRD-PARTY-NOTICES.txt"
cp "$P/config.example.json" "$STAGE/config.example.json"
cp LICENSE "$STAGE/LICENSE"
cp NOTICE "$STAGE/NOTICE"

echo "== manifest"
(cd "$STAGE" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)

ZIP="$OUTDIR/$NAME.zip"
# Same bytes for the same commit: the commit time (UTC) on every entry, sorted
# names, no directory entries, no extra fields.
python3 - "$WORK" "$NAME" "$ZIP.part" "$SOURCE_DATE_EPOCH" <<'PY'
import os, sys, time, zipfile
work, name, out, epoch = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
files = []
for root, _, fs in os.walk(os.path.join(work, name)):
    files += [os.path.relpath(os.path.join(root, f), work).replace(os.sep, "/") for f in fs]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    for f in sorted(files):
        zi = zipfile.ZipInfo(f, time.gmtime(epoch)[:6])
        zi.compress_type = zipfile.ZIP_DEFLATED
        zi.external_attr = 0o644 << 16
        with open(os.path.join(work, f), "rb") as src:
            z.writestr(zi, src.read())
PY
mv "$ZIP.part" "$ZIP"
(cd "$OUTDIR" && sha256sum "$NAME.zip" > "$NAME.zip.sha256")
echo
echo "built $ZIP ($(du -h "$ZIP" | cut -f1))"
cat "$ZIP.sha256"
