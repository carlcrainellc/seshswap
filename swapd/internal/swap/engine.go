package swap

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// Policy holds the safety margins. All times are judged by the ETH chain's
// latest block timestamp, never by the local clock.
type Policy struct {
	// Never broadcast a transaction that reveals a secret unless at least this
	// many seconds of its window remain (covers inclusion delay and sequencer
	// timestamp skew; use >= 3600 on Arbitrum).
	Margin uint64
	// Maker: extra slack (seconds) on top of 2*Margin + confirmation time that
	// must remain before t1 for the maker to lock XMR (see xmrLockDeadline).
	XmrLockBudget uint64
	// ETH confirmations before acting on a lock.
	EthConfs uint64
	// Maker: resubmit a relayed claim to the next relayer if not mined after this many seconds.
	RelayTimeout uint64
	// Maker: fee offered to relayers for native-coin (ETH) swaps, in wei.
	// For ERC-20 swaps use TokenRelayFee (token units). A leftover ETH wei
	// value must never be applied to a 6-decimal token: 1e12 wei is 1e6 USDC.
	RelayFee *big.Int
	// TokenRelayFee is the relayer fee in token units, per ERC-20 asset.
	// Missing entries default to 0 (the whole lock goes to the maker; a
	// relayer that requires a positive TokenMinFee will refuse, and the
	// maker self-relays with fee 0 if it has a gas key).
	TokenRelayFee map[eth.Address]*big.Int
	// Smallest intent freeze (seconds) we accept in a swap. The reveal must be
	// mined within this time after its intent is final. Default 6 h.
	MinGrace uint64
}

func DefaultPolicy() Policy {
	return Policy{Margin: 1800, XmrLockBudget: 900, EthConfs: 2, RelayTimeout: 120, RelayFee: big.NewInt(0), MinGrace: 6 * 3600}
}

// FeeFor is the relayer fee the maker will sign, in units of asset
// (wei for ETH, token units for an ERC-20). Unknown tokens default to 0;
// Policy.RelayFee is never used as a token fee.
func (p Policy) FeeFor(asset eth.Address) *big.Int {
	if asset.IsZero() {
		if p.RelayFee == nil {
			return big.NewInt(0)
		}
		return new(big.Int).Set(p.RelayFee)
	}
	if p.TokenRelayFee != nil {
		if f, ok := p.TokenRelayFee[asset]; ok && f != nil {
			return new(big.Int).Set(f)
		}
	}
	return big.NewInt(0)
}

// CheckRelayFee requires fee < minLock (and fee >= 0). The contract reverts
// BadParams when fee > value, so a fee at or above the smallest lock can
// never be claimed through a relayer.
func CheckRelayFee(fee, minLock *big.Int) error {
	if fee == nil {
		fee = big.NewInt(0)
	}
	if fee.Sign() < 0 {
		return fmt.Errorf("relay fee cannot be negative")
	}
	if minLock == nil || minLock.Sign() <= 0 {
		return fmt.Errorf("min lock must be positive to check the relay fee")
	}
	if fee.Cmp(minLock) >= 0 {
		return fmt.Errorf("relay fee %s is not less than min lock %s (must be in asset units, below the smallest lock; a leftover ETH wei fee will revert an ERC-20 claim)", fee, minLock)
	}
	return nil
}

// ApplyOfferRelayFee copies a token offer's advertised relay fee into the
// policy if that token has no TokenRelayFee yet. ETH offers are left alone
// (Policy.RelayFee is the native-coin fee). The copied value is still
// validated by ValidateMakerRelay: a 1e12 leftover on a 6-decimal token fails.
func ApplyOfferRelayFee(p *Policy, asset eth.Address, offerFee *big.Int) {
	if p == nil || asset.IsZero() || offerFee == nil {
		return
	}
	if p.TokenRelayFee == nil {
		p.TokenRelayFee = map[eth.Address]*big.Int{}
	}
	if _, ok := p.TokenRelayFee[asset]; !ok {
		p.TokenRelayFee[asset] = new(big.Int).Set(offerFee)
	}
}

