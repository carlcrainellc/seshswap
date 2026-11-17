package e2e

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/price"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

// driveETH is drive without mining Monero blocks (XMR confirmations stand still).
func (e *Env) driveETH(what string, maxRounds int, cond func() bool, parties ...*Party) {
	for i := 0; i < maxRounds && !cond(); i++ {
		for _, p := range parties {
			p.eng.StepAll(e.ctx)
		}
		e.mineETH()
	}
	if !cond() {
		e.t.Fatalf("timeout: %s", what)
	}
}

func stage(e *Env, id eth.Hash) uint8 { s, _ := e.k.Status(e.ctx, id); return s }

// The relayer censors the reveal. The claim intent already froze the taker, so
// even an online taker cannot refund before the intent deadline; once a relayer
// carries the reveal, the maker claims.
func TestRevealCensoredTakerStaysFrozen(t *testing.T) {
	e := NewEnv(t)
	e.relaySrv.Refuse = func(k string, _ eth.Hash) bool { return k == "claim" }
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("claim intent on-chain", 60, func() bool {
		r := lockedRecord(maker, id)
		return r.Swap != nil && stage(e, r.SwapID) == eth.StatusClaiming
	}, maker, taker)
	r := lockedRecord(maker, id)
	st, _ := e.k.State(e.ctx, r.SwapID)
	e.rounds(5, maker, taker)
	check(t, stage(e, r.SwapID) == eth.StatusClaiming, "reveal censored: still CLAIMING, nothing revealed")
	e.warp(st.Deadline - e.policy().Margin - 120 - nowETH(e))
	e.rounds(3, taker)
	check(t, stage(e, r.SwapID) == eth.StatusClaiming && taker.phase(id) != swap.TRefunding && taker.phase(id) != swap.TRefunded,
		"taker cannot refund during the freeze, even past t2 (now %d, t2 %d, deadline %d)", nowETH(e), r.Swap.T2, st.Deadline)
	e.relaySrv.Refuse = nil
	e.drive("maker claimed", 20, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)
	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
}

// The relayer censors the claim intent and the maker has no gas key. Nothing is
// revealed; after t2 the taker refunds and the maker recovers its XMR.
func TestIntentCensoredCleanRefund(t *testing.T) {
	e := NewEnv(t)
	e.relaySrv.Refuse = func(k string, _ eth.Hash) bool { return k == "claimIntent" }
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, nil)
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("taker ready", 60, func() bool { return taker.phase(id) == swap.TReady }, maker, taker)
	xmrAfterLock := maker.xmrBalance()
	e.rounds(3, maker, taker)
	r := lockedRecord(taker, id)
	check(t, stage(e, r.SwapID) == eth.StatusReady, "intent censored: still READY")
	e.warp(r.Swap.T2 - nowETH(e) + 1)
	e.drive("refund + recovery", 80, func() bool { return taker.phase(id) == swap.TRefunded && maker.phase(id) == swap.PDone }, maker, taker)
	check(t, lockedRecord(maker, id).ClaimTxHash == "", "maker never revealed its secret")
	e.mineXMR(2)
	got, sf := maker.xmrBalance(), sweepFees(maker, id)
	check(t, got == xmrAfterLock+2_000_000_000_000-sf, "maker recovered exactly 2 XMR minus sweep fee")
}

