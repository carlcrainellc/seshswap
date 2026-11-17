package e2e

import (
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

var oneEth = big.NewInt(1e18)

func take(t *testing.T, e *Env, taker, maker *Party, value *big.Int) *swap.Record {
	rec, err := api.Take(e.ctx, taker.eng, api.TakeParams{MakerURL: maker.makerSrv.URL, Value: value,
		MinXMR: 1, MinD1: 1800, MinD2: 1800, MaxConfs: 40, MinConfs: 2})
	if err != nil {
		t.Fatal("take:", err)
	}
	return rec
}

func check(t *testing.T, ok bool, msg string, a ...any) {
	t.Helper()
	if !ok {
		t.Fatalf("FAIL: "+msg, a...)
	}
	t.Logf("PASS: "+msg, a...)
}

// Happy path with a crash of each side in the middle; gasless relayed claim.
func TestHappyPathWithRestarts(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil) // maker has NO Ethereum key at all
	before := taker.xmrBalance()
	rec := take(t, e, taker, maker, oneEth)
	id := rec.ID
	check(t, rec.XMR == 2_000_000_000_000, "quote: 1 ETH -> %d piconero", rec.XMR)
	check(t, rec.MinConfs >= 10, "taker waits for at least 10 Monero confirmations (%d), never 3-5", rec.MinConfs)

	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	taker.Restart() // crash after locking
	e.drive("maker locked XMR", 30, func() bool {
		return maker.phase(id) == swap.MXmrLocked || maker.phase(id) == swap.MIntent || maker.phase(id) == swap.MClaiming
	}, maker, taker)
	maker.Restart() // crash after locking XMR
	e.drive("maker claimed", 60, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)

	bal, _ := e.eth.Balance(e.ctx, maker.payout)
	want := new(big.Int).Sub(oneEth, big.NewInt(1e15))
	check(t, bal.Cmp(want) == 0, "maker payout address received %s wei (1 ETH - relayer fee)", bal)
	var mrec swap.Record
	_ = maker.eng.Store.Get("swap-"+id, &mrec)
	if h, err := eth.ParseHash(mrec.ClaimTxHash); err == nil {
		if rc, _ := e.eth.Receipt(e.ctx, h); rc != nil {
			t.Logf("GAS claimRelayed (receipt, incl. intrinsic+calldata): %d", rc.Gas())
		}
	}
	cb, _ := e.eth.Balance(e.ctx, mrec.Claimer)
	check(t, cb.Sign() == 0, "claimer address never held ETH (gasless claim)")

	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, maker, taker)
	e.mineXMR(2)
	got := taker.xmrBalance() - before
	fee := sweepFees(taker, id)
	check(t, got == 2_000_000_000_000-fee, "taker received exactly 2 XMR minus its sweep fee: %d = 2e12 - %d", got, fee)
}

func lockedRecord(p *Party, id string) swap.Record {
	var r swap.Record
	_ = p.eng.Store.Get("swap-"+id, &r)
	return r
}