// ValidateMakerRelay checks that the fee for asset is below minLock, and that
// the maker has a gas key so it can submit its own claim if a relayer refuses
// or never submits it.
func ValidateMakerRelay(p Policy, asset eth.Address, minLock *big.Int, hasGasKey bool) error {
	if err := CheckRelayFee(p.FeeFor(asset), minLock); err != nil {
		return err
	}
	if !hasGasKey {
		if asset.IsZero() {
			return errors.New("maker needs a gas key (SWAPD_ETH_KEY) so it can self-relay if the relayer refuses or leaks a claim")
		}
		return errors.New("ERC-20 maker needs a gas key (SWAPD_ETH_KEY) so it can self-relay if the relayer refuses")
	}
	return nil
}

func (e *Engine) ethConfs(r *Record) uint64 {
	return max(e.Policy.EthConfs, r.EthConfs, 1)
}

// finalTime is the timestamp of the newest block we treat as final (EthConfs deep).
// Time conditions that would make a reveal safe are judged on it, so that a reorg
// cannot bring back a moment at which the other side could still act.
func (e *Engine) finalTime(ctx context.Context, r *Record, head *eth.Head) (uint64, error) {
	c := e.ethConfs(r)
	if c <= 1 || head.Number+1 < c {
		return head.Timestamp, nil
	}
	b, err := e.K.C.BlockAt(ctx, head.Number+1-c)
	if err != nil {
		return 0, err
	}
	return b.Timestamp, nil
}

// eventFinal reports whether this swap's event `kind` is mined at least EthConfs deep.
func (e *Engine) eventFinal(ctx context.Context, r *Record, head *eth.Head, kind string) (bool, error) {
	evs, err := e.K.Events(ctx, r.SwapID, r.StartBlock, head.Number)
	if err != nil {
		return false, err
	}
	for _, ev := range evs {
		if ev.Kind == kind {
			return head.Number+1 >= ev.Block+e.ethConfs(r), nil
		}
	}
	return false, nil
}

// Engine is the watchtower. It owns all records in a store.
type Engine struct {
	Role     Role
	Store    *store.Store
	K        *eth.Contract
	EthKey   *eth.Key // taker: funds+owner key. maker: optional gas key for self-relaying fallback.
	Wallet   *xmr.Wallet
	MainWal  string // our own Monero wallet file (maker: source of XMR; both: sweep destination)
	XmrNet   keys.Network
	Relayers []string
	Policy   Policy
	Logf     func(string, ...any)
	// CrashAfterIntent (tests only): if it returns true for an intent kind, the
	// engine stops right after persisting the intent, as if the process died.
	CrashAfterIntent func(kind string) bool
	// OnLocked (taker) is called after the lock is mined, to tell the maker the tx hash.
	OnLocked func(id, makerURL string, tx eth.Hash)
	// Daemon is monerod, used to measure mean block time (required when
	// building or checking windows, unless AssumeBlockSecs is set) and reorg depth.
	Daemon *xmr.Daemon
	// AssumeBlockSecs is an explicit mean Monero block time (seconds), used
	// when Daemon is unset (or if the daemon has no measurement yet). 0 means
	// no assumption: BlockSecs returns ErrNoBlockTime.
	AssumeBlockSecs uint64

	reorgMu  sync.Mutex
	reorgAt  time.Time
	reorgVal uint64
	blockAt  time.Time
	blockVal uint64

	// StepTimeout bounds one swap step (ETH and Monero calls), so a stalled
	// node link can't hold a swap forever. 0 = DefaultStepTimeout.
	StepTimeout time.Duration

	// mu guards the records map and the records in it. It is only held for
	// short in-memory work, never across a network call, so status reads
	// stay quick while a step waits on a slow node.
	mu      sync.Mutex
	records map[string]*Record
	// wmu serializes writers (a step, Update) per record.
	wmuMu sync.Mutex
	wmu   map[string]*sync.Mutex
}

