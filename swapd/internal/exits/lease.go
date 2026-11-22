// Package exits is the wallet's and the seller kit's side of paid Session
// Router exits: ExitLease (buy hours on an exit listed in SeshRegistry with
// tag "exit"), the exit RPC (MapExit/UnmapExit) and the seller's signed
// admissions. Testnet only.
package exits

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// TagExit is the SeshRegistry tag for exit listings.
const TagExit = "exit"

var (
	selSetOffer    = eth.Selector("setOffer(uint256,address,uint128,uint128,uint32)")
	selClearOffer  = eth.Selector("clearOffer(uint256)")
	selBuy         = eth.Selector("buy(uint256,address,uint32,uint256,string)")
	selQuote       = eth.Selector("quote(uint256,address,uint32)")
	selOffers      = eth.Selector("offers(uint256)")
	selActiveUntil = eth.Selector("activeUntil(uint256,string)")
	selLeaseCount  = eth.Selector("leaseCount()")
	selUSDC        = eth.Selector("usdc()")
	selRegistry    = eth.Selector("registry()")

	// TopicLeased is Leased(leaseId, listingId, buyer, payout, asset, amount, hoursBought, start, end, buyerSesh).
	TopicLeased = eth.Keccak32([]byte("Leased(uint256,uint256,address,address,address,uint256,uint32,uint64,uint64,string)"))
	// TopicOfferSet is OfferSet(listingId, owner, payout, weiPerHour, usdcPerHour, maxHours).
	TopicOfferSet = eth.Keccak32([]byte("OfferSet(uint256,address,address,uint128,uint128,uint32)"))
)

