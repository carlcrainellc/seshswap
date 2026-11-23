package api

import (
	"context"
	"math/big"
	"net/http"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/price"
)

const xmr1 = uint64(1e12)

func TestSpreadTierSelection(t *testing.T) {
	o := &Offer{SpreadBps: 50, SpreadTiers: []SpreadTier{{FromXmr: 20, Bps: 400}, {FromXmr: 0, Bps: 100}, {FromXmr: 5, Bps: 200}}}
	for _, c := range []struct{ xmr, want uint64 }{
		{0, 100}, {xmr1 / 10, 100}, {5*xmr1 - 1, 100}, {5 * xmr1, 200},
		{20*xmr1 - 1, 200}, {20 * xmr1, 400}, {1000 * xmr1, 400},
	} {
		if got, tiered := o.spreadFor(c.xmr); got != c.want || !tiered {
			t.Fatalf("spreadFor(%d) = %d, want %d", c.xmr, got, c.want)
		}
	}
	// No tier matches below the first one: the flat spread applies.
	o2 := &Offer{SpreadBps: 50, SpreadTiers: []SpreadTier{{FromXmr: 1, Bps: 300}}}
	if got, _ := o2.spreadFor(xmr1 / 2); got != 50 {
		t.Fatalf("below first tier: %d", got)
	}
	if got, _ := o2.spreadFor(xmr1); got != 300 {
		t.Fatalf("at first tier: %d", got)
	}
	// No tiers: the flat spread, not tiered.
	if got, tiered := (&Offer{SpreadBps: 75}).spreadFor(100 * xmr1); got != 75 || tiered {
		t.Fatalf("flat: %d %v", got, tiered)
	}
	for _, bad := range [][]SpreadTier{{{FromXmr: -1, Bps: 1}}, {{FromXmr: 1, Bps: 10000}}, {{FromXmr: 1, Bps: 1}, {FromXmr: 1, Bps: 2}}} {
		if CheckSpreadTiers(bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if err := CheckSpreadTiers(o.SpreadTiers); err != nil {
		t.Fatal(err)
	}
}

func TestStaticPriceWithAndWithoutTiers(t *testing.T) {
	m := testMaker(0) // static 1 XMR per 1e18 wei, SpreadBps 0
	m.Offer.SpreadBps = 100
	// Static, no tiers: the fixed rate as before (SpreadBps only applies to a live price).
	code, tr, _ := getQuote(t, m, "value=6000000000000000000")
	if code != 200 || tr.XMR != 6*xmr1 || tr.MarketPerUnit != nil || tr.SpreadBps != 0 {
		t.Fatalf("static: %d %+v", code, tr)
	}
	// Static with tiers: the tier markup on top of the static rate.
	m.Offer.SpreadTiers = []SpreadTier{{FromXmr: 0, Bps: 100}, {FromXmr: 5, Bps: 200}, {FromXmr: 20, Bps: 400}}
	for _, c := range []struct {
		q       string
		bps     uint64
		xmrPico uint64
	}{
		{"value=1000000000000000000", 100, xmr1 * 9900 / 10000},
		{"value=5000000000000000000", 200, 5 * xmr1 * 9800 / 10000},
		{"value=4999999000000000000", 100, 4999999 * (xmr1 / 1e6) * 9900 / 10000},
		{"value=20000000000000000000", 400, 20 * xmr1 * 9600 / 10000},
	} {
		code, tr, msg := getQuote(t, m, c.q)
		if code != 200 || tr.SpreadBps != c.bps || tr.XMR != c.xmrPico || tr.MarketPerUnit.Cmp(big.NewInt(1e12)) != 0 {
			t.Fatalf("%s: %d %q bps %d xmr %d", c.q, code, msg, tr.SpreadBps, tr.XMR)
		}
	}
}

type fixedSource struct{ xmr, eth float64 }

func (f fixedSource) Name() string { return "fixed" }
func (f fixedSource) Fetch(context.Context, *http.Client) (price.Quote, error) {
	return price.Quote{Source: "fixed", XMR: f.xmr, ETH: f.eth}, nil
}

func TestLivePriceTiers(t *testing.T) {
	m := testMaker(0)
	m.Offer.SpreadBps = 100
	m.Oracle = price.New(price.Config{MinSources: 2}, fixedSource{400, 2000}, fixedSource{400, 2000})
	if err := m.Oracle.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 1 ETH = 5 XMR at the market. Flat 1 %.
	code, tr, msg := getQuote(t, m, "value=1000000000000000000")
	if code != 200 || tr.SpreadBps != 100 || tr.MarketPerUnit == nil || tr.XMR != 5*xmr1*99/100 {
		t.Fatalf("flat live: %d %q %+v", code, msg, tr)
	}
	// Tiers by size at the market rate: 1 ETH = 5 XMR is in the 5 XMR tier.
	m.Offer.SpreadTiers = []SpreadTier{{FromXmr: 0, Bps: 100}, {FromXmr: 5, Bps: 200}, {FromXmr: 20, Bps: 400}}
	if _, tr, _ = getQuote(t, m, "value=1000000000000000000"); tr.SpreadBps != 200 || tr.XMR != 5*xmr1*98/100 {
		t.Fatalf("5 XMR tier: %+v", tr)
	}
	if _, tr, _ = getQuote(t, m, "value=100000000000000000"); tr.SpreadBps != 100 {
		t.Fatalf("small tier: %+v", tr)
	}
	if _, tr, _ = getQuote(t, m, "value=4000000000000000000"); tr.SpreadBps != 400 {
		t.Fatalf("20 XMR tier: %+v", tr)
	}
	// The offer shows the rate for the smallest swap.
	o, err := m.currentOffer()
	if err != nil || o.PicoPerUnit.Cmp(big.NewInt(5e12*99/100)) != 0 {
		t.Fatalf("offer rate %v %v", o.PicoPerUnit, err)
	}
}
