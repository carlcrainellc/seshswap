package keys

import (
	"bytes"
	"testing"
)

// FuzzParseMoneroAddress: any string ParseAddress accepts is the one
// canonical encoding of its bytes; real addresses parse to their network;
// a one-character change is caught by the checksum or decoding.
func FuzzParseMoneroAddress(f *testing.F) {
	f.Add("", []byte{1}, uint8(0), uint8(0), uint8('A'))
	f.Add("44AFFq5kSiGBoZ4NMDwYtN18obc8AemS33DBLWs3H7otXft3XjrpDtQGv7SqSsaBYBb98uNbr2VBBEt7f2wfn3RVGQBEP3A", []byte{7}, uint8(1), uint8(50), uint8('z'))
	f.Fuzz(func(t *testing.T, s string, seed []byte, netSel, pos, ch uint8) {
		if net, kind, err := ParseAddress(s); err == nil {
			b, derr := moneroB58Decode(s)
			if derr != nil || moneroB58(b) != s {
				t.Fatalf("accepted a non-canonical encoding %q (%v)", s, derr)
			}
			if net == "" || kind == "" {
				t.Fatalf("empty net/kind for %q", s)
			}
		}
		// A real address for keys derived from the seed.
		w := make([]byte, 64)
		copy(w, bytes.Repeat(append(seed, 1), 64))
		sp, err := ScalarFromUniform(w)
		if err != nil {
			return
		}
		w[0] ^= 0x55
		vw, _ := ScalarFromUniform(w)
		net := []Network{Mainnet, Testnet, Stagenet}[int(netSel)%3]
		addr, err := Address(net, sp.Public(), vw.Public())
		if err != nil {
			t.Fatal(err)
		}
		got, kind, err := ParseAddress(addr)
		if err != nil || got != net || kind != KindStandard {
			t.Fatalf("own address %s: %v %v %v", addr, got, kind, err)
		}
		i := int(pos) % len(addr)
		if c := alphabet[int(ch)%len(alphabet)]; c != addr[i] {
			typo := addr[:i] + string(c) + addr[i+1:]
			if _, _, err := ParseAddress(typo); err == nil {
				t.Fatalf("typo at %d accepted: %s", i, typo)
			}
		}
	})
}
