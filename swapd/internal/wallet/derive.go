package wallet

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/sha3"
)

// xmrSpendContext domain-separates the Monero spend key from the BIP-39 seed
// and from the Ethereum key. Do not change this string: existing backups
// derive the same XMR address from it.
const xmrSpendContext = "eth-xmr-swap-wallet xmr-spend-v1"

// ETHPath is BIP-44 for the first Ethereum account, same as MetaMask.
const ETHPath = "m/44'/60'/0'/0/0"

// Keys is the pair of addresses derived from one BIP-39 seed.
type Keys struct {
	EthKey    *eth.Key
	EthAddr   eth.Address
	XmrSpend  keys.Scalar
	XmrView   keys.Scalar
	XmrAddr   string
	XmrNet    keys.Network
}

// Derive produces ETH and XMR keys from a BIP-39 seed (the 64-byte PBKDF2 output).
// The two keys are unrelated: Ethereum follows BIP-32/BIP-44 on secp256k1;
// Monero uses an HMAC domain-separated from that path, then Monero's own
// hash-to-scalar for the view key.
func Derive(seed []byte, net keys.Network) (*Keys, error) {
	if len(seed) != 64 {
		return nil, errors.New("BIP-39 seed must be 64 bytes")
	}
	ethKey, err := deriveETH(seed)
	if err != nil {
		return nil, err
	}
	spend, err := deriveXMRSpend(seed)
	if err != nil {
		return nil, err
	}
	view, err := viewFromSpend(spend)
	if err != nil {
		return nil, err
	}
	addr, err := keys.Address(net, spend.Public(), view.Public())
	if err != nil {
		return nil, err
	}
	return &Keys{
		EthKey:   ethKey,
		EthAddr:  ethKey.Address(),
		XmrSpend: spend,
		XmrView:  view,
		XmrAddr:  addr,
		XmrNet:   net,
	}, nil
}

func deriveETH(seed []byte) (*eth.Key, error) {
	k, c := bip32Master(seed)
	// m/44'/60'/0'/0/0
	for _, i := range []uint32{
		bip32Hard + 44,
		bip32Hard + 60,
		bip32Hard + 0,
		0,
		0,
	} {
		var err error
		k, c, err = bip32CKD(k, c, i)
		if err != nil {
			return nil, err
		}
	}
	return eth.KeyFromHex(fmt.Sprintf("%x", k))
}

const bip32Hard = uint32(0x80000000)

func bip32Master(seed []byte) (key, chain []byte) {
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	mac.Write(seed)
	I := mac.Sum(nil)
	return I[:32], I[32:]
}

func bip32CKD(k, c []byte, i uint32) ([]byte, []byte, error) {
	mac := hmac.New(sha512.New, c)
	if i >= bip32Hard {
		mac.Write([]byte{0})
		mac.Write(k)
	} else {
		priv := secp256k1.PrivKeyFromBytes(k)
		mac.Write(priv.PubKey().SerializeCompressed())
	}
	var ib [4]byte
	binary.BigEndian.PutUint32(ib[:], i)
	mac.Write(ib[:])
	I := mac.Sum(nil)
	il, kr := I[:32], I[32:]
	var a, b, sum secp256k1.ModNScalar
	if a.SetByteSlice(il) {
		return nil, nil, errors.New("BIP-32: IL is not a valid scalar")
	}
	if b.SetByteSlice(k) {
		return nil, nil, errors.New("BIP-32: parent key is not a valid scalar")
	}
	sum.Add2(&a, &b)
	if sum.IsZero() {
		return nil, nil, errors.New("BIP-32: derived a zero key")
	}
	out := sum.Bytes()
	return out[:], kr, nil
}

func deriveXMRSpend(seed []byte) (keys.Scalar, error) {
	mac := hmac.New(sha512.New, []byte(xmrSpendContext))
	mac.Write(seed)
	wide := mac.Sum(nil)
	if len(wide) != 64 {
		return keys.Scalar{}, errors.New("HMAC-SHA512 size")
	}
	return keys.ScalarFromUniform(wide)
}

// viewFromSpend is Monero's deterministic wallet: view = sc_reduce32(keccak(spend)).
func viewFromSpend(spend keys.Scalar) (keys.Scalar, error) {
	h := keccak256(spend.Bytes())
	return scReduce32(h)
}

func keccak256(b []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	return h.Sum(nil)
}

// l is the ed25519 group order (Monero's sc_reduce32 modulus).
var ed25519L = mustL()

func mustL() *big.Int {
	// 2^252 + 27742317777372353535851937790883648493
	l := new(big.Int).Lsh(big.NewInt(1), 252)
	add, _ := new(big.Int).SetString("27742317777372353535851937790883648493", 10)
	return l.Add(l, add)
}

func scReduce32(le []byte) (keys.Scalar, error) {
	if len(le) != 32 {
		return keys.Scalar{}, errors.New("need 32 bytes")
	}
	n := new(big.Int).SetBytes(rev(le))
	n.Mod(n, ed25519L)
	if n.Sign() == 0 {
		return keys.Scalar{}, errors.New("zero scalar")
	}
	out := rev(n.FillBytes(make([]byte, 32)))
	return keys.ScalarFromBytes(out)
}

func rev(b []byte) []byte {
	o := make([]byte, len(b))
	for i := range b {
		o[len(b)-1-i] = b[i]
	}
	return o
}
