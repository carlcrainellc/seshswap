package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

// MakerRegistry makes a maker's swaps count against its SeshRegistry
// listing: it refuses swaps over the listing's bond cap, signs a commitment
// for every swap it agrees, and acks the swap on-chain once the XMR lock is
// broadcast (RunAcker). The listing key signs; it also pays the ack gas.
type MakerRegistry struct {
	Client       *registry.Client
	ChainID      *big.Int
	ListingID    uint64
	Key          *eth.Key
	AttestWindow uint64 // seconds; 0 = the contract's minimum
	Log          func(string, ...any)
}

func (r *MakerRegistry) logf(f string, a ...any) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}

// check refuses a swap the listing can't back right now.
func (r *MakerRegistry) check(ctx context.Context, asset eth.Address, value *big.Int) (*big.Int, uint64, error) {
	info, err := r.Client.Info(ctx, r.ListingID)
	if err != nil {
		return nil, 0, fmt.Errorf("registry: %w", err)
	}
	if info.Owner != r.Key.Address() {
		return nil, 0, errors.New("registry: this maker's key does not own the configured listing")
	}
	if info.Status != registry.StatusActive || info.MaxQuote.Sign() == 0 {
		return nil, 0, errors.New("registry: this maker's listing is not active or its bond is below the minimum")
	}
	ev, err := r.Client.EthValue(ctx, asset, value)
	if err != nil {
		return nil, 0, fmt.Errorf("registry: %w", err)
	}
	if ev.Cmp(info.MaxQuote) > 0 {
		return nil, 0, fmt.Errorf("amount is over this maker's bond cap (%s ETH)", registry.FormatWei(info.MaxQuote))
	}
	pen, err := r.Client.RequiredPenalty(ctx, asset, value)
	if err != nil {
		return nil, 0, fmt.Errorf("registry: %w", err)
	}
	free := new(big.Int).Sub(info.Bond, info.Reserved)
	if free.Cmp(pen) < 0 {
		return nil, 0, errors.New("registry: this maker's free bond is too small for another swap right now")
	}
	aw := r.AttestWindow
	if aw == 0 {
		p, err := r.Client.Params(ctx)
		if err != nil {
			return nil, 0, err
		}
		aw = p.MinAttest
	}
	return pen, aw, nil
}

// sign builds and signs the commitment for a swap the maker just agreed.
func (r *MakerRegistry) sign(ctx context.Context, rec *swap.Record, claimKey *big.Int) (*registry.SignedCommitment, error) {
	pen, aw, err := r.check(ctx, rec.Asset, rec.Value)
	if err != nil {
		return nil, err
	}
	sc := &registry.SignedCommitment{Registry: r.Client.Addr, Commitment: registry.Commitment{
		ListingID: r.ListingID, Taker: rec.Owner, Claimer: rec.Claimer, ClaimKey: claimKey,
		Asset: rec.Asset, Value: rec.Value, XMRAtomic: rec.XMR, Penalty: pen, AttestWindow: aw,
		// the taker locks before rec.Expires and records this right after
		Expiry: uint64(rec.Expires) + 600,
	}}
	sc.Sig = registry.SignCommitment(r.Key, r.ChainID, r.Client.Addr, &sc.Commitment)
	return sc, nil
}

// RunAcker acks every swap of ours that the taker recorded in the registry,
// as soon as our XMR lock is out. Anyone could submit the same signed ack;
// here the maker's own daemon is the watchtower.
func (r *MakerRegistry) RunAcker(ctx context.Context, e *swap.Engine, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, id := range e.IDs() {
			rec, ok := e.Snapshot(id)
			if !ok || rec.Registry == nil || rec.Role != swap.Maker ||
				rec.Registry.Acked || rec.Registry.Released || rec.Registry.Slashed {
				continue
			}
			switch rec.Phase {
			case swap.MXmrLocked, swap.MIntent, swap.MClaiming, swap.MClaimed, swap.MWaitRefund, swap.PSweeping, swap.PDone:
			default:
				continue
			}
			if rec.SwapID == (eth.Hash{}) {
				continue
			}
			r.ackOne(ctx, e, &rec)
		}
	}
}

