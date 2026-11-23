package wallet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

func TestConfSettingSavedAndUsedInDemoQuote(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(Config{DataDir: dir, Listen: "127.0.0.1:0", XmrNet: keys.Stagenet, Demo: true})
	h := s.Handler()
	if code, r := do(t, h, "GET", "/v1/settings/confirmations", "", ""); code != 200 || r["confirmations"] != "standard" {
		t.Fatalf("default %d %v", code, r)
	}
	if code, _ := do(t, h, "PUT", "/v1/settings/confirmations", `{"confirmations":"lots"}`, ""); code != 400 {
		t.Fatalf("bad level accepted: %d", code)
	}
	if code, r := do(t, h, "PUT", "/v1/settings/confirmations", `{"confirmations":"safer"}`, ""); code != 200 || r["minConfs"] != float64(20) {
		t.Fatalf("safer %d %v", code, r)
	}
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	if code, r := do(t, h, "POST", "/v1/wallet/restore", string(body), ""); code != 200 {
		t.Fatalf("restore %d %v", code, r)
	}
	code, q := do(t, h, "GET", "/v1/quote?eth=0.05", "", "")
	if code != 200 || q["minConfs"] != float64(20) || q["extraWaitSecs"] != float64(1200) || q["confLevel"] != "safer" {
		t.Fatalf("demo quote %d %v", code, q)
	}
	// Kept across a restart.
	s2 := NewServer(Config{DataDir: dir, Listen: "127.0.0.1:0", XmrNet: keys.Stagenet, Demo: true})
	if s2.wantConfs() != 20 {
		t.Fatalf("not saved: %d", s2.wantConfs())
	}
	if code, r := do(t, s2.Handler(), "GET", "/v1/settings", "", ""); code != 200 || r["confirmations"] != "safer" {
		t.Fatalf("settings %d %v", code, r)
	}
}

// The wallet asks the maker for its setting and refuses a maker that ignores it.
func TestQuoteAsksMakerForConfs(t *testing.T) {
	var asked string
	honor := true
	maker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/quote" {
			http.NotFound(w, r)
			return
		}
		asked = r.URL.Query().Get("minConfs")
		confs := 10
		if honor && asked == "30" {
			confs = 30
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": 10000000000000000, "xmr": 1230000000000,
			"premium": 0, "picoPerUnit": 1, "d1": 9000, "d2": 3600, "grace": 21600,
			"minConfs": confs, "estReadySecs": 1500, "estSpendableSecs": 2700, "blockSecs": 120,
			"expires": 2000000000,
		})
	}))
	defer maker.Close()
	s, h := newTestServer(t)
	s.Cfg.MakerURL = maker.URL
	s.Cfg.Demo = false
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	if code, r := do(t, h, "POST", "/v1/wallet/restore", string(body), ""); code != 200 {
		t.Fatalf("restore %d %v", code, r)
	}
	// Standard: the request is unchanged.
	if code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", ""); code != 200 || asked != "" || q["extraWaitSecs"] != nil {
		t.Fatalf("standard %d %q %v", code, asked, q)
	}
	do(t, h, "PUT", "/v1/settings/confirmations", `{"confirmations":"safest"}`, "")
	// 1.23 XMR needs 15 by size; Safest adds 15 more blocks.
	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 200 || asked != "30" || q["minConfs"] != float64(30) || q["sizeConfs"] != float64(15) || q["extraWaitSecs"] != float64(1800) {
		t.Fatalf("safest %d %q %v", code, asked, q)
	}
	honor = false // an older maker
	code, q = do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 422 || !strings.Contains(q["error"].(string), "older version") {
		t.Fatalf("old maker %d %v", code, q)
	}
}
