package wallet

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

// Amounts typed by the user or sent by a web page (Satchel) are untrusted text.
func FuzzAmountParsers(f *testing.F) {
	f.Add("0.5", 18, "")
	f.Add("1.0000000000000000001", 6, "123")
	f.Add("+.", 18, "-1")
	f.Fuzz(func(t *testing.T, s string, dec int, wei string) {
		d := dec % 40
		if v, err := parseUnits(s, d); err == nil {
			if v.Sign() <= 0 {
				t.Fatalf("parseUnits(%q,%d) = %v without error", s, d, v)
			}
			// formatting then parsing again gives the same amount
			back, err := parseUnits(formatUnits(v, d), d)
			if err != nil || back.Cmp(v) != 0 {
				t.Fatalf("round trip %q -> %v -> %q -> %v (%v)", s, v, formatUnits(v, d), back, err)
			}
		}
		if v, err := parseValue(wei, s); err == nil && v.Sign() <= 0 {
			t.Fatalf("parseValue(%q,%q) = %v", wei, s, v)
		}
		_ = weiToETH(new(big.Int).SetBytes([]byte(s)))
	})
}

// unlockedHandler makes a fresh demo wallet, unlocked, with no network use.
func unlockedHandler(f *testing.F) (http.Handler, string) {
	s := NewServer(Config{DataDir: f.TempDir(), Listen: "127.0.0.1:0", XmrNet: keys.Stagenet, Demo: true})
	h := s.Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	b := w.Body.String()
	const k = `name="wallet-token" content="`
	i := strings.Index(b, k)
	if i < 0 {
		f.Fatal("no token")
	}
	tok := b[i+len(k):]
	tok = tok[:strings.IndexByte(tok, '"')]
	call := func(path, body string) map[string]any {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Wallet-Token", tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		if w.Code != 200 {
			f.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return m
	}
	start := call("/v1/onboard/start", "{}")
	words := start["words"].([]any)
	ans := map[string]string{}
	for _, c := range start["checks"].([]any) {
		n := int(c.(float64))
		ans[strconv.Itoa(n)] = words[n-1].(string)
	}
	body, _ := json.Marshal(map[string]any{"password": "correct-password-ok", "words": ans})
	call("/v1/onboard/confirm", string(body))
	return h, tok
}

// The local wallet API: random JSON bodies, paths, Origin headers and token
// presence against the routes that need no network. Satchel requests come
// from arbitrary web pages.
func FuzzWalletAPI(f *testing.F) {
	h, tok := unlockedHandler(f)
	routes := []string{"/v1/status", "/v1/swaps", "/v1/swaps/", "/v1/satchel/propose", "/v1/satchel/pending",
		"/v1/satchel/reject", "/v1/wallet/backup"}
	f.Add(uint8(3), "", `{"eth":"0.01","minXmr":5}`, true, false)
	f.Add(uint8(3), "https://evil.example", `{"usdc":"1e9"}`, false, true)
	f.Add(uint8(2), "", `x/../../etc`, true, false)
	f.Add(uint8(5), "", `{"id":"00"}`, true, true)
	f.Fuzz(func(t *testing.T, which uint8, origin, body string, withToken, post bool) {
		p := routes[int(which)%len(routes)]
		if p == "/v1/swaps/" {
			p += strings.NewReplacer("?", "", "#", "", " ", "").Replace(body)
			if len(p) > 200 {
				p = p[:200]
			}
		}
		m := "GET"
		var rd io.Reader
		if post {
			m = "POST"
			rd = strings.NewReader(body)
		}
		if p == "/v1/swaps" && post {
			return // starts a swap with a maker over the network
		}
		r, err := http.NewRequest(m, "http://127.0.0.1"+p, rd)
		if err != nil {
			return
		}
		if withToken {
			r.Header.Set("X-Wallet-Token", tok)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if p == "/v1/wallet/backup" && w.Code == 200 && !withToken {
			t.Fatalf("backup without token: %s", w.Body.String())
		}
	})
}
