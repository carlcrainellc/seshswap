package exits

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Real Leased event from Arbitrum Sepolia (lease 0 on listing 4, 15 Nov 2026).
func fixtureLog(t *testing.T) eth.Log {
	t.Helper()
	b, err := os.ReadFile("testdata/leased-log.json")
	if err != nil {
		t.Fatal(err)
	}
	var l eth.Log
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

const (
	fixBuyer = "rgwybw99m4d464rbi9z95a7a364n1hauo74ip486ssckf1agw7zo.sesh"
	fixExit  = "wo7xwuzostufug8ptrth5natahx53nug9jsazm783nere8rj68ky.sesh"
)

func TestDecodeLeasedFixture(t *testing.T) {
	l := fixtureLog(t)
	got, err := DecodeLeased(&l)
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaseID != 0 || got.ListingID != 4 || got.Hours != 1 || got.BuyerSesh != fixBuyer {
		t.Fatalf("decoded %+v", got)
	}
	if got.End-got.Start != 3600 || got.Amount != "100000" {
		t.Fatalf("times/amount %+v", got)
	}
	if !strings.EqualFold(got.Asset.Hex(), "0x91280FA5450d481569C5fEBFdeF4E794C00cBCfa") {
		t.Fatalf("asset %s", got.Asset.Hex())
	}
	// Wrong topic: not ours.
	l2 := l
	l2.Topics = append([]eth.Hash{{1}}, l.Topics[1:]...)
	if x, err := DecodeLeased(&l2); err == nil && x != nil {
		t.Fatal("decoded a log with another topic")
	}
}

func TestMetaRoundTrip(t *testing.T) {
	m := Meta{Price: "0.10 USDC/h", Chain: "arbitrum-sepolia", Payout: "0x3331887875a9a9609Ad9410aA5777f441B85051C",
		Country: "DE", Policy: "tcp/80,tcp/443,dns", DNS: "10.195.0.53", Note: "test"}
	s, err := m.String()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "exit; ") || len(s) > 280 {
		t.Fatalf("meta %q", s)
	}
	back := ParseMeta(s)
	if back.Price != m.Price || back.Country != m.Country || back.DNS != m.DNS || back.Payout != m.Payout || back.Policy != m.Policy {
		t.Fatalf("round trip %+v", back)
	}
	long := m
	long.Note = strings.Repeat("x", 300)
	if _, err := long.String(); err == nil {
		t.Fatal("over-long meta accepted")
	}
}

func TestSeshFromPubAndRouterKey(t *testing.T) {
	// The bundled router printed "Client started @ rgwy….sesh" for this public key.
	pub, _ := hex.DecodeString(fixBuyerPub)
	if got := SeshFromPub(ed25519.PublicKey(pub)); got != fixBuyer {
		t.Fatalf("SeshFromPub = %s", got)
	}
	if got := SeshFromPub(make([]byte, 32)); got != strings.Repeat("y", 52)+".sesh" {
		t.Fatalf("zero key = %s", got)
	}
	p := filepath.Join(t.TempDir(), "k", "router.key")
	if _, err := RouterKey(p, false); err == nil {
		t.Fatal("missing key without create should fail")
	}
	k1, err := RouterKey(p, true)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	b, _ := os.ReadFile(p)
	if len(strings.TrimSpace(string(b))) != 128 {
		t.Fatalf("key file is %d chars, router wants 128 hex", len(strings.TrimSpace(string(b))))
	}
	k2, err := RouterKey(p, true)
	if err != nil || !k1.Equal(k2) {
		t.Fatal("key not stable across loads")
	}
	if !ValidClientSesh(SeshFromPub(k1.Public().(ed25519.PublicKey))) {
		t.Fatal("generated address is not a valid client .sesh")
	}
}