// DefaultStepTimeout is the longest one swap step may take.
const DefaultStepTimeout = 5 * time.Minute

func (e *Engine) writer(id string) *sync.Mutex {
	e.wmuMu.Lock()
	defer e.wmuMu.Unlock()
	if e.wmu == nil {
		e.wmu = map[string]*sync.Mutex{}
	}
	m := e.wmu[id]
	if m == nil {
		m = new(sync.Mutex)
		e.wmu[id] = m
	}
	return m
}

// cloneRecord deep-copies a record the way the store keeps it.
func cloneRecord(r *Record) (*Record, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	c := new(Record)
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (e *Engine) logf(r *Record, f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	if r != nil {
		r.Log = append(r.Log, time.Now().UTC().Format(time.RFC3339)+" "+msg)
		msg = "[" + string(e.Role) + " " + r.ID[:8] + "] " + msg
	}
	if e.Logf != nil {
		e.Logf("%s", msg)
	} else {
		log.Print(msg)
	}
}

// Load reads all records from the store (call once at startup = crash recovery).
// Note appends a line to a swap's log (and the process log).
func (e *Engine) Note(id, msg string) {
	_ = e.Update(id, func(r *Record) error { e.logf(r, "%s", msg); return nil })
}

func (e *Engine) Load() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = map[string]*Record{}
	names, err := e.Store.List("swap-")
	if err != nil {
		return err
	}
	for _, n := range names {
		var r Record
		if err := e.Store.Get(n, &r); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		e.records[r.ID] = &r
	}
	return nil
}

// Add persists a new record (secrets hit the disk before anything is sent).
func (e *Engine) Add(r *Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.records == nil {
		e.records = map[string]*Record{}
	}
	if _, ok := e.records[r.ID]; ok {
		return errors.New("swap id exists")
	}
	if err := e.Store.Put(r.name(), r); err != nil {
		return err
	}
	e.records[r.ID] = r
	return nil
}

// Update applies fn to a record under the engine lock and persists it.
// It waits for a running step of the same swap (bounded by StepTimeout).
func (e *Engine) Update(id string, fn func(r *Record) error) error {
	w := e.writer(id)
	w.Lock()
	defer w.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.records[id]
	if !ok {
		return errors.New("unknown swap")
	}
	if err := fn(r); err != nil {
		return err
	}
	return e.Store.Put(r.name(), r)
}

// Get returns a copy-safe snapshot of a record's phase and log.
func (e *Engine) Get(id string) (Phase, []string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.records[id]
	if !ok {
		return "", nil, false
	}
	return r.Phase, append([]string(nil), r.Log...), true
}

func (e *Engine) IDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []string
	for id := range e.records {
		ids = append(ids, id)
	}
	return ids
}

// Run steps every unfinished swap until ctx is cancelled.
func (e *Engine) Run(ctx context.Context, every time.Duration) {
	for {
		e.StepAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (e *Engine) StepAll(ctx context.Context) {
	for _, id := range e.IDs() {
		if ctx.Err() != nil {
			return
		}
		e.stepOne(ctx, id)
	}
}

// stepOne steps a copy of the record without holding e.mu (the step makes
// ETH and Monero calls), then puts the copy in place. Writers of the same
// swap wait on its writer lock; readers see the record as it was.
func (e *Engine) stepOne(ctx context.Context, id string) {
	w := e.writer(id)
	w.Lock()
	defer w.Unlock()
	e.mu.Lock()
	cur, ok := e.records[id]
	if !ok || cur.Phase.Terminal() {
		e.mu.Unlock()
		return
	}
	r, err := cloneRecord(cur)
	e.mu.Unlock()
	if err != nil {
		e.logf(nil, "swap %s: %v", id, err)
		return
	}
	d := e.StepTimeout
	if d <= 0 {
		d = DefaultStepTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, d)
	if err := e.safeStep(sctx, r); err != nil {
		if sctx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
			err = fmt.Errorf("step took longer than %s (node or wallet not answering): %w", d, err)
		}
		e.logf(r, "step error (will retry): %v", err)
	}
	cancel()
	e.mu.Lock()
	e.records[id] = r
	e.mu.Unlock()
	_ = e.Store.Put(r.name(), r)
}

// safeStep turns a panic in one swap's step into a logged error, so an
// unattended maker keeps serving every other swap (and retries this one).
func (e *Engine) safeStep(ctx context.Context, r *Record) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return e.step(ctx, r)
}

