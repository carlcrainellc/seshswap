package swap

import (
	"context"
	"errors"
	"fmt"
	"github.com/carlcrainellc/seshswap/swapd/internal/exitpay"
	"math/big"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/relay"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// stepMaker drives the XMR side ("Bob").
//
//	lock XMR        only after the taker's ETH/token lock is confirmed and valid,
//	                and only if >= XmrLockBudget remains before t1
//	ack             after the XMR lock is relayed, if the swap carries a premium
//	claim intent    when Ready, or from t1 on, any time before t2 (carries no secret)
//	claim (reveal)  only once the intent is final, and only while > margin remains
//	                before the intent's deadline (the taker is frozen until then)
//	otherwise       never reveal the secret; wait for the taker's refund and sweep
func (e *Engine) stepMaker(ctx context.Context, r *Record, head *eth.Head) error {
	now := head.Timestamp
	var st uint8
	var state *eth.State
	if r.Swap != nil {
		var err error
		if state, err = e.K.State(ctx, r.SwapID); err != nil {
			return err
		}
		st = state.Stage
		if st == eth.StatusRefunded && r.Phase != PSweeping {
			if r.XmrLock == nil {
				e.logf(r, "taker refunded before we locked XMR; nothing to recover")
				r.Phase = MAborted
				return nil
			}
			sec, err := e.secretFromEvents(ctx, r, "Refunded")
			if err != nil {
				return err
			}
			sc, err := keys.ScalarFromInt(sec)
			if err != nil {
				return err
			}
			r.TheirSecret = sc.Hex()
			e.logf(r, "taker refunded; its key share is public, recovering our XMR")
			r.Phase = PSweeping
		}
		if st == eth.StatusClaimed && r.Phase != MClaimed {
			e.logf(r, "claim confirmed on-chain; swap complete for maker")
			r.Phase = MClaimed
			e.settleExitPay(ctx, r)
			return nil
		}
	}

	switch r.Phase {
	case MNegotiated:
		ev, err := e.findLock(ctx, r, head)
		if err != nil {
			return err
		}
		if ev == nil {
			if time.Now().Unix() > r.Expires {
				e.logf(r, "offer expired without a valid lock; aborting (nothing was locked by us)")
				r.Phase = MAborted
			}
			return nil
		}
		if head.Number+1 < ev.Block+e.ethConfs(r) {
			return nil
		}
		if err := e.validateLock(r, ev.Swap, ev.ID, now); err != nil {
			e.logf(r, "rejecting lock %s: %v", ev.ID.Hex(), err)
			r.Phase = MAborted
			return nil
		}
		r.Swap, r.SwapID = ev.Swap, ev.ID
		e.logf(r, "taker locked %s in swap %s (t1=%d t2=%d)", ev.Swap.Value, ev.ID.Hex(), ev.Swap.T1, ev.Swap.T2)
		r.Phase = MEthLocked
	case MEthLocked:
		if r.XmrLock != nil { // prepared before a crash: never prepare a second one
			r.Phase = MXmrLocking
			return nil
		}
		if st != eth.StatusPending {
			return nil
		}
		if now >= e.xmrLockDeadline(r) {
			e.logf(r, "too close to t1 to lock XMR safely; not locking (taker will refund)")
			r.Phase = MAborted
			return nil
		}
		err := e.Wallet.Session(ctx, e.MainWal, func(s *xmr.Sess) error {
			if err := s.Refresh(ctx); err != nil {
				return err
			}
			p, err := s.PrepareTransfer(ctx, r.SharedAddr, r.XMR)
			if err != nil {
				return err
			}
			r.XmrLock = p
			r.Phase = MXmrLocking
			if err := e.Store.Put(r.name(), r); err != nil { // intent before relay
				r.XmrLock, r.Phase = nil, MEthLocked
				return err
			}
			if e.CrashAfterIntent != nil && e.CrashAfterIntent("xmr-lock") {
				return errCrashed
			}
			return s.Relay(ctx, p)
		})
		if err != nil {
			return err
		}
		e.logf(r, "locked %d piconero at shared address (tx %s)", r.XMR, r.XmrLock.TxHash)
		r.Phase = MXmrLocking
	case MXmrLocking:
		var confs int64
		var height uint64
		err := e.Wallet.Session(ctx, e.MainWal, func(s *xmr.Sess) error {
			_ = s.Refresh(ctx)
			c, err := s.TxConfirmations(ctx, r.XmrLock.TxHash)
			confs = c
			if c < 0 { // wallet doesn't know it: relay the stored transaction again
				return s.Relay(ctx, r.XmrLock)
			}
			if c > 0 {
				if h, err := s.Height(ctx); err == nil && h >= uint64(c) {
					height = h - uint64(c)
				}
			}
			return err
		})
		if err != nil {
			return err
		}
		if confs >= 0 {
			r.XmrLockHeight = height
			r.Phase = MXmrLocked
		}
	case MXmrLocked, MIntent, MClaiming:
		if r.Pending != nil {
			return nil
		}
		// "My XMR is locked": from now on a cancel by the taker costs it the premium.
		if r.Swap.PremiumOrZero().Sign() > 0 && !r.AckSent && st == eth.StatusPending && now < r.Swap.T1 {
			return e.ack(ctx, r)
		}
		if st != eth.StatusClaiming {
			if r.Phase == MClaiming {
				r.Phase = MIntent // the intent was reorged out; start over from the intent
			}
			if now >= r.Swap.T2 {
				e.logf(r, "claim window over without a claim intent; waiting for the taker's refund to recover XMR")
				r.Phase = MWaitRefund
				return nil
			}
			if !claimerWindow(r.Swap, state, now) {
				return nil
			}
			if r.Phase == MIntent && now < r.IntentAt+e.Policy.RelayTimeout {
				return nil // a relayed intent may still land
			}
			return e.claimIntent(ctx, r, now)
		}
		// CLAIMING: the taker cannot refund until state.Deadline.
		if r.Phase == MXmrLocked {
			r.Phase = MIntent
		}
		final, err := e.eventFinal(ctx, r, head, "ClaimIntent")
		if err != nil || !final {
			return err
		}
		if now+e.Policy.Margin >= state.Deadline {
			e.logf(r, "less than the safety margin left before the intent deadline; not revealing (waiting for the taker's refund)")
			r.Phase = MWaitRefund
			return nil
		}
		if r.Phase == MClaiming && now < r.RelaySentAt+e.Policy.RelayTimeout {
			return nil
		}
		return e.claim(ctx, r, now, state.Deadline)
	case MWaitRefund:
		// nothing to do; the status check above moves us to sweeping or claimed
	case PSweeping:
		done, err := e.sweep(ctx, r)
		if err != nil {
			return err
		}
		if done {
			e.logf(r, "XMR recovered to our wallet")
			r.Phase = PDone
		}
	}
	return nil
}

func (e *Engine) findLock(ctx context.Context, r *Record, head *eth.Head) (*eth.Event, error) {
	if r.LockHint != (eth.Hash{}) {
		rc, err := e.K.C.Receipt(ctx, r.LockHint)
		if err != nil || rc == nil || !rc.OK() {
			return nil, err
		}
		for _, l := range rc.Logs {
			if l.Address == e.K.Addr && len(l.Topics) == 2 && l.Topics[0] == eth.Hash(eth.TopicLocked) {
				evs, err := e.K.Events(ctx, l.Topics[1], rc.Block(), rc.Block())
				if err != nil {
					return nil, err
				}
				for i := range evs {
					if evs[i].Kind == "Locked" && evs[i].Swap.Claimer == r.Claimer {
						return &evs[i], nil
					}
				}
			}
		}
		return nil, nil
	}
	evs, err := e.K.LockedEvents(ctx, r.StartBlock, head.Number)
	if err != nil {
		return nil, err
	}
	for i := range evs {
		if evs[i].Swap.Claimer == r.Claimer {
			return &evs[i], nil
		}
	}
	return nil, nil
}

// validateLock checks the taker locked exactly what was agreed.
// It works on the swap as decoded from the chain (receipt log or log scan of OUR
// contract), never on anything the taker sent us, and runs before any XMR is locked.
func (e *Engine) validateLock(r *Record, s *eth.Swap, id eth.Hash, now uint64) error {
	my, err := keys.ScalarFromHex(r.MySpend)
	if err != nil {
		return err
	}
	their, err := keys.PointFromHex(r.TheirSpend)
	if err != nil {
		return err
	}
	switch {
	case s.ID() != id:
		return errors.New("swap id does not hash from the swap fields")
	case s.Claimer != r.Claimer:
		return errors.New("wrong claimer")
	case s.ClaimKey.Cmp(my.Public().Int()) != 0:
		return errors.New("claim key is not our key share")
	case s.RefundKey.Cmp(their.Int()) != 0:
		return errors.New("refund key is not the taker's announced key share")
	case s.Asset != r.Asset:
		return errors.New("wrong asset")
	case s.Value.Cmp(r.Value) != 0:
		return errors.New("value is not the agreed amount")
	case r.Owner.IsZero() || s.Owner != r.Owner:
		return errors.New("owner is not the taker's announced address")
	case s.T2 <= s.T1:
		return errors.New("t2 not after t1")
	case now >= e.xmrLockDeadlineFor(r, s.T1):
		return errors.New("t1 too close to lock XMR and let the taker confirm it")
	case s.T2-s.T1 < r.D2:
		return errors.New("claim window shorter than agreed")
	case s.Grace != r.Grace || s.Grace < e.Policy.MinGrace:
		return errors.New("intent freeze (grace) differs from what was agreed")
	case s.PremiumOrZero().Cmp(premiumOf(r)) != 0:
		return errors.New("premium differs from what was agreed")
	case s.T1 > now+r.D1+600:
		return errors.New("t1 later than agreed (would keep our XMR waiting)")
	}
	return nil
}

func premiumOf(r *Record) *big.Int {
	if r.Premium == nil {
		return new(big.Int)
	}
	return r.Premium
}

// claimerWindow mirrors XmrSwapV3.claimerWindow: may the claimer send a claim intent now?
func claimerWindow(s *eth.Swap, st *eth.State, now uint64) bool {
	if now >= s.T2 {
		return false
	}
	switch st.Stage {
	case eth.StatusReady:
		return true
	case eth.StatusPending:
		return now >= s.T1
	case eth.StatusCancelling:
		return now >= s.T1 && now >= st.Deadline
	}
	return false
}

// claimIntent sends step 1 of the claim (no secret): via a relayer, else our gas key.
func (e *Engine) claimIntent(ctx context.Context, r *Record, now uint64) error {
	ck, err := eth.KeyFromHex(r.ClaimerKey)
	if err != nil {
		return err
	}
	sig := eth.SignIntent(ck, e.K.ChainID, e.K.Addr, r.SwapID)
	r.IntentAt = now
	if len(e.Relayers) > 0 && (r.IntentTries < 2*len(e.Relayers) || e.EthKey == nil) {
		url := e.Relayers[r.IntentTries%len(e.Relayers)]
		r.IntentTries++
		h, err := relay.PostIntent(ctx, url, &relay.Intent{Kind: "claimIntent", Swap: r.Swap, Sig: sig})
		r.Phase = MIntent
		if err != nil {
			e.logf(r, "relayer %s refused the claim intent: %v", url, err)
			r.IntentAt = 0
			return nil
		}
		e.logf(r, "claim intent (no secret) submitted via relayer %s: %s", url, h.Hex())
		return nil
	}
	if e.EthKey == nil {
		return errors.New("no relayer accepted the claim intent and no gas key is configured")
	}
	return e.sendIntent(ctx, r, "claim-intent", e.EthKey, nil, eth.EncodeClaimIntent(r.Swap, sig), MIntent)
}

// ack tells the contract our XMR is locked (premium goes to PayoutTo if the taker then cancels).
func (e *Engine) ack(ctx context.Context, r *Record) error {
	ck, err := eth.KeyFromHex(r.ClaimerKey)
	if err != nil {
		return err
	}
	to := r.PayoutTo
	if r.ExitPay != nil { // a premium is not exit money
		to = r.ExitPay.RefundTo
	}
	sig := eth.SignAck(ck, e.K.ChainID, e.K.Addr, r.SwapID, to)
	r.AckSent = true // best effort, once: a lost ack only means no premium
	for _, url := range e.Relayers {
		h, err := relay.PostIntent(ctx, url, &relay.Intent{Kind: "ack", Swap: r.Swap, To: to, Sig: sig})
		if err == nil {
			e.logf(r, "acknowledged our XMR lock on-chain via relayer: %s", h.Hex())
			return nil
		}
		e.logf(r, "relayer %s refused the ack: %v", url, err)
	}
	if e.EthKey == nil {
		e.logf(r, "no relayer took the ack and no gas key: the premium will not be claimable")
		return nil
	}
	return e.sendIntent(ctx, r, "ack", e.EthKey, nil, eth.EncodeAckLocked(r.Swap, to, sig), r.Phase)
}

func (e *Engine) claim(ctx context.Context, r *Record, now, deadline uint64) error {
	ck, err := eth.KeyFromHex(r.ClaimerKey)
	if err != nil {
		return err
	}
	my, err := keys.ScalarFromHex(r.MySpend)
	if err != nil {
		return err
	}
	fee := e.Policy.FeeFor(r.Swap.Asset)
	// A fee at or above the locked value would revert on-chain (BadParams).
	// Skip the relayer and self-relay with fee 0 instead of signing a doomed claim.
	feeOK := r.Swap.Value == nil || fee.Cmp(r.Swap.Value) < 0
	if !feeOK {
		e.logf(r, "relay fee %s is not below locked value %s; self-relaying with fee 0", fee, r.Swap.Value)
		fee = big.NewInt(0)
	}
	if feeOK && len(e.Relayers) > 0 && (r.RelayTries < 2*len(e.Relayers) || e.EthKey == nil) {
		url := e.Relayers[r.RelayTries%len(e.Relayers)]
		r.RelayTries++
		c := eth.SignClaim(ck, e.K.ChainID, e.K.Addr, r.Swap, my.Int(), r.PayoutTo, eth.Address{}, fee)
		h, err := relay.Post(ctx, url, c)
		r.RelaySentAt = now
		r.Phase = MClaiming
		if err != nil {
			e.logf(r, "relayer %s refused: %v", url, err)
			r.RelaySentAt = 0 // try the next one on the next step (then self-relay)
			return nil
		}
		r.ClaimTxHash = h.Hex()
		e.logf(r, "claim submitted via relayer %s: %s (fee %s)", url, h.Hex(), fee)
		return nil
	}
	if e.EthKey == nil {
		return errors.New("no relayer accepted the claim and no gas key is configured")
	}
	// self-relay with our own gas key: same signed claim, fee 0 to ourselves
	c := eth.SignClaim(ck, e.K.ChainID, e.K.Addr, r.Swap, my.Int(), r.PayoutTo, e.EthKey.Address(), big.NewInt(0))
	r.RelaySentAt = now
	return e.sendReveal(ctx, r, "claim", e.EthKey, eth.EncodeClaimRelayed(c), MClaiming, deadline, now)
}

// xmrLockDeadline is the last chain time at which the maker may still lock XMR:
// the taker must be able to see MinConfs confirmations (at twice the block time
// the windows were planned with) before its cancel point t1 - 2*margin. See Windows.
func (e *Engine) xmrLockDeadline(r *Record) uint64 { return e.xmrLockDeadlineFor(r, r.Swap.T1) }

func (e *Engine) xmrLockDeadlineFor(r *Record, t1 uint64) uint64 {
	need := 2*e.Policy.Margin + ConfirmSecs(r.MinConfs, r.BlockSecs)
	if t1 < need {
		return 0
	}
	return t1 - need
}

// settleExitPay deploys the swap's ExitPay forwarder, which buys the lease
// with what the claim paid in (or refunds it). Best effort: the money waits at
// the forwarder address and anyone can settle it later.
func (e *Engine) settleExitPay(ctx context.Context, r *Record) {
	t := r.ExitPay
	if t == nil || r.ExitPayDone {
		return
	}
	if done, err := exitpay.Done(ctx, e.K.C, t.Address); err == nil && done {
		r.ExitPayDone = true
		e.logf(r, "exit time bought in the claim: %d h for %s", t.Hours, t.BuyerSesh)
		return
	}
	if e.EthKey == nil {
		e.logf(r, "exit pay: no gas key; settle the forwarder %s with `swapd exits pay-settle`", t.Address.Hex())
		return
	}
	data := exitpay.EncodeDeploy(t)
	if ok, err := exitpay.Deployed(ctx, e.K.C, t.Address); err == nil && ok {
		data = exitpay.EncodeSettle(t)
	}
	rc, err := e.K.C.SendAndWait(ctx, e.EthKey, &t.Factory, nil, data)
	if err != nil || !rc.OK() {
		e.logf(r, "exit pay: settle failed (%v); funds wait at %s", err, t.Address.Hex())
		return
	}
	done, _ := exitpay.Done(ctx, e.K.C, t.Address)
	r.ExitPayDone = true
	if done {
		e.logf(r, "exit time bought: %d h for %s (tx %s)", t.Hours, t.BuyerSesh, rc.TxHash.Hex())
	} else {
		e.logf(r, "exit pay: lease could not be bought; proceeds sent to %s (tx %s)", t.RefundTo.Hex(), rc.TxHash.Hex())
	}
}

// SettleExitPay retries the ExitPay settlement of a claimed swap.
func (e *Engine) SettleExitPay(ctx context.Context, id string) error {
	return e.Update(id, func(r *Record) error {
		if r.ExitPay == nil {
			return errors.New("this swap does not pay for exit time")
		}
		if r.Phase != MClaimed {
			return fmt.Errorf("swap is %s, not claimed", r.Phase)
		}
		e.settleExitPay(ctx, r)
		return nil
	})
}
