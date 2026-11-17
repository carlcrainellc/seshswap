package eth

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"golang.org/x/crypto/sha3"
)

type Address [20]byte

func (a Address) Hex() string    { return "0x" + hex.EncodeToString(a[:]) }
func (a Address) String() string { return a.Hex() }
func (a Address) IsZero() bool   { return a == Address{} }

func (a Address) MarshalText() ([]byte, error) { return []byte(a.Hex()), nil }
func (a *Address) UnmarshalText(b []byte) error {
	x, err := ParseAddress(string(b))
	if err != nil {
		return err
	}
	*a = x
	return nil
}

func ParseAddress(s string) (Address, error) {
	var a Address
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != 20 {
		return a, fmt.Errorf("bad address %q", s)
	}
	copy(a[:], b)
	return a, nil
}

func Keccak(parts ...[]byte) []byte {
	h := sha3.NewLegacyKeccak256()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func Keccak32(parts ...[]byte) [32]byte {
	var o [32]byte
	copy(o[:], Keccak(parts...))
	return o
}

// Key is a secp256k1 account key.
type Key struct{ priv *secp256k1.PrivateKey }

func KeyFromHex(h string) (*Key, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(h, "0x"))
	if err != nil || len(b) != 32 {
		return nil, errors.New("bad private key")
	}
	return &Key{secp256k1.PrivKeyFromBytes(b)}, nil
}

func NewKey() (*Key, error) {
	p, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return &Key{p}, nil
}

func (k *Key) Hex() string { b := k.priv.Key.Bytes(); return hex.EncodeToString(b[:]) }

func (k *Key) Address() Address {
	u := k.priv.PubKey().SerializeUncompressed()[1:]
	var a Address
	copy(a[:], Keccak(u)[12:])
	return a
}

// Sign returns (v in {0,1}, r, s) with low-s, over a 32-byte hash.
func (k *Key) Sign(hash []byte) (v byte, r, s [32]byte) {
	sig := ecdsa.SignCompact(k.priv, hash, false) // [27+recid][R][S], low-S
	v = sig[0] - 27
	copy(r[:], sig[1:33])
	copy(s[:], sig[33:65])
	return
}

// Recover returns the address that signed hash (v in {0,1}).
func Recover(hash []byte, v byte, r, s [32]byte) (Address, error) {
	sig := make([]byte, 65)
	sig[0] = 27 + v
	copy(sig[1:], r[:])
	copy(sig[33:], s[:])
	pub, _, err := ecdsa.RecoverCompact(sig, hash)
	if err != nil {
		return Address{}, err
	}
	var a Address
	copy(a[:], Keccak(pub.SerializeUncompressed()[1:])[12:])
	return a, nil
}

func u256(x *big.Int) []byte {
	if x.Sign() < 0 || x.BitLen() > 256 {
		panic("u256 out of range")
	}
	return x.FillBytes(make([]byte, 32))
}

func HexBig(s string) (*big.Int, error) {
	x, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		if s == "0x" || s == "0x0" {
			return new(big.Int), nil
		}
		return nil, fmt.Errorf("bad hex int %q", s)
	}
	return x, nil
}

// Hash is a 32-byte value that marshals as 0x-hex.
type Hash [32]byte

func (h Hash) Hex() string                   { return "0x" + hex.EncodeToString(h[:]) }
func (h Hash) MarshalText() ([]byte, error)  { return []byte(h.Hex()), nil }
func (h *Hash) UnmarshalText(b []byte) error { return h.set(string(b)) }
func (h *Hash) set(s string) error {
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != 32 {
		return fmt.Errorf("bad 32-byte hex %q", s)
	}
	copy(h[:], b)
	return nil
}
func ParseHash(s string) (Hash, error) { var h Hash; err := h.set(s); return h, err }