func (e *Engine) step(ctx context.Context, r *Record) error {
	head, err := e.K.C.Head(ctx)
	if err != nil {
		return err
	}
	if r.Pending != nil {
		done, err := e.settlePending(ctx, r, head)
		if err != nil || !done {
			return err
		}
	}
	if r.Role == Taker {
		return e.stepTaker(ctx, r, head)
	}
	return e.stepMaker(ctx, r, head)
}

// ---------------------------------------------------------------- ETH tx intents

// sendIntent signs a tx and persists it together with the phase we move to
// (so a restart can never "forget" it and send a second one), then broadcasts.
func (e *Engine) sendIntent(ctx context.Context, r *Record, kind string, key *eth.Key, value *big.Int, data []byte, next Phase) error {
	tx, err := e.K.C.BuildTx(ctx, key, &e.K.Addr, value, data)
	if err != nil {
		return fmt.Errorf("%s: build/estimate failed: %w", kind, err)
	}
	raw, h := tx.Sign(key)
	prevPhase, prevLock := r.Phase, r.LockTx
	r.Pending = &PendingTx{Kind: kind, Raw: hex.EncodeToString(raw), Hash: h}
	r.Phase = next
	if kind == "lock" {
		r.LockTx = h
	}
	if err := e.Store.Put(r.name(), r); err != nil { // intent on disk before broadcast
		r.Pending, r.Phase, r.LockTx = nil, prevPhase, prevLock
		return err
	}
	if e.CrashAfterIntent != nil && e.CrashAfterIntent(kind) {
		return errCrashed
	}
	if _, err := e.K.C.SendRaw(ctx, raw); err != nil && !alreadyKnown(err) {
		e.logf(r, "%s broadcast error (will rebroadcast): %v", kind, err)
	} else {
		r.Pending.Broadcast = true
		e.logf(r, "%s sent: %s", kind, h.Hex())
	}
	return nil
}

// Fixed gas limits for transactions that carry a secret (no eth_estimateGas, which
// would show the secret to the RPC node before we decide to broadcast). Measured
// use is ~131k (refund) and ~176k (relayed claim) for ETH; ERC-20 adds two token calls.
const (
	GasRefund = 300_000
	GasClaim  = 400_000
)

// sendReveal is sendIntent for a claim or refund. It refuses to sign unless more
// than Policy.Margin of the window is left (deadline = the contract time limit,
// 0 = none), and records the limits so a restart never broadcasts it too late.
func (e *Engine) sendReveal(ctx context.Context, r *Record, kind string, key *eth.Key, data []byte, next Phase, deadline, now uint64) error {
	if deadline != 0 && now+e.Policy.Margin >= deadline {
		return fmt.Errorf("%s: less than the safety margin left before the deadline; not revealing our secret", kind)
	}
	gas := uint64(GasClaim)
	if kind == "refund" {
		gas = GasRefund
	}
	tx, err := e.K.C.BuildTxFixedGas(ctx, key, &e.K.Addr, nil, data, gas)
	if err != nil {
		return fmt.Errorf("%s: build failed: %w", kind, err)
	}
	raw, h := tx.Sign(key)
	prevPhase := r.Phase
	p := &PendingTx{Kind: kind, Raw: hex.EncodeToString(raw), Hash: h, Reveal: true, Deadline: deadline,
		Nonce: tx.Nonce, FeeCap: tx.FeeCap.String(), TipCap: tx.TipCap.String()}
	if deadline != 0 {
		p.SafeUntil = deadline - e.Policy.Margin
	}
	r.Pending, r.Phase = p, next
	if err := e.Store.Put(r.name(), r); err != nil { // intent (and its limits) on disk before broadcast
		r.Pending, r.Phase = nil, prevPhase
		return err
	}
	if e.CrashAfterIntent != nil && e.CrashAfterIntent(kind) {
		return errCrashed
	}
	if _, err := e.K.C.SendRaw(ctx, raw); err != nil && !alreadyKnown(err) {
		e.logf(r, "%s broadcast error (will retry while it is still safe): %v", kind, err)
	} else {
		p.Broadcast = true
		e.logf(r, "%s sent: %s", kind, h.Hex())
	}
	return nil
}

