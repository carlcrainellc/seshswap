// Package keys holds the Monero-side key math: key shares, a Schnorr proof of
// knowledge for each share (stops rogue-key attacks on the shared address),
// point validation, the shared address, and conversion to the contract format.
package keys

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"math/big"

	"filippo.io/edwards25519"
)

// Scalar is an ed25519 scalar (mod l), 32-byte little-endian.
type Scalar struct{ s *edwards25519.Scalar }

// Point is a validated ed25519 point: canonical encoding, not identity, prime-order subgroup.
type Point struct{ p *edwards25519.Point }

func RandomScalar() (Scalar, error) {
	var w [64]byte
	if _, err := rand.Read(w[:]); err != nil {
		return Scalar{}, err
	}
	s, err := new(edwards25519.Scalar).SetUniformBytes(w[:])
	if err != nil {
		return Scalar{}, err
	}
	return Scalar{s}, nil
}

func ScalarFromBytes(le []byte) (Scalar, error) {
	s, err := new(edwards25519.Scalar).SetCanonicalBytes(le)
	if err != nil {
		return Scalar{}, err
	}
	if s.Equal(edwards25519.NewScalar()) == 1 {
		return Scalar{}, errors.New("zero scalar")
	}
	return Scalar{s}, nil
}

func ScalarFromHex(h string) (Scalar, error) {
	b, err := hex.DecodeString(h)
	if err != nil {
		return Scalar{}, err
	}
	return ScalarFromBytes(b)
}

// ScalarFromUniform reduces 64 little-endian bytes modulo the ed25519 group
// order (SetUniformBytes). Used to turn a wide hash (HMAC-SHA512) into a key.
func ScalarFromUniform(wide []byte) (Scalar, error) {
	if len(wide) != 64 {
		return Scalar{}, errors.New("need 64 bytes")
	}
	var w [64]byte
	copy(w[:], wide)
	s, err := new(edwards25519.Scalar).SetUniformBytes(w[:])
	if err != nil {
		return Scalar{}, err
	}
	if s.Equal(edwards25519.NewScalar()) == 1 {
		return Scalar{}, errors.New("zero scalar")
	}
	return Scalar{s}, nil
}

func (s Scalar) Bytes() []byte       { return s.s.Bytes() }
func (s Scalar) Hex() string         { return hex.EncodeToString(s.s.Bytes()) }
func (s Scalar) Public() Point       { return Point{new(edwards25519.Point).ScalarBaseMult(s.s)} }
func (s Scalar) Add(o Scalar) Scalar { return Scalar{new(edwards25519.Scalar).Add(s.s, o.s)} }

// Int is the scalar as the integer the contract receives (little-endian decode).
func (s Scalar) Int() *big.Int { return new(big.Int).SetBytes(rev(s.s.Bytes())) }

// ScalarFromInt converts a secret revealed in a contract event back to a scalar.
func ScalarFromInt(x *big.Int) (Scalar, error) {
	if x.Sign() <= 0 || x.BitLen() > 256 {
		return Scalar{}, errors.New("bad secret")
	}
	return ScalarFromBytes(rev(x.FillBytes(make([]byte, 32))))
}

var inv8 = func() *edwards25519.Scalar {
	var b [32]byte
	b[0] = 8
	e, _ := new(edwards25519.Scalar).SetCanonicalBytes(b[:])
	return new(edwards25519.Scalar).Invert(e)
}()

func PointFromBytes(b []byte) (Point, error) {
	p, err := new(edwards25519.Point).SetBytes(b)
	if err != nil {
		return Point{}, err
	}
	if hex.EncodeToString(p.Bytes()) != hex.EncodeToString(b) {
		return Point{}, errors.New("non-canonical point encoding")
	}
	if p.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return Point{}, errors.New("identity point")
	}
	// torsion-free iff inv8 * (8 * P) == P
	q := new(edwards25519.Point).ScalarMult(inv8, new(edwards25519.Point).MultByCofactor(p))
	if q.Equal(p) != 1 {
		return Point{}, errors.New("point has a small-order component")
	}
	return Point{p}, nil
}

func PointFromHex(h string) (Point, error) {
	b, err := hex.DecodeString(h)
	if err != nil {
		return Point{}, err
	}
	return PointFromBytes(b)
}

func (p Point) Bytes() []byte      { return p.p.Bytes() }
func (p Point) Hex() string        { return hex.EncodeToString(p.p.Bytes()) }
func (p Point) Equal(o Point) bool { return p.p.Equal(o.p) == 1 }
func (p Point) Add(o Point) Point  { return Point{new(edwards25519.Point).Add(p.p, o.p)} }

// Int is the contract encoding: the 32 encoded bytes read as a little-endian integer.
func (p Point) Int() *big.Int { return new(big.Int).SetBytes(rev(p.p.Bytes())) }

func PointFromInt(x *big.Int) (Point, error) {
	if x.Sign() <= 0 || x.BitLen() > 256 {
		return Point{}, errors.New("bad point int")
	}
	return PointFromBytes(rev(x.FillBytes(make([]byte, 32))))
}

func rev(b []byte) []byte {
	o := make([]byte, len(b))
	for i := range b {
		o[len(b)-1-i] = b[i]
	}
	return o
}

// ---------------------------------------------------------------- proof of knowledge

// Proof is a Schnorr proof of knowledge of s for P = s*B, bound to a context
// (swap session id + role) so it can't be replayed into another swap.
type Proof struct {
	R string `json:"r"` // point hex
	Z string `json:"z"` // scalar hex
}

func challenge(ctx []byte, P, R []byte) *edwards25519.Scalar {
	h := sha512.New()
	h.Write([]byte("xmrswap-v2/pok"))
	h.Write([]byte{byte(len(ctx))})
	h.Write(ctx)
	h.Write(P)
	h.Write(R)
	c, _ := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	return c
}

func Prove(s Scalar, ctx []byte) (Proof, error) {
	r, err := RandomScalar()
	if err != nil {
		return Proof{}, err
	}
	R := r.Public()
	c := challenge(ctx, s.Public().Bytes(), R.Bytes())
	z := new(edwards25519.Scalar).MultiplyAdd(c, s.s, r.s)
	return Proof{R: R.Hex(), Z: hex.EncodeToString(z.Bytes())}, nil
}

func Verify(P Point, ctx []byte, pr Proof) error {
	Rb, err := hex.DecodeString(pr.R)
	if err != nil {
		return err
	}
	R, err := new(edwards25519.Point).SetBytes(Rb)
	if err != nil {
		return err
	}
	zb, err := hex.DecodeString(pr.Z)
	if err != nil {
		return err
	}
	z, err := new(edwards25519.Scalar).SetCanonicalBytes(zb)
	if err != nil {
		return err
	}
	c := challenge(ctx, P.Bytes(), Rb)
	lhs := new(edwards25519.Point).ScalarBaseMult(z)
	rhs := new(edwards25519.Point).Add(R, new(edwards25519.Point).ScalarMult(c, P.p))
	if lhs.Equal(rhs) != 1 {
		return errors.New("proof of knowledge invalid")
	}
	return nil
}
