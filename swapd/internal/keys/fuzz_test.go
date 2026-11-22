package keys

import "testing"

// Peers send Monero key shares and proofs as hex.
func FuzzKeyParsers(f *testing.F) {
	f.Add("58", "00", "00")
	f.Fuzz(func(t *testing.T, p, r, z string) {
		_, _ = PointFromHex(p)
		_, _ = ScalarFromHex(p)
		if P, err := PointFromHex(p); err == nil {
			_ = Verify(P, []byte("ctx"), Proof{R: r, Z: z})
		}
	})
}
