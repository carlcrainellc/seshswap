# Changelog

## Unreleased (0.1.8)

- You can choose how many Monero confirmations to wait for: Settings →
  Monero confirmations, with Standard, Safer (at least 20) and Safest (at
  least 30). The quote shows the count and how much longer it takes.
- The wallet asks the maker for that number. Makers quote it with longer
  timeouts to match, or decline if it's over their limit (`maxConfs`,
  default 60). A quote from an older maker that offers fewer is refused.
  `swapd take -min-confs` sends the same request.
- Makers can set size-based spreads with `spreadTiers` (for example 1 %
  under 5 XMR, 2 % from 5 XMR, 4 % from 20 XMR). Without it the flat
  `spreadBps` works as before. The quote screen now shows the market rate,
  the maker's markup and your rate before you confirm.
- Larger swaps wait longer sooner: 15 confirmations from 1 XMR, 20 from
  5 XMR and 30 from 20 XMR (was 10, 50 and 200 XMR). Under 1 XMR is still 10.

No contract changes.

## 0.1.7

- Makers now need a gas key. `swapd maker` won't start without
  `SWAPD_ETH_KEY` (ETH makers too, not just token makers), so a maker can
  always submit its own claim if the relayer refuses or never sends it.
  Fund the key with a little ETH for gas.
- A taker now cancels before t1 even when it can't reach its Monero wallet
  or node, instead of retrying the check until the window closes.
- swapd reopens its Monero wallet if monero-wallet-rpc restarts underneath
  it, rather than failing every step until swapd is restarted.
- The relayer and maker HTTP servers have read, write and idle timeouts.
- New optional `service.maxOpenQuotes` caps unfunded quotes (off by default).
- New end-to-end tests with misbehaving makers, takers and relayers
  (build tag `hostile`).

No contract changes; same Arbitrum Sepolia contracts as 0.1.6.

## 0.1.6

- The wallet no longer freezes when the Monero node stops answering. Screens
  stay responsive and a stuck step gives up after a few minutes and retries.
- Two sends confirmed quickly one after another can no longer spend funds an
  open swap still needs.
- The relayer drops a repeated copy of a transaction it already sent, instead
  of paying for it again.
- New fuzz and sequence tests for Send, the lock-in window, the relayer and
  Monero stalls.

No contract changes; same Arbitrum Sepolia contracts as 0.1.5.
