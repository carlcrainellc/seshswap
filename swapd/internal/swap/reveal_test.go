package swap

import (
	"context"
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

// The reveal guard refuses to even sign a claim/refund once less than the margin
// is left (no RPC is touched: Engine.K is nil here and would panic if used).
func TestRevealGuardRefusesInsideMargin(t *testing.T) {
	e := &Engine{Policy: Policy{Margin: 600}}
	r := &Record{ID: "0123456789abcdef0123456789abcdef"}
	for _, now := range []uint64{10_000 - 600, 10_000 - 1, 10_000, 20_000} {
		if err := e.sendReveal(context.Background(), r, "claim", nil, nil, MClaiming, 10_000, now); err == nil {
			t.Fatalf("reveal allowed at now=%d with deadline 10000 and margin 600", now)
		}
	}
	if r.Pending != nil || r.Phase != "" {
		t.Fatal("a refused reveal must not change the record")
	}
}

// refund() against a cancel-intent deadline refuses inside the margin.
func TestRefundCancelDeadline(t *testing.T) {
	s, err := keys.RandomScalar()
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Policy: Policy{Margin: 600}}
	r := &Record{ID: "0123456789abcdef0123456789abcdef", MySpend: s.Hex(), Swap: &eth.Swap{T1: 10_000, T2: 20_000, Value: big.NewInt(1), ClaimKey: big.NewInt(1), RefundKey: big.NewInt(1)}}
	for _, now := range []uint64{9_400, 9_999, 12_000} {
		if err := e.refund(context.Background(), r, now, 10_000); err == nil {
			t.Fatalf("cancel refund allowed at %d with deadline 10000 and margin 600", now)
		}
	}
}

func TestClaimerWindowMirrorsContract(t *testing.T) {
	s := &eth.Swap{T1: 1000, T2: 2000}
	cases := []struct {
		st   eth.State
		now  uint64
		want bool
	}{
		{eth.State{Stage: eth.StatusPending}, 999, false},
		{eth.State{Stage: eth.StatusPending}, 1000, true},
		{eth.State{Stage: eth.StatusReady}, 1, true},
		{eth.State{Stage: eth.StatusReady}, 2000, false},
		{eth.State{Stage: eth.StatusCancelling, Deadline: 1500}, 1200, false},
		{eth.State{Stage: eth.StatusCancelling, Deadline: 1500}, 1500, true},
		{eth.State{Stage: eth.StatusCancelling, Deadline: 900}, 950, false}, // expired before t1: still t1
		{eth.State{Stage: eth.StatusClaiming, Deadline: 1500}, 1200, false},
	}
	for i, c := range cases {
		if got := claimerWindow(s, &c.st, c.now); got != c.want {
			t.Fatalf("case %d: got %v", i, got)
		}
	}
}

func TestConfsPolicy(t *testing.T) {
	const x = piconero
	for _, c := range []struct{ amt, reorg, want uint64 }{
		{x / 10, 0, 10}, {9 * x, 0, 10}, {10 * x, 0, 15}, {50 * x, 0, 20}, {500 * x, 0, 30},
		{x, 18, 22}, {500 * x, 30, 34},
	} {
		if got := XmrConfs(c.amt, c.reorg); got != c.want {
			t.Fatalf("XmrConfs(%d, %d) = %d, want %d", c.amt, c.reorg, got, c.want)
		}
	}
	p := DefaultPolicy()
	d1, d2 := Windows(10, 0, p)
	if d1 < ContractMinWindow || d2 < ContractMinWindow || d1 < 10*XmrBlockSecs+2*p.Margin {
		t.Fatal(d1, d2)
	}
	// a slow chain (280 s blocks, like stagenet in Nov 2026) stretches t1
	if d1s, _ := Windows(10, 280, p); d1s != p.XmrLockBudget+10*280*2+2*p.Margin || ConfirmSecs(10, 60) != 10*XmrBlockSecs*2 {
		t.Fatal("windows do not follow the observed block time", d1s)
	}
	if g := Grace(p); g < ContractMinGrace || g > ContractMaxGrace {
		t.Fatal(g)
	}
	if SpendableAt(100, 0) != 110 || SpendableAt(100, 130) != 140 || SpendableAt(0, 0) != 0 {
		t.Fatal("spendable")
	}
	// the maker must still have time to lock XMR after the lock (d1 - need >= budget)
	e := &Engine{Policy: p}
	r := &Record{MinConfs: 10}
	if e.xmrLockDeadlineFor(r, d1) < p.XmrLockBudget {
		t.Fatal("no time left to lock XMR inside d1")
	}
}
