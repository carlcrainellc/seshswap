package eth

import (
	"strings"
	"testing"
)

// FuzzUserAddress: what ParseUserAddress accepts is a non-zero address
// whose EIP-55 form parses back to it; any one-letter case flip of a
// checksummed address is refused.
func FuzzUserAddress(f *testing.F) {
	f.Add("0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", uint8(3))
	f.Add("0x0000000000000000000000000000000000000000", uint8(0))
	f.Add(" 0XFB6916095CA1DF60BB79CE92CE3EA74C37C5D359 ", uint8(9))
	f.Fuzz(func(t *testing.T, s string, pos uint8) {
		a, err := ParseUserAddress(s)
		if err != nil {
			return
		}
		if a.IsZero() {
			t.Fatalf("zero address accepted: %q", s)
		}
		cs := a.ChecksumHex()
		if b, err := ParseUserAddress(cs); err != nil || b != a {
			t.Fatalf("checksum form %s does not parse back (%v)", cs, err)
		}
		if b, err := ParseUserAddress(strings.ToLower(cs)); err != nil || b != a {
			t.Fatalf("lower-case form refused (%v)", err)
		}
		body := []byte(cs[2:])
		i := int(pos) % 40
		if c := body[i]; c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			body[i] ^= 0x20
			flipped := "0x" + string(body)
			if strings.ToLower(flipped[2:]) != flipped[2:] && strings.ToUpper(flipped[2:]) != flipped[2:] {
				if _, err := ParseUserAddress(flipped); err == nil {
					t.Fatalf("case typo accepted: %s (good %s)", flipped, cs)
				}
			}
		}
	})
}