// cancelSameNonce best-effort replaces a pending tx with a 0-value self-transfer at
// the same nonce and at least double the fees.
func (e *Engine) cancelSameNonce(ctx context.Context, r *Record, p *PendingTx) {
	if e.EthKey == nil {
		return
	}
	tip, cap, err := e.K.C.Fees(ctx)
	if err != nil {
		e.logf(r, "cancel: %v", err)
		return
	}
	oldTip, _ := new(big.Int).SetString(p.TipCap, 10)
	oldCap, _ := new(big.Int).SetString(p.FeeCap, 10)
	if oldTip != nil && new(big.Int).Mul(oldTip, big.NewInt(2)).Cmp(tip) > 0 {
		tip = new(big.Int).Mul(oldTip, big.NewInt(2))
	}
	if oldCap != nil && new(big.Int).Mul(oldCap, big.NewInt(2)).Cmp(cap) > 0 {
		cap = new(big.Int).Mul(oldCap, big.NewInt(2))
	}
	if cap.Cmp(tip) < 0 {
		cap = new(big.Int).Set(tip)
	}
	self := e.EthKey.Address()
	tx := &eth.Tx{ChainID: e.K.ChainID, Nonce: p.Nonce, TipCap: tip, FeeCap: cap, Gas: 21000, To: &self, Value: new(big.Int)}
	raw, h := tx.Sign(e.EthKey)
	if _, err := e.K.C.SendRaw(ctx, raw); err != nil {
		e.logf(r, "cancel tx not accepted (the original may already be mined): %v", err)
		return
	}
	e.logf(r, "sent same-nonce cancel %s", h.Hex())
}

