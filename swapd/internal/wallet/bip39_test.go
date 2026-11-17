package wallet

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestEncodeParseRoundtrip(t *testing.T) {
	for i := 0; i < 20; i++ {
		m, err := NewMnemonic()
		if err != nil {
			t.Fatal(err)
		}
		if n := len(strings.Fields(m)); n != 24 {
			t.Fatalf("want 24 words, got %d", n)
		}
		ent, err := ParseMnemonic(m)
		if err != nil {
			t.Fatalf("parse %q: %v", m, err)
		}
		if len(ent) != 32 {
			t.Fatalf("entropy %d", len(ent))
		}
		m2, err := EncodeMnemonic(ent)
		if err != nil {
			t.Fatal(err)
		}
		if m2 != m {
			t.Fatalf("roundtrip: %q vs %q", m, m2)
		}
	}
}

func TestParseDetectsBadWordAndChecksum(t *testing.T) {
	m, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(m)
	parts[7] = "notaword"
	if _, err := ParseMnemonic(strings.Join(parts, " ")); err == nil {
		t.Fatal("accepted a word that is not in the list")
	}
	parts = strings.Fields(m)
	parts[3], parts[4] = parts[4], parts[3]
	if _, err := ParseMnemonic(strings.Join(parts, " ")); err == nil {
		t.Fatal("accepted a phrase with a bad checksum")
	}
	if _, err := ParseMnemonic("abandon abandon"); err == nil {
		t.Fatal("accepted a short phrase")
	}
}

func TestKnownMnemonic(t *testing.T) {
	// BIP-39 test vector: 12× abandon + about. Entropy is all zeros, 12 words.
	const m = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	ent, err := ParseMnemonic(m)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(ent) != "00000000000000000000000000000000" {
		t.Fatalf("entropy %x", ent)
	}
	seed, err := Seed(m, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "5eb00bbddcf069084889a8ab9155568165f5c453ccb85e70811aaed6f6da5fc19a5ac40b389cd370d086206dec8aa6c43daea6690f20ad3d8d48b2d2ce9e38e4"
	if hex.EncodeToString(seed) != want {
		t.Fatalf("seed %x", seed)
	}
	z := make([]byte, 32)
	m24, err := EncodeMnemonic(z)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m24, "abandon ") {
		t.Fatalf("24-word zeros: %s", m24)
	}
	ent24, err := ParseMnemonic(m24)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(ent24) != strings.Repeat("00", 32) {
		t.Fatalf("24-word entropy %x", ent24)
	}
}
