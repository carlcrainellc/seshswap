package exits

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Leased logs come from an RPC node; the seller's watcher admits buyers from them.
func FuzzDecodeLeased(f *testing.F) {
	f.Add(make([]byte, 7*32), uint64(0))
	f.Add(make([]byte, 9*32), uint64(7*32))
	f.Add(make([]byte, 8*32), uint64(0x7fffffffffffffe5)) // offset + 32 wraps
	f.Fuzz(func(t *testing.T, data []byte, off uint64) {
		if len(data) >= 7*32 {
			binary.BigEndian.PutUint64(data[6*32+24:7*32], off)
		}
		l := &eth.Log{Topics: []eth.Hash{eth.Hash(TopicLeased), {}, {}, {}}, DataHex: "0x" + hex.EncodeToString(data)}
		_, _ = DecodeLeased(l)
	})
}

// Registry meta notes are free text anyone can post (280 bytes).
func FuzzParseMeta(f *testing.F) {
	f.Add("exit; price=0.00002 ETH/h; country=de; dns=10.0.0.1; note=hi")
	f.Add(";;=;a=b=c; PRICE = 1 ")
	f.Fuzz(func(t *testing.T, s string) {
		m := ParseMeta(s)
		out, err := m.String()
		if err != nil {
			return
		}
		// what we render must parse back to the same known fields
		// (String turns ";" into "," and newlines into spaces: display text.)
		m2 := ParseMeta(out)
		want := strings.TrimSpace(strings.NewReplacer(";", ",", "\n", " ").Replace(m.Price))
		if m2.Price != want && want != "" {
			t.Fatalf("price %q -> %q via %q", m.Price, m2.Price, out)
		}
	})
}

func FuzzValidClientSesh(f *testing.F) {
	f.Add("rgwybw99m4d464rbi9z95a7a364n1hauo74ip486ssckf1agw7zo.sesh")
	f.Fuzz(func(t *testing.T, s string) { _ = ValidClientSesh(s) })
}
