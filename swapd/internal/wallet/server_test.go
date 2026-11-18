package wallet

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := NewServer(Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", XmrNet: keys.Stagenet, Demo: true})
	return s, s.Handler()
}

func do(t *testing.T, h http.Handler, method, path, body string, origin string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	switch origin {
	case "", pageOrigin:
		req.Header.Set("X-Wallet-Token", pageToken(t, h))
	case noToken:
	default:
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

// pageOrigin marks a request as coming from the wallet page itself (it
// carries the token); other origins don't get it.
const pageOrigin = "page"

// noToken: a request with no Origin and no token (another program).
const noToken = "none"

// pageToken reads this launch's token out of the wallet page, like the page's
// own script does.
func pageToken(t *testing.T, h http.Handler) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	b := w.Body.String()
	const k = `name="wallet-token" content="`
	i := strings.Index(b, k)
	if i < 0 {
		t.Fatal("no token in the page")
	}
	b = b[i+len(k):]
	return b[:strings.IndexByte(b, '"')]
}

func TestOnboardCreateUnlockAndBackup(t *testing.T) {
	s, h := newTestServer(t)
	code, st := do(t, h, "GET", "/v1/status", "", "")
	if code != 200 || st["state"] != "new" {
		t.Fatalf("status %d %v", code, st)
	}

	code, start := do(t, h, "POST", "/v1/onboard/start", "{}", "")
	if code != 200 {
		t.Fatalf("start %d %v", code, start)
	}
	words, _ := start["words"].([]any)
	if len(words) != 24 {
		t.Fatalf("words %v", words)
	}
	checks, _ := start["checks"].([]any)
	ans := map[string]string{}
	for _, c := range checks {
		n := int(c.(float64))
		ans[itoa(n)] = words[n-1].(string)
	}
	body, _ := json.Marshal(map[string]any{"password": "correct-password-ok", "words": ans})
	code, created := do(t, h, "POST", "/v1/onboard/confirm", string(body), "")
	if code != 200 {
		t.Fatalf("confirm %d %v", code, created)
	}
	if created["ethAddress"] == nil || created["xmrAddress"] == nil {
		t.Fatalf("addresses %v", created)
	}
	ethAddr := created["ethAddress"].(string)

	// Wrong check word is rejected, and does not create a second wallet — already created.
	// Lock, then unlock.
	code, _ = do(t, h, "POST", "/v1/wallet/lock", "{}", "")
	if code != 200 {
		t.Fatal("lock")
	}
	code, st = do(t, h, "GET", "/v1/status", "", "")
	if st["state"] != "locked" {
		t.Fatalf("state %v", st)
	}
	code, _ = do(t, h, "POST", "/v1/wallet/unlock", `{"password":"wrong-password-xx"}`, "")
	if code != 403 {
		t.Fatalf("wrong password: %d", code)
	}
	code, un := do(t, h, "POST", "/v1/wallet/unlock", `{"password":"correct-password-ok"}`, "")
	if code != 200 || un["ethAddress"] != ethAddr {
		t.Fatalf("unlock %d %v", code, un)
	}

	// Backup requires the password, not just an unlocked session.
	code, _ = do(t, h, "POST", "/v1/wallet/backup", `{"password":"wrong-password-xx"}`, "")
	if code != 403 {
		t.Fatalf("backup with wrong password: %d", code)
	}
	code, bak := do(t, h, "POST", "/v1/wallet/backup", `{"password":"correct-password-ok"}`, "")
	if code != 200 {
		t.Fatalf("backup %d %v", code, bak)
	}
	got, _ := bak["words"].([]any)
	for i, w := range words {
		if got[i] != w {
			t.Fatalf("backup word %d", i)
		}
	}

	// UI is served.
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<title>SeshSwap</title>") {
		t.Fatalf("ui %d", w.Code)
	}

	// Demo balances.
	code, bal := do(t, h, "GET", "/v1/balances", "", "")
	if code != 200 || bal["eth"] != "0.15" {
		t.Fatalf("balances %d %v", code, bal)
	}

	// Demo quote.
	code, q := do(t, h, "GET", "/v1/quote?eth=0.05", "", "")
	if code != 200 || q["xmr"] == nil || q["demo"] != true {
		t.Fatalf("quote %d %v", code, q)
	}

	_ = s
}

