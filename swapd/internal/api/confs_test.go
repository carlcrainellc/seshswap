package api

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

// testMaker quotes 1 XMR per 1e18 wei (so 0.1 ETH = 0.1 XMR, the 10-conf tier).
func testMaker(maxConfs uint64) *Maker {
	return &Maker{
		Engine: &swap.Engine{Role: swap.Maker, Policy: swap.DefaultPolicy(), AssumeBlockSecs: 120},
		Offer: Offer{ChainID: big.NewInt(421614), PicoPerUnit: big.NewInt(1e12),
			Min: big.NewInt(1e15), Max: new(big.Int).Mul(big.NewInt(100), one18),
			MinConfs: 10, MaxConfs: maxConfs, XmrNet: keys.Stagenet, TTL: 900},
	}
}

func getQuote(t *testing.T, m *Maker, q string) (int, Terms, string) {
	t.Helper()
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("GET", "/v1/quote?"+q, nil))
	var tr Terms
	var e struct {
		Error string `json:"error"`
	}
	b := w.Body.Bytes()
	_ = json.Unmarshal(b, &tr)
	_ = json.Unmarshal(b, &e)
	return w.Code, tr, e.Error
}

func TestMakerQuotesTakerConfs(t *testing.T) {
	m := testMaker(0)
	pol := m.Engine.Policy
	small := "value=100000000000000000" // 0.1 XMR
	for _, c := range []struct {
		q    string
		want uint64
	}{
		{small, 10},                       // no preference: size tier
		{small + "&minConfs=5", 10},       // can't go below the floor
		{small + "&minConfs=20", 20},      // Safer
		{small + "&minConfs=30", 30},      // Safest
		{"value=6000000000000000000", 20}, // 6 XMR: size tier 20
		{"value=6000000000000000000&minConfs=30", 30},
		{"value=25000000000000000000&minConfs=20", 30}, // 25 XMR: size wins
	} {
		code, tr, msg := getQuote(t, m, c.q)
		if code != 200 || tr.MinConfs != c.want {
			t.Fatalf("%s: %d %q confs %d, want %d", c.q, code, msg, tr.MinConfs, c.want)
		}
		d1, _ := swap.Windows(c.want, 120, pol)
		if tr.D1 != d1 {
			t.Fatalf("%s: d1 %d, want %d (windows sized for %d confs)", c.q, tr.D1, d1, c.want)
		}
	}
	// More than the maker allows: declined, with the limit in the message.
	if code, _, msg := getQuote(t, m, small+"&minConfs=61"); code != 422 || !strings.Contains(msg, "at most 60") {
		t.Fatalf("over default max: %d %q", code, msg)
	}
	m2 := testMaker(25)
	if code, _, msg := getQuote(t, m2, small+"&minConfs=30"); code != 422 || !strings.Contains(msg, "at most 25") {
		t.Fatalf("over maker max: %d %q", code, msg)
	}
	if code, tr, _ := getQuote(t, m2, small+"&minConfs=20"); code != 200 || tr.MinConfs != 20 {
		t.Fatalf("under maker max: %d %d", code, tr.MinConfs)
	}
	if code, _, _ := getQuote(t, m, small+"&minConfs=x"); code != 400 {
		t.Fatalf("bad minConfs: %d", code)
	}
}

func TestTakerRefusesQuoteBelowItsConfs(t *testing.T) {
	e := &swap.Engine{Policy: swap.DefaultPolicy(), AssumeBlockSecs: 120}
	pol := e.Policy
	p := TakeParams{Value: big.NewInt(3e14), MinD1: 2*pol.Margin + 1800, MinD2: 2 * pol.Margin,
		MaxConfs: swap.XmrConfMax, MinConfs: 20}
	zero := new(big.Int)
	ctx := context.Background()
	var te TermsError
	// An older maker ignores the request and quotes 10, even with long windows.
	if _, err := CheckTerms(ctx, e, p, 20000, 3600, 10, 1e11, 21600, zero); !errors.As(err, &te) || !strings.Contains(err.Error(), "asked for 20") {
		t.Fatalf("old maker's 10 accepted: %v", err)
	}
	// A new maker quotes 20 with windows to match.
	d1, d2 := swap.Windows(20, 120, pol)
	if confs, err := CheckTerms(ctx, e, p, d1, d2, 20, 1e11, 21600, zero); err != nil || confs != 20 {
		t.Fatalf("new maker refused: %v (%d)", err, confs)
	}
	// Windows sized for 10 can't carry 20 confirmations.
	d1s, _ := swap.Windows(10, 120, pol)
	if _, err := CheckTerms(ctx, e, p, d1s, d2, 20, 1e11, 21600, zero); !errors.As(err, &te) {
		t.Fatalf("short t1 accepted: %v", err)
	}
	// Over the taker's own maximum.
	if _, err := CheckTerms(ctx, e, p, 100000, d2, 61, 1e11, 21600, zero); !errors.As(err, &te) {
		t.Fatalf("61 accepted: %v", err)
	}
	// Size still raises the count above the setting: 25 XMR needs 30.
	d30, _ := swap.Windows(30, 120, pol)
	if confs, err := CheckTerms(ctx, e, p, d30, d2, 30, 25e12, 21600, zero); err != nil || confs != 30 {
		t.Fatalf("size tier: %v (%d)", err, confs)
	}
}

// The request field is optional: Standard sends nothing new, and an older
// maker's decoder ignores it.
func TestSwapRequestConfsCompat(t *testing.T) {
	b, _ := json.Marshal(SwapRequest{Session: "s"})
	if strings.Contains(string(b), "minConfs") {
		t.Fatalf("standard request carries minConfs: %s", b)
	}
	b, _ = json.Marshal(SwapRequest{Session: "s", MinConfs: 20})
	var old struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(strings.NewReader(string(b))).Decode(&old); err != nil || old.Session != "s" {
		t.Fatalf("old decoder: %v", err)
	}
	var got SwapRequest
	_ = json.Unmarshal(b, &got)
	if got.MinConfs != 20 {
		t.Fatalf("round trip %d", got.MinConfs)
	}
}
