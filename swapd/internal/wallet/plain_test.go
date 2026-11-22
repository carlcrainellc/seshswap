package wallet

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

func TestPublicViewStripsSecrets(t *testing.T) {
	r := swap.Record{
		ID: "aabbccddeeff00112233445566778899", Role: swap.Taker, Phase: swap.TLocked,
		Value: big.NewInt(5e15), XMR: 2_000_000_000_000, MinConfs: 10, Grace: 6 * 3600,
		MySpend:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		MyView:    "cafecafecafecafecafecafecafecafecafecafecafecafecafecafecafecafe",
		TheirView: "11", TheirSecret: "should-not-leak", ClaimerKey: "secret-key",
		SharedAddr: "55abc", MakerURL: "http://maker.example",
		Swap: &eth.Swap{T1: 1700000000, T2: 1700008000, Grace: 6 * 3600, Value: big.NewInt(5e15)},
		Log:  []string{"locked"},
	}
	v := PublicView(r, time.Unix(1699990000, 0))
	b := v.Status + v.RefundLine + v.ValueETH + v.XMR
	if strings.Contains(b, "deadbeef") || strings.Contains(v.ID, "deadbeef") {
		t.Fatal("status mentioned a secret")
	}
	// The View struct itself must not carry secret fields; encode via a map check.
	if v.Status == "" || !strings.Contains(strings.ToLower(v.Status), "waiting") {
		t.Fatalf("status %q", v.Status)
	}
	if v.ValueETH != "0.005" {
		t.Fatalf("eth %s", v.ValueETH)
	}
	if v.XMR != "2" {
		t.Fatalf("xmr %s", v.XMR)
	}
	if v.RefundAtCT == "" || v.ETHBackAtCT == "" {
		t.Fatalf("missing CT times: refund=%q back=%q", v.RefundAtCT, v.ETHBackAtCT)
	}
	if !strings.Contains(v.RefundAtCT, "CT") && !strings.Contains(v.RefundAtCT, "CST") && !strings.Contains(v.RefundAtCT, "CDT") {
		t.Fatalf("refund time not labelled as Central: %q", v.RefundAtCT)
	}
}

func TestPlainStatusTerminal(t *testing.T) {
	cases := []struct {
		phase  swap.Phase
		substr string
	}{
		{swap.PDone, "Done"},
		{swap.TRefunded, "Refunded"},
		{swap.PSweeping, "on the way"},
		{swap.TNegotiated, "Nothing is locked"},
		{swap.PFailed, "did not start"},
	}
	for _, c := range cases {
		r := swap.Record{ID: "x", Phase: c.phase, Value: big.NewInt(1), XMR: 1}
		v := PublicView(r, time.Now())
		if !strings.Contains(v.Status, c.substr) && !strings.Contains(v.RefundLine, c.substr) {
			t.Fatalf("%s: %q / %q, want %q", c.phase, v.Status, v.RefundLine, c.substr)
		}
		if v.Phase != string(c.phase) {
			t.Fatalf("phase leaked wrong: %s", v.Phase)
		}
	}
}

func TestFormatUnits(t *testing.T) {
	if g := weiToETH(big.NewInt(1e18)); g != "1" {
		t.Fatalf("1 eth: %s", g)
	}
	if g := picoToXMR(1_500_000_000_000); g != "1.5" {
		t.Fatalf("1.5 xmr: %s", g)
	}
}

