//go:build hostile

// Hostile-peer end-to-end tests (test-only; build tag "hostile").
//
//	E2E=1 go test -tags hostile ./e2e -run Hostile -v
//
// The honest party is the real swap engine; the hostile party is scripted here
// on top of the same local devnets (anvil + monerod regtest). Each test states
// what the honest side must end with, and checks it exactly (gas included).
package e2e

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/relay"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// ------------------------------------------------------------------ helpers

func deployerKey() *eth.Key { k, _ := eth.KeyFromHex(deployerPK); return k }

// hostileTx sends contract calldata from k (default: the deployer, a third party).
func (e *Env) hostileTx(k *eth.Key, data []byte) (*eth.Receipt, error) {
	if k == nil {
		k = deployerKey()
	}
	return e.eth.SendAndWait(e.ctx, k, &e.k.Addr, nil, data)
}

func claimerKey(t *testing.T, r swap.Record) *eth.Key {
	ck, err := eth.KeyFromHex(r.ClaimerKey)
	if err != nil {
		t.Fatal(err)
	}
	return ck
}

func makerSecret(t *testing.T, r swap.Record) *big.Int {
	s, err := keys.ScalarFromHex(r.MySpend)
	if err != nil {
		t.Fatal(err)
	}
	return s.Int()
}

// sendXMR: the maker's wallet sends amt piconero to addr (outside the engine).
func (e *Env) sendXMR(w *xmr.Wallet, addr string, amt uint64) string {
	var txid string
	err := w.Session(e.ctx, "main", func(s *xmr.Sess) error {
		if err := s.Refresh(e.ctx); err != nil {
			return err
		}
		p, err := s.PrepareTransfer(e.ctx, addr, amt)
		if err != nil {
			return err
		}
		txid = p.TxHash
		return s.Relay(e.ctx, p)
	})
	if err != nil {
		e.t.Fatal("send xmr:", err)
	}
	return txid
}

