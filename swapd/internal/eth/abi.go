package eth

import (
	"errors"
	"math/big"
)

// Swap mirrors XmrSwapV3.Swap. Keys are ed25519 points as little-endian integers.
type Swap struct {
	Owner     Address  `json:"owner"`
	Claimer   Address  `json:"claimer"`
	ClaimKey  *big.Int `json:"claimKey"`
	RefundKey *big.Int `json:"refundKey"`
	T1        uint64   `json:"t1"`
	T2        uint64   `json:"t2"`
	Grace     uint64   `json:"grace"` // how long an intent freezes the other side
	Asset     Address  `json:"asset"`
	Value     *big.Int `json:"value"`
	Premium   *big.Int `json:"premium"` // native coin; nil/0 = none
	Salt      Hash     `json:"salt"`
}

// PremiumOrZero returns the premium, never nil.
func (s *Swap) PremiumOrZero() *big.Int {
	if s.Premium == nil {
		return new(big.Int)
	}
	return s.Premium
}

const swapTuple = "(address,address,uint256,uint256,uint64,uint64,uint64,address,uint256,uint256,bytes32)"

var (
	selLock         = selector("lock(address,uint256,uint256,uint64,uint64,uint64,address,uint256,uint256,bytes32)")
	selCancelIntent = selector("cancelIntent(" + swapTuple + ")")
	selClaimIntent  = selector("claimIntent(" + swapTuple + ",uint8,bytes32,bytes32)")
	selAckLocked    = selector("ackLocked(" + swapTuple + ",address,uint8,bytes32,bytes32)")
	selSetReady     = selector("setReady(" + swapTuple + ")")
	selRefund       = selector("refund(" + swapTuple + ",uint256)")
	selClaim        = selector("claim(" + swapTuple + ",uint256,address)")
	selClaimRelayed = selector("claimRelayed(" + swapTuple + ",uint256,address,address,uint256,uint8,bytes32,bytes32)")
	selStatus       = selector("status(bytes32)")
	selWithdraw     = selector("withdraw(address,address)")
	selApprove      = selector("approve(address,uint256)")
	selBalanceOf    = selector("balanceOf(address)")
	selOwed         = selector("owed(address,address)")

	TopicLocked       = Keccak32([]byte("Locked(bytes32," + swapTuple + ")"))
	TopicReady        = Keccak32([]byte("Ready(bytes32)"))
	TopicClaimed      = Keccak32([]byte("Claimed(bytes32,uint256,address,address,uint256)"))
	TopicRefunded     = Keccak32([]byte("Refunded(bytes32,uint256)"))
	TopicClaimIntent  = Keccak32([]byte("ClaimIntent(bytes32,uint64)"))
	TopicCancelIntent = Keccak32([]byte("CancelIntent(bytes32,uint64)"))
	TopicAcked        = Keccak32([]byte("Acked(bytes32,address)"))
	TopicPremiumPaid  = Keccak32([]byte("PremiumPaid(bytes32,address,uint256)"))
)

func selector(sig string) []byte { return Keccak([]byte(sig))[:4] }

func wAddr(a Address) []byte { w := make([]byte, 32); copy(w[12:], a[:]); return w }
func wU(x uint64) []byte     { return u256(new(big.Int).SetUint64(x)) }

func (s *Swap) words() []byte {
	var b []byte
	b = append(b, wAddr(s.Owner)...)
	b = append(b, wAddr(s.Claimer)...)
	b = append(b, u256(s.ClaimKey)...)
	b = append(b, u256(s.RefundKey)...)
	b = append(b, wU(s.T1)...)
	b = append(b, wU(s.T2)...)
	b = append(b, wU(s.Grace)...)
	b = append(b, wAddr(s.Asset)...)
	b = append(b, u256(s.Value)...)
	b = append(b, u256(s.PremiumOrZero())...)
	b = append(b, s.Salt[:]...)
	return b
}

// CheckNumbers reports an error if a swap built from untrusted input has a
// missing, negative or over-256-bit number (words / ID would panic on it).
func (s *Swap) CheckNumbers() error {
	if s == nil || !FitsU256(s.ClaimKey) || !FitsU256(s.RefundKey) || !FitsU256(s.Value) ||
		(s.Premium != nil && !FitsU256(s.Premium)) {
		return errors.New("swap has a missing or out-of-range number")
	}
	return nil
}

// ID = keccak256(abi.encode(swap)), identical to XmrSwapV3.swapId.
func (s *Swap) ID() Hash { return Hash(Keccak32(s.words())) }

