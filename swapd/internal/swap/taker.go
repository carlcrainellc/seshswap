package swap

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

// stepTaker drives the ETH/token side ("Alice").
//
// Deadlines (chain time):
//
//	setReady         any time before t1 once the XMR is confirmed (reveals nothing)
//	cancel intent    from t1-2*margin until t1 if the XMR is not confirmed (reveals nothing),
//	                 never before the lock-in ends (lock time + eth.LockIn; the contract refuses it)
//	cancel refund    once the cancel intent is final, while > margin is left before
//	                 its deadline (the maker is frozen until then)
//	late refund      once a FINAL block is at/after t2 (or after the maker's unrevealed
//	                 claim intent expired): the maker can then never act again
func (e *Engine) stepTaker(ctx context.Context, r *Record, head *eth.Head) error {
	now := head.Timestamp
	var state *eth.State
	if r.Swap != nil && r.Phase != TNegotiated && r.Phase != TLocking && r.Phase != PSweeping {
		var err error
		state, err = e.K.State(ctx, r.SwapID)
		if err != nil {
			return err
		}
		if cf := state.CancelFrom(); cf != 0 {
			r.CancelFrom = cf
		}
		switch state.Stage {
		case eth.StatusClaimed:
			ev, err := e.eventOf(ctx, r, "Claimed")
			if err != nil {
				return err
			}
			if r.ClaimTxHash == "" {
				r.ClaimTxHash = ev.TxHash.Hex()
			}
			if r.TheirSecret == "" {
				sc, err := keys.ScalarFromInt(ev.Secret)
				if err != nil {
					return err
				}
				r.TheirSecret = sc.Hex()
				e.logf(r, "maker claimed; its key share is public, we now hold the full Monero key (claim %s)", r.ClaimTxHash)
			}
			r.Phase = PSweeping
		case eth.StatusRefunded:
			if ev, err := e.eventOf(ctx, r, "Refunded"); err == nil && r.RefundTxHash == "" {
				r.RefundTxHash = ev.TxHash.Hex()
			}
			r.Phase = TRefunded
			e.logf(r, "refund confirmed; maker can recover its XMR (refund %s)", r.RefundTxHash)
			return nil
		case eth.StatusReady:
			if r.Phase == TLocked || r.Phase == TReadying {
				r.Phase = TReady
			}
		}
	}

	switch r.Phase {
	case TNegotiated:
		if r.Pending != nil {
			return nil
		}
		if r.Expires != 0 && time.Now().Unix() >= r.Expires {
			e.logf(r, "quote expired before we locked; nothing was locked")
			r.Phase = PFailed
			return nil
		}
		my, err := keys.ScalarFromHex(r.MySpend)
		if err != nil {
			return err
		}
		their, err := keys.PointFromHex(r.TheirSpend)
		if err != nil {
			return err
		}
		premium := premiumOf(r)
		value := new(big.Int).Add(r.Value, premium)
		if !r.Asset.IsZero() {
			if !r.Approved {
				r.Approved = true // set before sending; a re-approve is harmless
				return e.sendIntentTo(ctx, r, "approve", r.Asset, eth.EncodeApprove(e.K.Addr, r.Value), TNegotiated)
			}
			value = premium
		}
		data := eth.EncodeLock(r.Claimer, their.Int(), my.Public().Int(), r.D1, r.D2, r.Grace, r.Asset, r.Value, premium, r.Salt)
		return e.sendIntent(ctx, r, "lock", e.EthKey, value, data, TLocking)
	case TLocking:
		if r.Pending != nil {
			return nil
		}
		if r.Swap == nil {
			// receipt missing/reverted: look for our lock in the logs before giving up
			evs, err := e.K.LockedEvents(ctx, r.StartBlock, head.Number)
			if err != nil {
				return err
			}
			for i := range evs {
				if evs[i].Swap.Owner == r.Owner && evs[i].Swap.Salt == r.Salt {
					r.Swap, r.SwapID = evs[i].Swap, evs[i].ID
				}
			}
		}
		if r.Swap == nil {
			e.logf(r, "lock transaction failed; nothing was locked")
			r.Phase = PFailed
			return nil
		}
		if err := e.verifyOwnLock(r); err != nil {
			r.LockMismatch = err.Error()
			e.logf(r, "OUR LOCK DOES NOT MATCH THE AGREED SWAP (%v): will never mark it ready, only refund", err)
		}
		e.logf(r, "locked %s (asset %s, premium %s) in swap %s; t1=%d t2=%d grace=%d", r.Value, r.Asset.Hex(), premiumOf(r), r.SwapID.Hex(), r.Swap.T1, r.Swap.T2, r.Swap.Grace)
		r.Phase = TLocked
		if e.OnLocked != nil {
			go e.OnLocked(r.ID, r.MakerURL, r.LockTx)
		}
	case TLocked, TReady, TCancelling:
		if r.Pending != nil {
			return nil
		}
		s := r.Swap
		ft, err := e.finalTime(ctx, r, head)
		if err != nil {
			return err
		}
		switch state.Stage {
		case eth.StatusClaiming:
			// The maker announced its claim; we are frozen until state.Deadline.
			if ft >= state.Deadline {
				e.logf(r, "the maker's claim intent expired without a reveal; refunding")
				return e.refund(ctx, r, now, 0)
			}
			return nil
		case eth.StatusCancelling:
			if now+e.Policy.Margin < state.Deadline {
				final, err := e.eventFinal(ctx, r, head, "CancelIntent")
				if err != nil || !final {
					return err
				}
				if state.PremiumTo != (eth.Address{}) {
					e.logf(r, "cancelling after the maker's ack: the premium (%s) goes to the maker", premiumOf(r))
				}
				return e.refund(ctx, r, now, state.Deadline)
			}
			if ft >= s.T2 {
				return e.refund(ctx, r, now, 0)
			}
			return nil
		}
		// PENDING or READY
		if ft >= s.T2 {
			if r.Phase == TReady {
				e.logf(r, "maker did not claim before t2; refunding")
			}
			return e.refund(ctx, r, now, 0)
		}
		if state.Stage == eth.StatusReady || now >= s.T1 {
			return nil // claimer's window; wait for its intent or for t2 to be final
		}
		cancelDue := now+2*e.Policy.Margin >= s.T1 && cancelAllowed(state, now)
		need, err := e.requiredConfs(ctx, r)
		if err != nil {
			return err
		}
		funds, _, err := e.sharedFunds(ctx, r, need)
		if err != nil {
			if !cancelDue {
				return err
			}
			// Fail safe: without a working Monero link the XMR cannot be
			// confirmed, and waiting past t1 hands the claim window to the
			// maker. Cancel now (refund, minus the premium if acked).
			e.logf(r, "cannot check the shared XMR (%v) and t1 is near; cancelling", err)
			funds = 0
		}
		if funds >= r.XMR && r.LockMismatch == "" {
			e.logf(r, "XMR at shared address confirmed (%d >= %d piconero, %d+ confs); marking ready", funds, r.XMR, need)
			return e.sendIntent(ctx, r, "setReady", e.EthKey, nil, eth.EncodeSetReady(s), TReadying)
		}
		if now+2*e.Policy.Margin >= s.T1 {
			if !cancelAllowed(state, now) {
				return nil // lock-in window: the maker may still lock XMR and ack; cancel once it ends
			}
			e.logf(r, "XMR not confirmed in time (%d/%d); sending a cancel intent (no secret) before t1", funds, r.XMR)
			r.IntentAt = now
			return e.sendIntent(ctx, r, "cancel-intent", e.EthKey, nil, eth.EncodeCancelIntent(s), TCancelling)
		}
	case TReadying:
		if r.Pending == nil && r.Phase == TReadying {
			r.Phase = TLocked // setReady reverted (too late?) — fall back to waiting
		}
	case TRefunding:
		if r.Pending == nil {
			r.Phase = TLocked // reverted or dropped; the stage check above moves us on
		}
	case PSweeping:
		done, err := e.sweep(ctx, r)
		if err != nil {
			return err
		}
		if done {
			e.logf(r, "XMR swept to our wallet; swap complete")
			r.Phase = PDone
		}
	}
	return nil
}

