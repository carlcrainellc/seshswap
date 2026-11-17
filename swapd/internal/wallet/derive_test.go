package wallet

import (
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

func TestETHKnownVector(t *testing.T) {
	// Well-known BIP-39 mnemonic; first ETH account at m/44'/60'/0'/0/0
	// is the MetaMask / MEW address 0x9858EfFD232B4033E47d90003D41EC34EcaEda94.
	const m = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	seed, err := Seed(m, "")
	if err != nil {
		t.Fatal(err)
	}
	k, err := Derive(seed, keys.Stagenet)
	if err != nil {
		t.Fatal(err)
	}
	want := "0x9858effd232b4033e47d90003d41ec34ecaeda94"
	if k.EthAddr.Hex() != want {
		t.Fatalf("ETH address %s, want %s", k.EthAddr.Hex(), want)
	}
}

func TestXMRDomainSeparatedAndStable(t *testing.T) {
	const m = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	seed, err := Seed(m, "")
	if err != nil {
		t.Fatal(err)
	}
	a, err := Derive(seed, keys.Stagenet)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Derive(seed, keys.Stagenet)
	if err != nil {
		t.Fatal(err)
	}
	if a.XmrAddr != b.XmrAddr || a.XmrSpend.Hex() != b.XmrSpend.Hex() || a.XmrView.Hex() != b.XmrView.Hex() {
		t.Fatal("derivation is not stable")
	}
	// The XMR spend key must not be the ETH private key (domain separation).
	if a.XmrSpend.Hex() == a.EthKey.Hex() {
		t.Fatal("XMR spend key equals the ETH key; domain separation failed")
	}
	// View is determined by spend: derive twice and compare the address on mainnet vs stagenet (prefix only).
	main, err := Derive(seed, keys.Mainnet)
	if err != nil {
		t.Fatal(err)
	}
	if main.XmrSpend.Hex() != a.XmrSpend.Hex() {
		t.Fatal("network should not change the spend key")
	}
	if main.XmrAddr == a.XmrAddr {
		t.Fatal("mainnet and stagenet addresses should differ")
	}
	// A different seed must produce a different pair.
	m2, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	seed2, err := Seed(m2, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Derive(seed2, keys.Stagenet)
	if err != nil {
		t.Fatal(err)
	}
	if c.EthAddr == a.EthAddr || c.XmrAddr == a.XmrAddr {
		t.Fatal("two seeds produced the same address")
	}
}

func Test24WordDerive(t *testing.T) {
	m, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := Seed(m, "")
	if err != nil {
		t.Fatal(err)
	}
	k, err := Derive(seed, keys.Stagenet)
	if err != nil {
		t.Fatal(err)
	}
	if k.EthAddr.IsZero() || k.XmrAddr == "" {
		t.Fatal("empty address")
	}
	// Restore from the same words.
	seed2, err := Seed(m, "")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := Derive(seed2, keys.Stagenet)
	if err != nil {
		t.Fatal(err)
	}
	if k2.EthAddr != k.EthAddr || k2.XmrAddr != k.XmrAddr {
		t.Fatal("restore did not reproduce the addresses")
	}
}
