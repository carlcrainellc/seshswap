package wallet

import (
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

func ethWei(eth string) *big.Int {
	v, err := parseUnits(eth, 18)
	if err != nil {
		panic(err)
	}
	return v
}

func TestRangeReason(t *testing.T) {
	o := &api.Offer{Min: ethWei("0.0001"), Max: ethWei("0.005")}
	if r := rangeReason(o, ethWei("0.005")); r != "" {
		t.Fatalf("0.005 is in range, got %q", r)
	}
	want := "this maker takes 0.0001 to 0.005 ETH per swap"
	for _, v := range []string{"0.006", "0.00005"} {
		if r := rangeReason(o, ethWei(v)); r != want {
			t.Fatalf("%s: got %q", v, r)
		}
	}
	if r := rangeReason(nil, ethWei("1")); r != "" {
		t.Fatalf("no offer: %q", r)
	}
	usdc := &api.Offer{Asset: eth.Address{1}, Min: big.NewInt(1_000000), Max: big.NewInt(25_000000)}
	if r := rangeReason(usdc, big.NewInt(30_000000)); r != "this maker takes 1 to 25 USDC per swap" {
		t.Fatalf("usdc: %q", r)
	}
}

func TestCapMaxUsesOfferWhenSmaller(t *testing.T) {
	bond := ethWei("0.02") // 5x a 0.004 ETH bond
	mq := &makerQuote{MaxETH: "0.02", seen: &api.Offer{Max: ethWei("0.005")}}
	mq.capMax(bond)
	if mq.MaxETH != "0.005" {
		t.Fatalf("offer max below bond limit: %q", mq.MaxETH)
	}
	mq = &makerQuote{MaxETH: "0.01", seen: &api.Offer{Max: ethWei("0.05")}}
	mq.capMax(ethWei("0.01"))
	if mq.MaxETH != "0.01" {
		t.Fatalf("bond limit below offer max: %q", mq.MaxETH)
	}
	mq = &makerQuote{MaxETH: "0.02"}
	mq.capMax(bond)
	if mq.MaxETH != "0.02" {
		t.Fatalf("no offer: %q", mq.MaxETH)
	}
	mq = &makerQuote{MaxETH: "0.02", seen: &api.Offer{Asset: eth.Address{1}, Max: big.NewInt(5)}}
	mq.capMax(bond)
	if mq.MaxETH != "0.02" {
		t.Fatalf("usdc offer must not replace an ETH cap: %q", mq.MaxETH)
	}
}

func TestRangeSummary(t *testing.T) {
	refused := func(min, max string) *makerQuote {
		return &makerQuote{outRange: true, seen: &api.Offer{Min: ethWei(min), Max: ethWei(max)}}
	}
	qs := []*makerQuote{refused("0.0001", "0.005"), refused("0.0002", "0.004")}
	if got := rangeSummary(qs); got != "Makers here take 0.0001 to 0.005 ETH per swap." {
		t.Fatalf("combined: %q", got)
	}
	if got := rangeSummary(qs[:1]); got != "This maker takes 0.0001 to 0.005 ETH per swap." {
		t.Fatalf("one maker: %q", got)
	}
	other := &makerQuote{Reason: "no answer: the maker didn't answer in time"}
	if got := rangeSummary(append(qs, other)); got != "" {
		t.Fatalf("a maker failed for another reason: %q", got)
	}
	if got := rangeSummary(nil); got != "" {
		t.Fatalf("no makers: %q", got)
	}
	usdc := &makerQuote{outRange: true, seen: &api.Offer{Asset: eth.Address{1}, Min: big.NewInt(1), Max: big.NewInt(2)}}
	if got := rangeSummary(append(qs, usdc)); got != "" {
		t.Fatalf("mixed assets: %q", got)
	}
}
