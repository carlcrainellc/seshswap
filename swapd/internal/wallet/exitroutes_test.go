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
	old := exitRoutesWait
	exitRoutesWait = 500 * time.Millisecond
	t.Cleanup(func() { exitRoutesWait = old })
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

	// routes don't come up: no Connected, no DNS change, the reason shown
	f.setNoRoutes("no default gateway")
	c, err := s.connectExit(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if c.Phase != phaseNoRoutes || c.RoutesUp || c.RoutesReason != "no default gateway" || !c.SessionLive || !c.ExitCapable {
		t.Fatalf("conn %+v", c)
	}
	time.Sleep(200 * time.Millisecond)
	if len(d.set) != 0 {
		t.Fatalf("DNS switched without routes: %v", d.set)
	}
	if st := s.connStatus(); st["state"] == "connected" {
		t.Fatalf("sidebar says connected: %v", st)
	}
	if m := s.exitsStatusMap()["connected"].(map[string]any); m["phase"] != phaseNoRoutes || m["routesReason"] != "no default gateway" {
		t.Fatalf("status %v", m)
	}

	// they come up: now connected, and DNS switches
	f.setNoRoutes("")
	s.exitTick()
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