// settlePending returns done=true once the pending tx is mined (ok or reverted),
// or once it has been dropped on purpose.
func (e *Engine) settlePending(ctx context.Context, r *Record, head *eth.Head) (bool, error) {
	p := r.Pending
	rc, err := e.K.C.Receipt(ctx, p.Hash)
	if err != nil {
		return false, err
	}
	if rc == nil {
		now := head.Timestamp
		if p.Reveal && !p.Broadcast {
			// A crash may have hit between broadcasting and saving that fact: ask the node.
			if known, err := e.K.C.TxKnown(ctx, p.Hash); err == nil && known {
				p.Broadcast = true
			}
		}
		if p.Reveal && !p.Broadcast && p.SafeUntil != 0 && now >= p.SafeUntil {
			// Never published, and too little of its window is left to be sure it
			// lands in time. Publishing it now could hand the counterparty our
			// secret while our transaction fails. Drop it: the secret stays private.
			e.logf(r, "%s was never broadcast and less than the safety margin is left; dropping it (secret stays private)", p.Kind)
			r.Pending = nil
			if e.EthKey != nil {
				e.K.C.ForgetNonce(e.EthKey.Address(), p.Nonce) // give back only this nonce
			}
			return true, nil
		}
		if p.Reveal && p.Broadcast && p.Deadline != 0 && now >= p.Deadline {
			// Already public and now it can only revert. Try to push it out of the
			// mempool with a same-nonce no-op so it is less likely to be mined.
			e.logf(r, "%s not mined before its deadline; its secret may be public. Replacing it with a same-nonce no-op", p.Kind)
			e.cancelSameNonce(ctx, r, p)
			r.Pending = nil
			return true, nil
		}
		raw, _ := hex.DecodeString(p.Raw)
		if _, err := e.K.C.SendRaw(ctx, raw); err != nil && !alreadyKnown(err) {
			// Nonce already used: either ours was mined (and the node can't find the
			// receipt yet) or another tx took the nonce. Drop the intent; every phase
			// re-derives what happened from contract state (and the lock from logs).
			if strings.Contains(strings.ToLower(err.Error()), "nonce too low") {
				e.logf(r, "%s: nonce already used; re-checking chain state", p.Kind)
				r.Pending = nil
				return true, nil
			}
			return false, err
		}
		p.Broadcast = true
		return false, nil
	}
	r.Pending = nil
	if !rc.OK() {
		e.logf(r, "%s reverted (tx %s)", p.Kind, p.Hash.Hex())
		return true, nil
	}
	e.logf(r, "%s mined in block %d (gas %d)", p.Kind, rc.Block(), rc.Gas())
	if p.Kind == "lock" {
		for _, l := range rc.Logs {
			if l.Address == e.K.Addr && len(l.Topics) == 2 && l.Topics[0] == eth.Hash(eth.TopicLocked) {
				evs, err := e.K.Events(ctx, l.Topics[1], rc.Block(), rc.Block())
				if err != nil {
					return false, err
				}
				for _, ev := range evs {
					if ev.Kind == "Locked" {
						r.Swap, r.SwapID = ev.Swap, ev.ID
					}
				}
			}
		}
	}
	return true, nil
}

var errCrashed = errors.New("simulated crash after persisting intent")

func alreadyKnown(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already known") || strings.Contains(s, "already imported") || strings.Contains(s, "known transaction")
}

// eventOf returns the first event of kind for this swap in [startBlock, head].
func (e *Engine) eventOf(ctx context.Context, r *Record, kind string) (eth.Event, error) {
	head, err := e.K.C.Head(ctx)
	if err != nil {
		return eth.Event{}, err
	}
	evs, err := e.K.Events(ctx, r.SwapID, r.StartBlock, head.Number)
	if err != nil {
		return eth.Event{}, err
	}
	for _, ev := range evs {
		if ev.Kind == kind {
			return ev, nil
		}
	}
	return eth.Event{}, errors.New(kind + " event not found")
}

// secretFromEvents returns the secret revealed by kind ("Claimed"/"Refunded").
func (e *Engine) secretFromEvents(ctx context.Context, r *Record, kind string) (*big.Int, error) {
	ev, err := e.eventOf(ctx, r, kind)
	if err != nil {
		return nil, err
	}
	return ev.Secret, nil
}

// ---------------------------------------------------------------- Monero helpers

func (e *Engine) shared(r *Record) (*keys.Shared, keys.Scalar, error) {
	my, err := keys.ScalarFromHex(r.MySpend)
	if err != nil {
		return nil, keys.Scalar{}, err
	}
	myView, err := keys.ScalarFromHex(r.MyView)
	if err != nil {
		return nil, keys.Scalar{}, err
	}
	their, err := keys.PointFromHex(r.TheirSpend)
	if err != nil {
		return nil, keys.Scalar{}, err
	}
	theirView, err := keys.ScalarFromHex(r.TheirView)
	if err != nil {
		return nil, keys.Scalar{}, err
	}
	sh, err := keys.SharedAddress(r.XmrNet, my.Public(), their, myView, theirView)
	return sh, my, err
}

