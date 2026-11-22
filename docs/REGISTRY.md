# SeshRegistry: a bonded, on-chain list of .sesh makers and sites

Testnet only, not audited.

One permissionless contract (`contracts/src/registry/SeshRegistry.sol`) on
Arbitrum Sepolia. Every listing has the same shape:

| field | meaning |
|---|---|
| `sesh` | the `.sesh` name (lowercase `a-z0-9-` label + `.sesh`, no port). One listing per exact name. |
| `tag`  | a short lowercase word, packed into `bytes32` (`maker`, `forum`, `shop`, …) |
| `meta` | free text, at most 280 bytes (shown in search) |
| `owner`| the address that registered it |
| `bond` | ETH held by the contract |

There is no admin, no owner key, no upgrade path and no fee. The parameters
are fixed in the constructor. Events (`Listed`, `MetaUpdated`, `ToppedUp`,
`ExitRequested`, `ExitCancelled`, `Withdrawn`, `Challenged`, `ChallengeAnswered`,
`ChallengeUpheld`, `Committed`, `Acked`, `Released`, `Slashed`) are the index.
The wallet rebuilds the list from them into a local file and searches it
off-chain. Before it trusts a maker for a quote it reads that listing again
with `info(id)`.

## Why ETH bonds, not USDC

Bonds are ETH only:

- One asset keeps the cap and slash maths simple: a maker's bond, its cap
  and its penalty are all in wei.
- Every Arbitrum user already holds ETH for gas, and there is no `approve`
  step.
- USDC can be frozen by its issuer at any address, including this contract's.
  A bond that a third party can freeze is a weaker bond.

Swaps themselves can still be in USDC. For a USDC swap the contract values it
in ETH with the Chainlink ETH/USD feed (`maxFeedAge` freshness check) before
the cap and penalty checks.

## Two kinds of listing

### Makers (`tag = maker`): larger bond, slashable through acks

- Bond at least `makerMinBond`. Largest swap the maker may quote:
  `bond × capFactor` (in ETH value). The wallet does not ask a maker for an
  amount over its cap, and the maker daemon refuses one.
- For each swap the maker signs an EIP-712 `Commitment` (listing id, taker,
  claimer, Monero claim key, asset, value, XMR amount, penalty, attest window,
  expiry). It sends it with its swap terms. The wallet checks the signature
  against the listing owner it read from the chain.
- Right after the taker's ETH lock in XmrSwapV3, the wallet calls
  `commit(swap, commitment, sig)`. The contract checks the V3 swap is really
  locked with exactly those terms, that the amount is within the cap, that
  the maker's free bond covers the penalty, and that the attest deadline is
  before `t1`. Then it reserves `penalty = max(minPenalty, value × penaltyBps)`
  from the bond.
- The maker then has `attestWindow` seconds to post `ack(swapId, sig)`, a
  signature by the listing owner saying "my XMR is locked". Anyone may submit
  it. The maker's own daemon is its watchtower and acks automatically once its
  XMR lock is sent. Anyone can run another watchtower with the same signed ack.
- If the attest deadline passes with no ack, and the V3 swap is not READY,
  CLAIMED or REFUNDED, and the taker did not cancel before the deadline,
  anyone can call `slash(swapId)`. The penalty goes to the taker. The wallet
  does this itself, before it refunds.
- Each signed commitment can be recorded once, for one swap.
- After an ack, or once the V3 swap is READY or CLAIMED, `release` frees the
  reservation.

Honest limit: Ethereum cannot see Monero. The ack is the maker's own
statement, so this slashes a maker that *didn't say* it locked in time, not
one that lied about it. A maker that acks but never locks loses nothing here.
The swap protocol still refunds the taker's ETH in that case, so the taker
loses time, not funds. The bond prices in the griefing that the swap
protocol alone can't punish: walking away after the taker locked.

### Everything else (any other tag): small refundable bond, challenge only

- Bond at least `siteMinBond`. This is anti-spam, not insurance.
- Anyone may `challenge(id)` by posting `challengeBond`. The owner must call
  `answer(id)` from the owner key within `challengeWindow`. If it does, it
  keeps its listing and receives the challenge bond. If it doesn't,
  `resolveChallenge` removes the listing and pays its bond plus the challenge
  bond to the challenger.

Honest limit: this only proves the *owner key* is alive and watching. It does
not show that the site answers, is honest, or isn't a near-copy of another
name (exact duplicates are refused; look-alikes are not). Treat a site
listing as "someone paid a little to put this name here and still controls
the key", nothing more. The wallet's Directory screen says so.

## Leaving

`requestExit` hides the listing at once (wallets stop using it). `withdraw`
returns the bond only after `exitDelay`, with no open commitments and no open
challenge, so the bond stays slashable for that whole time. `cancelExit`
reverses the request. `topUp` is open to anyone. A failed ETH transfer is
credited to `owed[]` for a pull with `withdrawOwed`.

## Placeholder amounts (constructor arguments)

These are **placeholders** for the testnet, chosen to be cheap. They are not
recommendations for real money:

| parameter | placeholder |
|---|---|
| `siteMinBond` | 0.0002 ETH |
| `makerMinBond` | 0.002 ETH |
| `capFactor` | 5 (the minimum maker bond quotes up to 0.01 ETH) |
| `penaltyBps` | 500 (5% of the swap's ETH value) |
| `minPenalty` | 0.00005 ETH |
| `challengeBond` | 0.0001 ETH |
| `exitDelay` | 3 days |
| `challengeWindow` | 2 days |
| attest window | 10 to 30 minutes |
| USDC valuation | ETH/USD feed, at most 2 days old |

`swapd registry deploy` uses these (see `registry.PlaceholderParams`).

## Maker CLI

The listing key is read from `SWAPD_REGISTRY_KEY` (hex), never from a flag or
the config file. Commands:

```
swapd registry -config maker.json register -sesh <name>.sesh -tag maker -bond 0.002 -meta "…"
swapd registry -config maker.json topup -id N -amount 0.001
swapd registry -config maker.json update -id N -meta "…"
swapd registry -config maker.json exit -id N        # then, after the delay:
swapd registry -config maker.json withdraw -id N
swapd registry -config maker.json info -id N
swapd registry -config maker.json list [-tag maker] [-q text]
swapd registry -config maker.json challenge|answer|resolve -id N
```

With `"registry": {"contract": "0x…", "listingId": N}` in the maker config,
`swapd` signs a commitment for every swap and runs the acker. It refuses
to start if the key does not own the listing or the listing isn't a maker.

## Wallet

- With a registry configured, Get a quote asks every active maker over
  `.sesh` in parallel (through your own or the bundled Session Router; see
  WALLET.md). It skips makers whose cap is below the amount, refuses quotes
  more than `fairMaxBps` worse than the price feed and quotes from a server
  that claims a different listing, then picks the most XMR. The quote screen
  lists every maker asked and why each was or wasn't used.
- The configured single maker stays as a fallback when no bonded maker gives
  a usable quote.
- After the lock the wallet records the commitment, watches for the ack, and
  slashes on a miss. The swap itself (claim/refund) does not depend on any of
  this.
- Directory screen: search all listings by text or tag.