func abiString(s string) []byte {
	b := eth.WordU(uint64(len(s)))
	p := []byte(s)
	if r := len(p) % 32; r != 0 {
		p = append(p, make([]byte, 32-r)...)
	}
	return append(b, p...)
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// EncodeSetOffer: the listing owner sets payout, prices and max hours per purchase.
func EncodeSetOffer(listingID uint64, payout eth.Address, weiPerHour, usdcPerHour *big.Int, maxHours uint32) []byte {
	return cat(selSetOffer, eth.WordU(listingID), eth.WordAddr(payout), eth.WordBig(orZero(weiPerHour)),
		eth.WordBig(orZero(usdcPerHour)), eth.WordU(uint64(maxHours)))
}

// EncodeClearOffer takes the exit off sale (the registry listing stays).
func EncodeClearOffer(listingID uint64) []byte { return cat(selClearOffer, eth.WordU(listingID)) }

// EncodeBuy buys hours. asset zero = ETH (send expected as value).
func EncodeBuy(listingID uint64, asset eth.Address, hours uint32, expected *big.Int, buyerSesh string) []byte {
	return cat(selBuy, eth.WordU(listingID), eth.WordAddr(asset), eth.WordU(uint64(hours)),
		eth.WordBig(orZero(expected)), eth.WordU(5*32), abiString(buyerSesh))
}

func orZero(x *big.Int) *big.Int {
	if x == nil {
		return new(big.Int)
	}
	return x
}

// Offer is ExitLease.offers(id).
type Offer struct {
	Owner       eth.Address `json:"owner"`
	Payout      eth.Address `json:"payout"`
	WeiPerHour  *big.Int    `json:"weiPerHour"`
	USDCPerHour *big.Int    `json:"usdcPerHour"`
	MaxHours    uint32      `json:"maxHours"`
}

// ForSale reports whether an offer was ever set (the contract re-checks the listing).
func (o *Offer) ForSale() bool { return o != nil && !o.Payout.IsZero() }

// Lease is one Leased event.
type Lease struct {
	LeaseID   uint64      `json:"leaseId"`
	ListingID uint64      `json:"listingId"`
	Buyer     eth.Address `json:"buyer"`
	Payout    eth.Address `json:"payout"`
	Asset     eth.Address `json:"asset"`
	Amount    string      `json:"amount"` // base units, decimal
	Hours     uint32      `json:"hours"`
	Start     uint64      `json:"start"`
	End       uint64      `json:"end"`
	BuyerSesh string      `json:"buyerSesh"`
	Block     uint64      `json:"block"`
	Tx        eth.Hash    `json:"tx"`
}

// Contract reads and encodes calls for one ExitLease.
type Contract struct {
	C    *eth.Client
	Addr eth.Address
}

func (k *Contract) call(ctx context.Context, data []byte) ([]byte, error) {
	a := k.Addr
	return k.C.EthCall(ctx, eth.CallMsg{To: &a, Data: data})
}

func word(b []byte, i int) []byte {
	if len(b) < (i+1)*32 {
		return make([]byte, 32)
	}
	return b[i*32 : (i+1)*32]
}
func wBig(b []byte, i int) *big.Int { return new(big.Int).SetBytes(word(b, i)) }
func wAddr(b []byte, i int) eth.Address {
	var a eth.Address
	copy(a[:], word(b, i)[12:])
	return a
}

// Offer reads offers(id).
func (k *Contract) Offer(ctx context.Context, listingID uint64) (*Offer, error) {
	b, err := k.call(ctx, cat(selOffers, eth.WordU(listingID)))
	if err != nil {
		return nil, err
	}
	if len(b) < 5*32 {
		return nil, errors.New("exitlease: short answer (is this an ExitLease contract?)")
	}
	return &Offer{Owner: wAddr(b, 0), Payout: wAddr(b, 1), WeiPerHour: wBig(b, 2), USDCPerHour: wBig(b, 3),
		MaxHours: uint32(wBig(b, 4).Uint64())}, nil
}

// Quote is the exact price for hours in asset; it fails if the exit isn't for sale.
func (k *Contract) Quote(ctx context.Context, listingID uint64, asset eth.Address, hours uint32) (*big.Int, error) {
	b, err := k.call(ctx, cat(selQuote, eth.WordU(listingID), eth.WordAddr(asset), eth.WordU(uint64(hours))))
	if err != nil {
		return nil, explainRevert(err)
	}
	if len(b) < 32 {
		return nil, errors.New("exitlease: short answer")
	}
	return wBig(b, 0), nil
}

// ActiveUntil is the end of the paid time for buyerSesh on the listing (0 = never bought).
func (k *Contract) ActiveUntil(ctx context.Context, listingID uint64, buyerSesh string) (uint64, error) {
	b, err := k.call(ctx, cat(selActiveUntil, eth.WordU(listingID), eth.WordU(64), abiString(buyerSesh)))
	if err != nil {
		return 0, err
	}
	return wBig(b, 0).Uint64(), nil
}

// USDC is the token the contract accepts besides ETH (zero = none).
func (k *Contract) USDC(ctx context.Context) (eth.Address, error) {
	b, err := k.call(ctx, selUSDC)
	if err != nil {
		return eth.Address{}, err
	}
	return wAddr(b, 0), nil
}

// Registry is the SeshRegistry the contract checks listings against.
func (k *Contract) Registry(ctx context.Context) (eth.Address, error) {
	b, err := k.call(ctx, selRegistry)
	if err != nil {
		return eth.Address{}, err
	}
	return wAddr(b, 0), nil
}

func topicWord(x uint64) string { return "0x" + hex.EncodeToString(eth.WordU(x)) }
func topicAddr(a eth.Address) string {
	return "0x" + hex.EncodeToString(eth.WordAddr(a))
}

// Leases returns Leased events in [from, to], optionally only for one listing
// (listingID > 0) and/or one buyer address.
func (k *Contract) Leases(ctx context.Context, from, to, listingID uint64, buyer *eth.Address) ([]Lease, error) {
	topics := []any{"0x" + hex.EncodeToString(TopicLeased[:]), nil}
	if listingID > 0 || buyer != nil {
		if listingID > 0 {
			topics = append(topics, topicWord(listingID))
		} else {
			topics = append(topics, nil)
		}
		if buyer != nil {
			topics = append(topics, topicAddr(*buyer))
		}
	}
	logs, err := k.C.Logs(ctx, k.Addr, from, to, topics...)
	if err != nil {
		return nil, err
	}
	var out []Lease
	for i := range logs {
		l, err := DecodeLeased(&logs[i])
		if err != nil {
			continue
		}
		out = append(out, *l)
	}
	return out, nil
}

// DecodeLeased parses one Leased log.
func DecodeLeased(l *eth.Log) (*Lease, error) {
	if l.Removed || len(l.Topics) < 4 || l.Topics[0] != eth.Hash(TopicLeased) {
		return nil, errors.New("not a Leased log")
	}
	d := l.Data()
	if len(d) < 7*32 {
		return nil, errors.New("short Leased log")
	}
	off := int(wBig(d, 6).Int64())
	s, err := decodeString(d, off)
	if err != nil {
		return nil, err
	}
	var buyer eth.Address
	copy(buyer[:], l.Topics[3][12:])
	return &Lease{
		LeaseID:   new(big.Int).SetBytes(l.Topics[1][:]).Uint64(),
		ListingID: new(big.Int).SetBytes(l.Topics[2][:]).Uint64(),
		Buyer:     buyer,
		Payout:    wAddr(d, 0),
		Asset:     wAddr(d, 1),
		Amount:    wBig(d, 2).String(),
		Hours:     uint32(wBig(d, 3).Uint64()),
		Start:     wBig(d, 4).Uint64(),
		End:       wBig(d, 5).Uint64(),
		BuyerSesh: s,
		Block:     l.Block(),
		Tx:        l.TxHash,
	}, nil
}

func decodeString(b []byte, offset int) (string, error) {
	// offset and n come from an RPC log: compare without overflowing.
	if offset < 0 || offset > len(b)-32 {
		return "", errors.New("bad string offset")
	}
	nb := new(big.Int).SetBytes(b[offset : offset+32])
	if !nb.IsInt64() {
		return "", errors.New("bad string length")
	}
	n := int(nb.Int64())
	if n < 0 || n > len(b)-offset-32 {
		return "", errors.New("bad string length")
	}
	return string(b[offset+32 : offset+32+n]), nil
}

// Contract custom errors, by selector, in plain English.
var revertMsgs = map[string]string{
	hex.EncodeToString(eth.Selector("NoOffer()")):          "this exit isn't for sale right now",
	hex.EncodeToString(eth.Selector("NotExit()")):          "this listing isn't an active exit (it may be leaving or removed)",
	hex.EncodeToString(eth.Selector("AssetNotAccepted()")): "this exit doesn't take that currency",
	hex.EncodeToString(eth.Selector("TooManyHours()")):     "that's more hours than this exit sells at once",
	hex.EncodeToString(eth.Selector("WrongAmount()")):      "the price changed; get a new price and try again",
	hex.EncodeToString(eth.Selector("BadSesh()")):          "your Session Router address isn't valid",
	hex.EncodeToString(eth.Selector("BadParams()")):        "invalid number of hours or price",
	hex.EncodeToString(eth.Selector("TransferFailed()")):   "the payment didn't go through (check your balance and approval)",
	hex.EncodeToString(eth.Selector("NotOwner()")):         "only the listing's owner can do that",
}

func explainRevert(err error) error {
	if err == nil {
		return nil
	}
	s := strings.ToLower(err.Error())
	for sel, msg := range revertMsgs {
		if strings.Contains(s, sel) {
			return fmt.Errorf("%s", msg)
		}
	}
	return err
}

// ExplainRevert turns an ExitLease revert into a plain-English message.
func ExplainRevert(err error) error { return explainRevert(err) }

// ValidClientSesh mirrors the contract's check: 52 z-base32 chars + ".sesh".
func ValidClientSesh(s string) bool {
	if len(s) != 57 || !strings.HasSuffix(s, ".sesh") {
		return false
	}
	for _, c := range s[:52] {
		ok := (c >= 'a' && c <= 'z' && c != 'l' && c != 'v') || c == '1' || (c >= '3' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}
