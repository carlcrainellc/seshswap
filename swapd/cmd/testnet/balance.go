package main

import (
	"math/big"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// ethDeltas is the expected ETH change (gas excluded) of the taker, of the
// maker's payout address, and of the relayer from one swap's contract events.
// It is derived from the events only: after settlement the contract zeroes the
// swap's status, so reading premiumTo from State() afterwards returns 0 (the
// bug that made the first Sepolia report miss the forfeited premium).
func ethDeltas(evs []eth.Event, taker, payout, relayer eth.Address) (dTaker, dPayout, dRelayer *big.Int) {
	dTaker, dPayout, dRelayer = new(big.Int), new(big.Int), new(big.Int)
	value := new(big.Int)
	add := func(a eth.Address, x *big.Int) {
		if x == nil {
			return
		}
		if a == taker {
			dTaker.Add(dTaker, x)
		}
		if a == payout {
			dPayout.Add(dPayout, x)
		}
		if a == relayer {
			dRelayer.Add(dRelayer, x)
		}
	}
	for _, ev := range evs {
		switch ev.Kind {
		case "Locked":
			if ev.Swap != nil {
				value.Set(ev.Swap.Value)
				out := new(big.Int).Add(ev.Swap.Value, ev.Swap.PremiumOrZero())
				add(ev.Swap.Owner, new(big.Int).Neg(out))
			}
		case "Claimed":
			net := new(big.Int).Set(value)
			if ev.Fee != nil {
				net.Sub(net, ev.Fee)
				add(ev.Relayer, ev.Fee)
			}
			add(ev.To, net)
		case "Refunded":
			add(taker, value) // refunds always go to the owner (the taker here)
		case "PremiumPaid":
			add(ev.To, ev.Amount)
		}
	}
	return dTaker, dPayout, dRelayer
}

// relayerEthWant is the expected relayer ETH balance at the end of a run:
// start + fees received on Claimed events − gas the relayer paid.
func relayerEthWant(start, feesReceived, gasPaid *big.Int) *big.Int {
	w := new(big.Int).Add(start, feesReceived)
	return w.Sub(w, gasPaid)
}

// releaseNow decides when the delayed-reveal scenario lifts its censorship of
// the reveal: 5 min after t2 (so the taker's own refund time has passed while
// it is frozen), but never later than 30 min + 2 margins before the claim
// intent's deadline. The first Sepolia run released at t2 + 300 with a 2 h
// grace whose deadline fell 3 min before that, so the maker (correctly)
// declined to reveal and the taker refunded.
func releaseNow(now, t2, deadline, margin uint64) bool {
	latest := deadline - min(deadline, 1800+2*margin)
	return now >= min(t2+300, latest) && now > t2
}
