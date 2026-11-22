# ETH/ERC-20 ⇄ XMR atomic swap — v3 design

v3 builds on v2 (sections 1–11, updated where v3 changed them) and adds the
two-step "freeze, then reveal" claim and cancel (§12), the taker premium against
the free option (§13), an always-on maker service (§14), the confirmation and
timing policy (§9, §15), and a design for the reverse direction (§16). The
contract is a new one, `XmrSwapV3.sol`; `XmrSwapV2.sol` and its tests are kept
unchanged.

The desktop wallet ([docs/WALLET.md](docs/WALLET.md)) is the UI: it runs the
swap daemon and talks to makers directly or over Session Router. Exits
([docs/EXITS.md](docs/EXITS.md)) are a paid feature on top of it, testnet only:
the wallet rents a Session Router exit by the hour.

> Test-only. Not audited. Local devnets only. No real keys.

## Goal
The safest practical way for someone holding ETH or a token (USDC etc.) to buy
XMR from someone holding XMR, with no middleman, no custody, low fees, and no
need for the XMR seller to own ETH.

## What we learned from earlier projects
| Project | Lesson |
|---|---|
| AthanorLabs (archived) | Protocol is sound, but ~$11 mainnet gas, ~40 min, no liquidity, and the ETH side couldn't be an automated market maker (gas griefing). |
| MoneroSwap (hbs, Gnosis) | Cheap chains matter; a simple contract is easier to trust. |
| eigenwallet / COMIT (BTC-XMR) | Liquidity comes from always-on **makers** (the "ASB") that quote and swap automatically. Watchtower-style daemons with saved state are what make it usable. |
| Silo (Solana) | Checks Monero (ed25519) keys **on-chain**, so no cross-curve proof is needed. |

## Choices (and why)

### 1. Check the Monero key on-chain instead of a cross-curve proof
Options:
* **A. secp256k1 `ecrecover` trick + cross-curve DLEq proof** (AthanorLabs, v1 prototype). ~4k gas to check, but safety depends on an unaudited DLEq library (go-dleq, LGPL) and a ~1–2 kB proof exchanged off-chain. A bug there loses funds silently.
* **B. Verify ed25519 directly in Solidity.** The contract stores each party's Monero key share (a public ed25519 point) and checks `secret·B == point` when the secret is revealed. No DLEq, no second curve, no proof library. Costs ~94k gas per check.
* **C. Adaptor signatures** (Gugger paper). Needs ECDSA adaptor signatures plus DLEq again; more moving parts for Ethereum, which already has programmable timelocks.

**Pick B.** On the target chains (Base, Arbitrum, Optimism, Gnosis) execution gas is cheap, so ~94k extra gas costs a fraction of a cent, while removing the single riskiest dependency. The scalar multiplication is written from the curve definition (two 15-entry precomputed tables, extended coordinates, MODEXP precompile for the one inversion), checked against `filippo.io/edwards25519` with fixed vectors and differential fuzzing. On Ethereum mainnet option A would be cheaper; we don't target mainnet.

### 2. One contract, many swaps, minimal storage
Each swap is one status slot (stage, a 64-bit deadline, and the premium recipient — v2 used one byte) keyed by `keccak256(params)`; the parameters live in the `Locked` event and in calldata. No admin, no upgrade key, no fees to the contract, no owner. Same address can serve everyone on a chain.

### 3. Exclusive windows (why they never overlap)
v3 splits every secret-revealing action into two steps: first a notice *without*
the secret (an "intent"), then the reveal inside a freeze that the notice starts
(details in §12). The windows for the notices:

| Time | Alice (ETH side) | Bob (XMR side) |
|---|---|---|
| lock → t1, not ready | may mark ready, or send a **cancel intent** | — |
| ready → t2 | — | may send a **claim intent** |
| t1 → t2 (even if not ready) | — | may send a **claim intent** |
| after a claim intent, until its deadline | frozen | may reveal (claim) |
| after a cancel intent, until its deadline | may reveal (refund) | frozen |
| after t2 with no claim intent, or after an unrevealed intent expires | may refund | — |