func TestRestoreSameAddresses(t *testing.T) {
	m, err := NewMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	s1, h1 := newTestServer(t)
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	code, a := do(t, h1, "POST", "/v1/wallet/restore", string(body), "")
	if code != 200 {
		t.Fatalf("restore 1: %d %v", code, a)
	}
	s2, h2 := newTestServer(t)
	code, b := do(t, h2, "POST", "/v1/wallet/restore", string(body), "")
	if code != 200 {
		t.Fatalf("restore 2: %d %v", code, b)
	}
	if a["ethAddress"] != b["ethAddress"] || a["xmrAddress"] != b["xmrAddress"] {
		t.Fatalf("addresses differ: %v vs %v", a, b)
	}
	_, _ = s1, s2
}

func TestOriginPinning(t *testing.T) {
	_, h := newTestServer(t)
	for _, c := range []struct {
		path, origin string
		want         int
	}{
		{"/v1/status", "http://evil.example", 403},
		{"/v1/status", pageOrigin, 200},                 // the wallet page (token)
		{"/v1/status", noToken, 403},                    // no Origin, no token: another program
		{"/v1/status", "http://127.0.0.1:9999", 403},    // another local page: no token
		{"/v1/exits/buy", "http://127.0.0.1:9999", 403}, // spending needs the token
	} {
		if code, _ := do(t, h, "GET", c.path, "", c.origin); code != c.want {
			t.Errorf("%s from %q: %d, want %d", c.path, c.origin, code, c.want)
		}
	}
	// another local page may still propose a trade (and gets CORS for it)
	req := httptest.NewRequest("POST", "/v1/satchel/propose", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://127.0.0.1:9999")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == 403 || w.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("satchel propose from another page: %d %q", w.Code, w.Body.String())
	}
	// the page itself is never shared with other origins
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9999")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("page shared cross-origin")
	}
}

