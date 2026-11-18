package api

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

func TestCheckTermsMatchesWalletLimits(t *testing.T) {
	e := &swap.Engine{Policy: swap.DefaultPolicy(), AssumeBlockSecs: 120}
	pol := e.Policy
	p := TakeParams{Value: big.NewInt(3e14), MinD1: 2*pol.Margin + 1800, MinD2: 2 * pol.Margin, MaxConfs: 60, MinConfs: swap.XmrConfFloor}
	zero := new(big.Int)
	var te TermsError
	// The 0.1.0 testnet makers' terms: refused as terms, not as a failed check.
	if _, err := CheckTerms(context.Background(), e, p, 1800, 1800, 10, 1e9, 1800, zero); !errors.As(err, &te) {
		t.Fatalf("short timeouts accepted: %v", err)
	}
	// What the makers offer now.
	confs, err := CheckTerms(context.Background(), e, p, 10800, 3600, 10, 1e9, 21600, zero)
	if err != nil || confs != 10 {
		t.Fatalf("good terms refused: %v (confs %d)", err, confs)
	}
	// Grace outside 6 h to 12 h.
	if _, err := CheckTerms(context.Background(), e, p, 10800, 3600, 10, 1e9, 1800, zero); !errors.As(err, &te) {
		t.Fatalf("short grace accepted: %v", err)
	}
	// No block time: an error, but not a terms refusal.
	e2 := &swap.Engine{Policy: swap.DefaultPolicy()}
	if _, err := CheckTerms(context.Background(), e2, p, 10800, 3600, 10, 1e9, 21600, zero); err == nil || errors.As(err, &te) {
		t.Fatalf("missing block time: %v", err)
	}
}
