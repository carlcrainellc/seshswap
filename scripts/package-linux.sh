#!/usr/bin/env bash
# Build the one-download Linux bundle:
#   dist/release/seshswap-<version>-linux-x86_64.tar.gz (+ .sha256)
#
# Inside: the seshswap launcher (wallet, static Go), the bundled Session
# Router with its own libraries (glibc included, so it runs on Ubuntu 22.04
# and 24.04), and the official monero-wallet-rpc checked against binaryFate's
# signed hashes. Every download is checked against a pinned or signed SHA-256.
#
# Usage:
#   scripts/package-linux.sh [VERSION]
#   SR_BUILD=srnetx7 scripts/package-linux.sh   # a newer router: put its three
#       .debs and a SHA256SUMS in dist/session-router-srnetx7/ first
#
# Needs: go, curl, gpg, sha256sum, patchelf, python3, dpkg-deb (or ar), tar.
# Linux x86_64 only.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$HERE"
VER="${1:-${SESHSWAP_VERSION:-$(git describe --tags --always 2>/dev/null | sed 's/^v//')}}"
VER="${VER:-0.0.0-dev}"
SR_BUILD="${SR_BUILD:-srnetx6}"
MONERO_V=v0.18.5.4
MONERO_FPR=81AC591FE9C4B65C5806AFC3F0AF4D462A0BDF92   # binaryFate's signing key
NAME="seshswap-$VER-linux-x86_64"
OUTDIR="${OUTDIR:-$HERE/dist/release}"
CACHE="${SESHSWAP_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/seshswap-build}"
export SR_FETCH_CACHE="${SR_FETCH_CACHE:-$CACHE/session-router-debs}"
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}"

for t in go curl gpg sha256sum patchelf python3 tar; do
  command -v "$t" >/dev/null || { echo "missing tool: $t" >&2; exit 1; }
done
mkdir -p "$CACHE" "$OUTDIR"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STAGE="$WORK/$NAME"
mkdir -p "$STAGE/monero" "$STAGE/share"

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
unpack() { # deb dir
  if command -v dpkg-deb >/dev/null; then dpkg-deb -x "$1" "$2"
  else (cd "$WORK" && rm -f data.tar.* && ar x "$1" && tar -xf data.tar.* -C "$2"); fi
}

echo "== wallet ($VER)"
(cd swapd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -buildid= -X main.version=$VER" -o "$STAGE/seshswap" ./cmd/seshswap)

echo "== Session Router ($SR_BUILD)"
SR_BUILD="$SR_BUILD" bash scripts/fetch-session-router.sh "$STAGE/session-router" >/dev/null
SR="$STAGE/session-router"

# The router's own runtime (Ubuntu 24.04, noble-updates as of the snapshot
# date), glibc included, so it doesn't depend on the system's glibc version.
# Hashes are from the signed snapshot index (InRelease -> Packages).
RUNTIME=$(cat <<'PINS'
d821b41fc59cb4978ecd53bc83e5c964ac15d305c8d2ac4c8a35aebe55010fb9 https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/g/glibc/libc6_2.39-0ubuntu8.12_amd64.deb
a51f8de7829211db961a31f02158058ad1a95f92ac6d0a5dff6350e2821c54c0 https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/g/gcc-14/libstdc++6_14.2.0-4ubuntu2~24.04.1_amd64.deb
aa7fadbe33b78bcf99885318040601c550c208929565b179891d9a3cc2aa68cd https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/g/gcc-14/libgcc-s1_14.2.0-4ubuntu2~24.04.1_amd64.deb
219f43b1cd836a4da550938db5fda93160d269d39a4edcf1f1ce698a470db797 https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.16_amd64.deb
2da3fa28302cf007075e89cdc3aabe6973916afcd62c3884b86f67b77c73a5b4 https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/s/systemd/libsystemd0_255.4-1ubuntu8.18_amd64.deb
dfcf25061e07aad7efd3f4f880ba5ad4d4d09ebe7fc8cc77ab6b8a161d6d4727 https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/libz/libzstd/libzstd1_1.5.5+dfsg2-2build1.1_amd64.deb
9474785cd6f398512bf8c305c3901dbb111569dccb6f5832002373c0a8ac5832 https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/libx/libxcrypt/libcrypt1_4.4.36-4build1_amd64.deb
23c627c77c66552a658e62852f244f4946dc543898e0ec5428d5dc6bffde2d6a https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/libc/libcap2/libcap2_2.66-5ubuntu2.4_amd64.deb
376249c8de139568221dbb242318cc8cb8f02299e3a62b7432948e1754fbddf9 https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/z/zlib/zlib1g_1.3.dfsg-3.1ubuntu2.3_amd64.deb
957b9eeaa83cf6f2d5d5d76dbb8aa31d3297e6781d8a02fa08dcc897ae650a0b https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/libg/libgcrypt20/libgcrypt20_1.10.3-2ubuntu0.2_amd64.deb
319331270d5cc52d5ebffe51c941d7b01b432bc402c2924b557209a64d4ecbad https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/l/lz4/liblz4-1_1.9.4-1build1.1_amd64.deb
5bcf5da2339331e21d3f42a831e82c61c54c6b2f29804010e8d59abd0ae52167 https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/x/xz-utils/liblzma5_5.6.1+really5.4.5-1ubuntu0.4_amd64.deb
93654ee8180a73a0363f25c51dc673d67cbabcbecd164187b8a2deb54d007aef https://snapshot.ubuntu.com/ubuntu/20261115T000000Z/pool/main/libg/libgpg-error/libgpg-error0_1.47-3build2.1_amd64.deb
PINS
)
mkdir -p "$WORK/rt"
while read -r sum url; do
  [[ -z "$sum" ]] && continue
  unpack "$(fetch "$sum" "$url")" "$WORK/rt"
done <<<"$RUNTIME"
cp -a "$WORK/rt/usr/lib/x86_64-linux-gnu/"*.so* "$SR/lib/"
cp -a "$WORK/rt/usr/share/doc/"* "$SR/share/doc/" 2>/dev/null || true
[[ -e "$SR/lib/ld-linux-x86-64.so.2" ]] || { echo "no ld-linux-x86-64.so.2 in the runtime" >&2; exit 1; }

# Library lookups go through the router's own RPATH (DT_RPATH also covers
# the libraries it loads); the libraries carry none. Interpreter and RPATH
# get fixed-size slots that `seshswap` fills in with absolute paths where the
# bundle is unpacked (the loader ignores $ORIGIN for a setcap program).
for so in "$SR/lib/"*.so*; do
  [[ -L "$so" ]] && continue
  case "$(basename "$so")" in ld-linux*|libc.so*|libm.so*) continue;; esac
  patchelf --remove-rpath "$so" 2>/dev/null || true