func TestAdmissionSignVerify(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	pub := key.Public().(ed25519.PublicKey)
	exp := time.Now().Add(time.Hour).Unix()
	a, err := SignAdmission(key, fixExit, fixBuyer, exp, "lease-145049b2-0")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if r := VerifyAdmission(a, pub, fixExit, fixBuyer, now); r != "" {
		t.Fatalf("valid admission rejected: %s", r)
	}
	if r := VerifyAdmission(a, pub, fixExit, fixBuyer, exp+1); r == "" {
		t.Fatal("expired admission accepted")
	}
	if r := VerifyAdmission(a, pub, fixBuyer, fixExit, now); r == "" {
		t.Fatal("admission for another seller accepted")
	}
	_, other, _ := ed25519.GenerateKey(nil)
	if r := VerifyAdmission(a, other.Public().(ed25519.PublicKey), fixExit, fixBuyer, now); r == "" {
		t.Fatal("admission under another issuer accepted")
	}
	bad := *a
	bad.Exp++
	if r := VerifyAdmission(&bad, pub, fixExit, fixBuyer, now); r == "" {
		t.Fatal("tampered admission accepted")
	}

	// Cross-check with the router's own verifier when it's around:
	// EXIT_LEASE_VERIFY=/path/to/exit-lease-verify go test ./internal/exits
	if bin := os.Getenv("EXIT_LEASE_VERIFY"); bin != "" {
		st := &Store{Path: filepath.Join(t.TempDir(), "admissions.json")}
		if err := st.Put(a); err != nil {
			t.Fatal(err)
		}
		// Exit status 0 = allowed (prints the reason, "admission").
		out, err := exec.Command(bin, st.Path, hex.EncodeToString(pub), fixExit, fixBuyer).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "admission" {
			t.Fatalf("router verifier: %v %s", err, out)
		}
		out, err = exec.Command(bin, st.Path, hex.EncodeToString(pub), fixExit, "ybndrfg8ejkmcpqxot1uwisza345h769ybndrfg8ejkmcpqxot1u.sesh").CombinedOutput()
		if err == nil {
			t.Fatalf("router verifier admitted a stranger: %s", out)
		}
		out, err = exec.Command(bin, st.Path, hex.EncodeToString(pub), fixBuyer, fixBuyer).CombinedOutput()
		if err == nil {
			t.Fatalf("router verifier admitted for another seller: %s", out)
		}
	}
}

// fakeChain is a tiny JSON-RPC server: a head block and the fixture log.
func fakeChain(t *testing.T, head uint64, logs []eth.Log) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []json.RawMessage
		}
		_ = json.Unmarshal(b, &req)
		var res any
		switch req.Method {
		case "eth_getBlockByNumber":
			res = map[string]string{"number": hexN(head), "timestamp": hexN(uint64(time.Now().Unix()))}
		case "eth_getLogs":
			var q struct{ FromBlock, ToBlock string }
			_ = json.Unmarshal(req.Params[0], &q)
			out := []eth.Log{}
			for _, l := range logs {
				from, _ := eth.HexBig(q.FromBlock)
				to, _ := eth.HexBig(q.ToBlock)
				if l.Block() >= from.Uint64() && l.Block() <= to.Uint64() {
					out = append(out, l)
				}
			}
			res = out
		case "eth_chainId":
			res = "0x66eee"
		default:
			res = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}))
}

func hexN(n uint64) string { return "0x" + strings.TrimLeft(hex.EncodeToString(eth.WordU(n)), "0") }

func TestWatcherAdmitsAndPrunes(t *testing.T) {
	l := fixtureLog(t)
	lease, _ := DecodeLeased(&l)
	srv := fakeChain(t, l.Block()+25, []eth.Log{l})
	defer srv.Close()
	_, key, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	addr, _ := eth.ParseAddress("0x145049b20f382Ca9D0B281a986dc02DEb653e65c")
	now := time.Unix(int64(lease.Start)+60, 0)
	w := &Watcher{K: &Contract{C: eth.NewClient(srv.URL), Addr: addr}, ListingID: 4, ExitSesh: fixExit, Key: key,
		Store: &Store{Path: filepath.Join(dir, "admissions.json")}, StatePath: filepath.Join(dir, "state.json"),
		FromBlock: l.Block() - 100, Confirmations: 20, Now: func() time.Time { return now }}
	res, err := w.Step(context.Background())
	if err != nil || len(res) != 1 || res[0].Reason != "" {
		t.Fatalf("step: %v %+v", err, res)
	}
	recs, _ := w.Store.Records()
	if len(recs) != 1 || recs[0].Client != fixBuyer || recs[0].Exp != int64(lease.End) {
		t.Fatalf("records %+v", recs)
	}
	if r := VerifyAdmission(recs[0], key.Public().(ed25519.PublicKey), fixExit, fixBuyer, now.Unix()); r != "" {
		t.Fatalf("stored admission doesn't verify: %s", r)
	}
	// Second pass: nothing new (the state file remembers the block).
	if res, err := w.Step(context.Background()); err != nil || len(res) != 0 {
		t.Fatalf("second step: %v %+v", err, res)
	}
	// A restarted watcher with the same state file doesn't re-admit either.
	w2 := *w
	w2.last = 0
	if res, err := w2.Step(context.Background()); err != nil || len(res) != 0 {
		t.Fatalf("restarted step: %v %+v", err, res)
	}
	// After the end time the admission is pruned.
	now = time.Unix(int64(lease.End)+1, 0)
	if _, err := w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recs, _ := w.Store.Records(); len(recs) != 0 {
		t.Fatalf("expired admission kept: %+v", recs)
	}
}

