package wallet

import (
	"fmt"
	"math/big"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

const ctZone = "America/Chicago"

// View is the public, secret-free description of one swap for the UI.
type View struct {
	ID           string         `json:"id"`
	Phase        string         `json:"phase"`
	Status       string         `json:"status"`     // plain English
	RefundLine   string         `json:"refundLine"` // plain English, may be empty
	RefundAtCT   string         `json:"refundAtCT,omitempty"`
	RefundUnix   int64          `json:"refundUnix,omitempty"`
	ETHBackAtCT  string         `json:"ethBackAtCT,omitempty"`
	ETHBackUnix  int64          `json:"ethBackUnix,omitempty"`
	SpendableCT  string         `json:"spendableAtCT,omitempty"`
	ValueWei     string         `json:"valueWei"`
	ValueETH     string         `json:"valueEth,omitempty"`
	ValueUSDC    string         `json:"valueUsdc,omitempty"`
	ValueLabel   string         `json:"valueLabel"` // "0.005 ETH" or "5 USDC"
	Asset        string         `json:"asset"`      // "ETH" or "USDC"
	AssetAddr    string         `json:"assetAddr,omitempty"`
	XMRPico      uint64         `json:"xmrPico"`
	XMR          string         `json:"xmr"`
	MinConfs     uint64         `json:"minConfs"`
	Grace        uint64         `json:"grace"`
	PremiumWei   string         `json:"premiumWei,omitempty"`
	MakerURL     string         `json:"makerUrl,omitempty"`
	SharedAddr   string         `json:"sharedAddr,omitempty"`
	LockTx       string         `json:"lockTx,omitempty"`
	ClaimTx      string         `json:"claimTx,omitempty"`
	RefundTx     string         `json:"refundTx,omitempty"`
	XmrLockTx    string         `json:"xmrLockTx,omitempty"`
	XmrSeenConfs uint64         `json:"xmrSeenConfs,omitempty"`
	SwapID       string         `json:"swapId,omitempty"`
	T1           uint64         `json:"t1,omitempty"`
	T2           uint64         `json:"t2,omitempty"`
	Created      int64          `json:"created"`
	Terminal     bool           `json:"terminal"`
	LockMismatch string         `json:"lockMismatch,omitempty"`
	Log          []string       `json:"log,omitempty"`
	SweepDest    string         `json:"sweepDest,omitempty"`
	XmrLockH     uint64         `json:"xmrLockHeight,omitempty"`
	SpendableH   uint64         `json:"spendableAtHeight,omitempty"`
	Registry     map[string]any `json:"registry,omitempty"`
}

// PublicView builds a View from a record, stripping every key and secret.
func PublicView(r swap.Record, now time.Time) View {
	v := View{
		ID:           r.ID,
		Phase:        string(r.Phase),
		ValueWei:     bigStr(r.Value),
		ValueETH:     weiToETH(r.Value),
		XMRPico:      r.XMR,
		XMR:          picoToXMR(r.XMR),
		MinConfs:     r.MinConfs,
		Grace:        r.Grace,
		PremiumWei:   bigStr(r.Premium),
		MakerURL:     r.MakerURL,
		SharedAddr:   r.SharedAddr,
		Created:      r.Created,
		Terminal:     r.Phase.Terminal(),
		LockMismatch: r.LockMismatch,
		Log:          append([]string(nil), r.Log...),
		SweepDest:    r.SweepDest,
		XmrLockH:     r.XmrLockHeight,
		SpendableH:   swap.SpendableAt(r.XmrLockHeight, r.SweepHeight),
	}
	if r.LockTx != (eth.Hash{}) {
		v.LockTx = r.LockTx.Hex()
	}
	if r.ClaimTxHash != "" {
		v.ClaimTx = r.ClaimTxHash
	}
	if r.RefundTxHash != "" {
		v.RefundTx = r.RefundTxHash
	}
	if r.XmrSeenTx != "" {
		v.XmrLockTx = r.XmrSeenTx
	}
	v.XmrSeenConfs = r.XmrSeenConfs
	if r.SwapID != (eth.Hash{}) {
		v.SwapID = r.SwapID.Hex()
	}
	if r.Swap != nil {
		v.T1, v.T2, v.Grace = r.Swap.T1, r.Swap.T2, r.Swap.Grace
	}
	v.Status, v.RefundLine, v.RefundUnix, v.ETHBackUnix = plainStatus(r, now)
	v.RefundAtCT = formatCT(v.RefundUnix)
	v.ETHBackAtCT = formatCT(v.ETHBackUnix)
	if r.Phase == swap.PDone || r.Phase == swap.PSweeping {
		// Spendable ~ 10 Monero blocks after the sweep (or lock, as a lower bound).
		if v.SpendableH != 0 && r.SweepHeight == 0 {
			// only a height; leave the clock estimate from status
		}
	}
	labelAsset(&v, r)
	return v
}

func labelAsset(v *View, r swap.Record) {
	if r.Asset.IsZero() {
		v.Asset = "ETH"
		v.ValueLabel = v.ValueETH + " ETH"
		return
	}
	v.Asset = "USDC"
	v.AssetAddr = r.Asset.Hex()
	v.ValueUSDC = formatUnits(r.Value, 6)
	v.ValueLabel = v.ValueUSDC + " USDC"
	v.ValueETH = ""
}

func assetWord(r swap.Record) string {
	if r.Asset.IsZero() {
		return "ETH"
	}
	return "USDC"
}

func plainStatus(r swap.Record, now time.Time) (status, refund string, refundAt, backAt int64) {
	var t1, t2, grace uint64
	if r.Swap != nil {
		t1, t2, grace = r.Swap.T1, r.Swap.T2, r.Swap.Grace
	}
	switch r.Phase {
	case swap.TNegotiated:
		return fmt.Sprintf("Waiting to lock your %s.", assetWord(r)), "Nothing is locked yet. You can close this.", 0, 0
	case swap.TLocking:
		return fmt.Sprintf("Locking your %s.", assetWord(r)), fmt.Sprintf("If this fails, the %s stays in your wallet.", assetWord(r)), 0, 0
	case swap.TLocked:
		status = "Waiting for the other side to lock XMR."
		need := r.MinConfs
		if need == 0 {
			need = swap.XmrConfFloor
		}
		if r.XmrSeenTx != "" || r.XmrSeenConfs > 0 || r.XmrLockHeight > 0 {
			status = fmt.Sprintf("Waiting for Monero confirmations (%d/%d).", r.XmrSeenConfs, need)
		}
		if r.LockMismatch != "" {
			status = fmt.Sprintf("The lock did not match what was agreed. Your %s will be refunded; we will not mark this ready.", assetWord(r))
		}
		line := refundLine(r, t1, t2, grace)
		if r.CancelFrom != 0 && now.Unix() < int64(r.CancelFrom) {
			line += fmt.Sprintf(" An early cancel is possible from %s (the other side first gets time to lock XMR).", formatCT(int64(r.CancelFrom)))
		}
		return status, line, int64(t1), int64(t1 + grace)
	case swap.TReadying:
		return fmt.Sprintf("Monero confirmations are in. Marking the swap ready."), refundLine(r, t1, t2, grace), int64(t1), int64(t1 + grace)
	case swap.TReady:
		return fmt.Sprintf("Swap finishing. The other side is claiming the %s. That publishes the secret we need.", assetWord(r)),
			fmt.Sprintf("If this stalls, your %s is returned automatically after %s.", assetWord(r), formatCT(int64(t2))),
			int64(t2), int64(t2)
	case swap.TCancelling:
		back := t1 + grace
		if r.Swap != nil && r.IntentAt > 0 {
			back = r.IntentAt + grace
		}
		return fmt.Sprintf("Refund started. Your %s comes back after %s.", assetWord(r), formatCT(int64(back))),
			fmt.Sprintf("%s returns at %s. Any XMR the other side locked goes back to them.", assetWord(r), formatCT(int64(back))),
			int64(back), int64(back)
	case swap.TRefunding:
		return fmt.Sprintf("Refunding your %s.", assetWord(r)), fmt.Sprintf("Your %s is on the way back to your wallet.", assetWord(r)), 0, 0
	case swap.TRefunded:
		return fmt.Sprintf("Refunded. Your %s is back in your wallet.", assetWord(r)), "", 0, 0
	case swap.PSweeping:
		return "Swap done. XMR is on the way to your wallet.", fmt.Sprintf("No refund of %s; it has been claimed.", assetWord(r)), 0, 0
	case swap.PDone:
		return "Done. XMR is yours.", "", 0, 0
	case swap.PFailed:
		if t1 > 0 {
			return fmt.Sprintf("Something went wrong. Your %s can be refunded after %s.", assetWord(r), formatCT(int64(t1))),
				refundLine(r, t1, t2, grace), int64(t1), int64(t1 + grace)
		}
		return "This trade did not start. Nothing was locked.", "Nothing is locked.", 0, 0
	default:
		return fmt.Sprintf("Working (%s).", r.Phase), refundLine(r, t1, t2, grace), int64(t1), int64(t1 + grace)
	}
}

func refundLine(r swap.Record, t1, t2, grace uint64) string {
	w := assetWord(r)
	if t1 == 0 {
		return fmt.Sprintf("If the other side does not show up, your %s is returned automatically.", w)
	}
	back := t1 + grace
	if grace == 0 {
		back = t2
	}
	return fmt.Sprintf("If the other side does not show up, your %s is returned automatically. You can refund after %s. %s should be back around %s.",
		w, formatCT(int64(t1)), w, formatCT(int64(back)))
}

func formatCT(unix int64) string {
	if unix == 0 {
		return ""
	}
	loc, err := time.LoadLocation(ctZone)
	if err != nil {
		loc = time.FixedZone("CT", -6*3600)
	}
	t := time.Unix(unix, 0).In(loc)
	return t.Format("3:04 PM MST, Mon Jan 2")
}

func bigStr(x *big.Int) string {
	if x == nil {
		return ""
	}
	return x.String()
}

func weiToETH(x *big.Int) string {
	if x == nil {
		return "0"
	}
	return formatUnits(x, 18)
}

func picoToXMR(p uint64) string {
	return formatUnits(new(big.Int).SetUint64(p), 12)
}

func formatUnits(x *big.Int, dec int) string {
	s := x.String()
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if len(s) <= dec {
		s = "0." + padLeft(s, dec)
	} else {
		s = s[:len(s)-dec] + "." + s[len(s)-dec:]
	}
	s = trimTrailingZeros(s)
	if neg {
		s = "-" + s
	}
	return s
}

func padLeft(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

func trimTrailingZeros(s string) string {
	if !containsDot(s) {
		return s
	}
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

func containsDot(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return true
		}
	}
	return false
}