// sharedFunds reports how much of the agreed amount is at the shared address with >= confs
// confirmations and no unlock_time (a lock far in the future would freeze the coins).
func (e *Engine) sharedFunds(ctx context.Context, r *Record, confs uint64) (ok uint64, seen bool, err error) {
	sh, _, err := e.shared(r)
	if err != nil {
		return 0, false, err
	}
	name := "v-" + r.ID
	if err := e.Wallet.FromKeys(ctx, name, sh.Address, sh.ViewKey.Hex(), "", r.RestoreHeight); err != nil {
		return 0, false, err
	}
	err = e.Wallet.Session(ctx, name, func(s *xmr.Sess) error {
		if err := s.Refresh(ctx); err != nil {
			return err
		}
		in, pool, err := s.Incoming(ctx)
		if err != nil {
			return err
		}
		seen = len(in)+len(pool) > 0
		var best xmr.Transfer
		var have bool
		note := func(t xmr.Transfer) {
			if t.UnlockTime != 0 {
				e.logf(r, "ignoring incoming %s: non-zero unlock_time %d", t.TxID, t.UnlockTime)
				return
			}
			if t.Confirmations >= confs {
				ok += t.Amount
			}
			if !have || t.Amount > best.Amount || (t.Amount == best.Amount && t.Confirmations > best.Confirmations) {
				best, have = t, true
			}
		}
		for _, t := range in {
			note(t)
		}
		for _, t := range pool {
			if t.UnlockTime != 0 {
				continue
			}
			if !have || t.Amount > best.Amount || (t.Amount == best.Amount && t.Confirmations > best.Confirmations) {
				best, have = t, true
			}
		}
		if have {
			r.XmrSeenConfs = best.Confirmations
			r.XmrSeenTx = best.TxID
			if best.Height != 0 {
				r.XmrLockHeight = best.Height
			}
		}
		return nil
	})
	return ok, seen, err
}

// sweep moves everything at the shared address to our own wallet once we know the full key.
// Returns true when finished.
func (e *Engine) sweep(ctx context.Context, r *Record) (bool, error) {
	sh, my, err := e.shared(r)
	if err != nil {
		return false, err
	}
	their, err := keys.ScalarFromHex(r.TheirSecret)
	if err != nil {
		return false, err
	}
	full := my.Add(their)
	if !full.Public().Equal(sh.SpendPub) {
		return false, errors.New("revealed secret does not complete the shared key")
	}
	if r.SweepDest == "" {
		if err := e.Wallet.Session(ctx, e.MainWal, func(s *xmr.Sess) error {
			a, err := s.Address(ctx)
			r.SweepDest = a
			return err
		}); err != nil {
			return false, err
		}
	}
	name := "s-" + r.ID
	if err := e.Wallet.FromKeys(ctx, name, sh.Address, sh.ViewKey.Hex(), full.Hex(), r.RestoreHeight); err != nil {
		return false, err
	}
	finished := false
	err = e.Wallet.Session(ctx, name, func(s *xmr.Sess) error {
		if err := s.Refresh(ctx); err != nil {
			return err
		}
		if len(r.Sweep) > 0 { // re-relay prepared sweeps (idempotent) until confirmed
			all := true
			for i := range r.Sweep {
				c, _ := s.TxConfirmations(ctx, r.Sweep[i].TxHash)
				if c < 1 {
					all = false
					if err := s.Relay(ctx, &r.Sweep[i]); err != nil && !strings.Contains(err.Error(), "already") {
						e.logf(r, "sweep relay: %v", err)
					}
				}
			}
			finished = all
			if all && r.SweepHeight == 0 {
				c, _ := s.TxConfirmations(ctx, r.Sweep[0].TxHash)
				if h, err := s.Height(ctx); err == nil && c > 0 && h >= uint64(c) {
					r.SweepHeight = h - uint64(c)
					at := SpendableAt(0, r.SweepHeight)
					left := uint64(0)
					if at > h {
						left = at - h
					}
					e.logf(r, "XMR arrives in our wallet at Monero height %d and is spendable from height %d (about %d min from now)", r.SweepHeight, at, left*XmrBlockSecs/60)
				}
			}
			return nil
		}
		total, unlocked, err := s.Balance(ctx)
		if err != nil {
			return err
		}
		if total == 0 {
			return errors.New("shared address shows no funds yet")
		}
		if unlocked < total {
			return nil // wait for Monero's 10-block unlock
		}
		ps, err := s.PrepareSweep(ctx, r.SweepDest)
		if err != nil {
			return err
		}
		r.Sweep = ps
		if err := e.Store.Put(r.name(), r); err != nil {
			return err
		}
		for i := range ps {
			if err := s.Relay(ctx, &ps[i]); err != nil {
				return err
			}
		}
		e.logf(r, "swept %d tx(s) from shared address to own wallet", len(ps))
		return nil
	})
	return finished, err
}