// Premium: the maker acks its XMR lock on-chain; the taker then cancels (here:
// it does not see 10 confirmations before its cancel point). The premium goes to
// the maker; the taker gets the rest back; the maker recovers its XMR.
func TestPremiumForfeitedOnCancelAfterAck(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.newMaker(func(p *Party) { p.premiumBps = 50 }) // 0.5%
	start, balStart := headNumber(e), e.ethBalance(tk.Address())
	rec := take(t, e, taker, maker, oneEth)
	id := rec.ID
	premium := big.NewInt(5e15)
	check(t, rec.Premium.Cmp(premium) == 0, "premium agreed: %s wei", rec.Premium)
	e.drive("maker acked its XMR lock", 30, func() bool {
		r := lockedRecord(maker, id)
		if r.Swap == nil {
			return false
		}
		st, _ := e.k.State(e.ctx, r.SwapID)
		return st.PremiumTo == maker.payout
	}, maker, taker)
	r := lockedRecord(taker, id)
	m := e.policy().Margin
	e.warp(r.Swap.T1 - 2*m - nowETH(e) + 5)
	e.driveETH("taker cancelled + refunded", 20, func() bool { return taker.phase(id) == swap.TRefunded }, taker)
	pay := e.ethBalance(maker.payout)
	check(t, pay.Cmp(premium) == 0, "maker payout received the forfeited premium (%s)", pay)
	// Settlement zeroes the status (so PremiumTo reads 0 afterwards); who got the
	// premium must be read from the PremiumPaid event (testnet report bug).
	st, _ := e.k.State(e.ctx, r.SwapID)
	check(t, st.PremiumTo == (eth.Address{}), "status cleared after refund")
	evs, err := e.k.Events(e.ctx, r.SwapID, start, headNumber(e))
	check(t, err == nil, "events: %v", err)
	paid := false
	for _, ev := range evs {
		paid = paid || (ev.Kind == "PremiumPaid" && ev.To == maker.payout && ev.Amount.Cmp(premium) == 0)
	}
	check(t, paid, "PremiumPaid event names the maker's payout and the premium")
	fees := e.feesPaid(tk.Address(), start)
	want := new(big.Int).Sub(balStart, premium)
	check(t, new(big.Int).Add(e.ethBalance(tk.Address()), fees).Cmp(want) == 0, "taker got 1 ETH back, lost exactly the premium (plus gas)")
	e.drive("maker recovered XMR", 80, func() bool { return maker.phase(id) == swap.PDone }, maker)
	check(t, e.ethBalance(e.k.Addr).Sign() == 0, "contract holds nothing")
}

// Premium on the happy path: returned to the taker together with the claim.
func TestPremiumReturnedOnHappyPath(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.newMaker(func(p *Party) { p.premiumBps = 50 })
	start, balStart := headNumber(e), e.ethBalance(tk.Address())
	id := take(t, e, taker, maker, oneEth).ID
	e.drive("maker claimed", 80, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)
	fees := e.feesPaid(tk.Address(), start)
	want := new(big.Int).Sub(balStart, oneEth)
	check(t, new(big.Int).Add(e.ethBalance(tk.Address()), fees).Cmp(want) == 0, "taker paid exactly 1 ETH + gas: premium came back")
	check(t, e.ethBalance(maker.payout).Cmp(new(big.Int).Sub(oneEth, big.NewInt(1e15))) == 0, "maker got 1 ETH minus relayer fee")
	check(t, e.ethBalance(e.k.Addr).Sign() == 0, "contract holds nothing")
	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, taker)
}

func (e *Env) newMaker(opt func(*Party)) *Party {
	p := e.NewParty(swap.Maker, nil)
	opt(p)
	p.Restart()
	return p
}