func TestWatcherRefusesOtherPayoutAndListing(t *testing.T) {
	l := fixtureLog(t)
	lease, _ := DecodeLeased(&l)
	_, key, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	other, _ := eth.ParseAddress("0x000000000000000000000000000000000000dEaD")
	w := &Watcher{K: &Contract{Addr: other}, ListingID: 4, ExitSesh: fixExit, Key: key, Payout: other,
		Store: &Store{Path: filepath.Join(dir, "a.json")}, Now: func() time.Time { return time.Unix(int64(lease.Start), 0) }}
	if a := w.admit(*lease); a.Reason == "" {
		t.Fatal("admitted a lease paid to another address")
	}
	w.Payout = eth.Address{}
	w.ListingID = 5
	if a := w.admit(*lease); a.Reason == "" {
		t.Fatal("admitted a lease for another listing")
	}
	w.ListingID = 4
	w.Now = func() time.Time { return time.Unix(int64(lease.End), 0) }
	if a := w.admit(*lease); a.Reason == "" {
		t.Fatal("admitted an already-ended lease")
	}
}

func TestRouterRPCPayloads(t *testing.T) {
	var calls []string
	rr := &RouterRPC{Endpoint: "ipc:///x", Call: func(_ context.Context, ep, m string, p []byte) ([]byte, error) {
		calls = append(calls, m+" "+string(p))
		switch m {
		case "llarp.list_exits":
			return []byte(`{"result":[{"address":"` + fixExit + `","session_live":true,"ranges":["0.0.0.0/0"]}]}`), nil
		case "llarp.map_exit", "llarp.unmap_exit":
			return []byte(`{"result":"OK"}`), nil
		}
		return []byte("null"), nil
	}}
	ctx := context.Background()
	if err := rr.SupportsExits(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := rr.MapExit(ctx, fixExit); err != nil || st.Known {
		t.Fatal(st, err)
	}
	if err := rr.UnmapExit(ctx, fixExit); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, `llarp.map_exit {"address":"`+fixExit+`","full_tunnel":true,"ip_ranges":[]}`) {
		t.Fatalf("map payload: %s", joined)
	}
	// newer builds: route status at the top of list_exits and in map_exit's reply
	newer := &RouterRPC{Endpoint: "ipc:///x", Call: func(_ context.Context, _, m string, _ []byte) ([]byte, error) {
		if m == "llarp.list_exits" {
			return []byte(`{"result":{"routes_up":false,"routes_reason":"no first-hop routes","exits":[{"address":"` + fixExit + `","session_live":true}]}}`), nil
		}
		return []byte(`{"result":{"routes_up":true,"message":"mapped 1 range(s)"}}`), nil
	}}
	l, err := newer.ListExits(ctx)
	if err != nil || len(l.Exits) != 1 || l.Exits[0].Address != fixExit || !l.Routes.Known || l.Routes.Up || l.Routes.Reason != "no first-hop routes" {
		t.Fatalf("%+v %v", l, err)
	}
	if st, err := newer.MapExit(ctx, fixExit); err != nil || !st.Known || !st.Up {
		t.Fatal(st, err)
	}
	stock := &RouterRPC{Endpoint: "ipc:///x", Call: func(context.Context, string, string, []byte) ([]byte, error) { return []byte("null"), nil }}
	var uc *UnknownCommandError
	if err := stock.SupportsExits(ctx); !errors.As(err, &uc) {
		t.Fatalf("stock router: %v", err)
	}
}

func TestOMQRefusesRemote(t *testing.T) {
	for _, ep := range []string{"tcp://10.0.0.5:1190", "tcp://example.com:1190", "http://127.0.0.1:1190"} {
		if _, err := OMQCall(context.Background(), ep, "llarp.version", nil); err == nil {
			t.Fatalf("%s accepted", ep)
		}
	}
}

func TestOwnSeshFromDNS(t *testing.T) {
	// A one-shot DNS server that answers localhost.sesh with a CNAME, the way the router does.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		n, a, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		q := buf[:n]
		resp := append([]byte{q[0], q[1], 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0}, q[12:]...)
		target := encName(fixBuyer)
		resp = append(resp, 0xC0, 12, 0, 5, 0, 1, 0, 0, 0, 60, 0, byte(len(target)))
		resp = append(resp, target...)
		_, _ = pc.WriteTo(resp, a)
	}()
	got, err := OwnSesh(context.Background(), pc.LocalAddr().String())
	if err != nil || got != fixBuyer {
		t.Fatalf("OwnSesh = %q, %v", got, err)
	}
}

// Public half of the bundled router's exit key used in the live test.
const fixBuyerPub = "21a800d3ff5e87af6881afeffde3b8cfb4297313877556e8feb598a2cb06a76f"