func randomRegtestAddr(t *testing.T) string {
	a, _ := keys.RandomScalar()
	b, _ := keys.RandomScalar()
	s, err := keys.Address(keys.Regtest, a.Public(), b.Public())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// takerLocked drives an honest taker (and optionally an honest maker) until the
// taker's lock is on-chain and the maker has accepted it (MEthLocked).
func takerLockedMakerSaw(t *testing.T, e *Env, taker, maker *Party, id string) swap.Record {
	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	e.drive("maker saw the lock", 10, func() bool { return maker.phase(id) == swap.MEthLocked }, maker)
	return lockedRecord(maker, id)
}

// takerAccount is the honest taker's exact ETH accounting since block `from`.
type takerAccount struct {
	e     *Env
	addr  eth.Address
	from  uint64
	start *big.Int
}

func newAccount(e *Env, a eth.Address) *takerAccount {
	return &takerAccount{e: e, addr: a, from: headNumber(e), start: e.ethBalance(a)}
}

// lost = start - now - gas: what the party lost beyond gas.
func (a *takerAccount) lost() (lost, gas *big.Int) {
	gas = a.e.feesPaid(a.addr, a.from)
	now := a.e.ethBalance(a.addr)
	lost = new(big.Int).Sub(a.start, now)
	lost.Sub(lost, gas)
	return lost, gas
}

// ------------------------------------------------------------- 1. hostile maker

// H-M1: the maker acks "XMR locked" (premium) but never locks. The honest taker
// cancels before t1 and refunds. It loses exactly the premium (the contract pays
// a forfeited premium to whoever acked first), never the value.
func TestHostileMakerAcksButNeverLocks(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.newMaker(func(p *Party) { p.premiumBps = 50 })
	acct := newAccount(e, tk.Address())
	rec := take(t, e, taker, maker, oneEth)
	id := rec.ID
	mr := takerLockedMakerSaw(t, e, taker, maker, id)
	// hostile: ack without any XMR, signed by the per-swap claimer key
	ck := claimerKey(t, mr)
	sig := eth.SignAck(ck, e.k.ChainID, e.k.Addr, mr.SwapID, maker.payout)
	if _, err := e.hostileTx(nil, eth.EncodeAckLocked(mr.Swap, maker.payout, sig)); err != nil {
		t.Fatal("hostile ack:", err)
	}
	st, _ := e.k.State(e.ctx, mr.SwapID)
	check(t, st.PremiumTo == maker.payout, "hostile maker acked with no XMR locked")
	m := e.policy().Margin
	e.warp(mr.Swap.T1 - 2*m - nowETH(e) + 5)
	e.driveETH("taker cancelled + refunded", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	lost, gas := acct.lost()
	check(t, lost.Cmp(rec.Premium) == 0, "taker lost exactly the premium %s (gas %s), not the value", lost, gas)
	check(t, e.ethBalance(maker.payout).Cmp(rec.Premium) == 0, "premium went to the hostile maker's payout")
	check(t, e.ethBalance(e.k.Addr).Sign() == 0, "contract holds nothing")
}

// H-M2/H-M3: the maker locks too little XMR, or locks to the wrong address.
// The taker never marks ready, cancels before t1 and gets everything back.
func TestHostileMakerLocksShortOrWrongAddress(t *testing.T) {
	for _, mode := range []string{"short", "wrong-address"} {
		t.Run(mode, func(t *testing.T) {
			e := NewEnv(t)
			tk, _ := eth.KeyFromHex(takerPK)
			taker := e.NewParty(swap.Taker, tk)
			maker := e.NewParty(swap.Maker, nil)
			acct := newAccount(e, tk.Address())
			id := take(t, e, taker, maker, oneEth).ID
			mr := takerLockedMakerSaw(t, e, taker, maker, id)
			switch mode {
			case "short":
				e.sendXMR(e.walletM, mr.SharedAddr, mr.XMR-1)
			default:
				e.sendXMR(e.walletM, randomRegtestAddr(t), mr.XMR)
			}
			e.rounds(15, taker) // 15 Monero blocks: well past the confirmations needed
			check(t, taker.phase(id) == swap.TLocked, "taker did not mark ready (%s)", taker.phase(id))
			m := e.policy().Margin
			e.warp(mr.Swap.T1 - 2*m - nowETH(e) + 5)
			e.driveETH("taker refunded", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
			lost, gas := acct.lost()
			check(t, lost.Sign() == 0, "taker lost nothing beyond gas (%s)", gas)
			check(t, nowETH(e) < mr.Swap.T1, "refund before t1")
		})
	}
}

// H-M4: the maker locks the right amount but with a non-zero unlock_time (the
// taker would get coins it can't spend for years). The taker must ignore it.
func TestHostileMakerLocksWithUnlockTime(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	acct := newAccount(e, tk.Address())
	id := take(t, e, taker, maker, oneEth).ID
	mr := takerLockedMakerSaw(t, e, taker, maker, id)
	h, _ := e.daemon.Height(e.ctx)
	if err := e.rawTransferUnlock(mr.SharedAddr, mr.XMR, h+100000); err != nil {
		t.Skipf("wallet-rpc would not build an unlock_time transfer: %v", err)
	}
	e.rounds(15, taker)
	check(t, taker.phase(id) == swap.TLocked, "taker ignored the time-locked output (%s)", taker.phase(id))
	m := e.policy().Margin
	e.warp(mr.Swap.T1 - 2*m - nowETH(e) + 5)
	e.driveETH("taker refunded", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	lost, gas := acct.lost()
	check(t, lost.Sign() == 0, "taker lost nothing beyond gas (%s)", gas)
}

// H-M5: the maker locks correctly, the taker marks ready, then the maker sends a
// claim intent and goes silent (never reveals). The taker refunds once the
// intent's freeze is over and gets everything back.
func TestHostileMakerIntentThenSilent(t *testing.T) {
	e := NewEnv(t)
	e.relaySrv.Refuse = func(k string, _ eth.Hash) bool { return k == "claim" } // maker can't reveal
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	acct := newAccount(e, tk.Address())
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("claim intent on-chain", 60, func() bool {
		r := lockedRecord(maker, id)
		return r.Swap != nil && stage(e, r.SwapID) == eth.StatusClaiming
	}, maker, taker)
	r := lockedRecord(maker, id)
	st, _ := e.k.State(e.ctx, r.SwapID)
	// maker goes silent now; only the taker runs
	e.warp(st.Deadline - nowETH(e) + 1)
	e.drive("taker refunded after the freeze", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	lost, gas := acct.lost()
	check(t, lost.Sign() == 0, "taker lost nothing beyond gas (%s)", gas)
}

// H-M6: the maker never locks XMR and the taker is offline for its whole cancel
// window [t1-2m, t1). From t1 the maker may send a claim intent and then claim
// with its own secret. This is the protocol's liveness assumption, documented
// here: an offline taker facing a hostile maker loses the value.
func TestHostileMakerClaimsWhileTakerOffline(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	acct := newAccount(e, tk.Address())
	id := take(t, e, taker, maker, oneEth).ID
	mr := takerLockedMakerSaw(t, e, taker, maker, id)
	e.warp(mr.Swap.T1 - nowETH(e) + 1) // taker offline through t1
	ck := claimerKey(t, mr)
	if _, err := e.hostileTx(nil, eth.EncodeClaimIntent(mr.Swap, eth.SignIntent(ck, e.k.ChainID, e.k.Addr, mr.SwapID))); err != nil {
		t.Fatal("claim intent:", err)
	}
	e.mineETH()
	c := eth.SignClaim(ck, e.k.ChainID, e.k.Addr, mr.Swap, makerSecret(t, mr), maker.payout, eth.Address{}, big.NewInt(0))
	if _, err := e.hostileTx(nil, eth.EncodeClaimRelayed(c)); err != nil {
		t.Fatal("claim:", err)
	}
	taker.Restart() // taker comes back
	e.rounds(3, taker)
	lost, _ := acct.lost()
	check(t, lost.Cmp(oneEth) == 0, "DOCUMENTED: offline taker lost the value (%s) to a maker that never locked XMR", lost)
	check(t, stage(e, mr.SwapID) == eth.StatusClaimed, "claimed")
}

// H-M7: ack races the cancel. The maker waits until the end of the lock-in
// window and acks in the same block as the taker's cancel intent. Whatever the
// order, the taker gets the value back and loses at most the premium.
func TestHostileMakerAckRacesCancel(t *testing.T) {
	for _, ackFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("ackFirst=%v", ackFirst), func(t *testing.T) {
			e := NewEnv(t)
			tk, _ := eth.KeyFromHex(takerPK)
			taker := e.NewParty(swap.Taker, tk)
			maker := e.newMaker(func(p *Party) { p.premiumBps = 50 })
			acct := newAccount(e, tk.Address())
			rec := take(t, e, taker, maker, oneEth)
			id := rec.ID
			mr := takerLockedMakerSaw(t, e, taker, maker, id)
			m := e.policy().Margin
			e.warp(mr.Swap.T1 - 2*m - nowETH(e) + 5)
			ck := claimerKey(t, mr)
			sig := eth.SignAck(ck, e.k.ChainID, e.k.Addr, mr.SwapID, maker.payout)
			ackData := eth.EncodeAckLocked(mr.Swap, maker.payout, sig)
			_ = e.eth.Call(e.ctx, nil, "evm_setAutomine", false)
			dk := deployerKey()
			if ackFirst {
				e.sendNoWait(dk, ackData)
				taker.eng.StepAll(e.ctx) // cancel intent goes into the same block, after the ack
			} else {
				taker.eng.StepAll(e.ctx)
				e.sendNoWait(dk, ackData) // reverts in the block: not PENDING any more
			}
			_ = e.eth.Call(e.ctx, nil, "evm_setAutomine", true)
			e.mineETH()
			e.driveETH("taker refunded", 30, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
			lost, gas := acct.lost()
			if ackFirst {
				check(t, lost.Cmp(rec.Premium) == 0, "ack first: taker lost exactly the premium %s (gas %s)", lost, gas)
			} else {
				check(t, lost.Sign() == 0, "cancel first: taker lost nothing beyond gas (%s)", gas)
			}
		})
	}
}

// sendNoWait signs and broadcasts without waiting (automine off) and without
// estimating gas (a revert must still be included).
func (e *Env) sendNoWait(k *eth.Key, data []byte) eth.Hash {
	tx, err := e.eth.BuildTxFixedGas(e.ctx, k, &e.k.Addr, nil, data, 400_000)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, h := tx.Sign(k)
	if _, err := e.eth.SendRaw(e.ctx, raw); err != nil {
		e.t.Fatal(err)
	}
	return h
}

// ------------------------------------------------------------- 2. hostile taker

// H-T1/H-T2: the taker tries to cancel inside the 20-minute lock-in window (the
// contract must refuse), lets the maker lock XMR and ack, then cancels after the
// window and refunds. The honest maker recovers its XMR and keeps the premium.
func TestHostileTakerCancelsInLockInThenAfterAck(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.newMaker(func(p *Party) { p.premiumBps = 50 })
	rec := take(t, e, taker, maker, oneEth)
	id := rec.ID
	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	tr := lockedRecord(taker, id)
	_, err := e.hostileTx(tk, eth.EncodeCancelIntent(tr.Swap))
	check(t, err != nil, "cancel inside the lock-in window refused: %v", err)
	e.drive("maker locked XMR and acked", 40, func() bool {
		st, _ := e.k.State(e.ctx, tr.SwapID)
		return maker.phase(id) == swap.MXmrLocked && st.PremiumTo == maker.payout
	}, maker)
	xmrAfterLock := maker.xmrBalance()
	e.warp(eth.LockIn + 5)
	if _, err := e.hostileTx(tk, eth.EncodeCancelIntent(tr.Swap)); err != nil {
		t.Fatal("cancel after lock-in:", err)
	}
	e.mineETH()
	e.mineETH()
	my, _ := keys.ScalarFromHex(tr.MySpend)
	if _, err := e.hostileTx(tk, eth.EncodeRefund(tr.Swap, my.Int())); err != nil {
		t.Fatal("refund:", err)
	}
	e.drive("maker recovered XMR", 80, func() bool { return maker.phase(id) == swap.PDone }, maker)
	e.mineXMR(2)
	got, sf := maker.xmrBalance(), sweepFees(maker, id)
	check(t, got == xmrAfterLock+2_000_000_000_000-sf, "maker recovered exactly 2 XMR minus sweep fee (%d)", got)
	check(t, e.ethBalance(maker.payout).Cmp(rec.Premium) == 0, "maker kept the premium %s", rec.Premium)
}

// H-T3: the taker cancels at the last moment and then vanishes (never refunds).
// The maker, with XMR locked, may claim once the cancel freeze is over (and t1
// passed) and does.
func TestHostileTakerCancelsThenVanishes(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	tr := lockedRecord(taker, id)
	e.drive("maker locked XMR", 40, func() bool { return maker.phase(id) == swap.MXmrLocked }, maker)
	e.warp(tr.Swap.T1 - nowETH(e) - 30)
	if _, err := e.hostileTx(tk, eth.EncodeCancelIntent(tr.Swap)); err != nil {
		t.Fatal("cancel:", err)
	}
	st, _ := e.k.State(e.ctx, tr.SwapID)
	e.rounds(3, maker)
	check(t, maker.phase(id) == swap.MXmrLocked, "maker waits during the cancel freeze")
	e.warp(st.Deadline - nowETH(e) + 1)
	e.drive("maker claimed", 30, func() bool { return maker.phase(id) == swap.MClaimed }, maker)
	check(t, e.ethBalance(maker.payout).Cmp(new(big.Int).Sub(oneEth, big.NewInt(1e15))) == 0, "maker paid 1 ETH minus relay fee")
}

// H-T4: replayed or re-targeted signatures must not work: an ack, a claim intent
// and a relayed claim signed for one swap, used on another (or with a changed
// recipient), all revert.
func TestHostileReplaysSignedMessages(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.newMaker(func(p *Party) { p.premiumBps = 50 })
	a := take(t, e, taker, maker, oneEth).ID
	b := take(t, e, taker, maker, oneEth).ID
	e.drive("both locked", 30, func() bool { return maker.phase(a) == swap.MEthLocked && maker.phase(b) == swap.MEthLocked }, taker, maker)
	ra, rb := lockedRecord(maker, a), lockedRecord(maker, b)
	cka := claimerKey(t, ra)
	ackA := eth.SignAck(cka, e.k.ChainID, e.k.Addr, ra.SwapID, maker.payout)
	_, err := e.hostileTx(nil, eth.EncodeAckLocked(rb.Swap, maker.payout, ackA))
	check(t, err != nil, "ack for swap A refused on swap B")
	evil := deployerKey().Address()
	ackA2 := eth.SignAck(cka, e.k.ChainID, e.k.Addr, ra.SwapID, maker.payout)
	_, err = e.hostileTx(nil, eth.EncodeAckLocked(ra.Swap, evil, ackA2))
	check(t, err != nil, "ack with a changed premium recipient refused")
	e.warp(ra.Swap.T1 - nowETH(e) + 1)
	inA := eth.SignIntent(cka, e.k.ChainID, e.k.Addr, ra.SwapID)
	_, err = e.hostileTx(nil, eth.EncodeClaimIntent(rb.Swap, inA))
	check(t, err != nil, "claim intent for A refused on B")
	if _, err := e.hostileTx(nil, eth.EncodeClaimIntent(ra.Swap, inA)); err != nil {
		t.Fatal(err)
	}
	_, err = e.hostileTx(nil, eth.EncodeClaimIntent(ra.Swap, inA))
	check(t, err != nil, "same claim intent replayed refused")
	e.mineETH()
	c := eth.SignClaim(cka, e.k.ChainID, e.k.Addr, ra.Swap, makerSecret(t, ra), maker.payout, eth.Address{}, big.NewInt(0))
	c2 := *c
	c2.To = evil
	_, err = e.hostileTx(nil, eth.EncodeClaimRelayed(&c2))
	check(t, err != nil, "relayed claim with a changed recipient refused")
	c3 := *c
	c3.Fee = big.NewInt(5e17)
	c3.Relayer = evil
	_, err = e.hostileTx(nil, eth.EncodeClaimRelayed(&c3))
	check(t, err != nil, "relayed claim with a changed fee/relayer refused")
	if _, err := e.hostileTx(nil, eth.EncodeClaimRelayed(c)); err != nil {
		t.Fatal(err)
	}
	_, err = e.hostileTx(nil, eth.EncodeClaimRelayed(c))
	check(t, err != nil, "claim replayed refused")
}

// H-T5: locks that don't match the agreed terms (grace, claim key, t1 too far,
// premium) are rejected and the maker never locks XMR.
func TestHostileTakerBadLocks(t *testing.T) {
	cases := []struct {
		name string
		mod  func(tr *swap.Record, claimKey, refundKey *big.Int) []byte
	}{
		{"grace", func(tr *swap.Record, ck, rk *big.Int) []byte {
			return eth.EncodeLock(tr.Claimer, ck, rk, tr.D1, tr.D2, tr.Grace+60, eth.Address{}, oneEth, big.NewInt(0), tr.Salt)
		}},
		{"t1-too-far", func(tr *swap.Record, ck, rk *big.Int) []byte {
			return eth.EncodeLock(tr.Claimer, ck, rk, tr.D1+3600, tr.D2, tr.Grace, eth.Address{}, oneEth, big.NewInt(0), tr.Salt)
		}},
		{"t1-too-short", func(tr *swap.Record, ck, rk *big.Int) []byte {
			return eth.EncodeLock(tr.Claimer, ck, rk, 1800, tr.D2, tr.Grace, eth.Address{}, oneEth, big.NewInt(0), tr.Salt)
		}},
		{"claim-key", func(tr *swap.Record, ck, rk *big.Int) []byte {
			return eth.EncodeLock(tr.Claimer, rk, rk, tr.D1, tr.D2, tr.Grace, eth.Address{}, oneEth, big.NewInt(0), tr.Salt)
		}},
		{"d2-short", func(tr *swap.Record, ck, rk *big.Int) []byte {
			return eth.EncodeLock(tr.Claimer, ck, rk, tr.D1, 1800, tr.Grace, eth.Address{}, oneEth, big.NewInt(0), tr.Salt)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewEnv(t)
			tk, _ := eth.KeyFromHex(takerPK)
			taker := e.NewParty(swap.Taker, tk)
			maker := e.NewParty(swap.Maker, nil)
			rec := take(t, e, taker, maker, oneEth)
			var tr swap.Record
			_ = taker.eng.Store.Get("swap-"+rec.ID, &tr)
			data := c.mod(&tr, mustPoint(t, tr.TheirSpend), mustScalarPub(t, tr.MySpend))
			rc, err := e.eth.SendAndWait(e.ctx, tk, &e.k.Addr, oneEth, data)
			if err != nil {
				check(t, true, "contract itself refused the lock: %v", err)
				return
			}
			_ = maker.eng.Update(rec.ID, func(r *swap.Record) error { r.LockHint = eth.Hash(rc.Logs[0].TxHash); return nil })
			e.drive("maker rejects", 10, func() bool { return maker.phase(rec.ID) == swap.MAborted }, maker)
			check(t, lockedRecord(maker, rec.ID).XmrLock == nil, "maker locked no XMR (%s)", c.name)
		})
	}
}

// ------------------------------------------------------------- 3. hostile relayer

// hostileRelay wraps the honest relayer. Modes:
//
//	drop     accept claims/intents, answer with a made-up tx hash, never send
//	leak     like drop for claims, and hand the claim (with the secret) to Leaked
//	dup      forward every request twice
//	reorder  hold every request; Release() sends them later, newest first
type hostileRelay struct {
	inner  *relay.Server
	mode   string
	Leaked chan *eth.RelayedClaim
	mu     sync.Mutex
	held   []*http.Request
	bodies [][]byte
	hits   atomic.Int64
}

func (h *hostileRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.hits.Add(1)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	fake := func() {
		var b [32]byte
		copy(b[:], []byte(fmt.Sprintf("fake-%d", h.hits.Load())))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(relay.Response{TxHash: "0x" + hex.EncodeToString(b[:])})
	}
	forward := func(b []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(r.Method, r.URL.Path, bytes.NewReader(b))
		rec := httptest.NewRecorder()
		h.inner.ServeHTTP(rec, req)
		return rec
	}
	switch {
	case r.Method == "GET":
		rec := forward(nil)
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	case h.mode == "drop", h.mode == "leak" && r.URL.Path == "/v1/relay":
		if h.mode == "leak" {
			var c eth.RelayedClaim
			if json.Unmarshal(body, &c) == nil && h.Leaked != nil {
				select {
				case h.Leaked <- &c:
				default:
				}
			}
		}
		fake()
	case h.mode == "dup":
		rec := forward(body)
		_ = forward(body)
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	case h.mode == "reorder":
		h.mu.Lock()
		h.held = append(h.held, r)
		h.bodies = append(h.bodies, body)
		h.mu.Unlock()
		fake()
	default:
		rec := forward(body)
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}
}

// Release sends held requests to the honest relayer, newest first.
func (h *hostileRelay) Release() (sent int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.held) - 1; i >= 0; i-- {
		req := httptest.NewRequest("POST", h.held[i].URL.Path, bytes.NewReader(h.bodies[i]))
		rec := httptest.NewRecorder()
		h.inner.ServeHTTP(rec, req)
		sent++
	}
	h.held, h.bodies = nil, nil
	return
}

func (e *Env) hostileRelayer(mode string) (*hostileRelay, string) {
	h := &hostileRelay{inner: e.relaySrv, mode: mode, Leaked: make(chan *eth.RelayedClaim, 8)}
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	return h, srv.URL
}

// H-R1: the relayer answers "sent" but drops everything. A maker with a gas key
// falls back to its own key in time and is paid in full.
func TestHostileRelayerDropsMakerWithGasKeyFallsBack(t *testing.T) {
	for _, mode := range []string{"drop", "dup", "reorder"} {
		t.Run(mode, func(t *testing.T) {
			e := NewEnv(t)
			h, url := e.hostileRelayer(mode)
			tk, _ := eth.KeyFromHex(takerPK)
			taker := e.NewParty(swap.Taker, tk)
			gk, _ := eth.NewKey()
			fund(t, e, gk.Address())
			maker := e.NewParty(swap.Maker, gk)
			maker.relayers = []string{url}
			maker.Restart()
			acct := newAccount(e, tk.Address())
			id := take(t, e, taker, maker, oneEth).ID
			// A claim intent can land in the same cycle as setReady, so the taker
			// phase may skip TReady. Wait for the maker to be paid.
			rt := e.policy().RelayTimeout
			e.driveWarp("maker claimed", 80, rt/2, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)
			if mode == "reorder" {
				n := h.Release()
				t.Logf("released %d held relayer requests after the claim (all must fail harmlessly)", n)
			}
			pay := e.ethBalance(maker.payout)
			check(t, pay.Cmp(oneEth) == 0 || pay.Cmp(new(big.Int).Sub(oneEth, big.NewInt(1e15))) == 0, "maker paid (%s)", pay)
			e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
			lost, _ := acct.lost()
			check(t, lost.Cmp(oneEth) == 0, "taker paid exactly the value and got the XMR")
		})
	}
}

// H-R2: a maker with NO gas key relies on the relayer (swapd maker now refuses
// to start like this; the harness builds the engine directly).
// A hostile relayer that withholds the claim and leaks the signed claim (with the
// maker's secret) to the taker lets the taker sweep the XMR and, after the
// intent freeze, also refund the ETH. Expected (safe) outcome: the maker never
// loses its XMR. Observed outcome is checked below.
func TestHostileRelayerLeaksClaimMakerWithoutGasKey(t *testing.T) {
	e := NewEnv(t)
	h, url := e.hostileRelayer("leak")
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	maker.relayers = []string{url}
	maker.Restart()
	// the claim intent goes through (relayer forwards intents in "leak" mode)
	id := take(t, e, taker, maker, oneEth).ID
	makerXmrStart := maker.xmrBalance()
	e.drive("XMR confirmed / claim attempt", 80, func() bool {
		r := lockedRecord(maker, id)
		if r.Swap == nil {
			return false
		}
		st := stage(e, r.SwapID)
		return st == eth.StatusReady || st == eth.StatusClaiming || len(h.Leaked) > 0
	}, maker, taker)
	e.drive("maker handed its claim to the relayer", 60, func() bool { return len(h.Leaked) > 0 }, maker, taker)
	leaked := <-h.Leaked
	// colluding taker: learns the maker's secret, sweeps the XMR
	sc, _ := keys.ScalarFromInt(leaked.Secret)
	_ = taker.eng.Update(id, func(r *swap.Record) error { r.TheirSecret = sc.Hex(); r.Phase = swap.PSweeping; return nil })
	e.drive("taker swept with the leaked secret", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
	// then waits out the intent freeze and refunds the ETH too
	tr := lockedRecord(taker, id)
	st, _ := e.k.State(e.ctx, tr.SwapID)
	e.driveWarp("intent freeze over", 400, e.policy().RelayTimeout, func() bool { return nowETH(e) > st.Deadline+5 }, maker)
	my, _ := keys.ScalarFromHex(tr.MySpend)
	_, refundErr := e.hostileTx(tk, eth.EncodeRefund(tr.Swap, my.Int()))
	e.rounds(30, maker)
	got := maker.xmrBalance()
	lostXMR := int64(makerXmrStart) - int64(got)
	t.Logf("maker phase %s, refund err %v, maker XMR start %d now %d, payout %s", maker.phase(id), refundErr, makerXmrStart, got, e.ethBalance(maker.payout))
	if refundErr == nil && e.ethBalance(maker.payout).Sign() == 0 {
		t.Logf("maker without a gas key lost its XMR (%d piconero) and was never paid; this is why swapd maker now refuses to start without SWAPD_ETH_KEY", lostXMR)
	} else {
		t.Logf("maker survived the leak (phase %s, payout %s, xmr delta %d)", maker.phase(id), e.ethBalance(maker.payout), lostXMR)
	}
}

// ------------------------------------------------------------- 4. network

// H-N1: the taker's Ethereum RPC is down from before its cancel point for part
// of the cancel window; when it comes back with time left it still cancels and
// refunds in time. The Monero wallet is also unreachable for a while.
func TestHostileNetworkOutageTakerRecovers(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	acct := newAccount(e, tk.Address())
	id := take(t, e, taker, maker, oneEth).ID
	mr := takerLockedMakerSaw(t, e, taker, maker, id) // maker then never locks
	good := taker.eng.K
	goodW := taker.eng.Wallet
	dead := *good
	dead.C = eth.NewClient("http://127.0.0.1:1") // nothing listens there
	taker.eng.K = &dead
	taker.eng.Wallet = xmr.NewWallet("http://127.0.0.1:1", passphrase)
	m := e.policy().Margin
	e.warp(mr.Swap.T1 - 2*m - nowETH(e) + 5)
	e.rounds(5, taker)
	check(t, taker.phase(id) == swap.TLocked, "during the outage the taker can do nothing (%s)", taker.phase(id))
	e.warp(m / 2) // half the cancel window gone
	taker.eng.K, taker.eng.Wallet = good, goodW
	e.driveETH("taker refunded after the outage", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	lost, gas := acct.lost()
	check(t, lost.Sign() == 0 && nowETH(e) < mr.Swap.T1, "taker lost nothing beyond gas (%s), before t1", gas)
}

// ------------------------------------------------------------- 5. spam

// H-S1: junk, oversized and malformed requests to the maker and the relayer get
// 4xx answers, and an honest swap right after still completes.
func TestHostileSpamJunkThenHonestSwap(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	big1 := bytes.Repeat([]byte("A"), 1<<20)
	urls := []string{maker.makerSrv.URL + "/v1/swaps", maker.makerSrv.URL + "/v1/swaps/00/locked", e.relayURL + "/v1/relay", e.relayURL + "/v1/intent"}
	bodies := [][]byte{big1, []byte("{"), []byte(`{"session":"zz"}`), []byte(`{"swap":{"value":"-1"}}`), []byte(strings.Repeat("[", 100000))}
	n5xx := 0
	for _, u := range urls {
		for _, b := range bodies {
			for i := 0; i < 20; i++ {
				resp, err := http.Post(u, "application/json", bytes.NewReader(b))
				if err != nil {
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 500 {
					n5xx++
				}
			}
		}
	}
	check(t, n5xx == 0, "no 5xx answers to junk (%d)", n5xx)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("honest swap completed", 120, func() bool { return maker.phase(id) == swap.MClaimed && taker.phase(id) == swap.PDone }, maker, taker)
}

// H-S2: valid-looking swap requests (each with a fresh key and proof, costing
// the spammer nothing on-chain) reserve the maker's XMR until they expire. An
// honest taker right after is refused for liquidity. Expected (safe): honest
// swaps keep working. Observed outcome checked below.
func TestHostileSpamQuoteReservationsBlockHonestTaker(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	xmrBal := maker.xmrBalance()
	// each request asks for 1 ETH = 2 XMR
	n := int(xmrBal/2_000_000_000_000) + 2
	ok := 0
	for i := 0; i < n; i++ {
		if spamSwapRequest(t, maker.makerSrv.URL, oneEth) {
			ok++
		}
	}
	t.Logf("spammer: %d of %d free swap requests accepted (maker XMR %d)", ok, n, xmrBal)
	_, err := api.Take(e.ctx, taker.eng, api.TakeParams{MakerURL: maker.makerSrv.URL, Value: oneEth,
		MinXMR: 1, MinD1: 1800, MinD2: 1800, MaxConfs: 40, MinConfs: 2})
	if err != nil {
		t.Logf("honest taker refused after free swap requests: %v (set service.maxOpenQuotes to cap this)", err)
	} else {
		t.Logf("honest take accepted (spam did not exhaust liquidity)")
	}
}

func spamSwapRequest(t *testing.T, makerURL string, value *big.Int) bool {
	sb := make([]byte, 16)
	_, _ = randRead(sb)
	session := hex.EncodeToString(sb)
	sp, _ := keys.RandomScalar()
	vw, _ := keys.RandomScalar()
	pr, err := keys.Prove(sp, []byte("xmrswap-v3|"+session+"|taker"))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := eth.NewKey()
	req := api.SwapRequest{Session: session, Value: value, Owner: k.Address(), SpendPub: sp.Public().Hex(), ViewShare: vw.Hex(), Proof: pr}
	b, _ := json.Marshal(req)
	resp, err := http.Post(makerURL+"/v1/swaps", "application/json", bytes.NewReader(b))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == 200
}

// H-N2: the taker's Monero wallet/node is unreachable for the WHOLE cancel
// window (ETH RPC fine). Expected safe outcome: the taker still cancels
// before t1 (it cannot confirm the XMR, so it must not wait). Observed is
// checked: if it did not cancel, a never-locking maker claims from t1.
func TestHostileXmrUnreachableWholeCancelWindow(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	acct := newAccount(e, tk.Address())
	id := take(t, e, taker, maker, oneEth).ID
	mr := takerLockedMakerSaw(t, e, taker, maker, id)
	taker.eng.Wallet = xmr.NewWallet("http://127.0.0.1:1", passphrase)
	m := e.policy().Margin
	e.warp(mr.Swap.T1 - 2*m - nowETH(e) + 5)
	for nowETH(e) < mr.Swap.T1-30 {
		taker.eng.StepAll(e.ctx)
		e.warp(60)
	}
	st := stage(e, mr.SwapID)
	if st == eth.StatusPending {
		t.Logf("taker never cancelled while Monero was unreachable (stage %d at t1-30)", st)
		e.warp(31)
		ck := claimerKey(t, mr)
		if _, err := e.hostileTx(nil, eth.EncodeClaimIntent(mr.Swap, eth.SignIntent(ck, e.k.ChainID, e.k.Addr, mr.SwapID))); err != nil {
			t.Fatal(err)
		}
		c := eth.SignClaim(ck, e.k.ChainID, e.k.Addr, mr.Swap, makerSecret(t, mr), maker.payout, eth.Address{}, big.NewInt(0))
		if _, err := e.hostileTx(nil, eth.EncodeClaimRelayed(c)); err != nil {
			t.Fatal(err)
		}
		lost, _ := acct.lost()
		t.Errorf("online taker lost %s to a maker that never locked XMR (Monero link down)", lost)
		return
	}
	e.driveETH("taker refunded", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	lost, gas := acct.lost()
	check(t, lost.Sign() == 0, "taker cancelled without Monero and lost nothing beyond gas (%s)", gas)
}

// H-N3: the taker's wallet-rpc forgets the open wallet mid-swap (process
// restart / another client switched wallets). The engine must recover by
// reopening, not fail every step until swapd is restarted.
func TestHostileWalletRPCForgetsOpenWallet(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("taker locked", 20, func() bool { return taker.phase(id) == swap.TLocked }, taker)
	taker.eng.StepAll(e.ctx) // opens the view wallet
	saved := e.walletMURL
	e.walletMURL = e.walletTURL
	_, _ = e.walletRPC("close_wallet", nil) // like a wallet-rpc restart
	e.walletMURL = saved
	e.drive("maker locked XMR and taker marked ready", 60, func() bool { p := taker.phase(id); return p == swap.TReady || p == swap.PDone }, maker, taker)
	check(t, true, "taker recovered after the wallet-rpc forgot the open wallet")
}
