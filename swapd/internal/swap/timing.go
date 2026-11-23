package swap

import (
	"context"
	"errors"
	"time"
)

// Confirmation and timing policy.
//
// Monero confirmations never go below 10: received outputs are spend-locked for
// 10 blocks anyway, and Monero mainnet saw an 18-block reorg (Sep 2025, Qubic)
// and a 10-block one days later. The count scales UP with the amount and with
// the deepest reorg recently observed by our daemon.

const (
	XmrConfFloor   = 10  // hard floor; never configurable lower
	XmrConfMax     = 60  // most a taker may ask for, and the default cap a maker accepts
	XmrSpendLock   = 10  // blocks before a received Monero output can be spent
	XmrBlockSecs   = 120 // Monero target block time
	reorgSafetyAdd = 4   // extra blocks on top of the deepest reorg we have seen
	piconero       = uint64(1_000_000_000_000)

	ContractMinWindow = 1800      // XmrSwapV3.MIN_D1 / MIN_D2 (30 minutes)
	ContractMinGrace  = 1800      // XmrSwapV3.MIN_GRACE
	ContractMaxGrace  = 48 * 3600 // XmrSwapV3.MAX_GRACE
)

// XmrConfs is how many Monero confirmations the ETH side waits for before it
// marks the swap ready, for a swap of xmr piconero, given the deepest recent
// reorg (blocks). Larger swaps are worth more to an attacker who rents hashrate:
// under 1 XMR 10, from 1 XMR 15, from 5 XMR 20, from 20 XMR 30. A taker can
// ask for more (TakeParams.MinConfs); this is the least it ever waits.
func XmrConfs(xmr uint64, reorgDepth uint64) uint64 {
	n := uint64(XmrConfFloor)
	switch {
	case xmr >= 20*piconero:
		n = 30
	case xmr >= 5*piconero:
		n = 20
	case xmr >= 1*piconero:
		n = 15
	}
	return max(n, reorgDepth+reorgSafetyAdd)
}

// Windows picks t1 and t2 lengths (seconds after the lock) for a swap needing
// `confs` Monero confirmations. blockSecs is the observed recent Monero block
// interval (0 = target). The happy path never waits for t1 or t2: they only
// bound how long funds can stay stuck if the other side disappears.
//
//	d1: XMR lock budget + confs blocks at twice the observed block time + 2 margins
//	    (Monero block times are random: 10 blocks take more than 2x their mean
//	    only ~0.5% of the time; a slow chain must not push an honest taker
//	    into cancelling, which after the maker's ack costs it the premium)
//	d2: the claim intent only has to be mined before t2 (the reveal comes
//	    later, inside the intent's own freeze), so 2 margins suffice.
func Windows(confs, blockSecs uint64, p Policy) (d1, d2 uint64) {
	if blockSecs == 0 {
		blockSecs = XmrBlockSecs
	}
	d1 = p.XmrLockBudget + ConfirmSecs(confs, blockSecs) + 2*p.Margin
	d2 = 2 * p.Margin
	return max(d1, ContractMinWindow), max(d2, ContractMinWindow)
}

// ConfirmSecs is the time budget for `confs` Monero blocks: twice the observed
// mean block time (never below the 2-minute target).
func ConfirmSecs(confs, blockSecs uint64) uint64 {
	return confs * max(blockSecs, XmrBlockSecs) * 2
}

// Grace picks the intent freeze: long enough to get the reveal mined after the
// intent is final, even under congestion. Never below policy or the contract floor.
func Grace(p Policy) uint64 {
	return min(max(p.MinGrace, ContractMinGrace, 2*p.Margin), ContractMaxGrace)
}

// SpendableAt returns the Monero height from which the buyer can spend the XMR:
// the shared output unlocks 10 blocks after the lock tx; after the sweep to the
// buyer's own wallet, another 10 blocks.
func SpendableAt(lockHeight, sweepHeight uint64) uint64 {
	if sweepHeight != 0 {
		return sweepHeight + XmrSpendLock
	}
	if lockHeight != 0 {
		return lockHeight + XmrSpendLock
	}
	return 0
}

// reorgDepth asks the optional monerod (Engine.Daemon) for the deepest
// alternative chain it knows of. Cached for 10 minutes; 0 without a daemon.
func (e *Engine) reorgDepth(ctx context.Context) uint64 {
	if e.Daemon == nil {
		return 0
	}
	e.reorgMu.Lock()
	defer e.reorgMu.Unlock()
	if time.Since(e.reorgAt) < 10*time.Minute {
		return e.reorgVal
	}
	d, err := e.Daemon.MaxAltChainDepth(ctx)
	if err != nil {
		return e.reorgVal
	}
	e.reorgVal, e.reorgAt = d, time.Now()
	return d
}

// ErrNoBlockTime is returned when a window is being built or checked and we
// have no measured (or explicitly configured) Monero block time. Assuming the
// 120 s target silently is what made stagenet windows too short.
var ErrNoBlockTime = errors.New("monerod is required to measure block time (set xmrDaemonRpc, or xmrBlockSecs for an explicit mean); refusing to assume 120 s")

// BlockSecs is the recent mean Monero block time: measured by Daemon (cached
// 10 minutes), or Engine.AssumeBlockSecs if no daemon is set. A missing
// daemon and no explicit config is a hard error — we do not assume 120 s.
// The value is never below XmrBlockSecs (120); ConfirmSecs still uses
// 2*max(observed, 120).
func (e *Engine) BlockSecs(ctx context.Context) (uint64, error) {
	if e.Daemon == nil {
		if e.AssumeBlockSecs == 0 {
			return 0, ErrNoBlockTime
		}
		return max(e.AssumeBlockSecs, XmrBlockSecs), nil
	}
	e.reorgMu.Lock()
	defer e.reorgMu.Unlock()
	if time.Since(e.blockAt) < 10*time.Minute && e.blockVal != 0 {
		return e.blockVal, nil
	}
	s, err := e.Daemon.AvgBlockSecs(ctx, 60)
	if err != nil || s == 0 {
		if e.blockVal != 0 {
			return e.blockVal, nil
		}
		if e.AssumeBlockSecs != 0 {
			return max(e.AssumeBlockSecs, XmrBlockSecs), nil
		}
		if err != nil {
			return 0, err
		}
		return 0, ErrNoBlockTime
	}
	e.blockVal, e.blockAt = max(s, XmrBlockSecs), time.Now()
	return e.blockVal, nil
}

// ReorgDepth is the exported form for the API and metrics.
func (e *Engine) ReorgDepth(ctx context.Context) uint64 { return e.reorgDepth(ctx) }

// requiredConfs is the taker's confirmation requirement for this swap: what was
// agreed, but never below the floor/tier for the amount and current reorg depth.
func (e *Engine) requiredConfs(ctx context.Context, r *Record) (uint64, error) {
	return max(r.MinConfs, XmrConfs(r.XMR, e.reorgDepth(ctx))), nil
}