// Maker locks XMR, then disappears. Taker marks ready, waits until t2, refunds.
// Maker comes back later, sees the refund secret and recovers its XMR.
func TestMakerVanishesTakerRefundsMakerRecovers(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	maker.relayers = []string{"http://127.0.0.1:1"} // relayer unreachable
	maker.Restart()
	start, balStart := headNumber(e), e.ethBalance(tk.Address())
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("maker locked XMR", 40, func() bool { return maker.phase(id) == swap.MXmrLocked }, maker, taker)
	xmrAfterLock := maker.xmrBalance()
	// maker goes offline now
	e.drive("taker ready", 20, func() bool { return taker.phase(id) == swap.TReady }, taker)
	r := lockedRecord(taker, id)
	e.warp(r.Swap.T2 - uint64(nowETH(e)) + 1)
	e.drive("taker refunded", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	fees := e.feesPaid(tk.Address(), start)
	bal := e.ethBalance(tk.Address())
	check(t, new(big.Int).Add(bal, fees).Cmp(balStart) == 0, "taker has exactly its ETH back minus gas (%s + fees %s = %s)", bal, fees, balStart)
	maker.Restart() // back online
	e.drive("maker recovered XMR", 60, func() bool { return maker.phase(id) == swap.PDone }, maker)
	e.mineXMR(2)
	got := maker.xmrBalance()
	sf := sweepFees(maker, id)
	check(t, got == xmrAfterLock+2_000_000_000_000-sf, "maker recovered exactly 2 XMR minus its sweep fee (balance %d -> %d, fee %d)", xmrAfterLock, got, sf)
	var mr swap.Record
	_ = maker.eng.Store.Get("swap-"+id, &mr)
	check(t, mr.ClaimTxHash == "", "maker never revealed its secret")
}

func nowETH(e *Env) uint64 { h, _ := e.eth.Head(e.ctx); return h.Timestamp }

// Maker never locks XMR (goes offline right after the quote). Taker cancels before t1.
func TestMakerNeverLocksTakerCancels(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	r := lockedRecord(taker, id)
	m := e.policy().Margin
	e.warp(r.Swap.T1 - 2*m - nowETH(e) + 5) // inside the cancel window [t1-2m, t1-m)
	e.drive("taker cancelled", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	h := nowETH(e)
	check(t, h < r.Swap.T1, "refund landed before t1 (now=%d t1=%d)", h, r.Swap.T1)
	maker.Restart()
	e.drive("maker aborts", 10, func() bool { return maker.phase(id) == swap.MAborted }, maker)
	check(t, lockedRecord(maker, id).XmrLock == nil, "maker never locked XMR")
}

// Taker goes offline after the maker locked XMR (never marks ready). Maker claims
// after t1; taker comes back later and still gets the XMR.
func TestTakerOfflineMakerClaimsAfterT1(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	e.drive("maker locked XMR", 40, func() bool { return maker.phase(id) == swap.MXmrLocked }, maker)
	r := lockedRecord(maker, id)
	e.rounds(5, maker)
	check(t, maker.phase(id) == swap.MXmrLocked, "maker cannot claim before t1 without ready")
	e.warp(r.Swap.T1 - nowETH(e) + 1)
	e.drive("maker claimed", 20, func() bool { return maker.phase(id) == swap.MClaimed }, maker)
	taker.Restart()
	before := taker.xmrBalance()
	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
	e.mineXMR(2)
	got := taker.xmrBalance() - before
	check(t, got == 2_000_000_000_000-sweepFees(taker, id), "taker came back and received exactly 2 XMR minus its sweep fee (%d)", got)
}

// Maker comes back late but still before t2 (even inside t2 - margin): with the
// two-step claim this is fine. Its claim intent (no secret) freezes the taker,
// whose refund is now blocked even after t2, then it reveals and claims.
func TestMakerReturnsLateBeforeT2ClaimsViaIntent(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("maker locked XMR", 40, func() bool { return maker.phase(id) == swap.MXmrLocked }, maker, taker)
	e.drive("taker ready", 20, func() bool { return taker.phase(id) == swap.TReady }, taker)
	r := lockedRecord(maker, id)
	e.warp(r.Swap.T2 - e.policy().Margin - nowETH(e) + 1) // maker was offline until now
	e.drive("maker sent its claim intent", 5, func() bool {
		st, _ := e.k.Status(e.ctx, r.SwapID)
		return st == eth.StatusClaiming
	}, maker)
	e.warp(e.policy().Margin + 5) // past t2: taker is online but frozen
	e.rounds(2, taker)
	st, _ := e.k.Status(e.ctx, r.SwapID)
	check(t, st == eth.StatusClaiming && taker.phase(id) == swap.TReady, "after t2 the taker cannot refund during the intent freeze")
	e.drive("maker claimed", 20, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)
	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
}

// Maker comes back only after t2: it must NOT reveal its secret (it can no longer
// send a claim intent), waits for the taker's refund and recovers its XMR.
func TestMakerTooLateNeverRevealsSecret(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("maker locked XMR", 40, func() bool { return maker.phase(id) == swap.MXmrLocked }, maker, taker)
	r := lockedRecord(maker, id)
	e.warp(r.Swap.T2 - nowETH(e) + 1)
	e.drive("maker gives up claiming", 5, func() bool { return maker.phase(id) == swap.MWaitRefund }, maker)
	check(t, lockedRecord(maker, id).ClaimTxHash == "" && lockedRecord(maker, id).RelayTries == 0 && lockedRecord(maker, id).IntentTries == 0, "no claim or intent was ever sent")
	e.drive("refund + recovery", 80, func() bool { return maker.phase(id) == swap.PDone && taker.phase(id) == swap.TRefunded }, maker, taker)
}

// Relayer refuses (fee too high for it); maker falls back to its own gas key.
func TestRelayerRefusesFallbackToGasKey(t *testing.T) {
	e := NewEnv(t)
	e.relaySrv.MinFeeMultiplier = 1e9 // relayer wants an absurd fee
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	gk, _ := eth.KeyFromHex(deployerPK)
	maker := e.NewParty(swap.Maker, gk)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("maker claimed via own gas key", 80, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)
	bal, _ := e.eth.Balance(e.ctx, maker.payout)
	check(t, bal.Cmp(oneEth) == 0, "payout got the full 1 ETH (no relayer fee) %s", bal)
	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
}

// A lock that doesn't match the agreed terms is rejected and no XMR is locked.
func TestMakerRejectsUnderpaidLock(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	rec := take(t, e, taker, maker, oneEth)
	// the taker cheats: locks 0.5 ETH instead of 1 ETH, outside its engine
	var tr swap.Record
	_ = taker.eng.Store.Get("swap-"+rec.ID, &tr)
	theirPt := mustPoint(t, tr.TheirSpend)
	myPt := mustScalarPub(t, tr.MySpend)
	half := big.NewInt(5e17)
	data := eth.EncodeLock(tr.Claimer, theirPt, myPt, tr.D1, tr.D2, tr.Grace, eth.Address{}, half, big.NewInt(0), tr.Salt)
	rc, err := e.eth.SendAndWait(e.ctx, tk, &e.k.Addr, half, data)
	if err != nil {
		t.Fatal(err)
	}
	_ = maker.eng.Update(rec.ID, func(r *swap.Record) error { r.LockHint = eth.Hash(rc.Logs[0].TxHash); return nil })
	e.drive("maker rejects", 10, func() bool { return maker.phase(rec.ID) == swap.MAborted }, maker)
	check(t, lockedRecord(maker, rec.ID).XmrLock == nil, "maker locked no XMR for an underpaid lock")
}

// ERC-20 swap with a USDT-style token (transfer/transferFrom/approve declared with
// no return value, contracts/test/Tokens.sol UsdtLike); relayer paid in the token.
func TestERC20SwapRelayedTokenFee(t *testing.T) {
	e := NewEnv(t)
	dk, _ := eth.KeyFromHex(deployerPK)
	tk, _ := eth.KeyFromHex(takerPK)
	e.token = deploy(t, e.ctx, e.eth, dk, bytecode(t, "Tokens.sol", "UsdtLike"))
	sel := func(sig string) []byte { return eth.Keccak([]byte(sig))[:4] }
	word := func(b []byte) []byte { w := make([]byte, 32); copy(w[32-len(b):], b); return w }
	tok := e.token
	ta := tk.Address()
	mint := append(append(sel("mint(address,uint256)"), word(ta[:])...), word(oneEth.Bytes())...)
	if _, err := e.eth.SendAndWait(e.ctx, dk, &tok, nil, mint); err != nil {
		t.Fatal(err)
	}
	e.relaySrv.TokenMinFee[tok] = big.NewInt(1e15)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	maker.eng.Policy.TokenRelayFee = map[eth.Address]*big.Int{tok: big.NewInt(1e15)}
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("maker claimed", 80, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)
	got, _ := eth.TokenBalance(e.ctx, e.eth, tok, maker.payout)
	check(t, got.Cmp(new(big.Int).Sub(oneEth, big.NewInt(1e15))) == 0, "maker received %s token units (minus relayer token fee)", got)
	rk, _ := eth.KeyFromHex(relayerPK)
	rf, _ := eth.TokenBalance(e.ctx, e.eth, tok, rk.Address())
	check(t, rf.Cmp(big.NewInt(1e15)) == 0, "relayer was paid its fee in the token")
	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
}

// Crash right after persisting each irreversible intent (before broadcasting it).
// On restart the very same transactions are broadcast: exactly one ETH lock and
// exactly one XMR lock happen.
func TestCrashAfterIntentsNoDoubleLock(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	crashed := map[string]bool{}
	hook := func(kind string) bool {
		if crashed[kind] {
			return false
		}
		crashed[kind] = true
		return true
	}
	taker.eng.CrashAfterIntent = hook
	taker.eng.StepAll(e.ctx) // persists the lock intent, "dies" before broadcasting
	check(t, taker.phase(id) == swap.TLocking, "lock intent persisted (phase %s)", taker.phase(id))
	taker.Restart()
	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	h, _ := e.eth.Head(e.ctx)
	evs, _ := e.k.LockedEvents(e.ctx, 0, h.Number)
	check(t, len(evs) == 1, "exactly one ETH lock on-chain (%d)", len(evs))

	before := maker.xmrBalance()
	maker.eng.CrashAfterIntent = hook
	e.drive("maker xmr-lock intent", 20, func() bool { return crashed["xmr-lock"] }, maker)
	maker.Restart()
	e.drive("maker claimed", 80, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)
	spent := before - maker.xmrBalance()
	fee := lockedRecord(maker, id).XmrLock.Fee
	check(t, spent == 2_000_000_000_000+fee, "maker spent exactly one lock: %d = 2 XMR + fee %d", spent, fee)
	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
}

// Expired-intent refund, then sweep, with the reveal guard. The maker sends its
// claim intent, then persists the reveal (with its own gas key) and crashes BEFORE
// broadcasting it. It comes back only when less than the safety margin is left
// before the intent's deadline: it must drop the reveal without ever publishing
// it (the secret stays private). After the deadline the taker refunds, the maker
// rebuilds the full Monero key from the refund secret and sweeps. Exact amounts.
func TestRevealPastSafePointDroppedThenRefundSweep(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	gk, _ := eth.KeyFromHex(deployerPK)
	maker := e.NewParty(swap.Maker, gk)
	maker.relayers = nil // self-relay with the gas key
	maker.Restart()
	start, balStart := headNumber(e), e.ethBalance(tk.Address())
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("maker locked XMR", 40, func() bool { return maker.phase(id) == swap.MXmrLocked }, maker, taker)
	xmrAfterLock := maker.xmrBalance()
	e.drive("taker ready", 20, func() bool { return taker.phase(id) == swap.TReady }, taker)

	crashed := false
	maker.eng.CrashAfterIntent = func(kind string) bool {
		if kind == "claim" && !crashed {
			crashed = true
			return true
		}
		return false
	}
	e.drive("reveal persisted", 10, func() bool { return crashed }, maker)
	mr := lockedRecord(maker, id)
	check(t, mr.Pending != nil && mr.Pending.Reveal && !mr.Pending.Broadcast, "reveal persisted, not broadcast")
	claimHash := mr.Pending.Hash
	known, _ := e.eth.TxKnown(e.ctx, claimHash)
	check(t, !known, "node has never seen the reveal")

	r := lockedRecord(taker, id)
	stt, _ := e.k.State(e.ctx, r.SwapID)
	check(t, stt.Stage == eth.StatusClaiming, "claim intent on-chain; deadline %d", stt.Deadline)
	e.warp(stt.Deadline - e.policy().Margin - nowETH(e) + 1) // inside [deadline-margin, deadline)
	maker.Restart()
	e.drive("maker drops reveal, waits for refund", 10, func() bool { return maker.phase(id) == swap.MWaitRefund }, maker)
	known, _ = e.eth.TxKnown(e.ctx, claimHash)
	check(t, !known && lockedRecord(maker, id).Pending == nil, "reveal dropped without ever being broadcast")
	e.rounds(2, taker)
	st, _ := e.k.Status(e.ctx, r.SwapID)
	check(t, st == eth.StatusClaiming, "taker still frozen before the deadline; no secret on-chain")

	e.warp(e.policy().Margin + 5) // past the intent deadline
	e.drive("refund + maker sweep", 80, func() bool { return taker.phase(id) == swap.TRefunded && maker.phase(id) == swap.PDone }, maker, taker)
	e.mineXMR(2)
	got, sf := maker.xmrBalance(), sweepFees(maker, id)
	check(t, got == xmrAfterLock+2_000_000_000_000-sf, "maker swept exactly 2 XMR minus its sweep fee (%d -> %d, fee %d)", xmrAfterLock, got, sf)
	fees := e.feesPaid(tk.Address(), start)
	check(t, new(big.Int).Add(e.ethBalance(tk.Address()), fees).Cmp(balStart) == 0, "taker has exactly its ETH back minus gas")
	check(t, e.ethBalance(e.k.Addr).Sign() == 0, "contract holds nothing")
}

// The taker sends a cancel intent, persists its refund and crashes; it only
// returns when less than the margin is left before the cancel deadline. It must
// not publish the refund (after the deadline the maker may claim, and would learn
// the taker's key share). After t2 it refunds normally: exactly its ETH back.
func TestCancelRefundPastSafePointDropped(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	start, balStart := headNumber(e), e.ethBalance(tk.Address())
	id := take(t, e, taker, maker, oneEth).ID // maker goes offline: never locks XMR
	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	r := lockedRecord(taker, id)
	m := e.policy().Margin
	e.warp(r.Swap.T1 - 2*m - nowETH(e) + 5) // cancel window [t1-2m, t1)
	crashed := false
	taker.eng.CrashAfterIntent = func(kind string) bool {
		if kind == "refund" && !crashed {
			crashed = true
			return true
		}
		return false
	}
	e.drive("refund persisted", 10, func() bool { return crashed }, taker)
	tr := lockedRecord(taker, id)
	check(t, tr.Pending != nil && tr.Pending.Reveal && !tr.Pending.Broadcast, "refund persisted, not broadcast")
	h := tr.Pending.Hash
	stt, _ := e.k.State(e.ctx, r.SwapID)
	check(t, stt.Stage == eth.StatusCancelling, "cancel intent on-chain; deadline %d", stt.Deadline)
	e.warp(stt.Deadline - m - nowETH(e) + 1) // now inside [deadline-m, deadline)
	taker.Restart()
	e.rounds(2, taker)
	known, _ := e.eth.TxKnown(e.ctx, h)
	st, _ := e.k.Status(e.ctx, r.SwapID)
	check(t, !known && st == eth.StatusCancelling, "refund dropped unpublished; swap still CANCELLING")
	e.warp(r.Swap.T2 - nowETH(e) + 1)
	e.drive("late refund", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	fees := e.feesPaid(tk.Address(), start)
	check(t, new(big.Int).Add(e.ethBalance(tk.Address()), fees).Cmp(balStart) == 0, "taker has exactly its ETH back minus gas")
}
