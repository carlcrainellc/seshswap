package swap

import (
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// M-1 lock-in: the taker never sends cancelIntent before the window ends
// (the contract would revert it), exactly from its end on; the maker knows
// when its ack is late.
func TestLockInCancelBoundary(t *testing.T) {
	end := uint64(1_000_000 + eth.LockIn)
	st := &eth.State{Stage: eth.StatusPending, Deadline: end}
	if cancelAllowed(st, end-1) {
		t.Fatal("cancel allowed inside the lock-in")
	}
	if !cancelAllowed(st, end) || !cancelAllowed(st, end+1) {
		t.Fatal("cancel refused at/after the lock-in end")
	}
	if lateAck(st, end-1) || !lateAck(st, end) {
		t.Fatal("lateAck boundary")
	}
	// old deployment without a lock-in: deadline 0 while PENDING
	old := &eth.State{Stage: eth.StatusPending}
	if !cancelAllowed(old, 1) || lateAck(old, 1) {
		t.Fatal("no lock-in deployment")
	}
	// READY / CANCELLING: no cancel; CancelFrom only for PENDING
	for _, s := range []uint8{eth.StatusReady, eth.StatusCancelling, eth.StatusClaiming} {
		x := &eth.State{Stage: s, Deadline: end}
		if cancelAllowed(x, end+1) || x.CancelFrom() != 0 || lateAck(x, end+1) {
			t.Fatalf("stage %d", s)
		}
	}
}

func TestLockInShorterThanMinD1(t *testing.T) {
	if eth.LockIn >= 30*60 { // XmrSwapV3.MIN_D1
		t.Fatal("lock-in must end before the shortest t1")
	}
}
