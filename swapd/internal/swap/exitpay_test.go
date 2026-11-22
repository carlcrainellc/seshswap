package swap

import (
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/exitpay"
)

// L-1: a claim must never pay to an ExitPay forwarder that can't be deployed.
func TestClaimToExitPay(t *testing.T) {
	var fwd, refund, fac eth.Address
	fwd[19], refund[19], fac[19] = 1, 2, 3
	good := func() *exitpay.Target {
		return &exitpay.Target{Factory: fac, RefundTo: refund, Hours: 1, MaxCost: big.NewInt(5),
			BuyerSesh: "rgwybw99m4d464rbi9z95a7a364n1hauo74ip486ssckf1agw7zo.sesh", Address: fwd}
	}
	r := &Record{PayoutTo: fwd, ExitPay: good()}
	if to, err := claimTo(r); err != nil || to != fwd {
		t.Fatalf("good target: %v %v", to, err)
	}
	// zero hours / zero max cost: undeployable, claim goes to the refund address
	for _, f := range []func(*exitpay.Target){
		func(x *exitpay.Target) { x.Hours = 0 },
		func(x *exitpay.Target) { x.MaxCost = big.NewInt(0) },
		func(x *exitpay.Target) { x.Address = eth.Address{} },
	} {
		r := &Record{PayoutTo: fwd, ExitPay: good()}
		f(r.ExitPay)
		if to, err := claimTo(r); err == nil || to != refund {
			t.Fatalf("bad target: to %v err %v", to, err)
		}
	}
	// payout not the forwarder
	r = &Record{PayoutTo: refund, ExitPay: good()}
	if to, _ := claimTo(r); to != refund {
		t.Fatalf("mismatch: to %v", to)
	}
	// zero refund address: refuse to claim at all
	r = &Record{PayoutTo: fwd, ExitPay: good()}
	r.ExitPay.RefundTo = eth.Address{}
	if to, err := claimTo(r); err == nil || !to.IsZero() {
		t.Fatalf("zero refund: to %v err %v", to, err)
	}
	// no exit pay: unchanged
	r = &Record{PayoutTo: fwd}
	if to, err := claimTo(r); err != nil || to != fwd {
		t.Fatal("plain payout changed")
	}
}
