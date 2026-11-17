package main

import (
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

func TestEthDeltas(t *testing.T) {
	taker, payout, relayer := eth.Address{1}, eth.Address{2}, eth.Address{3}
	v, prem, fee := big.NewInt(5_000_000), big.NewInt(25_000), big.NewInt(1_000)
	lock := func(p *big.Int) eth.Event {
		return eth.Event{Kind: "Locked", Swap: &eth.Swap{Owner: taker, Value: v, Premium: p}}
	}
	cases := []struct {
		name       string
		evs        []eth.Event
		dt, dp, dr int64
	}{
		{"claimed via relayer", []eth.Event{lock(nil), {Kind: "Claimed", To: payout, Relayer: relayer, Fee: fee}}, -5_000_000, 4_999_000, 1_000},
		{"claimed, premium returned", []eth.Event{lock(prem), {Kind: "Acked", To: payout},
			{Kind: "Claimed", To: payout, Relayer: relayer, Fee: fee}, {Kind: "PremiumPaid", To: taker, Amount: prem}}, -5_000_000, 4_999_000, 1_000},
		// the case the first Sepolia report got wrong (State().PremiumTo is 0 after settlement)
		{"refund after ack, premium forfeited", []eth.Event{lock(prem), {Kind: "Acked", To: payout}, {Kind: "CancelIntent"},
			{Kind: "Refunded"}, {Kind: "PremiumPaid", To: payout, Amount: prem}}, -25_000, 25_000, 0},
		{"refund, no premium", []eth.Event{lock(nil), {Kind: "ClaimIntent"}, {Kind: "Refunded"}}, 0, 0, 0},
		{"refund, premium not forfeited", []eth.Event{lock(prem), {Kind: "Refunded"}, {Kind: "PremiumPaid", To: taker, Amount: prem}}, 0, 0, 0},
		{"still locked", []eth.Event{lock(prem)}, -5_025_000, 0, 0},
	}
	for _, c := range cases {
		dt, dp, dr := ethDeltas(c.evs, taker, payout, relayer)
		if dt.Int64() != c.dt || dp.Int64() != c.dp || dr.Int64() != c.dr {
			t.Errorf("%s: taker %s payout %s relayer %s; want %d %d %d", c.name, dt, dp, dr, c.dt, c.dp, c.dr)
		}
	}
}

func TestRelayerEthWant(t *testing.T) {
	start := big.NewInt(15_003_998_167_622_002)
	recv := big.NewInt(2_000_000_000_000) // two claimed swaps, 1e12 wei fee each
	paid := big.NewInt(18_186_120)
	got := relayerEthWant(start, recv, paid)
	want := new(big.Int).Add(start, recv)
	want.Sub(want, paid)
	if got.Cmp(want) != 0 {
		t.Fatalf("got %s want %s", got, want)
	}
	// end = start + fees received − gas paid, never just start + fees
	if got.Cmp(new(big.Int).Add(start, recv)) == 0 {
		t.Fatal("gas paid was ignored")
	}
}

func TestReleaseNow(t *testing.T) {
	const t2, m = 10_000, 300
	if releaseNow(t2+299, t2, t2+14_400, m) || !releaseNow(t2+300, t2, t2+14_400, m) {
		t.Fatal("ample grace: release at t2+300")
	}
	// grace deadline just after t2: release right after t2, not at t2+300
	if !releaseNow(t2+1, t2, t2+200, m) || releaseNow(t2, t2, t2+200, m) {
		t.Fatal("tight grace: release as soon as t2 has passed")
	}
}
