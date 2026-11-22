package eth

import (
	"math/big"
	"testing"
)

// Log.Block dereferenced a nil big.Int on a malformed blockNumber.
func TestLogBlockBadHex(t *testing.T) {
	l := Log{BlockNumber: "zz"}
	if l.Block() != 0 {
		t.Fatal("want 0")
	}
}

// A relayed claim with a fee over 256 bits (or negative numbers in the swap)
// panicked in u256 while hashing, before the signature check.
func TestVerifyClaimSigOutOfRange(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 300)
	ok := &Swap{ClaimKey: big.NewInt(2), RefundKey: big.NewInt(3), Value: big.NewInt(1)}
	cases := []*RelayedClaim{
		{Swap: ok, Fee: huge, V: 27},
		{Swap: ok, Fee: nil, V: 27},
		{Swap: &Swap{ClaimKey: big.NewInt(-1), RefundKey: big.NewInt(3), Value: big.NewInt(1)}, Fee: big.NewInt(0), V: 27},
		{Swap: &Swap{}, Fee: big.NewInt(0), V: 27},
	}
	for i, c := range cases {
		if VerifyClaimSig(big.NewInt(1), Address{}, c) == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	if VerifyIntentSig(big.NewInt(1), Address{}, &Swap{Value: huge, ClaimKey: big.NewInt(1), RefundKey: big.NewInt(1)}, Sig{V: 27}) == nil {
		t.Fatal("intent accepted")
	}
}
