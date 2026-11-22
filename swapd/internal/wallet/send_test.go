package wallet

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// fakeXmr is a monero-wallet-rpc stand-in: a balance, and it records relays.
type fakeXmr struct {
	mu       sync.Mutex
	unlocked uint64
	relayed  []string
	swept    int
}

func (f *fakeXmr) start(t *testing.T) *xmr.Wallet {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		var res any = map[string]any{}
		switch req.Method {
		case "get_balance":
			res = map[string]any{"balance": f.unlocked, "unlocked_balance": f.unlocked}
		case "transfer":
			d := req.Params["destinations"].([]any)[0].(map[string]any)
			amt := uint64(d["amount"].(float64))
			if amt+30_000_000 > f.unlocked {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": req.ID, "error": map[string]any{"code": -4, "message": "not enough money"}})
				return
			}
			res = map[string]any{"tx_hash": "aa11", "tx_metadata": "meta-1", "fee": 30_000_000, "amount": amt}
		case "sweep_all":
			f.swept++
			res = map[string]any{"tx_hash_list": []string{"bb22"}, "tx_metadata_list": []string{"meta-all"},
				"fee_list": []uint64{40_000_000}, "amount_list": []uint64{f.unlocked - 40_000_000}}
		case "relay_tx":
			f.relayed = append(f.relayed, req.Params["hex"].(string))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}))
	t.Cleanup(srv.Close)
	return xmr.NewWallet(srv.URL, "pw")
}

// fakeEth is an Arbitrum stand-in: balances, fees, gas, and it records raw txs.
type fakeEth struct {
	mu   sync.Mutex
	bal  *big.Int
	usdc *big.Int
	raws []string
}

func (f *fakeEth) start(t *testing.T) *eth.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		_ = json.Unmarshal(b, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		var res any
		switch req.Method {
		case "eth_chainId":
			res = "0x66eee"
		case "eth_getBalance":
			res = fmt.Sprintf("0x%x", f.bal)
		case "eth_call":
			res = "0x" + hex.EncodeToString(eth.WordBig(f.usdc))
		case "eth_estimateGas":
			res = "0x5208"
		case "eth_getBlockByNumber":
			res = map[string]string{"number": "0x20", "timestamp": fmt.Sprintf("0x%x", time.Now().Unix()), "baseFeePerGas": "0x5f5e100"}
		case "eth_maxPriorityFeePerGas":
			res = "0x0"
		case "eth_getTransactionCount":
			res = "0x7"
		case "eth_sendRawTransaction":
			f.raws = append(f.raws, req.Params[0].(string))
			res = "0x" + strings.Repeat("ab", 32)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}))
	t.Cleanup(srv.Close)
	return eth.NewClient(srv.URL)
}

// sendServer is an unlocked wallet with fake backends.
func sendServer(t *testing.T) (*Server, http.Handler, *fakeXmr, *fakeEth) {
	t.Helper()
	s := NewServer(Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", XmrNet: keys.Stagenet, Demo: true,
		USDC: eth.Address{0x75, 0x5d}})
	h := s.Handler()
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	if code, b := do(t, h, "POST", "/v1/wallet/restore", string(body), ""); code != 200 {
		t.Fatalf("restore %d %v", code, b)
	}
	fx := &fakeXmr{unlocked: 2_000_000_000_000}
	fe := &fakeEth{bal: big.NewInt(1e17), usdc: big.NewInt(50_000_000)}
	xw, c := fx.start(t), fe.start(t)
	s.mu.Lock()
	if s.vault == nil {
		t.Fatal("not unlocked after restore")
	}
	s.xmrW, s.eth = xw, c
	s.mu.Unlock()
	return s, h, fx, fe
}