func decodeSwap(b []byte) (*Swap, error) {
	if len(b) < 11*32 {
		return nil, errors.New("short swap data")
	}
	w := func(i int) []byte { return b[i*32 : (i+1)*32] }
	addr := func(i int) Address { var a Address; copy(a[:], w(i)[12:]); return a }
	s := &Swap{
		Owner: addr(0), Claimer: addr(1),
		ClaimKey: new(big.Int).SetBytes(w(2)), RefundKey: new(big.Int).SetBytes(w(3)),
		T1: new(big.Int).SetBytes(w(4)).Uint64(), T2: new(big.Int).SetBytes(w(5)).Uint64(),
		Grace: new(big.Int).SetBytes(w(6)).Uint64(),
		Asset: addr(7), Value: new(big.Int).SetBytes(w(8)), Premium: new(big.Int).SetBytes(w(9)),
	}
	copy(s.Salt[:], w(10))
	return s, nil
}

func EncodeLock(claimer Address, claimKey, refundKey *big.Int, d1, d2, grace uint64, asset Address, value, premium *big.Int, salt Hash) []byte {
	if premium == nil {
		premium = new(big.Int)
	}
	b := append([]byte{}, selLock...)
	b = append(b, wAddr(claimer)...)
	b = append(b, u256(claimKey)...)
	b = append(b, u256(refundKey)...)
	b = append(b, wU(d1)...)
	b = append(b, wU(d2)...)
	b = append(b, wU(grace)...)
	b = append(b, wAddr(asset)...)
	b = append(b, u256(value)...)
	b = append(b, u256(premium)...)
	return append(b, salt[:]...)
}

func EncodeCancelIntent(s *Swap) []byte {
	return append(append([]byte{}, selCancelIntent...), s.words()...)
}

// Sig is an EIP-712 signature (v = 27/28).
type Sig struct {
	V byte `json:"v"`
	R Hash `json:"r"`
	S Hash `json:"s"`
}

func (g Sig) words() []byte { return append(append(wU(uint64(g.V)), g.R[:]...), g.S[:]...) }

func EncodeClaimIntent(s *Swap, g Sig) []byte {
	return append(append(append([]byte{}, selClaimIntent...), s.words()...), g.words()...)
}

func EncodeAckLocked(s *Swap, to Address, g Sig) []byte {
	b := append(append(append([]byte{}, selAckLocked...), s.words()...), wAddr(to)...)
	return append(b, g.words()...)
}

func EncodeSetReady(s *Swap) []byte { return append(append([]byte{}, selSetReady...), s.words()...) }

func EncodeRefund(s *Swap, secret *big.Int) []byte {
	return append(append(append([]byte{}, selRefund...), s.words()...), u256(secret)...)
}

func EncodeClaim(s *Swap, secret *big.Int, to Address) []byte {
	b := append(append([]byte{}, selClaim...), s.words()...)
	return append(append(b, u256(secret)...), wAddr(to)...)
}

// RelayedClaim is what the XMR seller hands to a relayer.
type RelayedClaim struct {
	Swap    *Swap    `json:"swap"`
	Secret  *big.Int `json:"secret"`
	To      Address  `json:"to"`
	Relayer Address  `json:"relayer"`
	Fee     *big.Int `json:"fee"`
	V       byte     `json:"v"` // 27/28
	R       Hash     `json:"r"`
	S       Hash     `json:"s"`
}

func EncodeClaimRelayed(c *RelayedClaim) []byte {
	b := append(append([]byte{}, selClaimRelayed...), c.Swap.words()...)
	b = append(b, u256(c.Secret)...)
	b = append(b, wAddr(c.To)...)
	b = append(b, wAddr(c.Relayer)...)
	b = append(b, u256(c.Fee)...)
	b = append(b, wU(uint64(c.V))...)
	b = append(b, c.R[:]...)
	return append(b, c.S[:]...)
}

func EncodeWithdraw(asset, to Address) []byte {
	return append(append(append([]byte{}, selWithdraw...), wAddr(asset)...), wAddr(to)...)
}

func EncodeApprove(spender Address, v *big.Int) []byte {
	return append(append(append([]byte{}, selApprove...), wAddr(spender)...), u256(v)...)
}

// ---------------------------------------------------------------- EIP-712

