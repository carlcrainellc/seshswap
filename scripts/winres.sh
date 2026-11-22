#!/usr/bin/env bash
# Write a Windows resource object (icon + version info) for one command, so
# `GOOS=windows go build` links it in: <dir>/rsrc_windows_amd64.syso.
# The icon is branding/seshswap.ico (16/24/32/48/256). No manifest is added,
# so how Windows runs the program (elevation, DPI) doesn't change.
#
#   scripts/winres.sh DIR VERSION "File description" original-name.exe
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIR="${1:?dir}" VER="${2:?version}" DESC="${3:?description}" ORIG="${4:?original filename}"
# Windows version fields are four numbers: 0.1.4-test -> 0.1.4.0
NUM="$(printf '%s' "$VER" | sed -E 's/^v//; s/[^0-9.].*$//')"
[[ -n "$NUM" ]] || NUM=0.0.0
WINRES="${GO_WINRES:-go run github.com/tc-hib/go-winres@v0.3.3}"
(cd "$DIR" && $WINRES simply --arch amd64 --manifest none \
  --icon "$HERE/branding/seshswap.ico" \
  --product-name "SeshSwap" --product-version "$NUM" --file-version "$NUM" \
  --file-description "$DESC" --original-filename "$ORIG" \
  --copyright "Copyright (c) 2026 carlcrainellc" --out rsrc)