func stagenetAddr(t *testing.T) string {
	a, _ := keys.RandomScalar()
	b, _ := keys.RandomScalar()
	s, err := keys.Address(keys.Stagenet, a.Public(), b.Public())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func jsonBody(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSendNeedsUnlock(t *testing.T) {
	_, h := newTestServer(t)
	code, _ := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": stagenetAddr(t), "amount": "1"}), "")
	if code != 401 {
		t.Fatalf("locked prepare: %d", code)
	}
	code, _ = do(t, h, "POST", "/v1/wallet/send/confirm", `{"id":"x"}`, "")
	if code != 401 {
		t.Fatalf("locked confirm: %d", code)
	}
	// Another program (no token) can't prepare a send either.
	code, _ = do(t, h, "POST", "/v1/wallet/send/prepare", `{}`, noToken)
	if code == 200 {
		t.Fatal("tokenless request allowed")
	}
}

func TestSendAddressChecks(t *testing.T) {
	s, h, _, _ := sendServer(t)
	a, _ := keys.RandomScalar()
	b, _ := keys.RandomScalar()
	mainAddr, _ := keys.Address(keys.Mainnet, a.Public(), b.Public())
	good := eth.Address{0x12, 0x34, 0xab, 0xcd, 0xef, 0xfa, 0xbe, 0xef}
	mixed := good.ChecksumHex()
	flipped := []byte(mixed)
	for i := 2; i < len(flipped); i++ { // flip the case of one letter
		if c := flipped[i]; c >= 'a' && c <= 'f' {
			flipped[i] = c - 32
			break
		} else if c >= 'A' && c <= 'F' {
			flipped[i] = c + 32
			break
		}
	}
	cases := []struct {
		coin, to string
		ok       bool
		msg      string
	}{
		{"XMR", stagenetAddr(t), true, ""},
		{"XMR", mainAddr, false, "mainnet"},
		{"XMR", s.vault.Keys.XmrAddr, false, "own address"},
		{"XMR", "5" + strings.Repeat("a", 94), false, ""},
		{"XMR", good.Hex(), false, "Monero address"},
		{"ETH", strings.ToLower(good.Hex()), true, ""},
		{"ETH", mixed, true, ""},
		{"ETH", string(flipped), false, "checksum"},
		{"ETH", "0x1234", false, "40"},
		{"ETH", "0x0000000000000000000000000000000000000000", false, "zero address"},
		{"ETH", s.vault.Keys.EthAddr.Hex(), false, "own address"},
		{"USDC", s.Cfg.USDC.Hex(), false, "token contract"},
		{"ETH", stagenetAddr(t), false, ""},
		{"DOGE", mixed, false, "pick"},
	}
	for _, c := range cases {
		code, body := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": c.coin, "to": c.to, "amount": "0.001"}), "")
		if (code == 200) != c.ok {
			t.Fatalf("%s %s: code %d %v", c.coin, c.to, code, body)
		}
		if !c.ok && c.msg != "" && !strings.Contains(fmt.Sprint(body["error"]), c.msg) {
			t.Fatalf("%s %s: error %q, want %q", c.coin, c.to, body["error"], c.msg)
		}
	}
}

func TestSendAmountParse(t *testing.T) {
	if _, err := parseSendAmount("0.0000001", 6); err == nil {
		t.Fatal("7 decimals accepted for USDC")
	}
	for _, bad := range []string{"", "0", "-1", "1e3", "1.2.3", "abc"} {
		if _, err := parseSendAmount(bad, 12); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	v, err := parseSendAmount(" 1,000.5 ", 6)
	if err != nil || v.String() != "1000500000" {
		t.Fatalf("got %v %v", v, err)
	}
}

func TestSendXmrPrepareConfirmOnce(t *testing.T) {
	_, h, fx, _ := sendServer(t)
	to := stagenetAddr(t)
	code, p := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "xmr", "to": to, "amount": "0.5"}), "")
	if code != 200 {
		t.Fatalf("prepare %d %v", code, p)
	}
	if p["amount"] != "0.5" || p["fee"] != "0.00003" || p["total"] != "0.50003" || p["feeCoin"] != "XMR" {
		t.Fatalf("review: %v", p)
	}
	if len(fx.relayed) != 0 {
		t.Fatal("relayed before confirm")
	}
	id := p["id"].(string)
	code, c := do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": id}), "")
	if code != 200 || c["txid"] != "aa11" || !strings.Contains(fmt.Sprint(c["url"]), "stagenet.xmrchain.net/tx/aa11") {
		t.Fatalf("confirm %d %v", code, c)
	}
	code, _ = do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": id}), "")
	if code != 409 {
		t.Fatalf("second confirm: %d", code)
	}
	if len(fx.relayed) != 1 || fx.relayed[0] != "meta-1" {
		t.Fatalf("relays: %v", fx.relayed)
	}
	// More than the wallet has.
	code, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": to, "amount": "3"}), "")
	if code != 400 || !strings.Contains(fmt.Sprint(p["error"]), "only 2 XMR") {
		t.Fatalf("too much: %d %v", code, p)
	}
	// Send all sweeps.
	code, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": to, "all": true}), "")
	if code != 200 || p["amount"] != "1.99996" || fx.swept != 1 {
		t.Fatalf("send all: %d %v", code, p)
	}
}

func TestSendExpiresAndLockForgets(t *testing.T) {
	s, h, fx, _ := sendServer(t)
	old := sendTTL
	sendTTL = 20 * time.Millisecond
	defer func() { sendTTL = old }()
	to := stagenetAddr(t)
	_, p := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": to, "amount": "0.1"}), "")
	time.Sleep(40 * time.Millisecond)
	code, c := do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": p["id"]}), "")
	if code != 409 || !strings.Contains(fmt.Sprint(c["error"]), "expired") {
		t.Fatalf("expired confirm: %d %v", code, c)
	}
	sendTTL = time.Minute
	_, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": to, "amount": "0.1"}), "")
	xw := s.xmrW
	s.LockWallet()
	v, err := OpenVault(s.Cfg.DataDir, "correct-password-ok")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.vault, s.xmrW = v, xw
	s.mu.Unlock()
	code, _ = do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": p["id"]}), "")
	if code != 409 {
		t.Fatalf("confirm after lock: %d", code)
	}
	// Cancel forgets it too.
	_, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": to, "amount": "0.1"}), "")
	do(t, h, "POST", "/v1/wallet/send/cancel", jsonBody(map[string]any{"id": p["id"]}), "")
	if code, _ = do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": p["id"]}), ""); code != 409 {
		t.Fatalf("confirm after cancel: %d", code)
	}
	if len(fx.relayed) != 0 {
		t.Fatalf("relayed: %v", fx.relayed)
	}
}