At any moment **at most one person can act** (a fuzz test checks this for every second of a swap's life). This matters because both actions publish a key share: if both were allowed at once, whoever saw the other's transaction in the mempool could submit theirs too and end up knowing both halves. We considered letting Bob claim after t2 "until Alice refunds" (to rescue a late Bob), but it lets Bob front-run Alice's refund, so we rejected it.

**A secret is public the moment its transaction is broadcast.** A claim or refund that is censored, delayed past its deadline, or reverts still exposes the key share: reverted transactions are on-chain with their calldata, and pending ones sit in the mempool. Nothing in the contract can make it private again. So:

* **Contract floor:** each window (`d1`, `d2`) must be at least 30 minutes (`MIN_D1`, `MIN_D2`), so a swap with a 1-second window can't be created.
* **Daemon margin (the real protection):** `swapd` never *signs* a claim or refund unless more than `margin` of its window is left (default 30 min; use ≥ 1 h on Arbitrum, whose sequencer may skew timestamps by up to ~1 h). In v3 the window for a reveal is the freeze started by the party's own intent (default 6 h), not t1/t2, so the margin is easy to meet. A refund after which the other side can never act again (a final block is past t2 with no claim intent, or the maker's intent expired) has no deadline. Time is always the chain's block timestamp, never the local clock.
* **Finality before revealing (new in v3):** a daemon reveals only after its own intent is `ethConfs` blocks deep, and a late refund only once a block `ethConfs` deep has a timestamp past t2 (or past the expired intent's deadline). Otherwise a short reorg could remove the intent, or bring back a moment at which the other side could still act, after the secret is public.
* **Crash safety for reveals:** a signed claim/refund is saved with its limits before it is broadcast. If the daemon crashes and comes back after `deadline − margin`, a transaction the node has never seen is **dropped, unpublished** (the secret stays private). One that was already broadcast is re-sent until its deadline; after that it is replaced by a same-nonce no-op with higher fees (best effort — by then it may already be public).
* **No early leak to the RPC node:** claims and refunds use a fixed gas limit instead of `eth_estimateGas`, so the secret isn't sent to the node before we decide to broadcast.
* Handing a signed claim to a relayer counts as a reveal and follows the same margin (see 5).

### 3a. What a swap id binds, and who checks it
The swap id is `keccak256(abi.encode(Swap))`, and `Swap` holds owner, claimer, both Monero key shares (`claimKey`, `refundKey`), t1, t2, the intent freeze `grace`, asset, value, `premium` and salt. The keys are stored as the **full 32-byte ed25519 point encoding** (read as a little-endian integer), and `checkSecret` compares all 256 bits of `secret·B` against it (with `0 < secret < l`). There is no hashing or truncation (unlike v1, which compared only the low 160 bits — an Ethereum address). So a swap id binds every term of the trade.

Each daemon checks the swap **as read back from the chain** (the `Locked` event in the receipt or log scan of the configured contract, whose fields must hash to the event's id), never what the other side claims:
* The **maker**, before it locks any XMR, checks: id = hash(fields), claimer = its fresh claimer address, `claimKey` = its own share, `refundKey` = the taker's share that passed the proof-of-knowledge check, asset, value **exactly** the agreed amount, owner = the taker's announced address, t2 > t1, the claim window ≥ agreed `d2`, t1 not later than agreed, `grace` and `premium` exactly as agreed, and enough time before t1 to lock XMR and let it confirm. It also requires the on-chain status to be PENDING.
* The **taker** reads its own lock back the same way and checks every field against what it meant to lock and the maker's verified key. On any mismatch it never marks the swap ready; it only refunds.

**Peer keys:** every point received from the other side is decoded canonically and refused if it is the identity, small-order, or has any small-order component (checked as `[8⁻¹]([8]P) = P`), and must come with a Schnorr proof of knowledge bound to the session and role. Only then is it added into the shared Monero key. Scalars are decoded with `SetCanonicalBytes` (exactly 32 bytes, < l, non-zero); random scalars come from 64 bytes of `crypto/rand` with the error checked.

**Secrets at rest:** each daemon stores only its own key share (plus the other side's *public* point and the view-key shares), encrypted in its own store. The other side's spend share is stored only after it has been revealed on-chain.

### 4. Payouts never revert after the secret is checked
If sending ETH or a token fails (recipient contract rejects ETH, USDC blacklist, paused token, a token returning `false`), the amount is credited to `owed[asset][recipient]` and can be withdrawn later to any address. Otherwise a failed payout would publish the secret in a reverted transaction (calldata of a reverted tx is still public) and hand the other side both halves. Token calls copy at most 32 bytes of return data and accept no-return (USDT-style) tokens. Fee-on-transfer tokens are rejected at lock time. Rebasing tokens are not supported (daemons use a token allowlist).

### 5. Gasless claim for the XMR seller
Bob signs an EIP-712 message `Claim(swapId, to, relayer, fee)` with the key named as `claimer` — an address that never needs ETH. Anyone (or only `relayer`, if set) submits it and is paid `fee` from the swap. The signature fixes the recipient and fee, so a relayer cannot redirect funds or take more; copying the transaction only changes who gets the fee. Signatures can't be replayed across swaps (swap id), contracts or chains (EIP-712 domain). **Residual trust:** a relayer learns Bob's secret before it is on-chain. That is harmless while only Bob may act. In v2, a relayer that sat on it past t2 could let Alice refund *and* take the XMR. In v3 Bob only hands out the secret after his claim intent is final, so Alice is frozen until the intent's deadline (default 6 h); a relayer would have to withhold it for that long, while Bob's daemon re-sends to other relayers (or self-relays) every `RelayTimeout`. The intent itself carries no secret, so relayers submit it (and the "XMR locked" ack, §13) for free through `POST /v1/intent`; they are paid when they later submit the claim.

### 6. Who is the maker
The **XMR holder is the maker** (like eigenwallet's ASB): it publishes a quote, waits for a taker to lock ETH/tokens *first*, then locks XMR. This way the always-on maker never pays gas (gasless claim) and can't be gas-griefed; the taker (ETH side) only risks their own gas. Makers can quote ETH or allowlisted ERC-20s, with min/max sizes and a spread.

### 7. Transport
Plain HTTP+JSON, designed to sit behind a **Tor onion service** or a **Session Router `.sesh` address** (the name authenticates the server and hides both IPs). Only the setup messages go over it (quote, key shares, view-key shares); after the lock, everything else is read from the two chains, so a broken connection can't lose funds. libp2p was considered (AthanorLabs used it) but adds a large dependency and NAT/peer-discovery complexity for little gain at our scale. The `.sesh` transport is in `swapd/internal/sesh`: `.sesh` URLs are dialled only through Session Router DNS, never the system resolver. A Satchel offer board and companion remain design-only. A Tor onion service remains a valid way to publish the same HTTP API.

### 8. Liveness: the daemon is a watchtower
`swapd` keeps one encrypted state file per swap (Argon2id passphrase → XChaCha20-Poly1305; write-temp, fsync, rename). Secrets are saved **before** the matching public key is sent anywhere, and every irreversible step is recorded as "intent" before it is taken and checked against chain state on restart (for example, it re-checks the shared Monero address before re-sending XMR). On start it resumes every unfinished swap and keeps acting before each deadline: Bob claims as soon as ready (or after t1), Alice marks ready once the XMR has enough confirmations, refunds at t2, and both sides sweep the XMR once they hold the full key.

### 9. Speed and confirmations (corrected in v3)
v2 said Alice could mark ready after "2–3 confirmations for small amounts". **That was wrong, and v3 drops it.** Monero mainnet had an **18-block reorg on 14 Sep 2025** (Qubic mining pool; blocks 3,499,659–3,499,676, 118 transactions invalidated) and a 10-block one on 18 Sep 2025. If Alice marks ready on a lock that is later reorged out, Bob claims the ETH and Alice's XMR is gone. And there is nothing to gain from fewer confirmations anyway: a received Monero output cannot be **spent** until it is 10 blocks deep, so Alice could not use coins confirmed 3 times any sooner.

v3 rule (`swap.XmrConfs`): **never fewer than 10 confirmations**, rising with the amount and with the deepest recent reorg our monerod has seen (`get_alternate_chains`, if `xmrDaemonRpc` is set):

| XMR in the swap | Confirmations before Alice marks ready |
|---|---|
| < 10 XMR | 10 |
| 10 – 50 XMR | 15 |
| 50 – 200 XMR | 20 |
| ≥ 200 XMR | 30 |
| any, after a reorg of depth d was seen | at least d + 4 |

The taker applies this rule itself (the maker cannot lower it), and the maker sets t1 long enough for it (§15). Because the count is ≥ 10, the shared output is already spendable when Bob claims, so Alice's sweep goes out at once; her coins are spendable in her own wallet 10 blocks after the sweep. The quote and the daemon tell her when: `estReadySecs` / `estSpendableSecs` in the quote, "spendable from height N" in the log, `spendableAtHeight` in the swap status. See README for expected times.

**Reorg trade-off, honestly:** more confirmations make a swap slower (each block ~2 min) but not safer than the deepest reorg an attacker can afford. 10 blocks would not have survived the 18-block Qubic reorg; a swap that marked ready at 10 confirmations during that attack could have been robbed. The `d + 4` rule reacts only after our daemon has *seen* a deep reorg, and an attacker who can do a 30-block reorg can defeat every tier. Large swaps should be split, or wait longer. There is no reorg tracking after `setReady` (once ready, Bob can claim; that is the point of no return). On the Ethereum side `ethConfs` (default 2 blocks) guards the lock, the intents and the late-refund timing; on L2s with a centralized sequencer that is a soft finality and a sequencer reorg remains a risk.

### 10. Chains and cost
Target: Base, Optimism, Arbitrum, Gnosis. No chain-specific code; the contract uses Cancun opcodes (`TSTORE`) available on all four. Measured (forge, V3): lock with premium ~66k, setReady ~34k, claim intent ~46k, relayed claim ~189k — about 335k for a full ETH swap (~77k more than v2, for the intent step and the premium); see README.md.

### 11. Privacy (the Ethereum side is public)
* The `Locked` event links Alice's address, amount and timing; the claim links Bob's `to` address. Mitigations: fresh addresses per swap (the daemon derives one for `to` and for `claimer`), relayers so Bob's address is never funded, round amounts/standard sizes, and random delays before sweeping.
* Monero side: both parties know the shared view key, so each sweeps to their own wallet immediately after the outputs unlock; the counterparty cannot see where it goes.
* Network: run the maker as an onion service; takers connect over Tor; use a privacy-respecting RPC (own node) for Ethereum.
* Exits are web only: while one is connected the exit carries TCP 80/443 and its own DNS, and everything else is dropped. The wallet's chain RPCs work through it because they are on 443; an RPC on another port (such as your own node) can't be reached until you disconnect.

### 12. Freeze, then reveal (two-step claim and cancel)
**Problem in v2:** the claim and the refund each publish a key share, and each had a hard deadline (t2 for Bob, t1 for Alice). A reveal that was censored, delayed by a congested or halted chain, or simply landed a minute late was public *and* failed, handing the other side both halves. The daemon's only defence was to refuse to reveal near the deadline, so a maker who came back 20 minutes before t2 had to give up a swap it could still have finished.

**v3:** each side first sends an *intent*, which carries no secret:
* `claimIntent(swap, sig)` — Bob (signed by the claimer key, so anyone can submit it; relayers do it for free). Allowed when Bob may claim (ready, or after t1, before t2). It moves the swap to CLAIMING with `deadline = max(t2, now + grace)`. Until that deadline only `claim` is possible: Alice cannot refund, even after t2.
* `cancelIntent(swap)` — Alice, before t1, while not ready, and not during the first `LOCK_IN` (20 min) after the lock (§13). It moves the swap to CANCELLING with `deadline = now + grace`. Until then only Alice's `refund` is possible: Bob cannot claim, even after t1.

Then the reveal: Bob's `claim` (or `claimRelayed`) only while CLAIMING and before the deadline; Alice's `refund` while CANCELLING and before the deadline. Each intent can be sent once. If a freeze ends without a reveal the swap goes on as if the intent had not happened, except that it cannot be repeated: an expired claim intent lets Alice refund at once; an expired cancel intent lets Bob send a claim intent from t1 on (never earlier, so an offline Alice is not rushed). `grace` is part of the swap terms (30 min ≤ grace ≤ 48 h in the contract; `swapd` default 6 h).

The daemons reveal only after their intent is final (`ethConfs` deep) and while more than `margin` is left before the deadline. So a reveal can be late by up to `grace − margin` (5½ hours by default) without any loss, instead of `margin` minutes.

**Why the intent does not leak anything:** it contains only the swap id (and Bob's signature over it). An observer learns "Bob intends to claim", nothing about either key share.

**What the sketch had and we did not keep:** an earlier sketch kept a one-step early refund for Alice next to the two-step one. v3 has **no** one-step reveal at all: both claim and cancel are two-step, because a one-step refund before t1 has exactly the v2 problem (a refund landing after t1 is public while Bob may claim). It costs Alice one extra transaction (~39k gas) and `ethConfs` blocks when she cancels.

**Tests:** forge unit tests for every stage transition and boundary, signature rules and replay; fuzz tests that for any time the intents' freezes are exclusive (`testFuzz_ExclusiveWindows`, `testFuzz_ClaimIntentFreeze`, `testFuzz_CancelIntentFreeze`); an invariant suite (random sequences of every call and time warps) that checks solvency, conservation of funds, final states stay final, no reveal during the other side's freeze, and premium routing. E2E: maker returns after t2−margin and still claims via intent; reveal censored by the relayer while the taker stays frozen past t2; intent censored → clean refund; reveal persisted then the maker returns inside deadline−margin → dropped unpublished → refund after the deadline → maker sweeps; cancel refund persisted then the taker returns inside deadline−margin → dropped → refund after t2.

### 13. The free option and the taker premium
**Problem:** after Bob locks XMR, Alice can still cancel before t1. If the XMR price rises she can walk away for the cost of gas, while Bob's XMR was tied up for hours (an option worth roughly 0.4·σ·√T of the notional, ~0.15–0.6 % for a few hours of XMR volatility). Repeated, this also griefs makers' liquidity.

**Options considered:** (a) a bond Alice posts and loses on any cancel (punishes honest cancels when Bob was slow); (b) a fee to the maker on every swap (taxes honest takers); (c) shorter t1 (limited by the 10+ confirmations); (d) **a premium that Alice locks with the swap and gets back unless she cancels *after* Bob proved his XMR is locked**. We picked (d).

**How it works:** the swap carries `premium` (native coin, paid with `lock` on top of the value; for ERC-20 swaps msg.value is the premium; `swapd` currently quotes a premium only for native-coin swaps, since a token swap would need a token→ETH price). After Bob's daemon has broadcast the XMR lock, it calls `ackLocked(swap, premiumTo, sig)` — a signed "my XMR is locked" from the claimer key, free via a relayer. The contract records `premiumTo`. Then:
* claim → premium back to Alice (owner), with the value to Bob;
* refund after a cancel intent made **after** the ack → premium to `premiumTo` (Bob), value back to Alice — even if Alice waits until after t2 to refund, so waiting out the freeze does not dodge it;
* any other refund (no ack, Bob never locked, Bob let t2 pass without claiming, Bob's intent expired) → premium back to Alice.

**Can Bob abuse the ack?** He could ack without locking XMR. Then Alice's daemon never sees the XMR, cancels, and loses the premium. This is why the premium is small (the maker's `premiumBps`, default 0, e2e 0.5 %; the taker refuses more than `MaxPremiumBps`, default 1 %), and why the ack is only possible before t1 and while PENDING. A taker who sees no XMR at all after an ack has evidence against that maker's identity key (§14). The ack is not a proof of the Monero lock; making it one would need the maker to prove a Monero output to the contract, which we did not attempt.

**Lock-in window (fixes a front-run of the ack):** Alice can watch the XMR arrive (she has the shared view key) and send `cancelIntent` before Bob's `ackLocked` is mined; the ack then fails and she keeps the premium. So for `LOCK_IN` = 20 minutes after `lock` the contract refuses `cancelIntent` (the end time is kept in the status slot's `deadline` while PENDING/READY; no extra storage). An ack inside the window always lands before any cancel. `LOCK_IN` is a constant (nobody can lengthen or shorten it), shorter than `MIN_D1` (30 min), so it ends before t1; it never touches refunds, setReady, or Bob's claim window. Bob's daemon acks as soon as its XMR lock is broadcast and logs when an ack is late; an ack sent after the window can still be front-run. Alice's daemon waits for the window before it cancels, and the wallet shows when an early cancel becomes possible. Tests: `test_PoC_M1_AckFrontRunKeepsPremium`, `testFuzz_M1_LockInBoundary`, `testFuzz_M1_AckInWindowAlwaysPaysPremium`, `invariant_LockInProtectsAck`, Halmos `check_cancelOnlyAfterLockIn`.

**Reputation waiver:** the maker can set `waiveAfter`: no premium for a taker address that already completed that many swaps with it.

### 14. Always-on maker service
Like eigenwallet's ASB, `swapd maker` runs unattended:
* **Price:** median of Kraken, CoinGecko, Bitfinex and KuCoin (all public, no keys; checked live while writing this). A source more than 2 % from the median is dropped; at least 2 must agree; an optional XMR-per-ETH sanity range; a refresh that moves the rate by more than 10 % trips a circuit breaker; a price older than 2 minutes stops quoting. The maker's `spreadBps` is taken off the mid price. A USD stablecoin asset is priced at 1 USD.
* **Quotes with an expiry:** `GET /v1/quote?value=` returns the terms (XMR amount, confirmations, t1/t2, grace, premium, time estimates) and `expires`. The taker will not lock after `expires`; the maker aborts a swap whose lock it has not seen by then (and never locks XMR for it).
* **Limits and inventory:** per-swap min/max (offer), a total exposure cap (`maxExposure`: XMR promised plus XMR locked in unfinished swaps), and the wallet's unlocked balance. All three are checked before every new swap.
* **Recovery:** every swap's state is on disk before each irreversible step (as in v2); a panic in one swap's step is caught and retried without stopping the others.
* **Health and metrics:** `GET /healthz` (503 if the price breaker is open, or the wallet or Ethereum RPC is down) and `GET /metrics` (Prometheus text: swaps by phase, XMR reserved/locked/balance/unlocked, exposure limit, prices, sources, price age, breaker state, reorg depth, request counters).
* **Offer discovery hook:** `GET /v1/offer.signed` (and `swapd offer-doc`) returns a signed offer document: JSON bytes plus an ed25519 signature from the maker's long-term identity key (kept in the encrypted store). Because the exact bytes are signed, the document can be carried by anything — HTTP, a bulletin board, Nostr, a Satchel/.sesh session — and checked without trusting the carrier. Each swap response is also signed by the same key, binding the per-swap claimer address and every term to the maker; a taker that pinned the maker id (`-maker-id`) refuses quotes signed by anyone else.

### 15. Timing: t1, t2 and the 30-minute floors
`swap.Windows` picks the windows from the confirmations needed: `d1 = XmrLockBudget + ConfirmSecs(confs, blockSecs) + 2 × margin`, where `ConfirmSecs = confs × 2 × max(observed mean, 120 s)` (twice the observed Monero block time, never below the 2-minute target); `d2 = 2 × margin`; both at least the contract's 30 minutes. `blockSecs` is measured by monerod. A missing daemon is a hard error when the maker builds windows and when the taker checks t1, unless `xmrBlockSecs` is set explicitly — assuming 120 s silently made stagenet windows too short (stagenet blocks are ~280 s). With defaults and a 120 s mean (margin 30 min, 10 confirmations) that is d1 ≈ 1 h 45 min, d2 = 1 h, grace 6 h.

**Should the contract's 30-minute minimums be lowered?** We argue no, and did not change them:
* The happy path never waits for t1 or t2. A swap finishes as soon as the confirmations are in and the two short transactions are mined. Shorter windows would only make the *failure* cases faster.
* With ≥ 10 Monero confirmations (~20 min plus variance), d1 cannot usefully be below ~30 min anyway.
* d2 only has to cover getting the claim intent mined (§12). 30 minutes is already short for an L2 sequencer outage; the floor protects careless configurations.
* The windows are a floor, not a target: the daemons choose longer ones.
So `MIN_D1 = MIN_D2 = 30 min` stay, and no test for a lower floor was needed.

Worst cases with defaults: if Bob vanishes after locking ETH-side terms, Alice gets her refund at t2 ≈ 2 h 45 min after her lock. If Bob sends a claim intent and never reveals, Alice waits until the intent deadline (≤ t2 + 6 h). If Alice vanishes, Bob claims from t1 on and Alice sweeps her XMR whenever she returns.

### 16. Reverse direction (XMR → ETH): designed, not built in this pass
Here the maker sells ETH for the taker's XMR. Roles on the contract stay the same — **whoever holds ETH is the contract's owner and locks first; whoever holds XMR is the claimer** — so the maker becomes the owner and the taker the claimer. The engine already dispatches by role, so the plan is:
1. The taker (XMR seller) requests a quote with `direction: "xmr2eth"` and sends its claimer address and key share; the maker locks ETH (with the taker as claimer). The maker pays gas; the taker needs none (gasless claim via relayers).
2. The taker locks XMR, the maker waits for ≥ 10 confirmations and marks ready, the taker sends a claim intent and reveals; the maker sweeps the XMR.
3. **Risks for the maker:** now the *taker* has the free option (it may never lock XMR, leaving the maker's ETH locked until t1 and costing it gas for lock + cancel + refund). Mitigations: exposure caps per taker and in total, short quote TTL, a small "lock fee" paid by the taker up front (would need a contract change), and reputation via the taker's claimer history.
Not built because it needs a taker-side XMR lock flow, a maker-side cancel policy and its own e2e suite; doing it properly did not fit this pass alongside §12–§15.

## Remaining gaps (before an audit / real money)
1. **Independent audit** of `XmrSwapV3.sol` (and `XmrSwapV2.sol`), `Ed25519.sol` (hand-written assembly; checked by vectors and differential fuzzing, not formally verified) and the daemon.
2. **Relayer trust window:** a relayer sees Bob's secret before it is on-chain; if it withholds the claim until after the intent deadline (≥ grace) and shares it with Alice, Alice could refund *and* take the XMR. v3 makes this window hours long and Bob re-sends to other relayers every `RelayTimeout`; a maker with **no** gas key relies on relayers entirely, so real makers should configure one. Not eliminated. Needs relayer discovery/reputation and private submission.
3. **L2 specifics:** sequencer downtime or censorship near a deadline; forced-inclusion delays (~12 h OP Stack, ~24 h Arbitrum) are longer than our default windows. Arbitrum timestamps can drift ~1 h. Per-chain margins/defaults need to be set and tested.
4. **Monero reorgs:** ≥ 10 confirmations, scaling with amount and observed reorg depth (§9), but an attacker able to reorg deeper than our count can still rob the ETH side; no reorg tracking after `setReady`; the sweep is considered done at 1 confirmation.
5. **Daemon robustness:** one global lock (steps run one swap at a time; a slow wallet-rpc delays other swaps — should schedule by nearest deadline); metrics exist but no alerting; no rate-limiting of quote requests — every accepted `/v1/swaps` request **reserves** maker XMR until its quote expires, so a spammer can tie up the maker's exposure budget (short TTL and the exposure cap bound the damage); secrets in memory are not wiped.
6. **Pricing:** live median price with breakers (§14), but prices are last-trade/aggregate values with no depth; an attacker who moves two of four venues can still skew quotes within the deviation and jump limits. Reverse direction (§16) designed, not built.
7. **Free option:** reduced, not removed. The premium is small and depends on the maker's ack, which is not a proof that XMR was locked (§13). A taker who cancels *before* the ack pays nothing, but cannot cancel during the 20-minute lock-in, so a prompt ack always counts.
8. **ERC-20 coverage:** rebasing, fee-on-transfer (rejected), upgradeable tokens that change behavior later; the daemons should use an allowlist (relayer already does).
9. **Transport:** plain HTTP; Tor onion setup is not built into swapd (bind `listen` and publish yourself). Session Router `.sesh` dialling is built (`swapd/internal/sesh`) and is the wallet's private connection. Signed offer documents and signed quotes exist (§14); an offer *board* (Satchel/.sesh) is not built.
10. **Public testnet soak** (Sepolia/Base-Sepolia + stagenet) of v3 — not done here by design.
11. **Finality on L2s:** `ethConfs` blocks on a sequencer chain are not real finality; a sequencer reorg after a reveal could still hurt. Per-chain `ethConfs` should be set to the chain's safe/finalized head.
