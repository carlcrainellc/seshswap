package eth

import (
	"encoding/json"
	"math/big"
	"testing"
)

// Swaps and signatures arrive as JSON from peers (maker responses, relay
// requests). Decoding and checking them must never panic.
func FuzzSwapJSON(f *testing.F) {
	f.Add([]byte(`{"swap":{"value":"1","claimKey":"2","refundKey":"3"},"fee":"0","v":27}`))
	f.Add([]byte(`{"swap":{"value":"-1","claimKey":"2","refundKey":"3"},"fee":"1","v":28}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var c RelayedClaim
		if json.Unmarshal(b, &c) != nil || c.Swap == nil {
			return
		}
		cid := big.NewInt(11155111)
		_ = VerifyIntentSig(cid, Address{}, c.Swap, Sig{V: c.V, R: c.R, S: c.S})
		_ = VerifyClaimSig(cid, Address{}, &c)
	})
}

func FuzzDecodeSwap(f *testing.F) {
	f.Add(make([]byte, 11*32))
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := decodeSwap(b)
		if err == nil {
			_ = s.ID()
		}
	})
}

func FuzzParsers(f *testing.F) {
	f.Add("0x00000000000000000000000000000000000000aa")
	f.Add("0x")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ParseAddress(s)
		_, _ = HexBig(s)
		_, _ = ParseHash(s)
		l := Log{BlockNumber: s, DataHex: s}
		_ = l.Block()
		_ = l.Data()
	})
}
