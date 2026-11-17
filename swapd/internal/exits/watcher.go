package exits

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Watcher is the seller's admission desk: it reads ExitLease Leased events
// for one listing straight from the chain (read-only RPC, no keys that move
// funds) and, once a purchase is Confirmations blocks deep, writes a signed
// admission for the buyer's .sesh until the lease's end time. The exit router
// then accepts that buyer's session until then and drops it at expiry.
//
// The chain is the only input: there is no claim desk, no quote and no
// message from the buyer. A lease is final once mined; nothing renews.
type Watcher struct {
	K             *Contract
	ListingID     uint64
	ExitSesh      string             // this exit's own .sesh (lock_seller_id)
	Payout        eth.Address        // optional: only admit leases paid to this address
	Key           ed25519.PrivateKey // admission issuer key
	Store         *Store
	StatePath     string // remembers the last scanned block; "" = memory only
	FromBlock     uint64 // ExitLease deployment block (first scan)
	Confirmations uint64 // blocks before a purchase counts (0 = 20, about 5 s on Arbitrum)
	MaxRange      uint64 // eth_getLogs block range per call (0 = 50000)
	Log           func(string, ...any)
	Now           func() time.Time

	last uint64
}

type watcherState struct {
	Contract  eth.Address `json:"contract"`
	ListingID uint64      `json:"listingId"`
	LastBlock uint64      `json:"lastBlock"`
}

func (w *Watcher) logf(f string, a ...any) {
	if w.Log != nil {
		w.Log(f, a...)
	}
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) load() {
	if w.last != 0 || w.StatePath == "" {
		return
	}
	b, err := os.ReadFile(w.StatePath)
	if err != nil {
		return
	}
	var st watcherState
	if json.Unmarshal(b, &st) == nil && st.Contract == w.K.Addr && st.ListingID == w.ListingID {
		w.last = st.LastBlock
	}
}

func (w *Watcher) save() error {
	if w.StatePath == "" {
		return nil
	}
	b, _ := json.Marshal(watcherState{Contract: w.K.Addr, ListingID: w.ListingID, LastBlock: w.last})
	tmp := w.StatePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(w.StatePath), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, w.StatePath)
}

// Admitted is what one Step did for one lease.
type Admitted struct {
	Lease  Lease
	Reason string // "" = admitted now; otherwise why it was skipped
}

// Step scans new confirmed blocks once and returns what it did.
func (w *Watcher) Step(ctx context.Context) ([]Admitted, error) {
	if !ValidClientSesh(w.ExitSesh) {
		return nil, fmt.Errorf("exit .sesh %q is not a client address", w.ExitSesh)
	}
	w.load()
	head, err := w.K.C.Head(ctx)
	if err != nil {
		return nil, err
	}
	conf := w.Confirmations
	if conf == 0 {
		conf = 20
	}
	if head.Number < conf {
		return nil, nil
	}
	to := head.Number - conf
	from := w.last + 1
	if w.last == 0 {
		from = w.FromBlock
	}
	maxR := w.MaxRange
	if maxR == 0 {
		maxR = 50000
	}
	var out []Admitted
	for from <= to {
		end := from + maxR - 1
		if end > to {
			end = to
		}
		leases, err := w.K.Leases(ctx, from, end, w.ListingID, nil)
		if err != nil {
			return out, err
		}
		for _, l := range leases {
			out = append(out, w.admit(l))
		}
		w.last = end
		if err := w.save(); err != nil {
			return out, err
		}
		from = end + 1
	}
	if n, err := w.Store.Prune(w.now()); err == nil && n > 0 {
		w.logf("removed %d expired admission(s)", n)
	}
	return out, nil
}

func (w *Watcher) admit(l Lease) Admitted {
	tid := fmt.Sprintf("lease-%s-%d", w.K.Addr.Hex()[2:10], l.LeaseID)
	switch {
	case l.ListingID != w.ListingID:
		return Admitted{l, "another listing"}
	case !w.Payout.IsZero() && l.Payout != w.Payout:
		w.logf("lease %d paid %s, not our payout %s; not admitting", l.LeaseID, l.Payout.Hex(), w.Payout.Hex())
		return Admitted{l, "paid to another address"}
	case !ValidClientSesh(l.BuyerSesh):
		return Admitted{l, "bad buyer address"}
	case int64(l.End) <= w.now().Unix():
		return Admitted{l, "already over"}
	case w.Store.Has(tid):
		return Admitted{l, "already admitted"}
	}
	a, err := SignAdmission(w.Key, w.ExitSesh, l.BuyerSesh, int64(l.End), tid)
	if err == nil {
		err = w.Store.Put(a)
	}
	if err != nil {
		w.logf("lease %d: couldn't write the admission: %v", l.LeaseID, err)
		return Admitted{l, "store error: " + err.Error()}
	}
	w.logf("lease %d: admitted %s until %s (%d h, tx %s)", l.LeaseID, l.BuyerSesh,
		time.Unix(int64(l.End), 0).UTC().Format(time.RFC3339), l.Hours, l.Tx.Hex())
	return Admitted{l, ""}
}

// Run calls Step every interval until ctx ends.
func (w *Watcher) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = 5 * time.Second
	}
	for {
		if _, err := w.Step(ctx); err != nil && ctx.Err() == nil {
			w.logf("watcher: %v (will retry)", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