func (r *MakerRegistry) ackOne(ctx context.Context, e *swap.Engine, rec *swap.Record) {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cs, err := r.Client.Commit(cctx, rec.SwapID)
	if err != nil {
		return
	}
	mark := func(f func(l *swap.RegLink)) {
		_ = e.Update(rec.ID, func(x *swap.Record) error {
			if x.Registry != nil {
				f(x.Registry)
			}
			return nil
		})
	}
	switch cs.Stage {
	case registry.CommitNone:
		return // the taker has not recorded it (yet); nothing to ack
	case registry.CommitOpen:
	default:
		mark(func(l *swap.RegLink) {
			l.Acked = cs.Stage == registry.CommitAcked
			l.Released = cs.Stage == registry.CommitReleased
			l.Slashed = cs.Stage == registry.CommitSlashed
			l.Committed = true
		})
		if cs.Stage != registry.CommitAcked {
			mark(func(l *swap.RegLink) { l.Acked = true }) // nothing more to do either way
		}
		return
	}
	sig := registry.SignAck(r.Key, r.ChainID, r.Client.Addr, rec.SwapID, rec.Registry.ListingID)
	a := r.Client.Addr
	rc, err := r.Client.C.SendAndWait(cctx, r.Key, &a, nil, registry.EncodeAck(rec.SwapID, sig))
	if err != nil {
		r.logf("registry ack for swap %s: %v", rec.ID, err)
		mark(func(l *swap.RegLink) { l.Tries++; l.LastError = err.Error() })
		return
	}
	r.logf("registry: acked swap %s (listing %d) in tx %s", rec.ID, rec.Registry.ListingID, rc.TxHash.Hex())
	mark(func(l *swap.RegLink) {
		l.Committed = true
		l.Acked = true
		l.AckTx = rc.TxHash
		l.AttestBy = cs.AttestBy
	})
}

// attach is called by newSwap after the record is saved.
func (m *Maker) attachRegistry(ctx context.Context, rec *swap.Record, mySpend keys.Scalar, resp *SwapResponse) error {
	if m.Registry == nil {
		return nil
	}
	sc, err := m.Registry.sign(ctx, rec, mySpend.Public().Int())
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(sc)
	if err := m.Engine.Update(rec.ID, func(x *swap.Record) error {
		x.Registry = &swap.RegLink{Registry: sc.Registry, ListingID: sc.Commitment.ListingID, Signed: raw}
		return nil
	}); err != nil {
		return err
	}
	resp.Registry = sc
	return nil
}

// VerifyCommitment is the taker's check of a maker's signed commitment
// against what it is about to lock. wantOwner is the listing owner read from
// the chain (not from the maker).
func VerifyCommitment(sc *registry.SignedCommitment, chainID *big.Int, wantRegistry, wantOwner eth.Address, wantListing uint64,
	taker, claimer eth.Address, claimKey *big.Int, asset eth.Address, value *big.Int) error {
	c := &sc.Commitment
	switch {
	case sc.Registry != wantRegistry:
		return errors.New("maker's commitment is for a different registry")
	case c.ListingID != wantListing:
		return errors.New("maker's commitment is for a different listing")
	case c.Taker != taker || c.Claimer != claimer || c.Asset != asset:
		return errors.New("maker's commitment doesn't match this swap")
	case c.ClaimKey == nil || c.ClaimKey.Cmp(claimKey) != 0:
		return errors.New("maker's commitment names a different Monero key")
	case c.Value == nil || c.Value.Cmp(value) != 0:
		return errors.New("maker's commitment names a different amount")
	case c.Penalty == nil || c.Penalty.Sign() <= 0:
		return errors.New("maker's commitment has no penalty")
	}
	signer, err := sc.Signer(chainID)
	if err != nil {
		return err
	}
	if signer != wantOwner {
		return errors.New("maker's commitment is not signed by the listing's owner")
	}
	return nil
}
