package wallet

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/exits"
	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/srouter/srtest"
)

const testExit = "wo7xwuzostufug8ptrth5natahx53nug9jsazm783nere8rj68ky.sesh"

// fakeRPC records exit calls; patched=false acts like a stock router.
type fakeRPC struct {
	mu        sync.Mutex
	calls     []string
	patched   bool
	mapped    map[string]bool
	noRoutes  string // when set, list_exits says the routes are down, with this reason
	noSession bool   // the exit never answers: session_live false
	mapErr    string // map_exit fails with this
}

func (f *fakeRPC) rpc(ep string) *exits.RouterRPC {
	return &exits.RouterRPC{Endpoint: ep, Call: func(_ context.Context, ep, m string, p []byte) ([]byte, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, ep+" "+m)
		if !f.patched {
			return []byte("null"), nil
		}
		var req struct {
			Address string `json:"address"`
		}
		_ = json.Unmarshal(p, &req)
		if f.mapped == nil {
			f.mapped = map[string]bool{}
		}
		switch m {
		case "llarp.map_exit":
			f.mapped[req.Address] = true // like the router: recorded even if the reply is lost
			if f.mapErr != "" {
				b, _ := json.Marshal(map[string]any{"error": f.mapErr})
				return b, nil
			}
		case "llarp.unmap_exit":
			delete(f.mapped, req.Address)
		case "llarp.list_exits":
			var ex []map[string]any
			for a := range f.mapped {
				ex = append(ex, map[string]any{"address": a, "session_live": !f.noSession, "exit_capable": !f.noSession})
			}
			out := map[string]any{"routes_up": f.noRoutes == "", "exits": ex}
			if f.noRoutes != "" {
				out["routes_reason"] = f.noRoutes
			}
			b, _ := json.Marshal(map[string]any{"result": out})
			return b, nil
		}
		return []byte(`{"result":"OK"}`), nil
	}}
}

func (f *fakeRPC) setNoRoutes(r string) {
	f.mu.Lock()
	f.noRoutes = r
	f.mu.Unlock()
}

