#!/usr/bin/env bash
# One-time setup for the LOCAL end-to-end tests: official Monero CLI binaries,
# checked against binaryFate's signed hashes.txt (GPG) and then by SHA-256.
# Foundry (forge/anvil) and Go must already be installed.
set -euo pipefail
cd "$(dirname "$0")/.."
V=v0.18.5.4
FPR=81AC591FE9C4B65C5806AFC3F0AF4D462A0BDF92   # binaryFate's signing key
mkdir -p .monero && cd .monero
if [ ! -x "monero-x86_64-linux-gnu-$V/monerod" ]; then
  curl -sfL https://www.getmonero.org/downloads/hashes.txt -o hashes.txt
  curl -sfL https://raw.githubusercontent.com/monero-project/monero/master/utils/gpg_keys/binaryfate.asc -o binaryfate.asc
  GNUPGHOME=$(mktemp -d); export GNUPGHOME
  gpg -q --import binaryfate.asc
  # Machine-readable status lines, read in full (no pipe into grep -q, so no SIGPIPE).
  status=$(gpg --status-fd 1 --verify hashes.txt 2>/dev/null || true)
  if ! printf '%s\n' "$status" | grep -Eq "^\[GNUPG:\] VALIDSIG $FPR "; then
    echo "hashes.txt is not validly signed by $FPR" >&2; exit 1
  fi
  curl -sfL "https://downloads.getmonero.org/cli/monero-linux-x64-$V.tar.bz2" -o monero.tar.bz2
  want=$(awk -v f="monero-linux-x64-$V.tar.bz2" '$2 == f {print $1}' hashes.txt)
  [ -n "$want" ] || { echo "no hash for monero-linux-x64-$V.tar.bz2" >&2; exit 1; }
  echo "$want  monero.tar.bz2" | sha256sum -c -
  tar xjf monero.tar.bz2 && rm monero.tar.bz2
fi
echo "setup ok: $(pwd)/monero-x86_64-linux-gnu-$V"