var (
	domainTypeHash = Keccak([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	claimTypeHash  = Keccak([]byte("Claim(bytes32 swapId,address to,address relayer,uint256 fee)"))
	intentTypeHash = Keccak([]byte("ClaimIntent(bytes32 swapId)"))
	ackTypeHash    = Keccak([]byte("AckLocked(bytes32 swapId,address premiumTo)"))
)

func DomainSeparator(chainID *big.Int, contract Address) []byte {
	return Keccak(domainTypeHash, Keccak([]byte("XmrSwapV3")), Keccak([]byte("1")), u256(chainID), wAddr(contract))
}

func IntentDigest(chainID *big.Int, contract Address, id Hash) []byte {
	return Keccak([]byte{0x19, 0x01}, DomainSeparator(chainID, contract), Keccak(intentTypeHash, id[:]))
}

func AckDigest(chainID *big.Int, contract Address, id Hash, to Address) []byte {
	return Keccak([]byte{0x19, 0x01}, DomainSeparator(chainID, contract), Keccak(ackTypeHash, id[:], wAddr(to)))
}

func signDigest(k *Key, d []byte) Sig {
	v, r, s := k.Sign(d)
	return Sig{V: v + 27, R: Hash(r), S: Hash(s)}
}

// SignIntent signs a claim intent (no secret in it) with the claimer key.
func SignIntent(k *Key, chainID *big.Int, contract Address, id Hash) Sig {
	return signDigest(k, IntentDigest(chainID, contract, id))
}

// SignAck signs an "XMR locked" acknowledgement naming where a forfeited premium goes.
func SignAck(k *Key, chainID *big.Int, contract Address, id Hash, to Address) Sig {
	return signDigest(k, AckDigest(chainID, contract, id, to))
}

// VerifyIntentSig checks a claim-intent signature against the swap's claimer.
func VerifyIntentSig(chainID *big.Int, contract Address, s *Swap, g Sig) error {
	if err := s.CheckNumbers(); err != nil {
		return err
	}
	if g.V != 27 && g.V != 28 {
		return errors.New("bad v")
	}
	a, err := Recover(IntentDigest(chainID, contract, s.ID()), g.V-27, [32]byte(g.R), [32]byte(g.S))
	if err != nil {
		return err
	}
	if a != s.Claimer {
		return errors.New("intent signature is not from the swap's claimer")
	}
	return nil
}

func ClaimDigest(chainID *big.Int, contract Address, id Hash, to, relayer Address, fee *big.Int) []byte {
	sh := Keccak(claimTypeHash, id[:], wAddr(to), wAddr(relayer), u256(fee))
	return Keccak([]byte{0x19, 0x01}, DomainSeparator(chainID, contract), sh)
}

// SignClaim builds a relayed claim signed by the claimer key.
func SignClaim(k *Key, chainID *big.Int, contract Address, s *Swap, secret *big.Int, to, relayer Address, fee *big.Int) *RelayedClaim {
	d := ClaimDigest(chainID, contract, s.ID(), to, relayer, fee)
	v, r, ss := k.Sign(d)
	return &RelayedClaim{Swap: s, Secret: secret, To: to, Relayer: relayer, Fee: fee, V: v + 27, R: Hash(r), S: Hash(ss)}
}

// VerifyClaimSig checks the signature off-chain (relayers do this before simulating).
func VerifyClaimSig(chainID *big.Int, contract Address, c *RelayedClaim) error {
	if err := c.Swap.CheckNumbers(); err != nil {
		return err
	}
	if !FitsU256(c.Fee) || (c.Secret != nil && !FitsU256(c.Secret)) {
		return errors.New("claim has a missing or out-of-range number")
	}
	if c.V != 27 && c.V != 28 {
		return errors.New("bad v")
	}
	a, err := Recover(ClaimDigest(chainID, contract, c.Swap.ID(), c.To, c.Relayer, c.Fee), c.V-27, [32]byte(c.R), [32]byte(c.S))
	if err != nil {
		return err
	}
	if a != c.Swap.Claimer {
		return errors.New("signature is not from the swap's claimer")
	}
	return nil
}

// ---------------------------------------------------------------- helpers for other contracts (registry)

// Selector is the 4-byte function selector of an ABI signature.
func Selector(sig string) []byte { return selector(sig) }

// WordAddr, WordU and WordBig are 32-byte ABI words.
func WordAddr(a Address) []byte       { return wAddr(a) }
func WordU(x uint64) []byte           { return wU(x) }
func WordBig(x *big.Int) []byte       { return u256(x) }
func (g Sig) Words() []byte           { return g.words() }
func (s *Swap) Words() []byte         { return s.words() }
func SignDigest(k *Key, d []byte) Sig { return signDigest(k, d) }
