# Changelog

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
