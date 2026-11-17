// Package swap implements both sides of the protocol as restartable state
// machines driven by a watchtower loop (Engine). Every irreversible action is
// persisted as an "intent" (a signed ETH transaction or a signed-but-unrelayed
// Monero transaction) before it is broadcast, so a restart re-broadcasts the
// very same transaction instead of creating a second one.
package swap

import (
	"github.com/carlcrainellc/seshswap/swapd/internal/exitpay"
	"math/big"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

type Role string

const (
	Maker Role = "maker" // XMR seller ("Bob"): claims ETH/tokens
	Taker Role = "taker" // ETH/token buyer ("Alice"): locks first, receives XMR
)

type Phase string

// Taker phases
const (
	TNegotiated Phase = "negotiated"
	TLocking    Phase = "locking"
	TLocked     Phase = "locked" // waiting for XMR
	TReadying   Phase = "readying"
	TReady      Phase = "ready"      // waiting for claim
	TCancelling Phase = "cancelling" // cancel intent sent: the maker is frozen; refund follows
	TRefunding  Phase = "refunding"
	TRefunded   Phase = "refunded" // terminal
)

// Maker phases
const (
	MNegotiated Phase = "negotiated" // waiting for taker's lock
	MEthLocked  Phase = "eth_locked"
	MXmrLocking Phase = "xmr_locking"
	MXmrLocked  Phase = "xmr_locked"
	MIntent     Phase = "claim_intent" // claim intent sent (no secret): the taker is frozen
	MClaiming   Phase = "claiming"
	MWaitRefund Phase = "wait_refund" // too late to claim safely
	MClaimed    Phase = "claimed"     // terminal
	MAborted    Phase = "aborted"     // terminal: nothing was locked by us
)

// Shared phases
const (
	PSweeping Phase = "sweeping" // we hold the full Monero key; moving coins to our wallet
	PDone     Phase = "done"     // terminal
	PFailed   Phase = "failed"   // terminal; needs a human
)

func (p Phase) Terminal() bool {
	switch p {
	case TRefunded, MClaimed, MAborted, PDone, PFailed:
		return true
	}
	return false
}

// PendingTx is a signed ETH transaction we intend to broadcast (crash-safe).
type PendingTx struct {
	Kind string   `json:"kind"`
	Raw  string   `json:"raw"`
	Hash eth.Hash `json:"hash"`

	// Reveal transactions (claim, refund) carry our secret in calldata. It is
	// public from the moment the node sees the tx, whether or not it is mined or
	// reverts. These fields stop a crash-and-restart from publishing it too late.
	Reveal    bool   `json:"reveal,omitempty"`
	SafeUntil uint64 `json:"safeUntil,omitempty"` // first broadcast only while chain time < this (deadline - margin)
	Deadline  uint64 `json:"deadline,omitempty"`  // contract deadline; after it the tx can only revert
	Broadcast bool   `json:"broadcast,omitempty"` // a node accepted it at least once
	Nonce     uint64 `json:"nonce,omitempty"`
	FeeCap    string `json:"feeCap,omitempty"` // decimal wei, for a same-nonce cancel
	TipCap    string `json:"tipCap,omitempty"`
}

// Record is everything a daemon needs to finish (or safely abort) one swap.
// It contains secrets and is only ever stored encrypted.
type Record struct {
	ID      string `json:"id"`
	Role    Role   `json:"role"`
	Phase   Phase  `json:"phase"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"` // unix; maker aborts if no lock seen by then

	// agreed terms
	Contract  eth.Address  `json:"contract"`
	ChainID   *big.Int     `json:"chainId"`
	Asset     eth.Address  `json:"asset"`
	Value     *big.Int     `json:"value"`
	XMR       uint64       `json:"xmr"` // piconero
	D1        uint64       `json:"d1"`
	D2        uint64       `json:"d2"`
	MinConfs  uint64       `json:"minConfs"` // XMR confirmations before the taker marks ready
	XmrNet    keys.Network `json:"xmrNet"`
	Grace     uint64       `json:"grace"`               // freeze length of an intent (in the Swap struct)
	Premium   *big.Int     `json:"premium,omitempty"`   // taker premium, native coin (in the Swap struct)
	EthConfs  uint64       `json:"ethConfs,omitempty"`  // ETH blocks before a lock/intent counts as final
	BlockSecs uint64       `json:"blockSecs,omitempty"` // Monero mean block time the windows were planned with
	Direction string       `json:"direction,omitempty"` // "eth2xmr" (default) or "xmr2eth": who is the maker

	// key material (secret)
	MySpend    string `json:"mySpend"`              // scalar hex
	MyView     string `json:"myView"`               // scalar hex
	ClaimerKey string `json:"claimerKey,omitempty"` // maker: per-swap secp256k1 key, never funded

	// counterparty / shared
	TheirSpend string      `json:"theirSpend"` // point hex
	TheirView  string      `json:"theirView"`  // scalar hex
	Owner      eth.Address `json:"owner"`      // taker's ETH address
	Claimer    eth.Address `json:"claimer"`
	PayoutTo   eth.Address `json:"payoutTo,omitempty"`
	// ExitPay: the claim buys exit time (PayoutTo is its forwarder address).
	ExitPay       *exitpay.Target `json:"exitPay,omitempty"`
	ExitPayDone   bool            `json:"exitPayDone,omitempty"`
	SharedAddr    string          `json:"sharedAddr"`
	RestoreHeight uint64          `json:"restoreHeight"`
	StartBlock    uint64          `json:"startBlock"` // ETH block at negotiation (log scans start here)

	// on-chain
	Salt     eth.Hash   `json:"salt"`
	Swap     *eth.Swap  `json:"swap,omitempty"`
	SwapID   eth.Hash   `json:"swapId"`
	LockHint eth.Hash   `json:"lockHint,omitempty"`
	Pending  *PendingTx `json:"pending,omitempty"`

	// relayed claim bookkeeping (maker)
	Approved     bool   `json:"approved"`
	RelayTries   int    `json:"relayTries"`
	IntentTries  int    `json:"intentTries,omitempty"`
	IntentAt     uint64 `json:"intentAt,omitempty"` // chain time the claim/cancel intent was sent
	AckSent      bool   `json:"ackSent,omitempty"`
	RelaySentAt  uint64 `json:"relaySentAt"` // chain time of the last relay submission
	ClaimTxHash  string `json:"claimTxHash,omitempty"`
	RefundTxHash string `json:"refundTxHash,omitempty"`

	// monero
	XmrLock       *xmr.Prepared  `json:"xmrLock,omitempty"`
	Sweep         []xmr.Prepared `json:"sweep,omitempty"`
	SweepDest     string         `json:"sweepDest,omitempty"`
	TheirSecret   string         `json:"theirSecret,omitempty"`   // counterparty scalar once revealed
	XmrLockHeight uint64         `json:"xmrLockHeight,omitempty"` // Monero height of the lock tx (for "spendable at")
	SweepHeight   uint64         `json:"sweepHeight,omitempty"`
	XmrSeenConfs  uint64         `json:"xmrSeenConfs,omitempty"` // taker: confirmations so far on the incoming XMR lock
	XmrSeenTx     string         `json:"xmrSeenTx,omitempty"`    // taker: that lock's tx id (public)

	// SeshRegistry link (maker listed in the registry); nil = unlisted maker
	Registry *RegLink `json:"registry,omitempty"`

	MakerURL string   `json:"makerUrl,omitempty"` // taker: where to send the optional lock hint
	LockTx   eth.Hash `json:"lockTx,omitempty"`

	// taker: our own lock, read back from the chain, did not match what we meant
	// to lock. We then never mark it ready; we only refund.
	LockMismatch string `json:"lockMismatch,omitempty"`

	Log []string `json:"log"`
}

func (r *Record) name() string { return "swap-" + r.ID }
