# Changelog

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
