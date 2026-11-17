package wallet

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDNS struct {
	mu       sync.Mutex
	set      []string
	restores int
	setErr   error
	changed  bool
}

func (f *fakeDNS) Set(link, server string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return "", f.setErr
	}
	f.set = append(f.set, link+" "+server)
	f.changed = true
	return "file", nil
}

func (f *fakeDNS) Restore() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores++
	was := f.changed
	f.changed = false
	return was, nil
}

func waitDNS(t *testing.T, s *Server, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		s.ex.mu.Lock()
		got := ""
		if s.ex.Conn != nil {
			got = s.ex.Conn.DNSState
		}
		s.ex.mu.Unlock()
		if strings.HasPrefix(got, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("dns state never became %q", want)
}

func connectWithDNS(t *testing.T) (*Server, *fakeDNS, *fakeRPC) {
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
	tries := 0
	s.ex.linkFor = func(ip string) (string, error) {
		tries++
		if tries < 3 { // the router installs its routes a moment after map_exit
			return "", errors.New("not yet")
		}
		return "swrabc123", nil
	}
	if _, err := s.connectExit(ctx, 4); err != nil {
		t.Fatal(err)
	}
	return s, d, f
}

func TestExitDNSSwitchAndRestore(t *testing.T) {
	s, d, _ := connectWithDNS(t)
	waitDNS(t, s, "on")
	if len(d.set) != 1 || d.set[0] != "swrabc123 10.195.0.53" {
		t.Fatalf("set %v", d.set)
	}
	if _, err := os.Stat(s.dnsMarker()); err != nil {
		t.Fatal("no crash marker while DNS is changed")
	}
	if c := s.exitsStatusMap()["connected"].(map[string]any); c["dnsState"] != "on" {
		t.Fatalf("status %v", c)
	}
	s.disconnectExit(context.Background(), "test")
	if d.restores != 1 || d.changed {
		t.Fatalf("restores %d", d.restores)
	}
	if _, err := os.Stat(s.dnsMarker()); !os.IsNotExist(err) {
		t.Fatal("marker left after restore")
	}
}

func TestExitDNSRestoredWhenRouterStops(t *testing.T) {
	s, d, _ := connectWithDNS(t)
	waitDNS(t, s, "on")
	s.stopRouter()
	s.exitTick()
	if d.restores != 1 || d.changed {
		t.Fatalf("router stopped: restores %d", d.restores)
	}
}

func TestExitDNSCrashRecovery(t *testing.T) {
	s, d, _ := connectWithDNS(t)
	waitDNS(t, s, "on")
	// kill -9: nothing ran, the marker is still there. A new wallet on the
	// same data dir puts DNS back before anything else.
	s2 := NewServer(s.Cfg)
	s2.ex.dns = d
	s2.recoverDNS()
	if d.restores != 1 || d.changed {
		t.Fatalf("recovery restores %d", d.restores)
	}
	if _, err := os.Stat(s.dnsMarker()); !os.IsNotExist(err) {
		t.Fatal("marker left after recovery")
	}
	s2.recoverDNS() // nothing to do now; no helper call
	if d.restores != 1 {
		t.Fatal("helper called with no marker")
	}
}

func TestExitDNSFailureIsReported(t *testing.T) {
	s, f := exitTestServer(t)
	_ = f
	ctx := context.Background()
	s.Cfg.Transport = ModeAuto
	buyer, _ := s.buyerSesh(ctx, exitRoute{Source: SourceBundled})
	addLease(s, buyer, uint64(time.Now().Add(time.Hour).Unix()))
	s.ex.mu.Lock()
	s.ex.Leases[len(s.ex.Leases)-1].DNS = "10.195.0.53"
	s.ex.mu.Unlock()
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) {
		return uint64(time.Now().Add(time.Hour).Unix()), nil
	}
	s.ex.dns = &fakeDNS{setErr: errors.New("sudo needs a password")}
	s.ex.linkFor = func(string) (string, error) { return "swr1", nil }
	if _, err := s.connectExit(ctx, 4); err != nil {
		t.Fatal(err)
	}
	waitDNS(t, s, "failed: sudo needs a password")
	if _, err := os.Stat(s.dnsMarker()); !os.IsNotExist(err) {
		t.Fatal("marker left after a failed switch")
	}
	// exitDns "off": never touched
	s.disconnectExit(ctx, "t")
	s.Cfg.ExitDNS = "off"
	d := &fakeDNS{}
	s.ex.dns = d
	if _, err := s.connectExit(ctx, 4); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if len(d.set) != 0 {
		t.Fatal("exitDns off still switched DNS")
	}
}
