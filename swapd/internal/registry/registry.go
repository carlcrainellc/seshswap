// Package registry talks to SeshRegistry (contracts/src/registry): the
// bonded, permissionless on-chain directory of .sesh addresses. It builds
// and decodes calls, signs maker commitments and acks (EIP-712), and keeps a
// local index of listings built from the contract's events, so search
// happens on this computer and never asks a server.
//
// Test-only. Not audited. Testnets only.
package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Listing status, same numbers as SeshRegistry.Status.
const (
	StatusNone uint8 = iota
	StatusActive
	StatusExiting
	StatusRemoved
	StatusWithdrawn
)

// Commit stage, same numbers as SeshRegistry.CommitStage.
const (
	CommitNone uint8 = iota
	CommitOpen
	CommitAcked
	CommitReleased
	CommitSlashed
)

// TagMaker is the one tag with special rules (larger bond, quote cap, slash).
const TagMaker = "maker"

func StatusName(s uint8) string {
	switch s {
	case StatusActive:
		return "active"
	case StatusExiting:
		return "leaving"
	case StatusRemoved:
		return "removed"
	case StatusWithdrawn:
		return "withdrawn"
	}
	return "unknown"
}

// Tag packs a short tag into bytes32 (Solidity `bytes32 tag = "maker"`).
func Tag(s string) (eth.Hash, error) {
	var h eth.Hash
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 32 {
		return h, errors.New("tag must be 1 to 32 characters")
	}
	copy(h[:], s)
	return h, nil
}

// TagString unpacks a bytes32 tag.
func TagString(h eth.Hash) string { return string(bytes.TrimRight(h[:], "\x00")) }