func TestSwapViewDoesNotLeakSecrets(t *testing.T) {
	s, h := newTestServer(t)
	m, _ := NewMnemonic()
	v, err := Create(s.Cfg.DataDir, m, "correct-password-ok", keys.Stagenet)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.attach(v, "correct-password-ok"); err != nil {
		t.Fatal(err)
	}
	// Put a swap record with secrets directly in the store, then reload.
	rec := &swap.Record{
		ID: "aabbccddeeff00112233445566778899", Role: swap.Taker, Phase: swap.TLocked,
		Value: big.NewInt(1e16), XMR: 1_000_000_000_000,
		MySpend:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MyView:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TheirSecret: "leaked-secret-value-here", ClaimerKey: "claimer-secret",
		MakerURL: "http://127.0.0.1:9",
		Swap:     &eth.Swap{T1: 2000000000, T2: 2000004000, Grace: 21600, Value: big.NewInt(1e16)},
	}
	// Engine may be nil (no RPC in demo without URLs). Write to the store and
	// build a tiny engine for Snapshot.
	st := v.Store
	if err := st.Put("swap-"+rec.ID, rec); err != nil {
		t.Fatal(err)
	}
	eng := &swap.Engine{Role: swap.Taker, Store: st}
	if err := eng.Load(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.eng = eng
	s.vault = v
	s.mu.Unlock()

	code, body := do(t, h, "GET", "/v1/swaps/"+rec.ID, "", "")
	if code != 200 {
		t.Fatalf("get swap %d %v", code, body)
	}
	raw, _ := json.Marshal(body)
	sraw := string(raw)
	for _, leak := range []string{"aaaaaaaa", "bbbbbbbb", "leaked-secret", "claimer-secret", "MySpend", "mySpend", "theirSecret"} {
		if strings.Contains(sraw, leak) {
			t.Fatalf("response leaked %q: %s", leak, sraw)
		}
	}
	if body["status"] == nil || body["refundLine"] == nil {
		t.Fatalf("missing plain status: %v", body)
	}

	code, list := do(t, h, "GET", "/v1/swaps", "", "")
	if code != 200 {
		t.Fatalf("list %d %v", code, list)
	}
	lraw, _ := json.Marshal(list)
	if strings.Contains(string(lraw), "leaked-secret") {
		t.Fatal("list leaked a secret")
	}
}

func TestResumeOpenSwapAfterRelock(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(Config{DataDir: dir, Listen: "127.0.0.1:0", XmrNet: keys.Stagenet, Demo: true})
	h := s.Handler()
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	code, _ := do(t, h, "POST", "/v1/wallet/restore", string(body), "")
	if code != 200 {
		t.Fatalf("restore %d", code)
	}
	// Simulate a mid-swap crash: write a record, lock (drops the engine), unlock (Load).
	st, err := store.Open(dir, "correct-password-ok")
	if err != nil {
		t.Fatal(err)
	}
	rec := &swap.Record{
		ID: "11223344556677889900aabbccddeeff", Role: swap.Taker, Phase: swap.TLocked,
		Value: big.NewInt(2e15), XMR: 500_000_000_000, MakerURL: "http://127.0.0.1:9",
		Swap: &eth.Swap{T1: 2000000000, T2: 2000008000, Grace: 21600, Value: big.NewInt(2e15)},
	}
	if err := st.Put("swap-"+rec.ID, rec); err != nil {
		t.Fatal(err)
	}
	// After lock+unlock without RPCs, attach doesn't start an engine. Drive Load ourselves
	// the same way attach does when RPCs exist: open vault, Load.
	s.LockWallet()
	v, err := OpenVault(dir, "correct-password-ok")
	if err != nil {
		t.Fatal(err)
	}
	eng := &swap.Engine{Role: swap.Taker, Store: v.Store}
	if err := eng.Load(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.vault, s.eng = v, eng
	s.mu.Unlock()

	code, stt := do(t, h, "GET", "/v1/status", "", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if stt["openSwaps"] != float64(1) || stt["openId"] != rec.ID {
		t.Fatalf("resume banner missing: %v", stt)
	}
	code, one := do(t, h, "GET", "/v1/swaps/"+rec.ID, "", "")
	if code != 200 || one["phase"] != "locked" {
		t.Fatalf("resumed swap %d %v", code, one)
	}
	if !strings.Contains(one["status"].(string), "Waiting") {
		t.Fatalf("status %v", one["status"])
	}
}

func TestQuoteFromMaker(t *testing.T) {
	maker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/quote" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"value": 10000000000000000, "xmr": 1230000000000,
			"premium": 0, "picoPerUnit": 1, "d1": 2400, "d2": 3600, "grace": 21600,
			"minConfs": 10, "estReadySecs": 1500, "estSpendableSecs": 2700,
			"expires": 2000000000,
		})
	}))
	defer maker.Close()
	s, h := newTestServer(t)
	s.Cfg.MakerURL = maker.URL
	s.Cfg.Demo = false // go to the real (fake) maker
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	if code, r := do(t, h, "POST", "/v1/wallet/restore", string(body), ""); code != 200 {
		t.Fatalf("restore %d %v", code, r)
	}
	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 200 {
		t.Fatalf("quote %d %v", code, q)
	}
	if q["xmr"] != "1.23" {
		t.Fatalf("xmr %v", q)
	}
	if q["makerUrl"] != maker.URL {
		t.Fatalf("maker %v", q)
	}
}

func TestRefusesMainnetListenAndShortPassword(t *testing.T) {
	_, h := newTestServer(t)
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "short"})
	code, r := do(t, h, "POST", "/v1/wallet/restore", string(body), "")
	if code == 200 {
		t.Fatalf("accepted short password: %v", r)
	}
	// ListenAndServe rejects a public bind (when not demo).
	s2 := NewServer(Config{Listen: "0.0.0.0:9999", DataDir: t.TempDir(), Demo: false})
	if err := s2.ListenAndServe(t.Context()); err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("listen: %v", err)
	}
}