done
SLOT=1024
PI="/SESHSWAP-INTERP-SLOT/$(printf 'x%.0s' $(seq 1 $((SLOT - 23))))"
PR="/SESHSWAP-RPATH-SLOT/$(printf 'x%.0s' $(seq 1 $((SLOT - 22))))"
patchelf --set-interpreter "$PI" --force-rpath --set-rpath "$PR" "$SR/bin/session-router"
python3 - "$SR/bin/session-router" "$PI" "$PR" "$SLOT" > "$SR/RELOC" <<'PY'
import sys
data = open(sys.argv[1], 'rb').read()
slot = int(sys.argv[4])
print("# name offset size  (filled in by seshswap; see swapd/cmd/seshswap/reloc.go)")
for name, s in (("interp", sys.argv[2]), ("rpath", sys.argv[3])):
    b = s.encode()
    assert len(b) == slot - 1, (name, len(b))
    i = data.find(b + b"\0")
    assert i > 0 and data.find(b, i + 1) < 0, name + ": slot not found exactly once"
    print(name, i, slot)
PY
# Blank the slots so no placeholder text is left to look like a real path.
python3 - "$SR/bin/session-router" "$SR/RELOC" <<'PY'
import sys
p, r = sys.argv[1], sys.argv[2]
f = open(p, 'r+b')
for line in open(r):
    if line.startswith('#'): continue
    name, off, size = line.split(); f.seek(int(off)); f.write(b"\0" * int(size))
PY

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
MT="monero-linux-x64-$MONERO_V.tar.bz2"
want=$(awk -v f="$MT" '$2 == f {print $1}' "$MC/hashes.txt")
[[ -n "$want" ]] || { echo "no hash for $MT" >&2; exit 1; }
MF=$(fetch "$want" "https://downloads.getmonero.org/cli/$MT")
tar -xjf "$MF" -C "$WORK"
MD="$WORK/monero-x86_64-linux-gnu-$MONERO_V"
cp "$MD/monero-wallet-rpc" "$STAGE/monero/"
cp "$MD/LICENSE" "$STAGE/monero/LICENSE"
echo "$MONERO_V" > "$STAGE/monero/VERSION"

echo "== docs and installer"
P="$HERE/packaging/linux"
cp "$P/README.txt" "$STAGE/README.txt"
cp "$P/install.sh" "$STAGE/install.sh"
cp "$P/seshswap.desktop" "$P/seshswap.svg" "$STAGE/share/"
cp "$P/config.example.json" "$STAGE/config.example.json"
cp LICENSE "$STAGE/LICENSE"
cp NOTICE "$STAGE/NOTICE"
# Where the GPL source for this router build lives: dist/session-router-<build>/SOURCE
# (one line, a URL to the exact commit). Required for a release (CI sets RELEASE=1).
SR_SOURCE_FILE="$HERE/dist/session-router-$SR_BUILD/SOURCE"
if [[ -f "$SR_SOURCE_FILE" ]]; then
  SR_SOURCE=$(head -1 "$SR_SOURCE_FILE")
else
  [[ "${RELEASE:-0}" == 1 ]] && { echo "missing $SR_SOURCE_FILE: say where the router source is before releasing" >&2; exit 1; }
  SR_SOURCE="github.com/carlcrainellc/session-router ($(cat "$SR/VERSION"))"
  echo "WARNING: no $SR_SOURCE_FILE; fine for a local test build, not for a release" >&2
fi
sed -e "s/@VERSION@/$VER/g" -e "s/@SR_VERSION@/$(cat "$SR/VERSION")/g" -e "s/@MONERO_VERSION@/$MONERO_V/g" \
  -e "s|@SR_SOURCE@|$SR_SOURCE|g" \
  "$P/THIRD-PARTY-NOTICES.txt" > "$STAGE/THIRD-PARTY-NOTICES.txt"
chmod 755 "$STAGE/seshswap" "$STAGE/install.sh" "$STAGE/monero/monero-wallet-rpc" "$SR/bin/session-router"

echo "== self-check (in a copy, so the tarball keeps blank slots)"
cp -a "$STAGE" "$WORK/check"
"$WORK/check/seshswap" check
rm -rf "$WORK/check"

echo "== manifest"
(cd "$STAGE" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)

TAR="$OUTDIR/$NAME.tar.gz"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$SOURCE_DATE_EPOCH" \
  -C "$WORK" -cf - "$NAME" | gzip -n -9 > "$TAR.part"
mv "$TAR.part" "$TAR"
(cd "$OUTDIR" && sha256sum "$NAME.tar.gz" > "$NAME.tar.gz.sha256")
echo
echo "built $TAR ($(du -h "$TAR" | cut -f1))"
cat "$TAR.sha256"
