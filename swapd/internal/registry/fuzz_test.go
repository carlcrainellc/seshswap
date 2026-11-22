package registry

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Registry event logs come from an RPC node, which we do not trust to be
// well formed. Applying any log must never panic.
func FuzzIndexApply(f *testing.F) {
	topics := []eth.Hash{TopicListed, TopicMetaUpdated, TopicToppedUp, TopicExitReq, TopicExitCancel,
		TopicWithdrawn, TopicChallenged, TopicAnswered, TopicUpheld, TopicSlashed}
	f.Add(uint8(0), uint8(4), []byte{}, "0x1")
	f.Add(uint8(1), uint8(1), make([]byte, 64), "")
	f.Add(uint8(9), uint8(2), make([]byte, 96), "0xzz")
	f.Fuzz(func(t *testing.T, which, nTopics uint8, data []byte, block string) {
		l := &eth.Log{DataHex: "0x" + hexString(data), BlockNumber: block}
		l.Topics = append(l.Topics, topics[int(which)%len(topics)])
		for i := 1; i < int(nTopics%6); i++ {
			var h eth.Hash
			h[31] = byte(i)
			l.Topics = append(l.Topics, h)
		}
		ix := OpenIndex("", eth.Address{}, 0)
		ix.apply(l)
	})
}

// decodeString reads an ABI string at an offset taken from untrusted data.
func FuzzDecodeString(f *testing.F) {
	f.Add(make([]byte, 64), 0)
	f.Add(make([]byte, 64), -1)
	f.Fuzz(func(t *testing.T, b []byte, off int) {
		_, _ = decodeString(b, off)
	})
}

// A maker's signed commitment arrives in its swap response (JSON).
func FuzzSignedCommitment(f *testing.F) {
	f.Add([]byte(`{"commitment":{},"sig":{"v":27}}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var sc SignedCommitment
		if json.Unmarshal(b, &sc) != nil {
			return
		}
		_, _ = sc.Signer(big.NewInt(11155111))
		_ = sc.CheckAgainstSwap(&eth.Swap{Value: big.NewInt(1), ClaimKey: big.NewInt(2), RefundKey: big.NewInt(3)})
		_ = sc.CheckAgainstSwap(&eth.Swap{})
	})
}

func hexString(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 2*len(b))
	for i, c := range b {
		out[2*i], out[2*i+1] = d[c>>4], d[c&15]
	}
	return string(out)
}
