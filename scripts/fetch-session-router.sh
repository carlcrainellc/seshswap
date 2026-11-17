#!/usr/bin/env bash
# Fetch a pinned Session Router (1.1.1-2) and the shared libraries it needs,
# check every download against a pinned SHA-256, and unpack them next to the
# wallet so the wallet can start the router only while a swap needs it.
#
# By default the three Session Router packages are our
# patched 1.1.1-2 build (MapExit/UnmapExit, Linux route fixes, exit lease
# admission), shipped in dist/session-router-$SR_BUILD/ (default srnetx6) and
# pinned by SHA-256 below (or by the SHA256SUMS next to them). Stock 1.1.1 cannot route your traffic through an
# exit. SR_OFFICIAL=1 fetches the stock oxen packages instead (no exits).
#
# Output (not committed; swapd/bin/ is in .gitignore):
#   swapd/bin/session-router/bin/session-router
#   swapd/bin/session-router/lib/*.so*
#   swapd/bin/session-router/VERSION
#
# The router binary gets an absolute RUNPATH to that lib dir (patchelf), so it
# keeps finding its libraries after `setcap` (the loader ignores
# LD_LIBRARY_PATH for programs with file capabilities).
#
# Session Router 1.1.0 cannot build paths on the live network. Do not pin
# anything older than 1.1.1.
#
# Usage:
#   scripts/fetch-session-router.sh [OUT_DIR]          # fetch + unpack
#   scripts/fetch-session-router.sh --setcap [OUT_DIR] # also: sudo setcap cap_net_admin+ep
#
# Needs: curl, sha256sum, dpkg-deb (or ar + tar), patchelf.
# Linux x86_64 only. Licences: Session Router and the Oxen libraries are
# GPL-3.0; the Ubuntu libraries keep their own licences (copyright files are
# copied into OUT_DIR/share/doc).
set -euo pipefail

SETCAP=0
if [[ "${1:-}" == "--setcap" ]]; then SETCAP=1; shift; fi
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-$HERE/swapd/bin/session-router}"
SR_BUILD="${SR_BUILD:-srnetx6}"
VERSION="1.1.1-2+${SR_BUILD}~ubuntu2404"
if [[ "${SR_OFFICIAL:-0}" == 1 ]]; then VERSION="1.1.1-2~ubuntu2404"; fi

if [[ "$(uname -s)/$(uname -m)" != "Linux/x86_64" ]]; then
  echo "This script is for Linux x86_64. On Windows, see docs/WALLET.md (Windows)." >&2
  exit 1
fi
for t in curl sha256sum patchelf; do
  command -v "$t" >/dev/null || { echo "missing tool: $t (Ubuntu/Debian: apt install $t; or: pip install patchelf)" >&2; exit 1; }
done

