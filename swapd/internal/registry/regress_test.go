package registry

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Found by FuzzIndexApply: a registry log with only topic 0 panicked (index out of range).
func TestApplyShortLogNoPanic(t *testing.T) {
	ix := OpenIndex("", eth.Address{}, 0)
	for _, tp := range []eth.Hash{TopicMetaUpdated, TopicToppedUp, TopicWithdrawn, TopicUpheld} {
		ix.apply(&eth.Log{Topics: []eth.Hash{tp}, DataHex: "0x"})
	}
}

// A string offset near MaxInt64 wrapped offset+32 negative and slipped past the bounds check.
func TestDecodeStringOffsetOverflow(t *testing.T) {
	d := make([]byte, 128)
	binary.BigEndian.PutUint64(d[24:32], 0x7fffffffffffffe5)
	if _, err := decodeString(d, int(0x7fffffffffffffe5)); err == nil {
		t.Fatal("want error")
	}
	// same through a Listed log
	var id eth.Hash
	id[31] = 1
	l := &eth.Log{Topics: []eth.Hash{TopicListed, id, {}, {}}, DataHex: "0x" + hex.EncodeToString(d)}
	OpenIndex("", eth.Address{}, 0).apply(l)
	// a length word near MaxInt64
	binary.BigEndian.PutUint64(d[24:32], 32)
	binary.BigEndian.PutUint64(d[56:64], 0x7fffffffffffffff)
	if _, err := decodeString(d, 32); err == nil {
		t.Fatal("want error for huge length")
	}
}
