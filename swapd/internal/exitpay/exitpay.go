// Package exitpay pays for exit time with the proceeds of a swap claim: the
// claim goes to an ExitPay forwarder address whose constructor arguments are
// the lease target (see contracts/src/exits/ExitPay.sol).
package exitpay

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Target is one lease to buy. It is fixed when the swap is agreed and stored
// in the swap record; the claim pays to Target.Address.
type Target struct {
	Factory   eth.Address `json:"factory"`
	Asset     eth.Address `json:"asset"`    // zero = ETH, else the ExitLease USDC
	RefundTo  eth.Address `json:"refundTo"` // change, or everything if the lease can't be bought
	ListingID uint64      `json:"listingId"`
	Hours     uint32      `json:"hours"`
	MaxCost   *big.Int    `json:"maxCost"`
	Ref       eth.Hash    `json:"ref"`
	BuyerSesh string      `json:"buyerSesh"`
	Address   eth.Address `json:"address"` // the forwarder (from the factory)
}

const tupleSig = "(address,address,uint256,uint32,uint256,bytes32,string)"

var (
	selPayAddress = eth.Selector("payAddress" + "(" + tupleSig + ")")
	selDeploy     = eth.Selector("deploy" + "(" + tupleSig + ")")
	selSettle     = eth.Selector("settle" + "(" + tupleSig + ")")
)

// Ref is the agreement reference for a swap session: each swap gets its own
// forwarder even when everything else matches.
func Ref(session string) eth.Hash {
	return eth.Hash(eth.Keccak32([]byte("xmrswap-exitpay|" + session)))
}

func abiString(s string) []byte {
	b := eth.WordU(uint64(len(s)))
	p := []byte(s)
	if r := len(p) % 32; r != 0 {
		p = append(p, make([]byte, 32-r)...)
	}
	return append(b, p...)
}

func (t *Target) tuple() []byte {
	mc := t.MaxCost
	if mc == nil {
		mc = new(big.Int)
	}
	var out []byte
	out = append(out, eth.WordU(32)...) // offset of the (dynamic) tuple
	out = append(out, eth.WordAddr(t.Asset)...)
	out = append(out, eth.WordAddr(t.RefundTo)...)
	out = append(out, eth.WordU(t.ListingID)...)
	out = append(out, eth.WordU(uint64(t.Hours))...)
	out = append(out, eth.WordBig(mc)...)
	out = append(out, t.Ref[:]...)
	out = append(out, eth.WordU(7*32)...)
	return append(out, abiString(t.BuyerSesh)...)
}

func EncodePayAddress(t *Target) []byte { return append(append([]byte{}, selPayAddress...), t.tuple()...) }
func EncodeDeploy(t *Target) []byte     { return append(append([]byte{}, selDeploy...), t.tuple()...) }
func EncodeSettle(t *Target) []byte     { return append(append([]byte{}, selSettle...), t.tuple()...) }

// Check rejects targets the factory would refuse. ExitPay.deploy reverts on
// these, so the forwarder address could never get code and anything paid to
// it would be stuck: never compute, quote or pay to such a target.
func (t *Target) Check() error {
	if t == nil {
		return errors.New("exit pay: no target")
	}
	switch {
	case t.Factory.IsZero():
		return errors.New("exit pay: no factory")
	case t.RefundTo.IsZero():
		return errors.New("exit pay: no refund address")
	case t.Hours == 0:
		return errors.New("exit pay: hours must be at least 1")
	case t.MaxCost == nil || t.MaxCost.Sign() <= 0:
		return errors.New("exit pay: max cost must be set")
	case !eth.FitsU256(t.MaxCost):
		return errors.New("exit pay: max cost out of range")
	case !strings.HasSuffix(t.BuyerSesh, ".sesh") || len(t.BuyerSesh) != 57:
		return fmt.Errorf("exit pay: bad buyer address %q", t.BuyerSesh)
	}
	return nil
}

// Resolve asks the factory for the forwarder address and stores it in t.
func (t *Target) Resolve(ctx context.Context, c *eth.Client) error {
	if err := t.Check(); err != nil {
		return err
	}
	out, err := c.EthCall(ctx, eth.CallMsg{To: &t.Factory, Data: EncodePayAddress(t)})
	if err != nil {
		return err
	}
	if len(out) != 32 {
		return fmt.Errorf("exit pay: bad payAddress reply (%d bytes)", len(out))
	}
	copy(t.Address[:], out[12:])
	if t.Address.IsZero() {
		return errors.New("exit pay: factory returned the zero address")
	}
	return nil
}

// CheckPay reports whether a claim may pay to `to` for this target: the
// target must be deployable and `to` must be its resolved forwarder.
func (t *Target) CheckPay(to eth.Address) error {
	if err := t.Check(); err != nil {
		return err
	}
	if t.Address.IsZero() {
		return errors.New("exit pay: forwarder address not resolved")
	}
	if to != t.Address {
		return errors.New("exit pay: payout address is not the forwarder")
	}
	return nil
}

// Deployed reports whether the forwarder exists yet.
func Deployed(ctx context.Context, c *eth.Client, a eth.Address) (bool, error) {
	var code string
	if err := c.Call(ctx, &code, "eth_getCode", a.Hex(), "latest"); err != nil {
		return false, err
	}
	return code != "" && code != "0x", nil
}

// Done reports whether the forwarder has bought its lease.
func Done(ctx context.Context, c *eth.Client, a eth.Address) (bool, error) {
	out, err := c.EthCall(ctx, eth.CallMsg{To: &a, Data: eth.Selector("done()")})
	if err != nil {
		return false, err
	}
	return len(out) == 32 && out[31] == 1, nil
}
