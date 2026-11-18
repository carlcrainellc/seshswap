package wallet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
)

// anvil's well-known public dev keys (local devnet only)
var devKeys = []string{
	"ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
	"59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d",
	"5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a",
	"7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6",
	"47e179ec197488593b187f80a00eb0da91f1b9d0b13f8733639f19c30a34926a",
	"8b3a350cf5c34c9194ca85829a2df0ec3153be0318b5e2d3348e872092edffba",
}

func forgeArtifact(t *testing.T, file, name string) []byte {
	t.Helper()
	root, _ := filepath.Abs("../../..")
	b, err := os.ReadFile(filepath.Join(root, "contracts", "out", file, name+".json"))
	if err != nil {
		t.Skip("no forge artifacts (run forge build in contracts/)")
	}
	var j struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	_ = json.Unmarshal(b, &j)
	code, err := hex.DecodeString(strings.TrimPrefix(j.Bytecode.Object, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func anvil(t *testing.T) (*eth.Client, string) {
	t.Helper()
	p, err := exec.LookPath("anvil")
	if err != nil {
		p = filepath.Join(os.Getenv("HOME"), ".foundry/bin/anvil")
	}
	if _, err := os.Stat(p); err != nil {
		t.Skip("anvil not installed")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(p, "--port", fmt.Sprint(port), "--silent")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	c := eth.NewClient(url)
	for i := 0; i < 100; i++ {
		if _, err := c.ChainID(context.Background()); err == nil {
			return c, url
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("anvil did not start")
	return nil, ""
}

func deploy(t *testing.T, c *eth.Client, k *eth.Key, code []byte) (eth.Address, uint64) {
	t.Helper()
	rc, err := c.SendAndWait(context.Background(), k, nil, nil, code)
	if err != nil || rc.Contract == nil {
		t.Fatalf("deploy: %v", err)
	}
	return *rc.Contract, rc.Block()
}

// mockFeed deploys MockAggregatorV3(desc, answer) with 8 decimals.
func mockFeed(t *testing.T, c *eth.Client, k *eth.Key, usd int64) eth.Address {
	desc := []byte("TEST / USD")
	args := append(eth.WordU(64), eth.WordBig(big.NewInt(usd*100_000_000))...)
	args = append(args, eth.WordU(uint64(len(desc)))...)
	pad := make([]byte, 32)
	copy(pad, desc)
	args = append(args, pad...)
	a, _ := deploy(t, c, k, append(forgeArtifact(t, "MockAggregatorV3.sol", "MockAggregatorV3"), args...))
	return a
}

func wei(s string) *big.Int {
	r, _ := new(big.Rat).SetString(s)
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	return new(big.Int).Quo(r.Num(), r.Denom())
}

type fakeListing struct {
	sesh      string
	bond      string
	picoPerE  float64 // XMR (pico) per 1 ETH
	listingID uint64  // what it claims in its quote (0 = its real id)
}

// Bonded makers on a real registry (anvil), all served by one fake HTTP
// server that answers by Host name, reached through a fake existing router.
func TestRegistryQuotePicksBestBondedMakerWithinCap(t *testing.T) {
	c, url := anvil(t)
	ctx := context.Background()
	ks := make([]*eth.Key, len(devKeys))
	for i, h := range devKeys {
		ks[i], _ = eth.KeyFromHex(h)
	}
	v3, _ := deploy(t, c, ks[0], forgeArtifact(t, "XmrSwapV3.sol", "XmrSwapV3"))
	xmrFeed := mockFeed(t, c, ks[0], 300)
	ethFeed := mockFeed(t, c, ks[0], 3000)
	p := registry.PlaceholderParams(v3, eth.Address{}, ethFeed)
	p.USDCDecimals = 0
	reg, blk := deploy(t, c, ks[0], append(forgeArtifact(t, "SeshRegistry.sol", "SeshRegistry"), p.ConstructorArgs()...))

	// Fair: 1 ETH = $3000 = 10 XMR.
	makers := []fakeListing{
		{sesh: "makera.sesh", bond: "0.002", picoPerE: 10.0e12},                // cap 0.01 ETH, fair
		{sesh: "makerb.sesh", bond: "0.004", picoPerE: 10.2e12},                // cap 0.02, best
		{sesh: "makerc.sesh", bond: "0.004", picoPerE: 8.0e12},                 // 20% worse than the feed
		{sesh: "makerd.sesh", bond: "0.004", picoPerE: 11e12, listingID: 1234}, // claims another listing
	}
	mt, _ := registry.Tag(registry.TagMaker)
	st, _ := registry.Tag("forum")
	ids := map[string]uint64{}
	for i, m := range makers {
		if _, err := c.SendAndWait(ctx, ks[i+1], &reg, wei(m.bond), registry.EncodeRegister(m.sesh, mt, "test maker "+m.sesh)); err != nil {
			t.Fatal(err)
		}
		ids[m.sesh] = uint64(i + 1)
	}
	if _, err := c.SendAndWait(ctx, ks[5], &reg, wei("0.0002"), registry.EncodeRegister("someforum.sesh", st, "A Monero forum")); err != nil {
		t.Fatal(err)
	}

	var asked []string
	var askedMu sync.Mutex // makers are asked in parallel
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		askedMu.Lock()
		asked = append(asked, host+r.URL.Path)
		askedMu.Unlock()
		var m *fakeListing
		for i := range makers {
			if makers[i].sesh == host {
				m = &makers[i]
			}
		}
		if host == "configured.sesh" {
			m = &fakeListing{sesh: host, picoPerE: 9.9e12}
		}
		if m == nil {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/v1/offer":
			_ = json.NewEncoder(w).Encode(map[string]any{"contract": v3.Hex(), "asset": eth.Address{}.Hex()})
		case "/v1/quote":
			v, _ := new(big.Int).SetString(r.URL.Query().Get("value"), 10)
			pico, _ := new(big.Float).Mul(new(big.Float).SetInt(v), big.NewFloat(m.picoPerE/1e18)).Uint64()
			out := map[string]any{"value": v, "xmr": pico, "premium": 0, "d1": 2400, "d2": 3600, "grace": 21600,
				"minConfs": 10, "expires": 2000000000}
			if id := ids[m.sesh]; id != 0 {
				if m.listingID != 0 {
					id = m.listingID
				}
				out["registry"], out["listingId"] = reg.Hex(), id
			}
			_ = json.NewEncoder(w).Encode(out)
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.EthRPC, s.Cfg.Contract = url, v3
	s.Cfg.XMRUSDFeed, s.Cfg.ETHUSDFeed, s.Cfg.FairMaxBps = xmrFeed, ethFeed, 500
	s.Cfg.Registry, s.Cfg.RegistryFromBlock = reg, blk
	s.reg.port = ":" + port
	bundledFake(t, s)
	withExisting(t, s)
	restoreTestWallet(t, h)

	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 200 {
		t.Fatalf("quote %d %v", code, q)
	}
	if q["sesh"] != "makerb.sesh" || q["source"] != "registry" || q["xmr"] != "0.102" {
		t.Fatalf("best %v %v %v", q["sesh"], q["source"], q["xmr"])
	}
	why := map[string]string{}
	for _, x := range q["quotes"].([]any) {
		m := x.(map[string]any)
		r, _ := m["reason"].(string)
		if m["ok"] == true {
			r = "ok"
		}
		why[m["sesh"].(string)] = r
	}
	if len(why) != 4 || why["makera.sesh"] != "ok" || why["makerb.sesh"] != "ok" ||
		!strings.Contains(why["makerc.sesh"], "worse than the price feed") ||
		!strings.Contains(why["makerd.sesh"], "different listing") {
		t.Fatalf("reasons %v", why)
	}
	for _, a := range asked {
		if strings.HasPrefix(a, "someforum.sesh") {
			t.Fatal("asked a non-maker listing for a quote")
		}
	}

	// 0.015 ETH is over maker A's cap (0.002 × 5 = 0.01): it isn't even asked.
	asked = nil
	code, q = do(t, h, "GET", "/v1/quote?eth=0.015", "", "")
	if code != 200 || q["sesh"] != "makerb.sesh" {
		t.Fatalf("quote2 %d %v", code, q)
	}
	for _, x := range q["quotes"].([]any) {
		m := x.(map[string]any)
		if m["sesh"] == "makera.sesh" && (m["ok"] == true || !strings.Contains(m["reason"].(string), "bond cap")) {
			t.Fatalf("maker A not capped: %v", m)
		}
	}
	for _, a := range asked {
		if strings.HasPrefix(a, "makera.sesh") && strings.HasSuffix(a, "/v1/quote") {
			t.Fatal("over-cap maker was asked for a quote")
		}
	}

	// Over every cap: no bonded quote, so the configured maker is the fallback.
	code, q = do(t, h, "GET", "/v1/quote?eth=0.05", "", "")
	if code != 502 || !strings.Contains(fmt.Sprint(q["error"]), "no bonded maker") {
		t.Fatalf("no fallback configured: %d %v", code, q)
	}
	s.Cfg.MakerSeshURL = "http://configured.sesh:" + port
	code, q = do(t, h, "GET", "/v1/quote?eth=0.05", "", "")
	if code != 200 || q["source"] != "configured" || q["fallback"] != true || q["xmr"] != "0.495" {
		t.Fatalf("fallback %d %v", code, q)
	}

	// Search screen: text, tag, and inactive listings hidden.
	code, r := do(t, h, "GET", "/v1/registry/search?q=monero", "", "")
	if code != 200 || len(r["listings"].([]any)) != 1 {
		t.Fatalf("search %d %v", code, r)
	}
	code, r = do(t, h, "GET", "/v1/registry/search?tag=maker", "", "")
	if code != 200 || len(r["listings"].([]any)) != 4 || r["tags"].(map[string]any)["forum"] != float64(1) {
		t.Fatalf("tag search %d %v", code, r)
	}
	if _, err := c.SendAndWait(ctx, ks[3], &reg, nil, registry.EncodeRequestExit(3)); err != nil {
		t.Fatal(err)
	}
	s.reg.lastSync = time.Time{}
	if _, r = do(t, h, "GET", "/v1/registry/search?tag=maker", "", ""); len(r["listings"].([]any)) != 3 {
		t.Fatalf("exiting maker still listed: %v", r["listings"])
	}
}
