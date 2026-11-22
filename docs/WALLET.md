# Swap wallet (desktop)

The wallet holds ETH, USDC and XMR, has one Swap screen, and runs the swap
daemon and a watchtower loop in the background.

Test-only, not audited. Arbitrum Sepolia, Sepolia and Monero stagenet only.
No mainnet, no real funds.

## What it does

- Create a wallet (24-word BIP-39 backup, password, encrypted store) or restore
  from those words.
- One seed, domain-separated: ETH at BIP-44 `m/44'/60'/0'/0/0` (same first
  address as MetaMask); XMR spend from `HMAC-SHA512("eth-xmr-swap-wallet xmr-spend-v1", seed)`,
  view = Monero's usual `hash-to-scalar(spend)`.
- Encrypted at rest with the existing store (Argon2id, XChaCha20-Poly1305,
  crash-safe writes).
- A local page on `127.0.0.1` (not on all interfaces): balances, Swap, trade
  progress in plain English with a refund time in Central (CT), history, settings.
- Send XMR, ETH or USDC to an address you paste (see Send below).
- Request a quote from a configured maker, run an ETH→XMR swap with the
  existing taker engine, resume unfinished swaps on unlock after a crash or close.

## Run

```
cd swapd
go build -o bin/wallet ./cmd/wallet
./bin/wallet -config /path/to/wallet.json
```

Open `http://127.0.0.1:7676`. The process listens on loopback only.

### Linux download (one file)

