package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestExitNotConnectedUntilRoutesUp(t *testing.T) {
	shortMapWait(t)
	s, f := exitTestServer(t)
	ctx := context.Background()
	s.Cfg.Transport = ModeAuto
	buyer, _ := s.buyerSesh(ctx, exitRoute{Source: SourceBundled})
	end := uint64(time.Now().Add(time.Hour).Unix())
	s.ex.mu.Lock()
	s.exitsLoadLocked()
	s.ex.mu.Unlock()
	addLease(s, buyer, end)
	s.ex.mu.Lock()
	s.ex.Leases[len(s.ex.Leases)-1].DNS = "10.195.0.53"
	s.ex.mu.Unlock()
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) { return end, nil }
	d := &fakeDNS{}
	s.ex.dns = d
	s.ex.linkFor = func(string) (string, error) { return "swrabc123", nil }

	// routes don't come up: an error, the exit is unmapped again, no DNS change
	f.setNoRoutes("no default gateway")
	c, err := s.connectExit(ctx, 4)
	if err == nil || c != nil {
		t.Fatalf("connected without routes: %+v", c)
	}
	if !strings.Contains(err.Error(), "didn't come up (no default gateway)") || !strings.Contains(err.Error(), "normal connection is back") {
		t.Fatal(err)
	}
	if f.seen("llarp.unmap_exit") != 1 || s.exitHoldsRouter() {
		t.Fatalf("left mapped: unmaps %d", f.seen("llarp.unmap_exit"))
	}
	if len(d.set) != 0 {
		t.Fatalf("DNS switched without routes: %v", d.set)
	}
	if st := s.connStatus(); st["exit"] != nil {
		t.Fatalf("sidebar still shows the exit: %v", st)
	}

	// routes up: connected, and DNS switches
	f.setNoRoutes("")
	if _, err := s.connectExit(ctx, 4); err != nil {
		t.Fatal(err)
	}
	waitDNS(t, s, "on")
	if st := s.connStatus(); st["state"] != "connected" {
		t.Fatalf("sidebar %v", st)
	}

	// they drop while connected: shown, and DNS goes back
	f.setNoRoutes("first hop not pinned")
	s.exitTick()
	s.ex.mu.Lock()
	phase, dns := s.ex.Conn.Phase, s.ex.Conn.DNSState
	s.ex.mu.Unlock()
	if phase != phaseNoRoutes || dns != "" || d.restores != 1 || d.changed {
		t.Fatalf("after drop: phase %s dns %q restores %d", phase, dns, d.restores)
	}

	// back up: DNS switches again
	f.setNoRoutes("")
	s.exitTick()
	waitDNS(t, s, "on")
	if len(d.set) != 2 {
		t.Fatalf("set %v", d.set)
	}
}

func shortMapWait(t *testing.T) {
	t.Helper()
	old, oldR := exitMapWait, exitRoutesWait
	exitMapWait, exitRoutesWait = 1500*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { exitMapWait, exitRoutesWait = old, oldR })
}

// mapByAddr maps testExit through the exits panel and returns the HTTP result.
func mapByAddr(t *testing.T, s *Server) (int, map[string]any) {
	t.Helper()
	return do(t, s.Handler(), "POST", "/v1/exits/map", `{"address":"`+testExit+`"}`, "")
}

