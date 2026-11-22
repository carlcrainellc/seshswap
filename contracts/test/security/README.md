# Security fuzz suite

Test-only. Nothing here deploys or sends transactions.

    cd contracts
    forge test --match-path 'test/security/*'                      # quick
    FOUNDRY_PROFILE=security forge test --match-path 'test/security/*'  # 10k fuzz runs, 1000x128 invariants
    FOUNDRY_DYNAMIC_TEST_LINKING=false halmos --match-contract HalmosChecks
    FOUNDRY_DYNAMIC_TEST_LINKING=false medusa fuzz --config test/security/medusa/medusa.json

Files
- `XmrSwapV3Tokens.invariant.t.sol`: V3 with plain, false-returning, USDT-style,
  blacklisting, re-entering and fee-on-transfer tokens, a contract owner that
  rejects ETH, and an attacker (wrong secrets, wrong callers, forged or
  redirected signatures, pulling other people's owed balances).
- `RegistryLease.invariant.t.sol`: SeshRegistry listings, challenges, exits,
  maker commitments against real V3 locks, and ExitLease offers and purchases,
  with a re-entering payout contract.
- `Stateless.fuzz.t.sol`: input fuzzing of each entry point, exact time boundaries.
- `Halmos.t.sol`: symbolic checks of owner checks and time boundaries.
- `PoC.t.sol`: the findings below, now regression tests (they fail if the bug comes back).

Findings (H-1, H-2 fixed on security/contract-fixes; L-1 fixed in swapd on
security/go-fuzz; M-1 fixed with a lock-in window on security/m1-lockin)
- H-1 `test_PoC_H1_CommitmentReplayAcrossSalts`: one signed maker Commitment can
  be recorded against many V3 swaps (it does not bind the swap id). The taker
  slashes every extra one. SeshRegistry.sol `commit` (452-485), Commitment struct (65-79).
- H-2 `test_PoC_H2_CancelThenSlash`: the taker locks, commits and cancels in one
  block, so the maker never locks XMR and never acks, then slashes. `slash`
  (515-528) allows CANCELLING / REFUNDED. Drains the bond down to the minimum.
- M-1 `test_PoC_M1_AckFrontRunKeepsPremium`: the taker front-runs `ackLocked`
  with `cancelIntent` and keeps the premium. Fix: no `cancelIntent` for
  `LOCK_IN` (20 min) after the lock. Boundary fuzz `testFuzz_M1_*`,
  `invariant_LockInProtectsAck`, Halmos `check_cancelOnlyAfterLockIn`.
- L-1 `test_ExitPay_zeroRefundToIsUndeployable`: money sent to an ExitPay address
  with refundTo = 0 (or hours/maxCost = 0) is stuck for good. ExitPay.sol 202-214.

## Go fuzz (swapd)

Native `go test -fuzz` targets for the parsers that read untrusted input
(`fuzz_test.go` in registry, exits, eth, keys, relay, api, wallet). Seeds run
with plain `go test`. To fuzz one target:

    cd swapd && go test ./internal/relay -run '^$' -fuzz '^FuzzRelayHTTP$' -fuzztime 3m

Crashers found so far have a `regress_test.go` next to them.