`scripts/package-linux.sh 0.1.0` builds
`dist/release/seshswap-0.1.0-linux-x86_64.tar.gz`: the `seshswap` launcher
(this wallet with the testnet settings built in, data in `~/.seshswap`),
monero-wallet-rpc v0.18.5.4 (checked against binaryFate's signed hashes) and
the bundled Session Router with its own libraries, glibc included, so it runs
on Ubuntu 22.04 and 24.04. `seshswap` starts monero-wallet-rpc on 127.0.0.1
with a random login for each launch, picks the first public stagenet node
that answers, opens the browser and stops everything on exit.
`seshswap setup-router` is the one sudo step (setcap on the router).
`~/.seshswap/config.json` overrides any setting; see
`packaging/linux/config.example.json`.

New router build: put its three .debs and a `SHA256SUMS` in
`dist/session-router-<build>/` and run `SR_BUILD=<build> scripts/package-linux.sh`.
A tagged release also needs `dist/session-router-<build>/SOURCE` (one line:
where that build's GPL source is). `.github/workflows/release-linux.yml` does
the same on a `v*` tag and attaches the tarball to a draft release. `-demo`
fills sample balances and a sample quote for screenshots; it does not move
funds.

A config looks like swapd's, plus `makerUrl` and `listen`:

```json
{
  "listen": "127.0.0.1:7676",
  "dataDir": "/home/you/.swapwallet",
  "ethRpc": "https://ethereum-sepolia-rpc.publicnode.com",
  "ethRpcs": ["https://1rpc.io/sepolia"],
  "contract": "0x…",
  "xmrWalletRpc": "http://127.0.0.1:38083",
  "xmrDaemonRpc": "http://127.0.0.1:38081",
  "xmrNet": "stagenet",
  "makerUrl": "http://127.0.0.1:19000",
  "relayers": ["http://127.0.0.1:19001"],
  "mainWallet": "swapwallet-main"
}
```

The password you choose is also used as the monero-wallet-rpc wallet password
when the app creates a wallet from your keys. Environment variables are not
needed: there is no `SWAPD_ETH_KEY` on the command line; the ETH key comes
from the seed.

If you close the window mid-swap, start the same process again, unlock, and
the watchtower loop resumes from the encrypted store. Reveal transactions are
still persisted before broadcast, same as swapd.



## Private connection: bundled Session Router

The wallet ships with Session Router and starts it **only
while it needs to talk to the maker**, the way Tor Browser starts Tor. It is
not a background service. Nothing runs until you ask for a quote or start a
swap, and the router stops again when it is no longer needed.

### Your own Session Router, if you already run one

If Session Router is already running on the computer (a system service
such as `session-router-client`, or one you started yourself), the wallet
uses it instead of starting its own. Settings → Connection has four choices
(config key `transport`):

| Choice | What it does |
|---|---|
| **Auto** (default, `auto`; the old value `private` means this too) | On the first maker request, look for a router that is already running. Use it if it resolves the maker's `.sesh` name **and** a test connection to the maker opens. Otherwise start the bundled one (only while needed). |
| **Bundled only** (`bundled`) | Always the bundled one; never looks for yours. |
| **Existing only** (`existing`) | Only a router that is already running; never starts the bundled one. If none works you get a plain error. |
| **Direct** (`direct`) | No Session Router. The maker sees your IP. |

Where the wallet looks for a running router, in order: `seshDns` from the
config (if you set it, e.g. `127.0.0.1:5353`); the system resolver, but only
nameservers in `/etc/resolv.conf` that are on this computer (`127.x`, `::1`,
e.g. systemd-resolved with a `.sesh` routing domain), so a `.sesh` name is
never sent to your ISP's or LAN's DNS; then Session Router's default
`127.0.0.1:53`. A failed search is remembered for 60 s, so Auto doesn't
wait on a missing router before every request. If a router that worked
stops working, Auto searches again and falls back to the bundled one.

**The wallet never stops, restarts or reconfigures a router it didn't
start.** It only asks that router's DNS for the maker's address and connects
to it. Locking the wallet, going idle, finishing a swap or quitting only
ever stop the wallet's own bundled router.

What you see in the sidebar:

| Line | Meaning |
|---|---|
| Private connection (uses your Session Router if it's running, otherwise starts the bundled one…) | Auto, nothing connected yet. |
| Private connection (starts when you get a quote or swap) | Bundled only, router is off. |
| Connecting privately… (bundled) | The bundled router is starting and finding a path to the maker. |
| Connected privately (bundled) (ready in N s) | The maker answered over `.sesh` through the bundled router. The maker sees a Session Router address, not your IP. |
| Connected privately (using your Session Router) | Same, through the router that was already running. |
| Private connection failed + a reason | A plain-English reason, e.g. not installed, no permission, maker unreachable. |
| Direct connection (the maker can see your IP address) | Fallback mode is selected. |

### When it starts and stops

- **Starts** on the first request to a `.sesh` maker: a quote, a swap, a
  Satchel-proposed swap, or the lock hint.
- **Stays up** while any swap is unfinished, and for `idleStop` (default 2
  minutes) after the last maker request when no swap is open (so a quote
  followed by "Swap" reuses the same router).
- **Stops** when the last swap finishes or refunds, when you sign out of the
  wallet, when you switch to Direct, and when the wallet exits.
- **Crash or kill -9:** the router stops too (measured 0.2 s). Linux drops
  the parent-death signal when a `setcap` program starts, so the wallet runs
  a tiny guard (the wallet binary itself, `wallet __session-router-guard …`)
  that holds that signal and stops the router. On the next launch the wallet
  removes the dead run's temp dir. If the guard was killed too, the next
  launch also stops a leftover router.
- **Crash-resume:** after you unlock with an unfinished swap, the wallet
  starts the router again (fresh), so the lock hint can still reach the maker
  and the status line is accurate. Swap safety never depends on this
  connection: after the ETH lock both sides only read the two chains.

### Every launch is isolated

Each start writes a new `session-router.ini` in a fresh temp dir
(`$TMPDIR/swap-wallet-sr-*`), deleted when the router stops:

- fresh `data-dir` (stale 0-RTT tickets made warm restarts slower, so
  nothing is reused);
- client only: `reachable=false`, `[exit] enable=false`, no keyfile;
- `auto-routing=false`, `blackhole-routes=false`. The system default route
  is not touched;
- `[dns] listen=127.x.y.z:<random high port>` and `no-resolvconf=1`. The
  wallet resolves `.sesh` names only through that DNS, never the system
  resolver;
- its own adapter name (`swr` + 6 hex), its own private `10.160–223.0.0/16`
  and `fd2e:7365:7372:xxxx::/64` ranges (picked to avoid existing
  interfaces), and a random UDP port.

The router runs as you, not root. The wallet refuses to start it when the
wallet itself runs as root.

All of the above is for swaps. Connecting an exit changes it (next section).

### Install (Linux x86_64)

```
scripts/fetch-session-router.sh --setcap
cd swapd && go build -o bin/wallet ./cmd/wallet
```

`fetch-session-router.sh` downloads Session Router **1.1.1-2** (the
`deb.oxen.io` noble build) plus the libraries it needs (the Oxen libraries,
and pinned Ubuntu 24.04 release-pocket libraries such as libfmt9, spdlog,
gnutls and unbound), checks every file against a pinned SHA-256, and unpacks
them to `swapd/bin/session-router/{bin,lib}` (about 21 MB on disk; ~6.5 MB
download). It sets an absolute RUNPATH with `patchelf`, because the loader
ignores `LD_LIBRARY_PATH` for programs with file capabilities. Tested on
Debian 13. The packages are Ubuntu 24.04 builds, so Ubuntu 24.04 should
work too, but the bundle itself has not been tried there yet. Needs `curl`,
`sha256sum`, `dpkg-deb` (or `ar` + `tar`) and `patchelf` (`apt install
patchelf` or `pip install patchelf`).

`--setcap` runs, once, with sudo:

```
sudo setcap cap_net_admin+ep swapd/bin/session-router/bin/session-router
```

That lets the router create its own network adapter (TUN) without root.
If it is missing, the wallet says so in plain English, with the exact
command. If `setcap` is not installed, the script writes the same file
capability with Python.

Do **not** use Session Router 1.1.0: it cannot build paths on the live
network. The binary is not committed (GPL-3.0, and ~21 MB with libraries);
the script is the pinned source of truth.

### Config

```json
{
  "transport": "private",
  "makerSeshUrl": "http://<52-char name>.sesh",
  "makerUrl": "http://<direct maker>:19000",
  "sessionRouter": {
    "binary": "",
    "idleStop": "2m",
    "tempDir": ""
  }
}
```

- `transport`: `"private"` (bundled router) or `"direct"`. Empty means
  private when `makerSeshUrl` is set, otherwise direct.
- `makerSeshUrl`: the maker's `.sesh` address (private mode).
- `makerUrl`: the direct address, kept as the fallback. Direct mode is the
  old behaviour; a `.sesh` `makerUrl` with your own router and `seshDns`
  still works there.
- `sessionRouter.binary`: empty means `<wallet dir>/session-router/bin/session-router`,
  which is where the fetch script puts it for `swapd/bin/wallet`.

Settings has a **Connection to the maker** choice (Private / Direct) and
both addresses. `PUT /v1/settings` accepts `transport`, `makerSeshUrl` and
`makerUrl`. `GET /v1/status` has a `connection` object
(`mode`, `state`, `label`, `detail`, `startupMs`, `launches`).

### Windows (not the target yet)

Session Router on Windows uses the **wintun** driver: `wintun.dll` must sit
next to `session-router.exe`, and the first run that creates the adapter
needs Administrator rights. The wallet's guard on Windows simply runs the router
(there is no parent-death signal), and the permission check is skipped.
Packaging for Windows (a signed `session-router.exe` + `wintun.dll`, adapter
creation at install time) is not done yet.

### Measured startup inside the wallet

Launch → first reply from the maker over `.sesh` (official 1.1.1-2 build,
maker on a test server): 2.4–3.3 s in 8 of 9 cold starts, with one 16.8 s
outlier. A live 0.001 ETH→XMR swap over `.sesh`, including a `kill -9`
and resume, passed with exact balances.
A quote allows 60 s, and a swap start 90 s.

## Bonded makers from the registry

With `"registry": "0x…"` (the Arbitrum Sepolia default is built in), the
wallet finds makers in the on-chain SeshRegistry instead of one configured
address. It asks every bonded maker for a quote over `.sesh`, checks each
against the price feed and the maker's bond cap, and takes the best one. The
Directory screen searches every listing by text or tag. Design, trust limits
and the placeholder bond amounts: [REGISTRY.md](REGISTRY.md).

## Multi-chain (Arbitrum Sepolia default)

The wallet, swapd (maker), and the relayer share a per-chain table in
`swapd/internal/chaincfg`: chain id, RPCs with failover, contract and mock
USDC addresses, explorer URL, and price feeds. The default chain is
**Arbitrum Sepolia** (chain id 421614). Sepolia is still fully supported.

Set `"chain": "arbitrum-sepolia"` or `"chain": "sepolia"` in the JSON config.
A legacy file that already has Sepolia RPCs or the Sepolia contract address,
and no `chain` field, stays on Sepolia. Explicit RPC, contract, and feed
fields always win over the table.

Transaction links in the UI use the chain's explorer (`sepolia.arbiscan.io`
on Arbitrum Sepolia, `sepolia.etherscan.io` on Sepolia). The network label
follows the chain.

## Fair price

On the quote screen the wallet reads an on-chain XMR/USD AggregatorV3 and
warns if the maker's quote is more than `fairMaxBps` (default 500, or 5%)
away from that fair price.

**There is no official Chainlink XMR/USD feed on Arbitrum Sepolia** (the
Chainlink Arbitrum Sepolia feed directory 404s, and no XMR/USD aggregator
was found). A test-only `MockAggregatorV3` (8 decimals, same interface) is
deployed instead; the quote screen says so. Official Chainlink ETH/USD
(`0xd30e2101a97dcbAeBCBC04F14C3f624E67A35165`) is used for ETH quotes.
USDC quotes treat 1 USDC as $1.

## Tests

```
cd swapd
go test ./internal/wallet/ ./internal/keys/ ./internal/srouter/...
```

`internal/srouter` tests use a fake router (the test binary re-runs itself
and serves DNS), so they need no network and no capabilities. They cover
the ini (client only, no route or resolv.conf changes), fresh dirs and
ports per launch, start/stop, plain-English errors (missing binary, no
permission, crash), stale-dir cleanup, and that a `kill -9` of the wallet
also stops the router. `internal/wallet/private_test.go` covers start on
first quote, stop when idle and on lock, direct mode never starting the
router, and switching modes in Settings.

## Live test

Live Sepolia and stagenet runs through this wallet: ETH→XMR with a mid-swap
restart, a forced ETH refund, a mock-USDC→XMR swap, and a local Satchel
propose/approve demo.

## USDC

The wallet can sell mock USDC (6 decimals) as well as ETH. Set `usdc` in the
config to the token address. A quote uses `GET /v1/quote?usdc=5`, and the
maker's offer must be for that token (`offer.asset` and `service.assetDecimals: 6`).
The v3 contract already pulls the token with `transferFrom` after the wallet's
`approve`. The included `contracts/src/MockUSDC.sol` is test-only, mintable by
its deployer, and is not the real USD Coin.

A maker that offers an ERC-20 must set `policy.TokenRelayFee` (and the
relayer's `relayTokenMinFee`) in **token units**, strictly below the
smallest lock, and must have `SWAPD_ETH_KEY` so it can self-relay with
fee 0 if the relayer refuses. A leftover ETH wei fee (for example 1e12)
is rejected at startup: against a 6-decimal USDC lock it would be larger
than the locked amount and the claim would revert. The relayer advertises
its token fee at `GET /v1/fee?asset=0x…`.

## Test coins

On Arbitrum Sepolia with a stagenet Monero wallet, the home screen has a
**Get test coins** button. It sends this wallet's own addresses to the
SeshSwap faucet, does a few seconds of proof-of-work in the background, and
shows what was sent: 0.0005 ETH, 25 mock USDC and 0.02 stagenet XMR, or why
a coin wasn't sent (already claimed today, or that drip is paused until the
faucet is topped up).

The faucet is `https://faucet.seshswap.app` unless the config sets
`"faucetUrl"`; `"faucetUrl": "off"` hides the button. The button never shows
on a mainnet chain, with Monero mainnet, or in demo mode.

The wallet identifies itself with an `X-SeshSwap-Client` header. The wallet
is open source, so that header only keeps casual scripts out; the faucet's
real limits (one claim per address a day, three per IP, daily caps and the
proof-of-work) are enforced on the server.

## Send

The **Send** screen moves coins out of the wallet while it's unlocked. Pick
XMR, ETH or USDC, paste an address, enter an amount or tick **Send all**, then
**Review**. Nothing leaves until you press **Send** on the review screen.

- Addresses are checked before review: a Monero address must decode, its
  checksum must match and its network must be this wallet's (a mainnet address
  is refused on stagenet). An Ethereum address must be `0x` + 40 hex; if it
  mixes upper and lower case the EIP-55 checksum must match. The zero address,
  this wallet's own address and the USDC contract itself are refused.
- Coins an unfinished swap still needs stay put: XMR promised to a swap that
  hasn't locked it yet, ETH or USDC for a swap you started that hasn't locked
  yet, and 0.0002 ETH of gas per unfinished swap for its claim or refund.
  **Send all** of XMR is off while a swap is holding XMR.
- Review shows the amount, the network fee and the total. XMR: the wallet
  signs the transaction(s) at review (`transfer`/`sweep_all` with
  `do_not_relay`) and broadcasts them on Send, so the fee shown is exact.
  ETH/USDC: the fee shown is the most it can cost (gas estimate +25% at the
  current fee cap); the transaction is signed with those exact limits on Send.
  USDC sends pay their fee in ETH.
- A review is good for 3 minutes and works once; locking the wallet drops it.
  On Send the wallet checks the balance and open swaps again, then shows the
  transaction id and a block explorer link.

API: `POST /v1/wallet/send/prepare` `{coin, to, amount | all}` returns
`{id, amount, fee, feeCoin, total, expiresIn}`; `POST /v1/wallet/send/confirm`
`{id}` returns `{txid, url}` (a second confirm of the same id gets 409);
`POST /v1/wallet/send/cancel` `{id}` drops a review. All need the page token.

## Satchel hookup (local test)

Satchel sits alongside the wallet, not around it. A page on another local port
can ask for a quote and propose a trade (`POST /v1/satchel/propose`). The wallet
shows a confirmation window; the page cannot start the swap itself
(`POST /v1/swaps` from that origin is refused). Approve or reject only in the
wallet.

Run a stand-in page (this is not the real Satchel plugin):

    swap-wallet -config wallet.json -satchel-test 127.0.0.1:7677

Then open http://127.0.0.1:7677/. The real wallet stays at http://127.0.0.1:7676/.

## Exits

The **Exits** screen rents a Session Router exit by the hour (testnet
payments). Details for buyers and sellers are in [EXITS.md](EXITS.md). What
connecting does to this computer:

- The bundled router restarts with a saved key (`exit-router.key` in the
  data dir), because paid time belongs to that router address. It runs with
  its RPC on, `auto-routing=true`, and the exit mapped as a full tunnel, so it
  installs a **default route** through its adapter.
- The exit carries web traffic (TCP 80 and 443) and its own DNS. Everything
  else is dropped, so other programs stall until you disconnect.
- Once the routes are up, the wallet points **system DNS** at the exit's
  resolver through a small root helper (`sudo -n`, see EXITS.md), with
  systemd-resolved or by rewriting `/etc/resolv.conf`. It puts DNS back on
  disconnect, on lock, on close, when the routes drop, and on the next start
  after a crash.
- The page shows three checks: the session to the exit is up, the exit
  accepts traffic, and this computer's routes are installed (with the
  router's reason when they aren't). It says **Connected** only when the
  routes are installed, and only then switches DNS. If they don't come up
  within 30 seconds it says why and offers Retry or Disconnect; it keeps
  checking every few seconds while the exit is mapped.
- The connection stays up until you press Disconnect or the paid time ends.
  Locking or closing the wallet disconnects first.
- Buy and Add 1 hour each show a confirm step with the price and the new
  end time. Paid hours are not refunded if the exit goes down.

`exits.json` in the data dir lists your purchases (exit, hours, times, tx).
It holds no keys, but it is not encrypted.

## Local API

The page and its API are on `127.0.0.1` only. Each launch makes a random
token and writes it into the page; every `/v1` call must carry it
(`X-Wallet-Token`). Browser pages on other sites or ports can't read it, so
they can't spend, connect an exit or read balances. The only calls open to
another local page are `GET /v1/quote` and `POST /v1/satchel/propose` (see
Satchel above). A program running as your user can still read the page, so
this does not protect against malware on your account; moving the exit and
DNS work into a separate process is later work.
