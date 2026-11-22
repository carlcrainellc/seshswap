package exitpay

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

const bsesh = "rgwybw99m4d464rbi9z95a7a364n1hauo74ip486ssckf1agw7zo.sesh"

// Same call encoded by `cast calldata`.
const castPayAddress = "bb07447200000000000000000000000000000000000000000000000000000000000000200000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000c4a60000000000000000000000000000000000000000000000000000000000000004000000000000000000000000000000000000000000000000000000000000000a0000000000000000000000000000000000000000000000000000b5e620f4800000000000000000000000000000000000000000000000000000000000000000ff00000000000000000000000000000000000000000000000000000000000000e0000000000000000000000000000000000000000000000000000000000000003972677779627739396d3464343634726269397a39356137613336346e316861756f373469703438367373636b6631616777377a6f2e7365736800000000000000"

func target() *Target {
	rt, _ := eth.ParseAddress("0x000000000000000000000000000000000000c4a6")
	var ref eth.Hash
	ref[31] = 0xff
	return &Target{Factory: rt, RefundTo: rt, ListingID: 4, Hours: 10,
		MaxCost: big.NewInt(200000000000000), Ref: ref, BuyerSesh: bsesh}
}

func TestEncodeMatchesCast(t *testing.T) {
	if got := hex.EncodeToString(EncodePayAddress(target())); got != castPayAddress {
		t.Fatalf("encoding differs:\n got %s\nwant %s", got, castPayAddress)
	}
	d, s := EncodeDeploy(target()), EncodeSettle(target())
	if hex.EncodeToString(d[4:]) != castPayAddress[8:] || hex.EncodeToString(s[4:]) != castPayAddress[8:] {
		t.Fatal("deploy/settle args differ from payAddress args")
	}
}

func TestCheck(t *testing.T) {
	if err := target().Check(); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*Target){
		"no refund":  func(x *Target) { x.RefundTo = eth.Address{} },
		"no hours":   func(x *Target) { x.Hours = 0 },
		"no max":     func(x *Target) { x.MaxCost = nil },
		"bad sesh":   func(x *Target) { x.BuyerSesh = "example.com" },
		"no factory": func(x *Target) { x.Factory = eth.Address{} },
	} {
		x := target()
		f(x)
		if x.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRefPerSession(t *testing.T) {
	if Ref("a") == Ref("b") || Ref("a") != Ref("a") {
		t.Fatal("ref must be per session and stable")
	}
}

// L-1: ExitPay.deploy reverts on these, so the forwarder could never get
// code; swapd must not resolve, settle or pay to such a target.
func TestUndeployableRejected(t *testing.T) {
	for name, f := range map[string]func(*Target){
		"zero refund":    func(x *Target) { x.RefundTo = eth.Address{} },
		"zero hours":     func(x *Target) { x.Hours = 0 },
		"zero max":       func(x *Target) { x.MaxCost = big.NewInt(0) },
		"negative max":   func(x *Target) { x.MaxCost = big.NewInt(-1) },
		"max > 256 bits": func(x *Target) { x.MaxCost = new(big.Int).Lsh(big.NewInt(1), 256) },
	} {
		x := target()
		f(x)
		if x.Check() == nil {
			t.Errorf("%s: Check accepted", name)
		}
		if err := x.Resolve(nil, nil); err == nil { // must fail before any RPC
			t.Errorf("%s: Resolve accepted", name)
		}
		x.Address = x.Factory
		if x.CheckPay(x.Address) == nil {
			t.Errorf("%s: CheckPay accepted", name)
		}
	}
	var nilT *Target
	if nilT.Check() == nil {
		t.Error("nil target accepted")
	}
}

func TestCheckPay(t *testing.T) {
	x := target()
	if x.CheckPay(eth.Address{}) == nil {
		t.Error("unresolved forwarder accepted")
	}
	x.Address[19] = 9
	if err := x.CheckPay(x.Address); err != nil {
		t.Fatal(err)
	}
	if x.CheckPay(x.RefundTo) == nil {
		t.Error("payout to a non-forwarder accepted")
	}
}
