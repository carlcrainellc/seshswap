package eth

import (
	"encoding/hex"
	"errors"
	"strings"
)

// ChecksumHex is the EIP-55 mixed-case form of the address.
func (a Address) ChecksumHex() string {
	low := hex.EncodeToString(a[:])
	h := hex.EncodeToString(Keccak([]byte(low)))
	out := []byte(low)
	for i, c := range out {
		if c >= 'a' && c <= 'f' && h[i] >= '8' {
			out[i] = c - 32
		}
	}
	return "0x" + string(out)
}

// ParseUserAddress checks an address typed by a person: 0x plus 40 hex
// digits, and if it mixes upper and lower case, a valid EIP-55 checksum.
// The zero address is refused.
func ParseUserAddress(s string) (Address, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") || len(s) != 42 {
		return Address{}, errors.New("an Ethereum address is 0x followed by 40 letters and digits")
	}
	a, err := ParseAddress("0x" + s[2:])
	if err != nil {
		return Address{}, errors.New("an Ethereum address is 0x followed by 40 letters and digits")
	}
	body := s[2:]
	if body != strings.ToLower(body) && body != strings.ToUpper(body) && a.ChecksumHex()[2:] != body {
		return Address{}, errors.New("this Ethereum address has a typo (checksum doesn't match)")
	}
	if a.IsZero() {
		return Address{}, errors.New("that's the zero address; coins sent there are lost")
	}
	return a, nil
}