# sha256  url   (oxen = deb.oxen.io noble; ubuntu = archive.ubuntu.com noble release pocket, frozen)
PINS=$(cat <<'PINS'
1ac17678679e030559e4382b2343f38fcaf7eceb98e6c6921c96cacd7c2bdb73 https://deb.oxen.io/pool/main/s/session-router/session-router-bin_1.1.1-2~ubuntu2404_amd64.deb
2f253c869c716a78e16505a57efa512a8074c535f820628834549f7bae3ece80 https://deb.oxen.io/pool/main/s/session-router/libsessionrouter1.1_1.1.1-2~ubuntu2404_amd64.deb
d274a87a8dec7aa319d907eecb4196267058250a6ef6c56649bbc751b259fc40 https://deb.oxen.io/pool/main/s/session-router/libsessionrouter-core1.1_1.1.1-2~ubuntu2404_amd64.deb
215549eceba16eda99e7f514fe1ca96304404374e99ea6a9a74254995f25e17e https://deb.oxen.io/pool/main/n/ngtcp2/libngtcp2-16_1.26.0+stf1-1~ubuntu2404_amd64.deb
a051969a2ab3005e4038a048d8948a4fc379d523ea072802d8169dc5362874c5 https://deb.oxen.io/pool/main/n/ngtcp2/libngtcp2-crypto-gnutls8_1.26.0+stf1-1~ubuntu2404_amd64.deb
dc1b918e1dc04d6255396fbac545aa2bcde57406ecb726216db60bf180dcfa33 https://deb.oxen.io/pool/main/o/oxen-logging/liboxen-logging1.2_1.2.0-2~ubuntu2404_amd64.deb
18d2374dc12c4957d7512d8a5dfb75149fe403f16ea102c07180a58897b638a4 https://deb.oxen.io/pool/main/o/oxen-libquic/liboxen-quic-event1.14_1.14.1-1~ubuntu2404_amd64.deb
ef4efe92ad9b0e7b19230a3e3298b6f8a7a867ab58384d5cfcf7463995986119 https://deb.oxen.io/pool/main/o/oxen-libquic/liboxen-quic-utils1.14_1.14.1-1~ubuntu2404_amd64.deb
ea2886203371f75b1992b0926e11463007f9fb8176ef1a235fa16216a78a6f53 https://deb.oxen.io/pool/main/o/oxen-libquic/liboxen-quic1.14_1.14.1-1~ubuntu2404_amd64.deb
187f7b8efd28d43da9d16dcc4af0896b6fb63d01bd94189c0cc86ddd32af7259 https://deb.oxen.io/pool/main/o/oxenmq/liboxenmq1.3.2_1.3.2-6~ubuntu2404_amd64.deb
f95eddfc9e9028567a21b8162a1cb8aae96b2fb168f149c0a5b01d72bfd42471 http://archive.ubuntu.com/ubuntu/pool/main/libe/libevent/libevent-2.1-7t64_2.1.12-stable-9ubuntu2_amd64.deb
3da51e864e4067779a2ba3a87daf5e4d8030c0aaa6291a1ce12483d4f73bf752 http://archive.ubuntu.com/ubuntu/pool/main/libe/libevent/libevent-core-2.1-7t64_2.1.12-stable-9ubuntu2_amd64.deb
f9bf6e3681179df772eea7d04b98b597560c428c39b7fca05bfaf64691c327b1 http://archive.ubuntu.com/ubuntu/pool/main/libe/libevent/libevent-pthreads-2.1-7t64_2.1.12-stable-9ubuntu2_amd64.deb
637e6a7744de08cd331a41f4efd0d24e6ea9064843dea9d1c6ca87bdb5f038a2 http://archive.ubuntu.com/ubuntu/pool/main/libf/libffi/libffi8_3.4.6-1build1_amd64.deb
0a9dd337aea7ae59aba44994a92dd2780365b534a714518477c4e906f0efd297 http://archive.ubuntu.com/ubuntu/pool/universe/f/fmtlib/libfmt9_9.1.0+ds1-2_amd64.deb
649a5ee5f74f0aa1e1b9c4f169eabbae56a79aa5aac51ef733eb9e18700bd8fc http://archive.ubuntu.com/ubuntu/pool/main/g/gmp/libgmp10_6.3.0+dfsg-2ubuntu6_amd64.deb
fdaa2c2fd34d7ac4ebd7d62e5b5d94c3cf9322b1fa1760d70b8e86425e9f6c36 http://archive.ubuntu.com/ubuntu/pool/main/g/gnutls28/libgnutls30t64_3.8.3-1.1ubuntu3_amd64.deb
594a82d7b70ecff393753ee0ab6611b0b18226c4f922d27ccb3f66caeae3d5d1 http://archive.ubuntu.com/ubuntu/pool/main/n/nettle/libhogweed6t64_3.9.1-2.2build1_amd64.deb
f1be32391a2fabf06c27618a2000ae8f9a26c30fad1b482a007049ae8156d19c http://archive.ubuntu.com/ubuntu/pool/main/libi/libidn2/libidn2-0_2.3.7-2build1_amd64.deb
0cccf7701765b5a633b964ed551c2bd1c074cad17daea8fa20a4e9cad0adb5a8 http://archive.ubuntu.com/ubuntu/pool/universe/j/jemalloc/libjemalloc2_5.3.0-2build1_amd64.deb
4cac2064e2984c4bc5a31659d4f00c7efe0c4b691559d536dd726c05d3632a57 http://archive.ubuntu.com/ubuntu/pool/main/n/nettle/libnettle8t64_3.9.1-2.2build1_amd64.deb
2496ec055ee1660b8733e267fa0aa27068869548bc7a898773607024f2df50c3 http://archive.ubuntu.com/ubuntu/pool/main/p/p11-kit/libp11-kit0_0.25.3-4ubuntu2_amd64.deb
0bd5072c20e1de04155d7a68c61aa04edccda4f4fe6c60c072a94fae8fbcc1b1 http://archive.ubuntu.com/ubuntu/pool/main/libs/libsodium/libsodium23_1.0.18-1build3_amd64.deb
e69e315e1596b6cf97a714ae797e55d42765664a739a0b30beac35164a4edaff http://archive.ubuntu.com/ubuntu/pool/universe/s/spdlog/libspdlog1.12_1.12.0+ds-2build1_amd64.deb
e30d8b4b7769966185b5acb40ac81205467022c96d7b1688bf06a3eaded2b98c http://archive.ubuntu.com/ubuntu/pool/main/libt/libtasn1-6/libtasn1-6_4.19.0-3build1_amd64.deb
b03fe3bf216e15dacd280bcef656070598ae414016991772e0041c8e50b207dd http://archive.ubuntu.com/ubuntu/pool/main/u/unbound/libunbound8_1.19.2-1ubuntu3_amd64.deb
78acc4c141a499e0c1e4cc47ca8e9082996f3972662c4d92337669ecc9b0a519 http://archive.ubuntu.com/ubuntu/pool/main/libu/libunistring/libunistring5_1.1-2build1_amd64.deb
PINS
)

