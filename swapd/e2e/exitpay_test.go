package e2e

import (
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/exitpay"
	"github.com/carlcrainellc/seshswap/swapd/internal/exits"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

const (
	exitSellerPK = "7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6"
	makerGasPK   = "47e179ec197488593b187f80a00eb0da91f1b9d0b13f8733639f19c30a34926a"
	exitSesh     = "wo7xwuzostufug8ptrth5natahx53nug9jsazm783nere8rj68ky.sesh"
	buyerSesh    = "rgwybw99m4d464rbi9z95a7a364n1hauo74ip486ssckf1agw7zo.sesh"
)

func word(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }

// The XMR seller (maker) claims 1 ETH to its ExitPay forwarder: the claim pays
// for 10 hours on a listed exit for its .sesh, the change goes to its wallet.
func TestXmrPaysForExitTime(t *testing.T) {
	e := NewEnv(t)
	dk, _ := eth.KeyFromHex(deployerPK)
	sk, _ := eth.KeyFromHex(exitSellerPK)
	gk, _ := eth.KeyFromHex(makerGasPK)

	p := registry.PlaceholderParams(e.k.Addr, eth.Address{}, eth.Address{})
	p.USDCDecimals, p.MaxFeedAge = 0, 0
	reg := deploy(t, e.ctx, e.eth, dk, append(bytecode(t, "SeshRegistry.sol", "SeshRegistry"), p.ConstructorArgs()...))
	el := deploy(t, e.ctx, e.eth, dk, append(bytecode(t, "ExitLease.sol", "ExitLease"),
		append(word(reg[:]), make([]byte, 32)...)...))
	factory := deploy(t, e.ctx, e.eth, dk, append(bytecode(t, "ExitPay.sol", "ExitPay"),
		append(word(el[:]), word(e.k.Addr[:])...)...))

	tag, _ := registry.Tag(exits.TagExit)
	if _, err := e.eth.SendAndWait(e.ctx, sk, &reg, big.NewInt(2e14), registry.EncodeRegister(exitSesh, tag, "test exit")); err != nil {
		t.Fatal(err)
	}
	sellerPayout, _ := eth.NewKey()
	weiH := big.NewInt(2e13)
	if _, err := e.eth.SendAndWait(e.ctx, sk, &el, nil, exits.EncodeSetOffer(1, sellerPayout.Address(), weiH, nil, 24)); err != nil {
		t.Fatal(err)
	}

	tk, _ := eth.KeyFromHex(takerPK)
	taker := e.NewParty(swap.Taker, tk)
	maker := e.NewParty(swap.Maker, gk)
	maker.maker.ExitPay = &exitpay.Target{Factory: factory, RefundTo: maker.payout, ListingID: 1, Hours: 10,
		MaxCost: big.NewInt(3e14), BuyerSesh: buyerSesh}

	rec := take(t, e, taker, maker, oneEth)
	id := rec.ID
	mrec := lockedRecord(maker, id)
	check(t, mrec.ExitPay != nil && mrec.PayoutTo == mrec.ExitPay.Address && !mrec.PayoutTo.IsZero(),
		"agreed claim destination is the ExitPay forwarder %s", mrec.PayoutTo.Hex())
	check(t, mrec.ExitPay.Ref == exitpay.Ref(id), "forwarder is bound to this swap")

	e.drive("maker claimed", 120, func() bool { return maker.phase(id) == swap.MClaimed }, maker, taker)
	mrec = lockedRecord(maker, id)
	check(t, mrec.ExitPayDone, "exit pay settled after the claim")

	k := &exits.Contract{C: e.eth, Addr: el}
	until, err := k.ActiveUntil(e.ctx, 1, buyerSesh)
	check(t, err == nil && until > 0, "lease active for %s until %d", buyerSesh, until)
	paid, _ := e.eth.Balance(e.ctx, sellerPayout.Address())
	want := new(big.Int).Mul(weiH, big.NewInt(10))
	check(t, paid.Cmp(want) == 0, "exit seller received %s wei for 10 h", paid)
	change, _ := e.eth.Balance(e.ctx, maker.payout)
	wantChange := new(big.Int).Sub(new(big.Int).Sub(oneEth, big.NewInt(1e15)), want)
	check(t, change.Cmp(wantChange) == 0, "change %s = 1 ETH - relay fee - lease", change)
	fb, _ := e.eth.Balance(e.ctx, mrec.ExitPay.Address)
	check(t, fb.Sign() == 0, "nothing left in the forwarder")
	done, _ := exitpay.Done(e.ctx, e.eth, mrec.ExitPay.Address)
	check(t, done, "forwarder reports the lease bought")
	e.drive("taker swept", 60, func() bool { return taker.phase(id) == swap.PDone }, maker, taker)
}
