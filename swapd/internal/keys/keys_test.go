package keys

import (
	"encoding/hex"
	"testing"

	"filippo.io/edwards25519"
)

func TestProofRoundTripAndBinding(t *testing.T) {
	s, _ := RandomScalar()
	pr, err := Prove(s, []byte("swap-1/taker"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(s.Public(), []byte("swap-1/taker"), pr); err != nil {
		t.Fatal(err)
	}
	if Verify(s.Public(), []byte("swap-2/taker"), pr) == nil {
		t.Fatal("proof replayed into another context")
	}
	o, _ := RandomScalar()
	if Verify(o.Public(), []byte("swap-1/taker"), pr) == nil {
		t.Fatal("proof accepted for another key")
	}
}

func TestPointValidation(t *testing.T) {
	if _, err := PointFromBytes(edwards25519.NewIdentityPoint().Bytes()); err == nil {
		t.Fatal("identity accepted")
	}
	// a small-order (torsion) point: encoding of order-4 point (x != 0, y = 0)
	tors, _ := hex.DecodeString("0000000000000000000000000000000000000000000000000000000000000000")
	if _, err := PointFromBytes(tors); err == nil {
		t.Fatal("torsion point accepted")
	}
	s, _ := RandomScalar()
	p := s.Public()
	// P + T (T of order 2: y = -1) must be rejected
	t2b, _ := hex.DecodeString("ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f")
	t2, err := new(edwards25519.Point).SetBytes(t2b)
	if err != nil {
		t.Fatal(err)
	}
	mixed := new(edwards25519.Point).Add(p.p, t2)
	if _, err := PointFromBytes(mixed.Bytes()); err == nil {
		t.Fatal("point with torsion component accepted")
	}
	if _, err := PointFromBytes(p.Bytes()); err != nil {
		t.Fatal(err)
	}
	// non-canonical: y + p (set top bits) for y small isn't representable generally; check y=1 + p encoding
	nc, _ := hex.DecodeString("eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f") // y = p+1
	if _, err := PointFromBytes(nc); err == nil {
		t.Fatal("non-canonical accepted")
	}
}

func TestIntRoundTrips(t *testing.T) {
	s, _ := RandomScalar()
	s2, err := ScalarFromInt(s.Int())
	if err != nil || s2.Hex() != s.Hex() {
		t.Fatal("scalar int roundtrip")
	}
	p := s.Public()
	p2, err := PointFromInt(p.Int())
	if err != nil || !p2.Equal(p) {
		t.Fatal("point int roundtrip")
	}
}

func TestSharedKeyCombines(t *testing.T) {
	a, _ := RandomScalar()
	b, _ := RandomScalar()
	va, _ := RandomScalar()
	vb, _ := RandomScalar()
	sh, err := SharedAddress(Regtest, a.Public(), b.Public(), va, vb)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Add(b).Public().Equal(sh.SpendPub) {
		t.Fatal("combined spend key mismatch")
	}
	if len(sh.Address) != 95 || sh.Address[0] != '4' {
		t.Fatalf("unexpected address %s", sh.Address)
	}
}

// Every small-order point (the 8 torsion points, incl. non-canonical sign variants)
// must be refused before it can be added into a shared key.
func TestAllSmallOrderPointsRejected(t *testing.T) {
	for _, h := range []string{
		"0100000000000000000000000000000000000000000000000000000000000000", // identity
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", // order 2
		"0000000000000000000000000000000000000000000000000000000000000000", // order 4
		"0000000000000000000000000000000000000000000000000000000000000080", // order 4, other sign
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05", // order 8
		"26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85",
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a",
		"c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac03fa",
	} {
		if _, err := PointFromHex(h); err == nil {
			t.Fatalf("small-order point %s accepted", h)
		}
	}
}

// Scalars are decoded canonically: exactly 32 bytes, < l, non-zero. No silent reduction.
func TestScalarCanonicalOnly(t *testing.T) {
	l := "edd3f55c1a631258d69cf7a2def9de1400000000000000000000000000000010" // l, little-endian
	for _, h := range []string{
		l, // == l
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"0000000000000000000000000000000000000000000000000000000000000000", // zero
		"01", // short
		"010000000000000000000000000000000000000000000000000000000000000000", // 33 bytes
	} {
		if _, err := ScalarFromHex(h); err == nil {
			t.Fatalf("non-canonical scalar %s accepted", h)
		}
	}
	if _, err := ScalarFromHex("ecd3f55c1a631258d69cf7a2def9de1400000000000000000000000000000010"); err != nil {
		t.Fatal("l-1 rejected:", err)
	}
}