func (f *fakeRPC) seen(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func exitTestServer(t *testing.T) (*Server, *fakeRPC) {
	t.Helper()
	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.Registry, _ = eth.ParseAddress("0xc3576aefa11cef4e7758dacfa9e58560eca148d8")
	s.Cfg.ExitLease, _ = eth.ParseAddress("0x145049b20f382Ca9D0B281a986dc02DEb653e65c")
	o := srtest.Options("ok")
	o.BaseDir = t.TempDir()
	o.Log = t.Logf
	o.Exits = true
	s.router = srouter.New(o)
	noExisting(t, s)
	s.idleStop = 100 * time.Millisecond
	restoreTestWallet(t, h)
	f := &fakeRPC{patched: true}
	s.ex.rpcFor = f.rpc
	t.Cleanup(s.stopRouter)
	return s, f
}

func TestExitRouterModes(t *testing.T) {
	s, f := exitTestServer(t)
	ctx := context.Background()
	s.Cfg.Transport = ModeDirect
	if _, err := s.pickExitRouter(ctx); err == nil || err.Error() != msgExitsDirect {
		t.Fatalf("direct: %v", err)
	}
	s.Cfg.Transport = ModeExisting
	if _, err := s.pickExitRouter(ctx); err == nil || err.Error() != msgExitsExisting {
		t.Fatalf("existing without an RPC address: %v", err)
	}
	s.Cfg.ExitRouterRPC = "tcp://127.0.0.1:1190"
	if rt, err := s.pickExitRouter(ctx); err != nil || rt.Source != SourceExisting || rt.RPC != s.Cfg.ExitRouterRPC {
		t.Fatalf("existing with a patched router: %+v %v", rt, err)
	}
	f.patched = false
	if _, err := s.pickExitRouter(ctx); err == nil {
		t.Fatal("existing mode used a stock router for exits")
	}
	s.Cfg.Transport = ModeAuto
	if rt, err := s.pickExitRouter(ctx); err != nil || rt.Source != SourceBundled {
		t.Fatalf("auto with a stock own router should fall back to bundled: %+v %v", rt, err)
	}
	f.patched = true
	if rt, _ := s.pickExitRouter(ctx); rt.Source != SourceExisting {
		t.Fatalf("auto with a patched own router: %+v", rt)
	}
	if s.router.Running() {
		t.Fatal("picking a router started one")
	}
	if !localRPC("ipc:///run/sr.sock") || !localRPC("tcp://127.0.0.1:1190") || localRPC("tcp://10.1.2.3:1190") || localRPC("http://127.0.0.1:1") {
		t.Fatal("localRPC")
	}
}

func addLease(s *Server, buyer string, end uint64) {
	s.ex.mu.Lock()
	s.exitsLoadLocked()
	s.ex.Leases = append(s.ex.Leases, exitLeaseRec{Lease: exits.Lease{ListingID: 4, BuyerSesh: buyer, End: end, Amount: "100000"}, ExitSesh: testExit})
	s.ex.mu.Unlock()
}

func TestExitBundledConnectHoldsRouterAndUnmapsAtExpiry(t *testing.T) {
	s, f := exitTestServer(t)
	ctx := context.Background()
	s.Cfg.Transport = ModeAuto
	buyer, err := s.buyerSesh(ctx, exitRoute{Source: SourceBundled})
	if err != nil || !exits.ValidClientSesh(buyer) {
		t.Fatalf("buyer %q %v", buyer, err)
	}
	k, _ := exits.RouterKey(s.exitKeyFile(), false)
	if exits.SeshFromPub(k.Public().(ed25519.PublicKey)) != buyer {
		t.Fatal("buyer address doesn't match the key file")
	}
	end := uint64(time.Now().Add(2 * time.Second).Unix())
	addLease(s, buyer, end)
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) { return end, nil }

	c, err := s.connectExit(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceBundled || !s.router.Running() || s.router.LaunchedKeyFile() != s.exitKeyFile() {
		t.Fatalf("conn %+v running=%v key=%q", c, s.router.Running(), s.router.LaunchedKeyFile())
	}
	if !strings.HasPrefix(s.router.RPC(), "ipc://") || f.seen("llarp.map_exit") != 1 {
		t.Fatalf("rpc %q calls %v", s.router.RPC(), f.calls)
	}
	if st := s.connStatus(); !strings.Contains(st["label"].(string), "exit #4") {
		t.Fatalf("label %v", st)
	}
	// Idle for longer than idleStop: a mapped exit keeps the router up.
	time.Sleep(300 * time.Millisecond)
	s.reconcile()
	if !s.router.Running() {
		t.Fatal("idle stop closed the router under a mapped exit")
	}
	// Paid time ends: unmap, forget the exit key for the next launch.
	time.Sleep(time.Until(time.Unix(int64(end), 0)) + 50*time.Millisecond)
	s.exitTick()
	if f.seen("llarp.unmap_exit") != 1 || s.exitHoldsRouter() {
		t.Fatalf("expiry: calls %v held=%v", f.calls, s.exitHoldsRouter())
	}
	if !strings.Contains(s.exitsStatusMap()["last"].(string), "paid time ended") {
		t.Fatalf("last %v", s.exitsStatusMap()["last"])
	}
	s.reconcile()
	if s.router.Running() {
		t.Fatal("router kept running after the exit ended and nothing needs it")
	}
	// Connecting again after the end is refused.
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) { return end, nil }
	if _, err := s.connectExit(ctx, 4); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("connect after end: %v", err)
	}
}

func TestExitExistingRouterIsNeverStopped(t *testing.T) {
	s, f := exitTestServer(t)
	ctx := context.Background()
	dns, _ := withExisting(t, s)
	_ = dns
	s.Cfg.Transport = ModeExisting
	s.Cfg.ExitRouterRPC = "tcp://127.0.0.1:1190"
	// The fake DNS answers every .sesh with an A record, not the CNAME: the
	// wallet must say it couldn't read the router's address, not guess one.
	if _, err := s.buyerSesh(ctx, exitRoute{Source: SourceExisting}); err == nil {
		t.Fatal("made up an address for the existing router")
	}
	// Pretend the existing router's address is known and paid for.
	end := uint64(time.Now().Add(time.Hour).Unix())
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) { return end, nil }
	conn := &exitConn{ListingID: 4, ExitSesh: testExit, Source: SourceExisting, RPC: s.Cfg.ExitRouterRPC, End: end}
	s.ex.mu.Lock()
	s.ex.Conn = conn
	s.ex.mu.Unlock()
	s.reconcile()
	s.disconnectExit(ctx, "test")
	if f.seen("tcp://127.0.0.1:1190 llarp.unmap_exit") != 1 {
		t.Fatalf("calls %v", f.calls)
	}
	if f.seen("halt") != 0 || s.router.Running() {
		t.Fatal("the wallet touched a router it did not start, or started its own")
	}
}

func TestExitsHTTPNeedsSetup(t *testing.T) {
	s, h := newTestServer(t)
	_ = s
	if code, _ := do(t, h, "GET", "/v1/exits", "", ""); code != 404 {
		t.Fatalf("exits without ExitLease: %d", code)
	}
	if code, _ := do(t, h, "POST", "/v1/exits/buy", `{"listingId":4,"hours":1}`, ""); code != 404 {
		t.Fatalf("buy without ExitLease: %d", code)
	}
}
