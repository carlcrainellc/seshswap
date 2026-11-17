package feed

import (
	"math"
	"math/big"
	"testing"
	"time"
)

func TestImpliedUSDC(t *testing.T) {
	// 2 USDC (2e6) for 0.005 XMR (5e9 pico) → $400 / XMR
	val := big.NewInt(2_000_000)
	got, err := ImpliedXMRUSD("USDC", val, 5_000_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-400) > 0.01 {
		t.Fatalf("implied %v", got)
	}
}

func TestImpliedETH(t *testing.T) {
	// 0.005 ETH at $2000, for 0.025 XMR (2.5e10 pico) → $400 / XMR
	// usd = 0.005*2000 = 10; xmr = 0.025; 10/0.025 = 400
	val := big.NewInt(5_000_000_000_000_000) // 0.005 ETH
	got, err := ImpliedXMRUSD("ETH", val, 25_000_000_000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-400) > 0.01 {
		t.Fatalf("implied %v", got)
	}
}

func TestOffBpsAndWarn(t *testing.T) {
	if OffBps(400, 400) != 0 {
		t.Fatal("exact")
	}
	// 5% high = 500 bps
	if OffBps(420, 400) != 500 {
		t.Fatalf("5%%: %d", OffBps(420, 400))
	}
	// 6% → warn at 500 max
	c := Evaluate(400, 424, 500, "mock XMR/USD", "0xabc", time.Unix(1_700_000_000, 0))
	if !c.Warn || c.OffBps != 600 {
		t.Fatalf("want warn at 600 bps: %+v", c)
	}
	c2 := Evaluate(400, 410, 500, "mock XMR/USD", "0xabc", time.Unix(1_700_000_000, 0))
	if c2.Warn || c2.OffBps != 250 {
		t.Fatalf("2.5%% should be ok: %+v", c2)
	}
}

func TestImpliedBad(t *testing.T) {
	if _, err := ImpliedXMRUSD("ETH", big.NewInt(1), 1, 0); err == nil {
		t.Fatal("ETH without ETH/USD should fail")
	}
	if _, err := ImpliedXMRUSD("USDC", big.NewInt(0), 1, 0); err == nil {
		t.Fatal("zero value")
	}
}