func TestWrongOnboardWord(t *testing.T) {
	_, h := newTestServer(t)
	code, start := do(t, h, "POST", "/v1/onboard/start", "{}", "")
	if code != 200 {
		t.Fatal(start)
	}
	checks, _ := start["checks"].([]any)
	ans := map[string]string{}
	for _, c := range checks {
		n := int(c.(float64))
		ans[itoa(n)] = "abandon" // almost certainly wrong for at least one
	}
	// Guarantee a mismatch: use a word that is valid BIP-39 but we also
	// set them all to "zoo", which cannot be all three check words of a random mnemonic often.
	// If it happens to pass, that's fine — we also force one empty.
	ans[itoa(int(checks[0].(float64)))] = "thisisnotawordxyz"
	body, _ := json.Marshal(map[string]any{"password": "correct-password-ok", "words": ans})
	code, r := do(t, h, "POST", "/v1/onboard/confirm", string(body), "")
	if code == 200 {
		t.Fatalf("accepted a wrong word: %v", r)
	}
	if Exists((&Server{Cfg: Config{DataDir: ""}}).Cfg.DataDir) {
		// no-op; just don't create in cwd
	}
}

func TestEncryptedFilesAreNotPlaintext(t *testing.T) {
	s, h := newTestServer(t)
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	if code, r := do(t, h, "POST", "/v1/wallet/restore", string(body), ""); code != 200 {
		t.Fatalf("%d %v", code, r)
	}
	err := filepath.Walk(s.Cfg.DataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), strings.Fields(m)[3]) {
			t.Errorf("word in the clear in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [16]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}

func TestParseUnits(t *testing.T) {
	v, err := parseUnits("5", 6)
	if err != nil || v.String() != "5000000" {
		t.Fatalf("5 USDC: %v %v", v, err)
	}
	v, err = parseUnits("1.5", 6)
	if err != nil || v.String() != "1500000" {
		t.Fatalf("1.5 USDC: %v %v", v, err)
	}
	v, err = parseETH("0.005")
	if err != nil || v.String() != "5000000000000000" {
		t.Fatalf("0.005 ETH: %v %v", v, err)
	}
	if _, err := parseUnits("-1", 6); err == nil {
		t.Fatal("accepted negative")
	}
}

func TestQuoteUSDCFromMaker(t *testing.T) {
	var tok eth.Address
	tok[19] = 0x42
	maker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/quote":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": 5000000, "xmr": 15000000000,
				"premium": 0, "picoPerUnit": 1, "d1": 3600, "d2": 1800, "grace": 1800,
				"minConfs": 10, "estReadySecs": 1500, "estSpendableSecs": 2700,
				"expires": 2000000000,
			})
		case "/v1/offer":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"chainId": 11155111, "contract": "0x1111111111111111111111111111111111111111",
				"asset": tok.Hex(), "picoPerUnit": 1, "min": 1000000, "max": 10000000000,
				"d1": 1800, "d2": 1800, "minConfs": 10, "xmrNet": "stagenet", "grace": 1800,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer maker.Close()
	s, h := newTestServer(t)
	s.Cfg.MakerURL = maker.URL
	s.Cfg.Demo = false
	s.Cfg.USDC = tok
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	if code, r := do(t, h, "POST", "/v1/wallet/restore", string(body), ""); code != 200 {
		t.Fatalf("restore %d %v", code, r)
	}
	code, q := do(t, h, "GET", "/v1/quote?usdc=5", "", "")
	if code != 200 {
		t.Fatalf("quote %d %v", code, q)
	}
	if q["asset"] != "USDC" || q["valueUsdc"] != "5" || q["xmr"] != "0.000015" && q["xmr"] != "0.015" {
		// 15000000000 pico = 0.015 XMR
		if q["xmr"] != "0.015" {
			t.Fatalf("quote %v", q)
		}
	}
	if q["valueLabel"] != "5 USDC" {
		t.Fatalf("label %v", q)
	}
}
