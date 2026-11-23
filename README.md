# SeshSwap

A wallet that swaps ETH or USDC for XMR with an atomic swap. No custodian:
either both sides get paid or both get their coins back. Makers are found in
an on-chain registry and reached over Session Router (`.sesh`), so neither
side sees the other's IP. The wallet can also rent a Session Router exit by
the hour (Exit Markets).

**Testnet only.** Arbitrum Sepolia (or Sepolia) and Monero stagenet. Not
audited. Don't use it with real money. The daemon refuses mainnets.

## Install (Linux x86_64)

Ubuntu 22.04 or 24.04. Download `seshswap-<version>-linux-x86_64.tar.gz` from
[Releases](https://github.com/carlcrainellc/seshswap/releases), then:

    tar xzf seshswap-*-linux-x86_64.tar.gz
    cd seshswap-*-linux-x86_64
    ./seshswap

Your browser opens the wallet at http://127.0.0.1:7676. Make a wallet, write
down the 24 words, then press **Get test coins**. Keep the terminal open
while you use it; Ctrl-C quits.

Private swaps need Session Router to make its own network adapter. Run this
once (it asks for your password; the router still runs as you):

    ./seshswap setup-router

If your account can't use sudo, run `./seshswap check` once, then ask
someone who can to run this from the SeshSwap folder:

    sudo setcap cap_net_admin+ep session-router/bin/session-router

Optional: `./install.sh` adds SeshSwap to your app menu (no sudo).
`./seshswap check` checks that the bundled programs run on your machine.
Your wallet lives in `~/.seshswap`.

## Confirmations

Before it finishes a swap, the wallet waits for the XMR to get enough Monero
confirmations (about 2 minutes each). Small swaps wait 10. Larger ones wait
more: 15 from 1 XMR, 20 from 5 XMR, 30 from 20 XMR. In Settings you can pick
Safer (at least 20) or Safest (at least 30). Details: [design](DESIGN.md) §9.

The tarball has the wallet, a bundled Session Router and the official
`monero-wallet-rpc` (v0.18.5.4, checked against binaryFate's signed hashes).
Testnet settings are built in.

## Build from source

Needs Go (see `swapd/go.mod`), plus `curl gpg patchelf python3` for the
bundle.

    cd swapd && go test ./internal/...      # unit tests
    scripts/package-linux.sh 0.1.0          # dist/release/seshswap-0.1.0-linux-x86_64.tar.gz

Contracts (Foundry): `git submodule update --init && cd contracts && forge test`.
End-to-end tests on local devnets: `scripts/setup.sh`, then
`E2E=1 go test ./e2e` in `swapd` (needs `anvil`).

## Layout

| Path | What |
|---|---|
| `swapd/` | wallet (`cmd/seshswap`), swap daemon and relayer (`cmd/swapd`) |
| `contracts/` | swap contract (`XmrSwapV3`), `SeshRegistry`, `ExitLease`, `ExitPay` |
| `seller-kit/` | run a Session Router exit and list it |
| `packaging/linux/`, `scripts/` | Linux bundle and release |
| `dist/session-router-srnetx6/` | Session Router build used by the bundle (source: `SOURCE`) |

Docs: [wallet](docs/WALLET.md), [registry](docs/REGISTRY.md),
[exits](docs/EXITS.md), [design](DESIGN.md).

## License

MIT (see [LICENSE](LICENSE)). Third-party parts: [NOTICE](NOTICE).
Session Router is GPL-3.0; source for the bundled build is at
https://github.com/carlcrainellc/session-router/tree/seshswap/router-1.1.1-srnetx6.
