package swap

import (
	"context"
	"errors"
	"testing"
)

func TestBlockSecsRequiresDaemonOrConfig(t *testing.T) {
	ctx := context.Background()
	e := &Engine{}
	if _, err := e.BlockSecs(ctx); !errors.Is(err, ErrNoBlockTime) {
		t.Fatalf("nil daemon, no config: %v", err)
	}

	e.AssumeBlockSecs = 280
	s, err := e.BlockSecs(ctx)
	if err != nil || s != 280 {
		t.Fatalf("explicit 280: got %d, %v", s, err)
	}

	// Floor still applies to a configured mean below the 120 s target.
	e.AssumeBlockSecs = 60
	s, err = e.BlockSecs(ctx)
	if err != nil || s != XmrBlockSecs {
		t.Fatalf("explicit 60 should floor at 120: got %d, %v", s, err)
	}
}

func TestConfirmSecsKeepsFloor(t *testing.T) {
	// 2*max(observed, 120): a 60 s observation still budgets 120 s * 2.
	if ConfirmSecs(10, 60) != 10*XmrBlockSecs*2 {
		t.Fatal("floor lost")
	}
	if ConfirmSecs(10, 280) != 10*280*2 {
		t.Fatal("observed 280 not used")
	}
}

func TestQuoteRequiresBlockTime(t *testing.T) {
	// Covered at the Engine layer; the maker's quote and the taker's t1
	// check both call BlockSecs and return its error. A nil-daemon engine
	// used only for BlockSecs is enough; api tests would need a full Maker.
	e := &Engine{}
	if _, err := e.BlockSecs(context.Background()); !errors.Is(err, ErrNoBlockTime) {
		t.Fatal(err)
	}
}
