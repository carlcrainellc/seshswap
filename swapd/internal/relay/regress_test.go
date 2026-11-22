package relay

import (
	"bytes"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Found by FuzzRelayHTTP: with an operator policy (Refuse) set, an intent
// with no swap panicked (Swap.ID on nil) before the missing-field check, and
// a claim with a fee over 256 bits panicked while hashing.
func TestRelayMalformedRequestsNoPanic(t *testing.T) {
	s := &Server{K: &eth.Contract{ChainID: big.NewInt(1)}, Refuse: func(string, eth.Hash) bool { return false }}
	for _, c := range []struct{ path, body string }{
		{"/v1/intent", `{"kind":"ack"}`},
		{"/v1/intent", `{"kind":"claimIntent","swap":{"value":-1,"claimKey":2,"refundKey":3}}`},
		{"/v1/relay", `{"swap":{"value":1,"claimKey":2,"refundKey":3},"secret":1,"fee":` + strings.Repeat("9", 90) + `}`},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", c.path, bytes.NewReader([]byte(c.body))))
		if w.Code != 422 {
			t.Fatalf("%s %s: %d %s", c.path, c.body, w.Code, w.Body.String())
		}
	}
}
