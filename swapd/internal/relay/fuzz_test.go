package relay

import (
	"bytes"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// The relayer's HTTP API is open to anyone. With no chain behind it every
// request must end in an error response, never a panic, and never reach
// the chain unless the signature checks pass (they can't for random input).
func FuzzRelayHTTP(f *testing.F) {
	f.Add(uint8(0), []byte(`{"kind":"claimIntent","swap":{"value":1,"claimKey":2,"refundKey":3},"sig":{"v":27}}`))
	f.Add(uint8(1), []byte(`{"swap":{"value":1,"claimKey":2,"refundKey":3},"secret":1,"fee":99999999999999999999999999999999999999999999999999999999999999999999999999999999999}`))
	f.Add(uint8(0), []byte(`{"kind":"ack"}`))
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		k, _ := eth.KeyFromHex("0x" + "11" + string(bytes.Repeat([]byte("22"), 31)))
		s := &Server{K: &eth.Contract{Addr: eth.Address{1}, ChainID: big.NewInt(11155111)}, Key: k,
			Refuse: func(string, eth.Hash) bool { return false }, Logf: func(string, ...any) {}}
		paths := []string{"/v1/intent", "/v1/relay", "/v1/fee?asset=" + string(body)}
		p := paths[int(which)%len(paths)]
		m := http.MethodPost
		if which%3 == 2 {
			m = http.MethodGet
		}
		var req *http.Request
		if m == http.MethodGet {
			req = httptest.NewRequest(m, "/v1/fee", nil)
			q := req.URL.Query()
			q.Set("asset", string(body))
			req.URL.RawQuery = q.Encode()
		} else {
			req = httptest.NewRequest(m, p, bytes.NewReader(body))
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code == 200 && m == http.MethodPost {
			t.Fatalf("random input accepted: %s", w.Body.String())
		}
	})
}