func httpGet(t *testing.T, url string) (int, []byte) {
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// Always-on maker service: live price (median of sources, spread), quotes with
// expiry, exposure limit, health and metrics, signed offer document, signed
// quote pinned to the maker identity, then a full swap at the oracle price.
func TestMakerService(t *testing.T) {
	e := NewEnv(t)
	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	or := price.New(price.Config{}, price.Static{ID: "a", XMR: 150, ETH: 300}, price.Static{ID: "b", XMR: 150, ETH: 300},
		price.Static{ID: "c", XMR: 900, ETH: 300}) // c is an outlier and is dropped
	if err := or.Refresh(e.ctx); err != nil {
		t.Fatal(err)
	}
	maker := e.newMaker(func(p *Party) { p.oracle = or; p.maxExposure = 5_000_000_000_000 })
	id, err := api.LoadIdentity(maker.eng.Store)
	if err != nil {
		t.Fatal(err)
	}
	maker.maker.Identity = id
	maker.maker.Offer.SpreadBps = 100
	maker.maker.Endpoints = []string{maker.makerSrv.URL}
	u := maker.makerSrv.URL

	code, b := httpGet(t, u+"/v1/quote?value=1000000000000000000")
	var q api.Terms
	_ = json.Unmarshal(b, &q)
	check(t, code == 200 && q.XMR == 1_980_000_000_000, "quote: 1 ETH -> %d piconero (2 XMR/ETH median, 1%% spread)", q.XMR)
	check(t, q.MinConfs >= 10 && q.Expires > time.Now().Unix() && q.EstSpendableSecs > q.EstReadySecs, "quote has >=10 confs (%d), an expiry, and time estimates (ready %ds, spendable %ds)", q.MinConfs, q.EstReadySecs, q.EstSpendableSecs)

	code, b = httpGet(t, u+"/v1/offer.signed")
	var so api.SignedOffer
	_ = json.Unmarshal(b, &so)
	doc, err := api.VerifyOffer(&so, id.ID(), time.Now())
	check(t, code == 200 && err == nil && doc.MinConfs >= 10 && doc.Endpoints[0] == u, "signed offer document verifies against the maker identity (%v)", err)
	so.Payload = strings.Replace(so.Payload, "A", "B", 1)
	_, err = api.VerifyOffer(&so, "", time.Now())
	check(t, err != nil, "a tampered offer document is rejected")

	code, b = httpGet(t, u+"/healthz")
	check(t, code == 200 && strings.Contains(string(b), `"ok":true`), "healthz ok: %s", b)

	// wrong pinned maker: refused
	_, err = api.Take(e.ctx, taker.eng, api.TakeParams{MakerURL: u, Value: oneEth, MinXMR: 1, MaxConfs: 40, MinConfs: 2,
		MakerID: strings.Repeat("ab", 32)})
	check(t, err != nil, "quote signed by another maker is refused: %v", err)
	rec, err := api.Take(e.ctx, taker.eng, api.TakeParams{MakerURL: u, Value: oneEth, MinXMR: 1, MaxConfs: 40, MinConfs: 2, MakerID: id.ID()})
	if err != nil {
		t.Fatal(err)
	}
	check(t, rec.XMR == 1_980_000_000_000, "signed quote accepted at the oracle price")
	// exposure: 2 x 1.98 XMR reserved (the refused quote above also reserved
	// until it expires); a third would exceed the 5 XMR limit
	_, err = api.Take(e.ctx, taker.eng, api.TakeParams{MakerURL: u, Value: oneEth, MinXMR: 1, MaxConfs: 40, MinConfs: 2})
	check(t, err != nil && strings.Contains(err.Error(), "exposure"), "exposure limit refuses a second swap: %v", err)

	code, b = httpGet(t, u+"/metrics")
	for _, m := range []string{"swapd_xmr_reserved_piconero 3.96e+12", "swapd_price_ok 1", "swapd_price_sources 2", `swapd_swaps{phase="negotiated"} 2`, "swapd_xmr_unlocked_piconero"} {
		check(t, code == 200 && strings.Contains(string(b), m), "metrics expose %q", m)
	}
	e.drive("maker claimed", 80, func() bool { return maker.phase(rec.ID) == swap.MClaimed }, maker, taker)
	e.drive("taker swept", 60, func() bool { return taker.phase(rec.ID) == swap.PDone }, taker)
	_, lg, _ := taker.eng.Get(rec.ID)
	check(t, strings.Contains(strings.Join(lg, "\n"), "spendable from height"), "taker is told when its XMR becomes spendable")

	// circuit breaker: a stale price stops quoting and health goes red
	or.Now = func() time.Time { return time.Now().Add(time.Hour) }
	code, _ = httpGet(t, u+"/v1/quote?value=1000000000000000000")
	check(t, code == 422, "stale price: no quotes (%d)", code)
	code, _ = httpGet(t, u+"/healthz")
	check(t, code == 503, "stale price: healthz 503")
}
