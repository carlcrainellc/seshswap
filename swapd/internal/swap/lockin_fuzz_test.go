package swap

import (
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// FuzzLockIn checks the taker's cancel gate and the maker's late-ack flag
// against the contract rule: cancelIntent is accepted only while PENDING and
// from Status.deadline (the lock-in end) on.
func FuzzLockIn(f *testing.F) {
	f.Add(uint8(eth.StatusPending), uint64(1_001_200), uint64(1_001_199))
	f.Add(uint8(eth.StatusPending), uint64(1_001_200), uint64(1_001_200))
	f.Add(uint8(eth.StatusPending), uint64(0), uint64(5))
	f.Add(uint8(eth.StatusReady), uint64(1_001_200), uint64(2_000_000))
	f.Add(uint8(eth.StatusPending), ^uint64(0), ^uint64(0)-1)
	f.Fuzz(func(t *testing.T, stage uint8, deadline, now uint64) {
		st := &eth.State{Stage: stage, Deadline: deadline}
		contractOK := stage == eth.StatusPending && now >= deadline
		if got := cancelAllowed(st, now); got != contractOK {
			t.Fatalf("cancelAllowed(stage %d, deadline %d, now %d) = %v, contract accepts %v", stage, deadline, now, got, contractOK)
		}
		cf := st.CancelFrom()
		if stage != eth.StatusPending && cf != 0 {
			t.Fatalf("CancelFrom %d outside PENDING", cf)
		}
		// A late ack is exactly an ack the taker could front-run with a cancel
		// (on a lock-in deployment).
		if got, want := lateAck(st, now), cf != 0 && contractOK; got != want {
			t.Fatalf("lateAck = %v, want %v (stage %d, deadline %d, now %d)", got, want, stage, deadline, now)
		}
	})
}
