package exits

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Found by FuzzDecodeLeased: a Leased log whose string offset is near
// MaxInt64 made offset+32 wrap and the slice panicked (the exit's watcher
// reads these logs from an RPC node).
func TestDecodeLeasedOffsetOverflow(t *testing.T) {
	d := make([]byte, 8*32)
	binary.BigEndian.PutUint64(d[6*32+24:7*32], 0x7fffffffffffffe5)
	l := &eth.Log{Topics: []eth.Hash{eth.Hash(TopicLeased), {}, {}, {}}, DataHex: "0x" + hex.EncodeToString(d)}
	if _, err := DecodeLeased(l); err == nil {
		t.Fatal("want error")
	}
}
