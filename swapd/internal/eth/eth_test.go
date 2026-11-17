package eth

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
)

func TestRLPVectors(t *testing.T) {
	long := strings.Repeat("a", 56)
	cases := []struct {
		got  []byte
		want string
	}{
		{rlpBytes([]byte("dog")), "83646f67"},
		{rlpList(rlpBytes([]byte("cat")), rlpBytes([]byte("dog"))), "c88363617483646f67"},
		{rlpBytes(nil), "80"},
		{rlpList(), "c0"},
		{rlpUint(0), "80"},
		{rlpUint(15), "0f"},
		{rlpUint(1024), "820400"},
		{rlpBytes([]byte(long)), "b838" + hex.EncodeToString([]byte(long))},
	}
	for i, c := range cases {
		if hex.EncodeToString(c.got) != c.want {
			t.Fatalf("case %d: got %x want %s", i, c.got, c.want)
		}
	}
}

func TestSignRecoverAndAddress(t *testing.T) {
	// anvil/hardhat dev key #0 (public test key) -> well-known address
	k, _ := KeyFromHex("ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80")
	if k.Address().Hex() != strings.ToLower("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266") {
		t.Fatal("address derivation", k.Address().Hex())
	}
	h := Keccak([]byte("hello"))
	v, r, s := k.Sign(h)
	a, err := Recover(h, v, r, s)
	if err != nil || a != k.Address() {
		t.Fatal("recover")
	}
	if new(big.Int).SetBytes(s[:]).Cmp(new(big.Int).Rsh(secpN, 1)) > 0 {
		t.Fatal("high-s signature")
	}
}

var secpN, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)

func TestClaimSignatureBinding(t *testing.T) {
	k, _ := NewKey()
	contract := Address{1}
	s := &Swap{Claimer: k.Address(), ClaimKey: big.NewInt(5), RefundKey: big.NewInt(6), Value: big.NewInt(100)}
	c := SignClaim(k, big.NewInt(8453), contract, s, big.NewInt(7), Address{9}, Address{}, big.NewInt(3))
	if err := VerifyClaimSig(big.NewInt(8453), contract, c); err != nil {
		t.Fatal(err)
	}
	if VerifyClaimSig(big.NewInt(10), contract, c) == nil {
		t.Fatal("cross-chain replay accepted")
	}
	if VerifyClaimSig(big.NewInt(8453), Address{2}, c) == nil {
		t.Fatal("cross-contract replay accepted")
	}
	c.Fee = big.NewInt(4)
	if VerifyClaimSig(big.NewInt(8453), contract, c) == nil {
		t.Fatal("fee change accepted")
	}
}
