package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

// fakeFaucet checks the work and answers like the real faucet.
func fakeFaucet(t *testing.T, bits int, claim func(eth, xmr string) (int, any)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("X-SeshSwap-Client"), "seshswap-wallet/") {
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "app only"})
			return
		}
		switch r.URL.Path {
		case "/api/challenge":
			json.NewEncoder(w).Encode(map[string]any{"challenge": "abc.def", "bits": bits})
		case "/api/claim":
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			h := sha256.Sum256([]byte(req["challenge"] + "|" + strings.ToLower(req["ethAddress"]) + "|" + req["xmrAddress"] + "|" + req["nonce"]))
			if faucetLeadingZeros(h) < bits {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "Proof of work does not check out."})
				return
			}
			code, body := claim(req["ethAddress"], req["xmrAddress"])
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(body)
		}
	}))
}

const tEth = "0x00000000000000000000000000000000000000Ab"
const tXmr = "5AAAA"

func TestFaucetClaimPayoutsAndPausedXMR(t *testing.T) {
	srv := fakeFaucet(t, 12, func(e, x string) (int, any) {
		if e != tEth || x != tXmr {
			t.Errorf("addresses %q %q", e, x)
		}
		return 200, map[string]any{"ok": true, "payouts": map[string]any{
			"eth":  map[string]string{"amount": "0.0005", "tx": "0xaa"},
			"usdc": map[string]string{"amount": "25", "tx": "0xbb"},
			"xmr":  map[string]string{"note": "XMR drip is paused until the faucet is topped up."},
		}}
	})
	defer srv.Close()
	s := &Server{}
	res, err := s.ClaimFaucet(context.Background(), srv.URL, tEth, tXmr, "https://sepolia.arbiscan.io/")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Payouts) != 3 {
		t.Fatalf("%+v", res)
	}
	if res.Payouts[0].TxURL != "https://sepolia.arbiscan.io/tx/0xaa" || res.Payouts[1].Amount != "25" {
		t.Fatalf("%+v", res.Payouts)
	}
	if res.Payouts[2].Tx != "" || !strings.Contains(res.Payouts[2].Note, "paused") {
		t.Fatalf("xmr %+v", res.Payouts[2])
	}
}

func TestFaucetCooldownMessage(t *testing.T) {
	srv := fakeFaucet(t, 4, func(e, x string) (int, any) {
		return 429, map[string]any{"ok": false, "error": "This Ethereum address already claimed in the last 24 hours."}
	})
	defer srv.Close()
	res, err := (&Server{}).ClaimFaucet(context.Background(), srv.URL, tEth, tXmr, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Message, "24 hours") {
		t.Fatalf("%+v", res)
	}
}

func TestFaucetRefusesHugeWork(t *testing.T) {
	srv := fakeFaucet(t, 40, nil)
	defer srv.Close()
	if _, err := (&Server{}).ClaimFaucet(context.Background(), srv.URL, tEth, tXmr, ""); err == nil || !strings.Contains(err.Error(), "too much work") {
		t.Fatalf("got %v", err)
	}
}

func TestFaucetOffline(t *testing.T) {
	_, err := (&Server{}).ClaimFaucet(context.Background(), "http://127.0.0.1:1", tEth, tXmr, "")
	if err != errFaucetOffline {
		t.Fatalf("got %v", err)
	}
}

func TestFaucetOnlyOnTestnet(t *testing.T) {
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{ChainID: 421614, XmrNet: keys.Stagenet}, DefaultFaucetURL},
		{Config{ChainID: 421614, XmrNet: keys.Stagenet, FaucetURL: "https://x.trycloudflare.com/"}, "https://x.trycloudflare.com"},
		{Config{ChainID: 421614, XmrNet: keys.Stagenet, FaucetURL: "off"}, ""},
		{Config{ChainID: 42161, XmrNet: keys.Stagenet}, ""},
		{Config{ChainID: 421614, XmrNet: keys.Mainnet}, ""},
		{Config{ChainID: 421614, XmrNet: keys.Stagenet, AllowMainnet: true}, ""},
		{Config{ChainID: 421614, XmrNet: keys.Stagenet, Demo: true}, ""},
	}
	for i, c := range cases {
		if got := (&Server{Cfg: c.cfg}).faucetURL(); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

func TestFaucetClaimNeedsUnlock(t *testing.T) {
	s := &Server{Cfg: Config{ChainID: 421614, XmrNet: keys.Stagenet}}
	w := httptest.NewRecorder()
	s.faucetClaim(w, httptest.NewRequest("POST", "/v1/faucet/claim", nil))
	if w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
}