// refund reveals our key share. deadline = the cancel intent's deadline (the
// maker is frozen until then), or 0 when the maker can never act again.
func (e *Engine) refund(ctx context.Context, r *Record, now, deadline uint64) error {
	my, err := keys.ScalarFromHex(r.MySpend)
	if err != nil {
		return err
	}
	return e.sendReveal(ctx, r, "refund", e.EthKey, eth.EncodeRefund(r.Swap, my.Int()), TRefunding, deadline, now)
}

// verifyOwnLock reads our lock back (as decoded from the chain's Locked event) and
// checks every field against what we meant to lock and the maker's announced keys.
func (e *Engine) verifyOwnLock(r *Record) error {
	my, err := keys.ScalarFromHex(r.MySpend)
	if err != nil {
		return err
	}
	their, err := keys.PointFromHex(r.TheirSpend)
	if err != nil {
		return err
	}
	s := r.Swap
	switch {
	case s.ID() != r.SwapID:
		return errors.New("swap id does not hash from the swap fields")
	case s.Owner != r.Owner:
		return errors.New("owner is not us")
	case s.Claimer != r.Claimer:
		return errors.New("claimer is not the maker's announced address")
	case s.ClaimKey.Cmp(their.Int()) != 0:
		return errors.New("claim key is not the maker's verified key share")
	case s.RefundKey.Cmp(my.Public().Int()) != 0:
		return errors.New("refund key is not our key share")
	case s.Asset != r.Asset:
		return errors.New("wrong asset")
	case s.Value.Cmp(r.Value) != 0:
		return errors.New("wrong value")
	case s.Salt != r.Salt:
		return errors.New("wrong salt")
	case s.T2 <= s.T1 || s.T2-s.T1 != r.D2:
		return errors.New("claim window differs from what was agreed")
	case s.Grace != r.Grace:
		return errors.New("grace differs from what was agreed")
	case s.PremiumOrZero().Cmp(premiumOf(r)) != 0:
		return errors.New("premium differs from what was agreed")
	}
	return nil
}

// sendIntentTo is sendIntent for a call to another contract (token approve).
func (e *Engine) sendIntentTo(ctx context.Context, r *Record, kind string, to eth.Address, data []byte, next Phase) error {
	k := *e.K
	k.Addr = to
	tmp := &Engine{Role: e.Role, Store: e.Store, K: &k, Logf: e.Logf, CrashAfterIntent: e.CrashAfterIntent}
	return tmp.sendIntent(ctx, r, kind, e.EthKey, new(big.Int), data, next)
}

// cancelAllowed: the contract accepts cancelIntent only while PENDING and from
// the end of the lock-in window (CancelFrom; 0 on deployments without one).
func cancelAllowed(state *eth.State, now uint64) bool {
	return state.Stage == eth.StatusPending && now >= state.CancelFrom()
}