// ValidSesh mirrors the contract's name rule: "<label>.sesh", label 1-63 of
// [a-z0-9-], no leading or trailing '-'.
func ValidSesh(n string) bool {
	if !strings.HasSuffix(n, ".sesh") {
		return false
	}
	l := strings.TrimSuffix(n, ".sesh")
	if len(l) < 1 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, c := range l {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Commitment mirrors SeshRegistry.Commitment.
type Commitment struct {
	ListingID    uint64      `json:"listingId"`
	Taker        eth.Address `json:"taker"`
	Claimer      eth.Address `json:"claimer"`
	ClaimKey     *big.Int    `json:"claimKey"`
	Asset        eth.Address `json:"asset"`
	Value        *big.Int    `json:"value"`
	XMRAtomic    uint64      `json:"xmrAtomic"`
	Penalty      *big.Int    `json:"penalty"`
	AttestWindow uint64      `json:"attestWindow"`
	Expiry       uint64      `json:"expiry"`
}

func (c *Commitment) words() []byte {
	var b []byte
	b = append(b, eth.WordU(c.ListingID)...)
	b = append(b, eth.WordAddr(c.Taker)...)
	b = append(b, eth.WordAddr(c.Claimer)...)
	b = append(b, eth.WordBig(c.ClaimKey)...)
	b = append(b, eth.WordAddr(c.Asset)...)
	b = append(b, eth.WordBig(c.Value)...)
	b = append(b, eth.WordU(c.XMRAtomic)...)
	b = append(b, eth.WordBig(c.Penalty)...)
	b = append(b, eth.WordU(c.AttestWindow)...)
	b = append(b, eth.WordU(c.Expiry)...)
	return b
}

// SignedCommitment is what a registered maker returns with a swap.
type SignedCommitment struct {
	Registry   eth.Address `json:"registry"`
	Commitment Commitment  `json:"commitment"`
	Sig        eth.Sig     `json:"sig"`
}

var (
	domainTypeHash     = eth.Keccak([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	commitmentTypeHash = eth.Keccak([]byte("Commitment(uint256 listingId,address taker,address claimer,uint256 claimKey,address asset,uint256 value,uint256 xmrAtomic,uint256 penalty,uint64 attestWindow,uint64 expiry)"))
	ackTypeHash        = eth.Keccak([]byte("RegistryAck(bytes32 swapId,uint256 listingId)"))
)

func domain(chainID *big.Int, reg eth.Address) []byte {
	return eth.Keccak(domainTypeHash, eth.Keccak([]byte("SeshRegistry")), eth.Keccak([]byte("1")), eth.WordBig(chainID), eth.WordAddr(reg))
}

func CommitmentDigest(chainID *big.Int, reg eth.Address, c *Commitment) []byte {
	return eth.Keccak([]byte{0x19, 0x01}, domain(chainID, reg), eth.Keccak(commitmentTypeHash, c.words()))
}

func AckDigest(chainID *big.Int, reg eth.Address, swapID eth.Hash, listingID uint64) []byte {
	return eth.Keccak([]byte{0x19, 0x01}, domain(chainID, reg), eth.Keccak(ackTypeHash, swapID[:], eth.WordU(listingID)))
}

func SignCommitment(k *eth.Key, chainID *big.Int, reg eth.Address, c *Commitment) eth.Sig {
	return eth.SignDigest(k, CommitmentDigest(chainID, reg, c))
}

func SignAck(k *eth.Key, chainID *big.Int, reg eth.Address, swapID eth.Hash, listingID uint64) eth.Sig {
	return eth.SignDigest(k, AckDigest(chainID, reg, swapID, listingID))
}

// Signer recovers who signed a commitment.
func (sc *SignedCommitment) Signer(chainID *big.Int) (eth.Address, error) {
	// The commitment comes from the maker: its numbers must fit a uint256
	// before they are ABI-encoded (eth.WordBig panics otherwise).
	c := &sc.Commitment
	for _, x := range []*big.Int{c.ClaimKey, c.Value, c.Penalty} {
		if !eth.FitsU256(x) {
			return eth.Address{}, errors.New("commitment has a missing or out-of-range number")
		}
	}
	return eth.Recover(CommitmentDigest(chainID, sc.Registry, &sc.Commitment), sc.Sig.V-27, sc.Sig.R, sc.Sig.S)
}

// CheckAgainstSwap verifies the commitment binds exactly this V3 swap.
func (sc *SignedCommitment) CheckAgainstSwap(s *eth.Swap) error {
	c := &sc.Commitment
	switch {
	case c.Taker != s.Owner:
		return errors.New("commitment names a different taker")
	case c.Claimer != s.Claimer:
		return errors.New("commitment names a different claimer")
	case c.ClaimKey == nil || c.ClaimKey.Cmp(s.ClaimKey) != 0:
		return errors.New("commitment names a different Monero key")
	case c.Asset != s.Asset:
		return errors.New("commitment names a different asset")
	case c.Value == nil || c.Value.Cmp(s.Value) != 0:
		return errors.New("commitment names a different amount")
	}
	return nil
}

// ---------------------------------------------------------------- calldata

var (
	selRegister    = eth.Selector("register(string,bytes32,string)")
	selTopUp       = eth.Selector("topUp(uint256)")
	selUpdateMeta  = eth.Selector("updateMeta(uint256,string)")
	selRequestExit = eth.Selector("requestExit(uint256)")
	selCancelExit  = eth.Selector("cancelExit(uint256)")
	selWithdraw    = eth.Selector("withdraw(uint256,address)")
	selCommit      = eth.Selector("commit((address,address,uint256,uint256,uint64,uint64,uint64,address,uint256,uint256,bytes32),(uint256,address,address,uint256,address,uint256,uint256,uint256,uint64,uint64),uint8,bytes32,bytes32)")
	selAck         = eth.Selector("ack(bytes32,uint8,bytes32,bytes32)")
	selRelease     = eth.Selector("release(bytes32)")
	selSlash       = eth.Selector("slash(bytes32)")
	selChallenge   = eth.Selector("challenge(uint256)")
	selAnswer      = eth.Selector("answer(uint256)")
	selResolve     = eth.Selector("resolveChallenge(uint256)")
	selInfo        = eth.Selector("info(uint256)")
	selCommits     = eth.Selector("commits(bytes32)")
	selReqPenalty  = eth.Selector("requiredPenalty(address,uint256)")
	selMakerMin    = eth.Selector("makerMinBond()")
	selSiteMin     = eth.Selector("siteMinBond()")
	selCapFactor   = eth.Selector("capFactor()")
	selExitDelay   = eth.Selector("exitDelay()")
	selMinAttest   = eth.Selector("minAttest()")
	selCount       = eth.Selector("listingCount()")
	selEthValue    = eth.Selector("ethValue(address,uint256)")
)

func abiString(s string) []byte {
	b := eth.WordU(uint64(len(s)))
	p := []byte(s)
	if r := len(p) % 32; r != 0 {
		p = append(p, make([]byte, 32-r)...)
	}
	return append(b, p...)
}

func EncodeRegister(sesh string, tag eth.Hash, meta string) []byte {
	a, m := abiString(sesh), abiString(meta)
	b := append([]byte{}, selRegister...)
	b = append(b, eth.WordU(96)...)
	b = append(b, tag[:]...)
	b = append(b, eth.WordU(uint64(96+len(a)))...)
	return append(append(b, a...), m...)
}

func EncodeUpdateMeta(id uint64, meta string) []byte {
	b := append(append([]byte{}, selUpdateMeta...), eth.WordU(id)...)
	return append(append(b, eth.WordU(64)...), abiString(meta)...)
}

func encID(sel []byte, id uint64) []byte { return append(append([]byte{}, sel...), eth.WordU(id)...) }
func EncodeTopUp(id uint64) []byte       { return encID(selTopUp, id) }
func EncodeRequestExit(id uint64) []byte { return encID(selRequestExit, id) }
func EncodeCancelExit(id uint64) []byte  { return encID(selCancelExit, id) }
func EncodeChallenge(id uint64) []byte   { return encID(selChallenge, id) }
func EncodeAnswer(id uint64) []byte      { return encID(selAnswer, id) }
func EncodeResolve(id uint64) []byte     { return encID(selResolve, id) }
func EncodeWithdraw(id uint64, to eth.Address) []byte {
	return append(encID(selWithdraw, id), eth.WordAddr(to)...)
}
func EncodeCommit(s *eth.Swap, sc *SignedCommitment) []byte {
	b := append(append([]byte{}, selCommit...), s.Words()...)
	return append(append(b, sc.Commitment.words()...), sc.Sig.Words()...)
}
func EncodeAck(swapID eth.Hash, g eth.Sig) []byte {
	return append(append(append([]byte{}, selAck...), swapID[:]...), g.Words()...)
}
func EncodeRelease(swapID eth.Hash) []byte {
	return append(append([]byte{}, selRelease...), swapID[:]...)
}
func EncodeSlash(swapID eth.Hash) []byte { return append(append([]byte{}, selSlash...), swapID[:]...) }

// ---------------------------------------------------------------- views

// Client reads one SeshRegistry.
type Client struct {
	C    *eth.Client
	Addr eth.Address
}

func (r *Client) call(ctx context.Context, data []byte) ([]byte, error) {
	a := r.Addr
	return r.C.EthCall(ctx, eth.CallMsg{To: &a, Data: data})
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

func (r *Client) uintView(ctx context.Context, sel []byte) (*big.Int, error) {
	b, err := r.call(ctx, sel)
	if err != nil {
		return nil, err
	}
	if len(b) < 32 {
		return nil, errors.New("registry: short answer (is this a SeshRegistry?)")
	}
	return wBig(b, 0), nil
}

// Info is SeshRegistry.info(id).
type Info struct {
	Owner      eth.Address `json:"owner"`
	Status     uint8       `json:"status"`
	Tag        string      `json:"tag"`
	Bond       *big.Int    `json:"bond"`
	Reserved   *big.Int    `json:"reserved"`
	ExitAt     uint64      `json:"exitAt"`
	Challenger eth.Address `json:"challenger"`
	MaxQuote   *big.Int    `json:"maxQuote"`
}

func (r *Client) Info(ctx context.Context, id uint64) (*Info, error) {
	b, err := r.call(ctx, encID(selInfo, id))
	if err != nil {
		return nil, err
	}
	if len(b) < 8*32 {
		return nil, errors.New("registry: short info answer")
	}
	var tag eth.Hash
	copy(tag[:], word(b, 2))
	return &Info{Owner: wAddr(b, 0), Status: uint8(wBig(b, 1).Uint64()), Tag: TagString(tag), Bond: wBig(b, 3),
		Reserved: wBig(b, 4), ExitAt: wBig(b, 5).Uint64(), Challenger: wAddr(b, 6), MaxQuote: wBig(b, 7)}, nil
}

// CommitState is SeshRegistry.commits(swapId).
type CommitState struct {
	Stage     uint8
	AttestBy  uint64
	ListingID uint64
	Taker     eth.Address
	Penalty   *big.Int
}

func (r *Client) Commit(ctx context.Context, swapID eth.Hash) (*CommitState, error) {
	b, err := r.call(ctx, append(append([]byte{}, selCommits...), swapID[:]...))
	if err != nil {
		return nil, err
	}
	if len(b) < 5*32 {
		return nil, errors.New("registry: short commits answer")
	}
	return &CommitState{Stage: uint8(wBig(b, 0).Uint64()), AttestBy: wBig(b, 1).Uint64(), ListingID: wBig(b, 2).Uint64(),
		Taker: wAddr(b, 3), Penalty: wBig(b, 4)}, nil
}

func (r *Client) RequiredPenalty(ctx context.Context, asset eth.Address, value *big.Int) (*big.Int, error) {
	b, err := r.call(ctx, append(append(append([]byte{}, selReqPenalty...), eth.WordAddr(asset)...), eth.WordBig(value)...))
	if err != nil {
		return nil, err
	}
	return wBig(b, 0), nil
}

func (r *Client) EthValue(ctx context.Context, asset eth.Address, value *big.Int) (*big.Int, error) {
	b, err := r.call(ctx, append(append(append([]byte{}, selEthValue...), eth.WordAddr(asset)...), eth.WordBig(value)...))
	if err != nil {
		return nil, err
	}
	return wBig(b, 0), nil
}

// Params are the constructor's placeholder amounts, read back from the chain.
type Params struct {
	SiteMinBond  *big.Int `json:"siteMinBond"`
	MakerMinBond *big.Int `json:"makerMinBond"`
	CapFactor    *big.Int `json:"capFactor"`
	ExitDelay    uint64   `json:"exitDelay"`
	MinAttest    uint64   `json:"minAttest"`
}

func (r *Client) Params(ctx context.Context) (*Params, error) {
	var p Params
	var err error
	if p.SiteMinBond, err = r.uintView(ctx, selSiteMin); err != nil {
		return nil, err
	}
	if p.MakerMinBond, err = r.uintView(ctx, selMakerMin); err != nil {
		return nil, err
	}
	if p.CapFactor, err = r.uintView(ctx, selCapFactor); err != nil {
		return nil, err
	}
	x, err := r.uintView(ctx, selExitDelay)
	if err != nil {
		return nil, err
	}
	p.ExitDelay = x.Uint64()
	if x, err = r.uintView(ctx, selMinAttest); err != nil {
		return nil, err
	}
	p.MinAttest = x.Uint64()
	return &p, nil
}

func (r *Client) Count(ctx context.Context) (uint64, error) {
	x, err := r.uintView(ctx, selCount)
	if err != nil {
		return 0, err
	}
	return x.Uint64(), nil
}

// ---------------------------------------------------------------- events

var (
	TopicListed      = eth.Keccak32([]byte("Listed(uint256,address,bytes32,string,string,uint256)"))
	TopicMetaUpdated = eth.Keccak32([]byte("MetaUpdated(uint256,string)"))
	TopicToppedUp    = eth.Keccak32([]byte("ToppedUp(uint256,uint256,uint256)"))
	TopicExitReq     = eth.Keccak32([]byte("ExitRequested(uint256,uint64)"))
	TopicExitCancel  = eth.Keccak32([]byte("ExitCancelled(uint256)"))
	TopicWithdrawn   = eth.Keccak32([]byte("Withdrawn(uint256,address,uint256)"))
	TopicChallenged  = eth.Keccak32([]byte("Challenged(uint256,address,uint64)"))
	TopicAnswered    = eth.Keccak32([]byte("ChallengeAnswered(uint256,address,uint256)"))
	TopicUpheld      = eth.Keccak32([]byte("ChallengeUpheld(uint256,address,uint256)"))
	TopicCommitted   = eth.Keccak32([]byte("Committed(bytes32,uint256,address,uint256,uint256,uint64)"))
	TopicAcked       = eth.Keccak32([]byte("Acked(bytes32,uint256)"))
	TopicReleased    = eth.Keccak32([]byte("Released(bytes32,uint256)"))
	TopicSlashed     = eth.Keccak32([]byte("Slashed(bytes32,uint256,address,uint256)"))
)

func decodeString(b []byte, offset int) (string, error) {
	// offset and n come from untrusted data: compare without overflowing.
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

func topicU(h eth.Hash) uint64 { return new(big.Int).SetBytes(h[:]).Uint64() }

// FormatWei renders wei as ETH without trailing zeros.
func FormatWei(x *big.Int) string {
	if x == nil {
		return "0"
	}
	s := fmt.Sprintf("%019s", x.String())
	i, f := s[:len(s)-18], strings.TrimRight(s[len(s)-18:], "0")
	i = strings.TrimLeft(i, "0")
	if i == "" {
		i = "0"
	}
	if f == "" {
		return i
	}
	return i + "." + f
}