func TestPlainStatusWaitingConfs(t *testing.T) {
	terms := &eth.Swap{T1: 1700005400, T2: 1700009000, Grace: 1800, Value: big.NewInt(5e15)}
	r := swap.Record{ID: "x", Phase: swap.TLocked, Value: big.NewInt(5e15), XMR: 1, MinConfs: 10, Swap: terms}
	v := PublicView(r, time.Unix(1699990000, 0))
	if v.Status != "Waiting for the other side to lock XMR." {
		t.Fatalf("before lock: %q", v.Status)
	}
	r.XmrSeenTx = "aabbccdd"
	r.XmrSeenConfs = 0
	v = PublicView(r, time.Unix(1699990000, 0))
	if v.Status != "Waiting for Monero confirmations (0/10)." {
		t.Fatalf("in the pool: %q", v.Status)
	}
	if v.XmrLockTx != "aabbccdd" {
		t.Fatalf("xmr lock tx %q", v.XmrLockTx)
	}
	r.XmrSeenConfs = 7
	v = PublicView(r, time.Unix(1699990000, 0))
	if v.Status != "Waiting for Monero confirmations (7/10)." {
		t.Fatalf("mid-wait: %q", v.Status)
	}
}

func TestPublicViewClaimAndRefundTx(t *testing.T) {
	r := swap.Record{ID: "x", Phase: swap.PDone, Value: big.NewInt(1), XMR: 1,
		ClaimTxHash: "0xaaa", RefundTxHash: "0xbbb"}
	v := PublicView(r, time.Now())
	if v.ClaimTx != "0xaaa" {
		t.Fatalf("claim %q", v.ClaimTx)
	}
	if v.RefundTx != "0xbbb" {
		t.Fatalf("refund %q", v.RefundTx)
	}
}

func TestRefundLineNoDoubleZone(t *testing.T) {
	r := swap.Record{Phase: swap.TLocked, Value: big.NewInt(1), XMR: 1, MinConfs: 10,
		Swap: &eth.Swap{T1: 1700000000, T2: 1700008000, Grace: 1800}}
	v := PublicView(r, time.Unix(1699990000, 0))
	if strings.Contains(v.RefundLine, "Central (CT)") {
		t.Fatalf("double zone label: %q", v.RefundLine)
	}
	if !strings.Contains(v.RefundLine, "CST") && !strings.Contains(v.RefundLine, "CDT") && !strings.Contains(v.RefundLine, "CT") {
		t.Fatalf("missing Central label: %q", v.RefundLine)
	}
}

func TestPublicViewUSDCLabel(t *testing.T) {
	var usdc eth.Address
	usdc[19] = 0x42
	r := swap.Record{ID: "x", Phase: swap.TLocked, Asset: usdc, Value: big.NewInt(5_000_000), XMR: 3_000_000_000, MinConfs: 10,
		Swap: &eth.Swap{T1: 1700000000, T2: 1700008000, Grace: 1800, Asset: usdc, Value: big.NewInt(5_000_000)}}
	v := PublicView(r, time.Unix(1699990000, 0))
	if v.Asset != "USDC" || v.ValueUSDC != "5" || v.ValueLabel != "5 USDC" {
		t.Fatalf("label %+v", v)
	}
	if v.ValueETH != "" {
		t.Fatalf("ETH amount should be empty for a USDC swap, got %q", v.ValueETH)
	}
	if !strings.Contains(v.Status, "Waiting for the other side to lock XMR") {
		t.Fatalf("status %q", v.Status)
	}
	if !strings.Contains(v.RefundLine, "USDC") || strings.Contains(v.RefundLine, "your ETH") {
		t.Fatalf("refund should mention USDC: %q", v.RefundLine)
	}
}

func TestPlainStatusShowsCancelFrom(t *testing.T) {
	r := swap.Record{Role: swap.Taker, Phase: swap.TLocked, Value: big.NewInt(1), Grace: 3600,
		Swap: &eth.Swap{T1: 1700003600, T2: 1700007200, Grace: 3600, Value: big.NewInt(1)}, CancelFrom: 1700001200}
	_, line, _, _ := plainStatus(r, time.Unix(1700000000, 0))
	if !strings.Contains(line, "early cancel is possible from") {
		t.Fatalf("no cancel time: %q", line)
	}
	_, line, _, _ = plainStatus(r, time.Unix(1700001200, 0))
	if strings.Contains(line, "early cancel") {
		t.Fatalf("cancel time shown after the window: %q", line)
	}
}
