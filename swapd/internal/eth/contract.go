package eth

import (
	"context"
	"errors"
	"math/big"
)

// Stage values of XmrSwapV3.Stage.
const (
	StatusNone uint8 = iota
	StatusPending
	StatusReady
	StatusClaimed
	StatusRefunded
	StatusClaiming   // claim intent made; the owner is frozen until Deadline
	StatusCancelling // cancel intent made; the claimer is frozen until Deadline
)

// LockIn is XmrSwapV3.LOCK_IN (seconds): after lock() the owner can't send
// cancelIntent for this long, so the claimer can lock XMR and ack first.
const LockIn = 20 * 60

// State is XmrSwapV3.status(id).
type State struct {
	Stage     uint8
	Deadline  uint64 // CLAIMING/CANCELLING: end of the freeze; PENDING/READY: end of the lock-in
	PremiumTo Address
}

// CancelFrom is the first chain time at which the owner may send
// cancelIntent (the end of the lock-in window). 0 if not PENDING, or on a
// deployment without a lock-in.
func (s *State) CancelFrom() uint64 {
	if s.Stage != StatusPending {
		return 0
	}
	return s.Deadline
}

// Contract wraps an XmrSwapV3 deployment.
type Contract struct {
	C       *Client
	Addr    Address
	ChainID *big.Int
}

func (k *Contract) State(ctx context.Context, id Hash) (*State, error) {
	out, err := k.C.EthCall(ctx, CallMsg{To: &k.Addr, Data: append(append([]byte{}, selStatus...), id[:]...)})
	if err != nil {
		return nil, err
	}
	if len(out) != 96 {
		return nil, errors.New("bad status return")
	}
	st := &State{Stage: out[31], Deadline: new(big.Int).SetBytes(out[32:64]).Uint64()}
	copy(st.PremiumTo[:], out[76:96])
	return st, nil
}

// Status returns only the stage.
func (k *Contract) Status(ctx context.Context, id Hash) (uint8, error) {
	st, err := k.State(ctx, id)
	if err != nil {
		return 0, err
	}
	return st.Stage, nil
}

func (k *Contract) Owed(ctx context.Context, asset, who Address) (*big.Int, error) {
	out, err := k.C.EthCall(ctx, CallMsg{To: &k.Addr, Data: append(append(append([]byte{}, selOwed...), wAddr(asset)...), wAddr(who)...)})
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

func TokenBalance(ctx context.Context, c *Client, token, who Address) (*big.Int, error) {
	out, err := c.EthCall(ctx, CallMsg{To: &token, Data: append(append([]byte{}, selBalanceOf...), wAddr(who)...)})
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(out), nil
}

// Event is a decoded XmrSwapV3 event for one swap.
type Event struct {
	Kind     string // Locked, Ready, Claimed, Refunded, ClaimIntent, CancelIntent, Acked, PremiumPaid
	Deadline uint64 // ClaimIntent / CancelIntent
	ID       Hash
	Swap     *Swap    // Locked
	Secret   *big.Int // Claimed / Refunded
	To       Address  // Claimed
	Relayer  Address  // Claimed
	Fee      *big.Int // Claimed
	Amount   *big.Int // PremiumPaid
	Block    uint64
	TxHash   Hash
}

// Events returns all events for swap id in [from, to].
func (k *Contract) Events(ctx context.Context, id Hash, from, to uint64) ([]Event, error) {
	logs, err := k.C.Logs(ctx, k.Addr, from, to, nil, id.Hex())
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, l := range logs {
		if l.Removed || len(l.Topics) < 2 || l.Topics[1] != id {
			continue
		}
		e := Event{ID: id, Block: l.Block(), TxHash: l.TxHash}
		d := l.Data()
		switch Hash(l.Topics[0]) {
		case Hash(TopicLocked):
			s, err := decodeSwap(d)
			if err != nil {
				return nil, err
			}
			if s.ID() != id {
				return nil, errors.New("Locked event params do not hash to its id")
			}
			e.Kind, e.Swap = "Locked", s
		case Hash(TopicReady):
			e.Kind = "Ready"
		case Hash(TopicClaimed):
			if len(d) < 128 {
				return nil, errors.New("short Claimed data")
			}
			e.Kind = "Claimed"
			e.Secret = new(big.Int).SetBytes(d[:32])
			copy(e.To[:], d[44:64])
			copy(e.Relayer[:], d[76:96])
			e.Fee = new(big.Int).SetBytes(d[96:128])
		case Hash(TopicClaimIntent), Hash(TopicCancelIntent):
			if len(d) < 32 {
				return nil, errors.New("short intent data")
			}
			e.Kind = "ClaimIntent"
			if Hash(l.Topics[0]) == Hash(TopicCancelIntent) {
				e.Kind = "CancelIntent"
			}
			e.Deadline = new(big.Int).SetBytes(d[:32]).Uint64()
		case Hash(TopicAcked):
			if len(d) < 32 {
				return nil, errors.New("short Acked data")
			}
			e.Kind = "Acked"
			copy(e.To[:], d[12:32])
		case Hash(TopicRefunded):
			if len(d) < 32 {
				return nil, errors.New("short Refunded data")
			}
			e.Kind = "Refunded"
			e.Secret = new(big.Int).SetBytes(d[:32])
		case Hash(TopicPremiumPaid):
			if len(d) < 64 {
				return nil, errors.New("short PremiumPaid data")
			}
			e.Kind = "PremiumPaid"
			copy(e.To[:], d[12:32])
			e.Amount = new(big.Int).SetBytes(d[32:64])
		default:
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// LockedEvents returns all Locked events in [from, to] (used by makers that
// were not told the lock transaction).
func (k *Contract) LockedEvents(ctx context.Context, from, to uint64) ([]Event, error) {
	logs, err := k.C.Logs(ctx, k.Addr, from, to, Hash(TopicLocked).Hex())
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, l := range logs {
		if l.Removed || len(l.Topics) != 2 {
			continue
		}
		s, err := decodeSwap(l.Data())
		if err != nil || s.ID() != l.Topics[1] {
			continue
		}
		out = append(out, Event{Kind: "Locked", ID: l.Topics[1], Swap: s, Block: l.Block(), TxHash: l.TxHash})
	}
	return out, nil
}