// The exit never answers (say it is down): after the map
// wait the wallet unmaps it and says so, instead of sitting on "Mapping".
func TestExitMapRollsBackWhenExitNotAnswering(t *testing.T) {
	shortMapWait(t)
	s, f := exitTestServer(t)
	s.Cfg.Transport = ModeAuto
	f.noSession = true
	start := time.Now()
	code, m := mapByAddr(t, s)
	if code != 409 || !strings.Contains(fmt.Sprint(m["error"]), "isn't answering (no path to it after 1 s)") {
		t.Fatalf("%d %v", code, m)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	f.mu.Lock()
	left := len(f.mapped)
	f.mu.Unlock()
	if left != 0 || f.seen("llarp.unmap_exit") != 1 {
		t.Fatalf("still mapped on the router: %d", left)
	}
	if st := s.exitsStatusMap(); st["connected"] != nil {
		t.Fatalf("status still shows a connection: %v", st)
	}
}

// map_exit itself keeps failing: still unmapped at the end (the router may
// have recorded it), with the router's error in the message.
func TestExitMapErrorUnmaps(t *testing.T) {
	shortMapWait(t)
	s, f := exitTestServer(t)
	s.Cfg.Transport = ModeAuto
	f.mapErr = "no path to exit"
	code, m := mapByAddr(t, s)
	if code != 409 || !strings.Contains(fmt.Sprint(m["error"]), "no path to exit") {
		t.Fatalf("%d %v", code, m)
	}
	f.mu.Lock()
	left := len(f.mapped)
	f.mu.Unlock()
	if left != 0 {
		t.Fatal("left mapped after a failed map")
	}
}

// Another VPN holds the route: Rusty's preflight says so and nothing is mapped.
func TestExitMapRefusedUnderOtherVPN(t *testing.T) {
	s, f := exitTestServer(t)
	s.Cfg.ExitRouterRPC = "tcp://127.0.0.1:1190"
	old := MapPreflight
	t.Cleanup(func() { MapPreflight = old })
	msg := "Another VPN is still holding your connection (NordVPN). Disconnect it or turn off its kill switch, then map again."
	MapPreflight = func(context.Context) (string, []string, error) { return msg, nil, nil }
	rt := exitRoute{Source: SourceExisting, RPC: s.Cfg.ExitRouterRPC}
	mk := func() *exitConn { return &exitConn{ExitSesh: testExit, Source: SourceExisting} }
	if _, err := s.mapOn(context.Background(), rt, mk()); err == nil || err.Error() != msg {
		t.Fatalf("got %v", err)
	}
	if f.seen("llarp.map_exit") != 0 {
		t.Fatal("mapped anyway")
	}
	// VPN off: the preflight passes and the map goes ahead.
	MapPreflight = func(context.Context) (string, []string, error) { return "", nil, nil }
	if _, err := s.mapOn(context.Background(), rt, mk()); err != nil {
		t.Fatal(err)
	}
	if f.seen("llarp.map_exit") != 1 {
		t.Fatal("didn't map")
	}
}

// Routes "up" but the public IP didn't change: traffic isn't going through
// the exit (another VPN's routes win). Unmapped, with an error.
func TestExitMapFailsWhenIPUnchanged(t *testing.T) {
	shortMapWait(t)
	s, f := exitTestServer(t)
	s.Cfg.Transport = ModeAuto
	oldB, oldP := exitIPBefore, exitProbe
	t.Cleanup(func() { exitIPBefore, exitProbe = oldB, oldP })
	exitIPBefore = func(context.Context) string { return "198.51.100.7" }
	exitProbe = func(context.Context) (string, error) { return "198.51.100.7", nil }
	code, m := mapByAddr(t, s)
	if code != 409 || !strings.Contains(fmt.Sprint(m["error"]), "Couldn't reach the internet through") {
		t.Fatalf("%d %v", code, m)
	}
	f.mu.Lock()
	left := len(f.mapped)
	f.mu.Unlock()
	if left != 0 {
		t.Fatal("left mapped")
	}
}

// Rusty's exit guard unmaps a connected exit (it stopped answering, or a VPN
// came on): the wallet notices, drops it and shows Rusty's reason.
func TestExitGoneUnderneathShowsRustyReason(t *testing.T) {
	s, f := exitTestServer(t)
	s.Cfg.Transport = ModeAuto
	if code, m := mapByAddr(t, s); code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	old := LastRollback
	t.Cleanup(func() { LastRollback = old })
	why := "Exit 9khgtg5q…xcbhcto stopped answering, so it was unmapped. Your normal connection is back."
	LastRollback = func() (string, time.Time) { return why, time.Now() }
	f.mu.Lock()
	f.mapped = map[string]bool{}
	f.mu.Unlock()
	s.exitTick()
	st := s.exitsStatusMap()
	if st["connected"] != nil || !strings.Contains(fmt.Sprint(st["last"]), "stopped answering") {
		t.Fatalf("%v", st)
	}
}

func TestLockDisconnectsExit(t *testing.T) {
	s, d, f := connectWithDNS(t)
	waitDNS(t, s, "on")
	if code, _ := do(t, s.Handler(), "POST", "/v1/wallet/lock", "{}", ""); code != 200 {
		t.Fatal(code)
	}
	if s.exitHoldsRouter() || d.changed || f.seen("llarp.unmap_exit") != 1 {
		t.Fatalf("lock left the exit up: restores %d unmaps %d", d.restores, f.seen("llarp.unmap_exit"))
	}
}

func TestExitQuoteForConfirm(t *testing.T) {
	s, _, _ := connectWithDNS(t)
	s.ex.priceFor = func(_ context.Context, id uint64, asset string, h uint32) (string, error) {
		return "0.10 USDC", nil
	}
	s.ex.mu.Lock()
	s.ex.Conn.Asset = "usdc"
	end := s.ex.Conn.End
	s.ex.mu.Unlock()
	code, m := do(t, s.Handler(), "POST", "/v1/exits/quote", `{"hours":1}`, "")
	if code != 200 || m["price"] != "0.10 USDC" || m["asset"] != "usdc" || uint64(m["newEnd"].(float64)) != end+3600 {
		t.Fatalf("%d %v", code, m)
	}
	if code, _ := do(t, s.Handler(), "POST", "/v1/exits/quote", `{"hours":1}`, noToken); code != 403 {
		t.Fatalf("quote without token: %d", code)
	}
}

// A network error on the usual currency is shown, not quietly swapped for the
// other one; only "doesn't take that currency" switches.
func TestExitQuoteSwitchesOnlyOnWrongCurrency(t *testing.T) {
	s, _, _ := connectWithDNS(t)
	s.ex.mu.Lock()
	s.ex.Conn.Asset = "usdc"
	s.ex.mu.Unlock()
	usdcErr := errors.New("dial tcp: i/o timeout")
	s.ex.priceFor = func(_ context.Context, id uint64, asset string, h uint32) (string, error) {
		if asset == "usdc" {
			return "", usdcErr
		}
		return "0.00002 ETH", nil
	}
	if code, m := do(t, s.Handler(), "POST", "/v1/exits/quote", `{"hours":1}`, ""); code != 409 || !strings.Contains(fmt.Sprint(m["error"]), "i/o timeout") {
		t.Fatalf("network error: %d %v", code, m)
	}
	usdcErr = errors.New("this exit doesn't take that currency")
	if code, m := do(t, s.Handler(), "POST", "/v1/exits/quote", `{"hours":1}`, ""); code != 200 || m["asset"] != "eth" {
		t.Fatalf("wrong currency: %d %v", code, m)
	}
}