// addSwap puts an unfinished swap in a fresh engine on the server.
func addSwap(t *testing.T, s *Server, r *swap.Record) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eng == nil {
		s.eng = &swap.Engine{Role: r.Role, Store: s.vault.Store}
		if err := s.eng.Load(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.eng.Add(r); err != nil {
		t.Fatal(err)
	}
}

func TestSendKeepsWhatSwapsNeed(t *testing.T) {
	s, h, fx, fe := sendServer(t)
	to := stagenetAddr(t)
	// A maker-side swap holds 1.5 XMR of our 2.
	addSwap(t, s, &swap.Record{ID: "aa00000000000000000000000000000a", Role: swap.Maker, Phase: swap.MEthLocked,
		XMR: 1_500_000_000_000, Value: big.NewInt(1e15)})
	code, p := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": to, "amount": "1"}), "")
	if code != 400 || !strings.Contains(fmt.Sprint(p["error"]), "held for an open swap") {
		t.Fatalf("over reserve: %d %v", code, p)
	}
	code, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": to, "all": true}), "")
	if code != 400 || fx.swept != 0 {
		t.Fatalf("send all with a swap open: %d %v", code, p)
	}
	if code, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": to, "amount": "0.4"}), ""); code != 200 {
		t.Fatalf("within free: %d %v", code, p)
	}
	_ = fe
}

func TestSendEthAndUsdc(t *testing.T) {
	s, h, _, fe := sendServer(t)
	to := eth.Address{0x99, 0x01}
	// fee cap = 2*0.1 gwei; gas 21000*5/4 = 26250 -> 0.00000525 ETH
	code, p := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "ETH", "to": to.Hex(), "amount": "0.05"}), "")
	if code != 200 || p["fee"] != "0.00000525" || p["total"] != "0.05000525" || p["to"] != to.ChecksumHex() {
		t.Fatalf("eth prepare %d %v", code, p)
	}
	code, c := do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": p["id"]}), "")
	if code != 200 || !strings.HasPrefix(fmt.Sprint(c["url"]), s.explorer()+"/tx/0x") {
		t.Fatalf("eth confirm %d %v", code, c)
	}
	if len(fe.raws) != 1 {
		t.Fatalf("raw txs: %d", len(fe.raws))
	}
	// An unlocked taker swap holds 0.06 ETH: 0.05 more no longer fits.
	addSwap(t, s, &swap.Record{ID: "bb00000000000000000000000000000b", Role: swap.Taker, Phase: swap.TNegotiated,
		Value: big.NewInt(6e16), XMR: 1})
	code, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "ETH", "to": to.Hex(), "amount": "0.05"}), "")
	if code != 400 || !strings.Contains(fmt.Sprint(p["error"]), "kept for open swaps") {
		t.Fatalf("eth over reserve: %d %v", code, p)
	}
	// Send all leaves the swap's ETH, its gas, and the fee.
	code, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "ETH", "to": to.Hex(), "all": true}), "")
	if code != 200 || p["amount"] != "0.03979475" {
		t.Fatalf("eth all: %d %v", code, p)
	}
	// USDC: amount goes in the transfer call; ETH only pays gas.
	code, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "USDC", "to": to.Hex(), "amount": "12.5"}), "")
	if code != 200 || p["amount"] != "12.5" || !strings.Contains(fmt.Sprint(p["total"]), "12.5 USDC") {
		t.Fatalf("usdc prepare %d %v", code, p)
	}
	if code, c = do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": p["id"]}), ""); code != 200 {
		t.Fatalf("usdc confirm %d %v", code, c)
	}
	raw := fe.raws[1]
	want := hex.EncodeToString(append(append(eth.Selector("transfer(address,uint256)"), eth.WordAddr(to)...), eth.WordBig(big.NewInt(12_500_000))...))
	if !strings.Contains(raw, want) || !strings.Contains(strings.ToLower(raw), hex.EncodeToString(s.Cfg.USDC[:])) {
		t.Fatalf("usdc tx doesn't carry the transfer: %s", raw)
	}
	code, p = do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "USDC", "to": to.Hex(), "amount": "51"}), "")
	if code != 400 || !strings.Contains(fmt.Sprint(p["error"]), "only 50 USDC") {
		t.Fatalf("usdc too much: %d %v", code, p)
	}
}

func TestSendDemo(t *testing.T) {
	_, h := newTestServer(t)
	m, _ := NewMnemonic()
	do(t, h, "POST", "/v1/wallet/restore", jsonBody(map[string]string{"mnemonic": m, "password": "correct-password-ok"}), "")
	code, p := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "XMR", "to": stagenetAddr(t), "amount": "0.25"}), "")
	if code != 200 {
		t.Fatalf("demo prepare %d %v", code, p)
	}
	if code, c := do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": p["id"]}), ""); code != 200 || c["txid"] == "" {
		t.Fatalf("demo confirm %d %v", code, c)
	}
}