// Reserved is the XMR (piconero) the maker may still have to lock for open swaps.
func (e *Engine) Reserved() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var sum uint64
	for _, r := range e.records {
		if r.Role == Maker && (r.Phase == MNegotiated || r.Phase == MEthLocked) && r.XmrLock == nil {
			sum += r.XMR
		}
	}
	return sum
}

// Exposure is the XMR (piconero) this maker has promised or locked in swaps that
// are not finished: reserved (not yet locked) plus locked-but-unsettled.
func (e *Engine) Exposure() (reserved, locked uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.records {
		if r.Role != Maker || r.Phase.Terminal() || r.Phase == MClaimed {
			continue
		}
		if r.XmrLock == nil {
			if r.Phase == MNegotiated || r.Phase == MEthLocked {
				reserved += r.XMR
			}
		} else {
			locked += r.XMR
		}
	}
	return
}

// PhaseCounts counts records per phase (for metrics).
func (e *Engine) PhaseCounts() map[Phase]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := map[Phase]int{}
	for _, r := range e.records {
		m[r.Phase]++
	}
	return m
}

// CompletedBy counts swaps with this taker (owner address) that we completed
// (claimed). Used to waive the premium for takers with a track record.
func (e *Engine) CompletedBy(owner eth.Address) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, r := range e.records {
		if r.Role == Maker && r.Owner == owner && r.Phase == MClaimed {
			n++
		}
	}
	return n
}

// Snapshot returns a copy of a record (for status displays).
func (e *Engine) Snapshot(id string) (Record, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.records[id]
	if !ok {
		return Record{}, false
	}
	return *r, true
}

// TakerHeld is what unfinished taker swaps may still take from our own
// wallet: ETH (wei, value plus any premium) and tokens (per asset) for swaps
// not locked yet, and how many swaps are unfinished at all (they may still
// need gas for a refund or a ready transaction).
func (e *Engine) TakerHeld() (ethWei *big.Int, tokens map[eth.Address]*big.Int, open int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ethWei, tokens = new(big.Int), map[eth.Address]*big.Int{}
	for _, r := range e.records {
		if r.Phase.Terminal() {
			continue
		}
		open++
		if r.Role != Taker || (r.Phase != TNegotiated && r.Phase != TLocking) {
			continue
		}
		if r.Premium != nil {
			ethWei.Add(ethWei, r.Premium)
		}
		if r.Value == nil {
			continue
		}
		if r.Asset.IsZero() {
			ethWei.Add(ethWei, r.Value)
		} else {
			if tokens[r.Asset] == nil {
				tokens[r.Asset] = new(big.Int)
			}
			tokens[r.Asset].Add(tokens[r.Asset], r.Value)
		}
	}
	return
}

// XmrHeld is Reserved plus swaps whose XMR lock is being sent right now
// (the transfer may be in flight): what a manual send must leave alone.
func (e *Engine) XmrHeld() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var sum uint64
	for _, r := range e.records {
		if r.Role == Maker && r.XmrLock == nil && (r.Phase == MNegotiated || r.Phase == MEthLocked || r.Phase == MXmrLocking) {
			sum += r.XMR
		}
	}
	return sum
}
