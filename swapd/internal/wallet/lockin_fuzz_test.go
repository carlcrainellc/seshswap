package wallet

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

// FuzzPlainCancelLine: the "early cancel is possible from" hint shows exactly
// while the lock-in is still running, and never panics on odd times.
func FuzzPlainCancelLine(f *testing.F) {
	f.Add(uint64(1700001200), int64(1700000000), uint64(1700003600))
	f.Add(uint64(1700001200), int64(1700001200), uint64(1700003600))
	f.Add(^uint64(0), int64(0), ^uint64(0))
	f.Add(uint64(0), int64(-1), uint64(0))
	f.Fuzz(func(t *testing.T, cf uint64, now int64, t1 uint64) {
		r := swap.Record{Role: swap.Taker, Phase: swap.TLocked, Value: big.NewInt(1), Grace: 3600,
			Swap: &eth.Swap{T1: t1, T2: t1 + 3600, Grace: 3600, Value: big.NewInt(1)}, CancelFrom: cf}
		_, line, _, _ := plainStatus(r, time.Unix(now, 0))
		want := cf != 0 && cf <= 1<<62 && now < int64(cf)
		if got := strings.Contains(line, "early cancel is possible from"); got != want && cf <= 1<<62 {
			t.Fatalf("cancelFrom %d now %d: hint shown %v, want %v (%q)", cf, now, got, want, line)
		}
	})
}
