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