# The patched build (exits): pinned by dist/session-router-$SR_BUILD/SHA256SUMS
LOCAL_PINS=""
LOCAL_DIR="${SR_EXIT_DEBS:-$HERE/dist/session-router-$SR_BUILD}"
if [[ -z "$LOCAL_PINS" && "${SR_OFFICIAL:-0}" != 1 ]]; then
  # A newer build: drop its three .debs and a SHA256SUMS (sha256sum format)
  # into dist/session-router-<build>/, commit them, and run with SR_BUILD=<build>.
  if [[ -f "$LOCAL_DIR/SHA256SUMS" ]]; then
    LOCAL_PINS=$(grep -E '^[0-9a-f]{64}  (session-router-bin|libsessionrouter1\.1|libsessionrouter-core1\.1)_.*\.deb$' "$LOCAL_DIR/SHA256SUMS" || true)
  fi
  if [[ $(grep -c . <<<"$LOCAL_PINS") != 3 ]]; then
    echo "no pins for build $SR_BUILD (need $LOCAL_DIR/SHA256SUMS listing the three router .debs)" >&2; exit 1
  fi
  VERSION=$(sed -nE 's/^[0-9a-f]{64}  session-router-bin_(.*)_amd64\.deb$/\1/p' <<<"$LOCAL_PINS")
fi
if [[ "${SR_OFFICIAL:-0}" != 1 ]]; then
  # drop the stock router packages; the patched ones replace them
  PINS=$(grep -vE '/(session-router-bin|libsessionrouter1\.1|libsessionrouter-core1\.1)_' <<<"$PINS")
fi

CACHE="${SR_FETCH_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/swap-wallet/session-router-debs}"
mkdir -p "$CACHE"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

unpack() { # deb dir
  if command -v dpkg-deb >/dev/null; then
    dpkg-deb -x "$1" "$2"
  else
    (cd "$WORK" && rm -f data.tar.* && ar x "$1" && tar -xf data.tar.* -C "$2")
  fi
}

while read -r sum url; do
  [[ -z "$sum" ]] && continue
  f="$CACHE/$(basename "$url")"
  if [[ ! -f "$f" ]] || ! echo "$sum  $f" | sha256sum -c --status; then
    echo "fetch $(basename "$url")"
    curl -fsSL --retry 3 -o "$f.part" "$url"
    mv "$f.part" "$f"
  fi
  if ! echo "$sum  $f" | sha256sum -c --status; then
    echo "CHECKSUM MISMATCH for $(basename "$url"); refusing to continue" >&2
    rm -f "$f"
    exit 1
  fi
  unpack "$f" "$WORK/root"
done <<<"$PINS"

if [[ "${SR_OFFICIAL:-0}" != 1 ]]; then
  while read -r sum name; do
    [[ -z "$sum" ]] && continue
    f="$LOCAL_DIR/$name"
    if [[ ! -f "$f" ]] || ! echo "$sum  $f" | sha256sum -c --status; then
      echo "CHECKSUM MISMATCH or missing $name in $LOCAL_DIR; refusing to continue" >&2
      exit 1
    fi
    unpack "$f" "$WORK/root"
  done <<<"$LOCAL_PINS"
fi

rm -rf "$OUT.new"
mkdir -p "$OUT.new/bin" "$OUT.new/lib" "$OUT.new/share/doc"
cp "$WORK/root/usr/bin/session-router" "$OUT.new/bin/"
cp -a "$WORK/root/usr/lib/x86_64-linux-gnu/"*.so* "$OUT.new/lib/"
cp -a "$WORK/root/usr/share/doc/"* "$OUT.new/share/doc/" 2>/dev/null || true
OUT_ABS="$(mkdir -p "$OUT" && cd "$OUT" && pwd)"
patchelf --set-rpath "$OUT_ABS/lib" "$OUT.new/bin/session-router"
for so in "$OUT.new/lib/"*.so*; do
  [[ -L "$so" ]] && continue
  patchelf --set-rpath "$OUT_ABS/lib" "$so" 2>/dev/null || true
done
echo "$VERSION" > "$OUT.new/VERSION"
rm -rf "$OUT"
mv "$OUT.new" "$OUT"

if ! "$OUT/bin/session-router" --version >/dev/null 2>&1; then
  echo "session-router does not run here (missing system library?):" >&2
  ldd "$OUT/bin/session-router" | grep "not found" >&2 || true
  exit 1
fi
echo "installed $("$OUT/bin/session-router" --version 2>/dev/null | head -1) in $OUT"

if [[ $SETCAP == 1 ]]; then
  echo "Giving the router permission to create its private network adapter (needs sudo once):"
  if command -v setcap >/dev/null; then
    sudo setcap cap_net_admin+ep "$OUT/bin/session-router"
  else
    # No libcap tools: write the same xattr setcap would (v2, cap_net_admin, +ep).
    sudo python3 -c 'import os,struct,sys; os.setxattr(sys.argv[1],"security.capability",struct.pack("<IIIII",0x02000001,1<<12,0,0,0))' "$OUT/bin/session-router"
  fi
  echo "done: the router can create its own adapter; it still runs as you, not root."
else
  echo
  echo "One more step (once, needs sudo): let the router create its private network"
  echo "adapter without running as root:"
  echo "  sudo setcap cap_net_admin+ep $OUT/bin/session-router"
fi
