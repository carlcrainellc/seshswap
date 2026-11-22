package eth

import (
	"strings"
	"testing"
)

func TestChecksumEIP55(t *testing.T) {
	for _, v := range []string{ // vectors from EIP-55
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	} {
		a, err := ParseUserAddress(v)
		if err != nil || a.ChecksumHex() != v {
			t.Fatalf("%s: %v %s", v, err, a.ChecksumHex())
		}
		if _, err := ParseUserAddress(strings.ToLower(v)); err != nil {
			t.Fatalf("lower %s: %v", v, err)
		}
	}
	if _, err := ParseUserAddress("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD"); err == nil {
		t.Fatal("bad checksum accepted")
	}
}
