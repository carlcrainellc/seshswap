package wallet

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/srouter/srtest"
)

// noExisting makes the "already running router" search hermetic: no
// resolv.conf nameservers, no 127.0.0.1:53.
func noExisting(t *testing.T, s *Server) {
	t.Helper()
	s.ext = &existingRouter{resolvConf: filepath.Join(t.TempDir(), "none"), extra: []string{}}
}

// withExisting points the search at a fake router DNS that this test runs
// itself (standing in for a system service or a user-run router).
func withExisting(t *testing.T, s *Server) (dns string, stop func()) {
	t.Helper()
	dns, stop, err := srtest.StartDNS()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	s.ext = &existingRouter{resolvConf: filepath.Join(t.TempDir(), "none"), extra: []string{dns}}
	return dns, stop
}

func bundledFake(t *testing.T, s *Server) {
	t.Helper()
	o := srtest.Options("ok")
	o.BaseDir = t.TempDir()
	o.Log = t.Logf
	s.router = srouter.New(o)
}

func dnsAnswers(dns string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := (&existingRouter{}).probe(ctx, dns, testSesh, "1")
	return err == nil || !strings.Contains(err.Error(), "doesn't resolve")
}

func TestAutoUsesExistingRouterAndNeverStopsIt(t *testing.T) {
	port, hosts := fakeMaker(t)
	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.MakerSeshURL = "http://" + testSesh + ":" + port
	bundledFake(t, s)
	dns, _ := withExisting(t, s)
	s.idleStop = 100 * time.Millisecond
	restoreTestWallet(t, h)

	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 200 || q["xmr"] != "1.23" {
		t.Fatalf("quote %d %v", code, q)
	}
	if len(*hosts) == 0 || !strings.HasPrefix((*hosts)[0], testSesh) {
		t.Fatalf("maker saw Host %v", *hosts)
	}
	if n := s.router.Status().Launches; n != 0 {
		t.Fatalf("bundled router launched %d times although an existing one works", n)
	}
	_, st := do(t, h, "GET", "/v1/status", "", "")
	c := st["connection"].(map[string]any)
	if c["label"] != "Connected privately (using your Session Router)" || c["source"] != "existing" || c["dns"] != dns {
		t.Fatalf("status %v", c)
	}
	// Idle, lock: the wallet must leave the existing router alone.
	time.Sleep(150 * time.Millisecond)
	s.reconcile()
	s.LockWallet()
	time.Sleep(100 * time.Millisecond)
	if !dnsAnswers(dns) {
		t.Fatal("existing router stopped answering after idle/lock")
	}
}

func TestAutoFallsBackToBundledWhenExistingCantReachMaker(t *testing.T) {
	port, _ := fakeMaker(t)
	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.MakerSeshURL = "http://" + testSesh + ":" + port
	bundledFake(t, s)
	withExisting(t, s)
	// The existing router resolves the name but its path to the maker is broken.
	s.ext.dial = func(context.Context, string) error { return errors.New("no path") }
	restoreTestWallet(t, h)

	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 200 {
		t.Fatalf("quote %d %v", code, q)
	}
	if n := s.router.Status().Launches; n != 1 {
		t.Fatalf("launches %d, want the bundled fallback", n)
	}
	_, st := do(t, h, "GET", "/v1/status", "", "")
	if c := st["connection"].(map[string]any); c["label"] != "Connected privately (bundled)" {
		t.Fatalf("status %v", c)
	}
	// A second quote soon after does not re-probe slowly or relaunch.
	if code, _ := do(t, h, "GET", "/v1/quote?eth=0.01", "", ""); code != 200 || s.router.Status().Launches != 1 {
		t.Fatalf("second quote %d launches %d", code, s.router.Status().Launches)
	}
}

func TestExistingOnlyNeverStartsBundled(t *testing.T) {
	port, _ := fakeMaker(t)
	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.MakerSeshURL = "http://" + testSesh + ":" + port
	s.Cfg.Transport = ModeExisting
	bundledFake(t, s)
	noExisting(t, s)
	restoreTestWallet(t, h)

	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 502 || !strings.Contains(q["error"].(string), "Couldn't find a working Session Router") {
		t.Fatalf("quote %d %v", code, q)
	}
	if s.router.Status().Launches != 0 {
		t.Fatal("existing-only mode launched the bundled router")
	}
	_, st := do(t, h, "GET", "/v1/status", "", "")
	if c := st["connection"].(map[string]any); c["state"] != "error" || c["mode"] != "existing" {
		t.Fatalf("status %v", c)
	}
	// Once the user starts their router, the same mode works.
	withExisting(t, s)
	if code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", ""); code != 200 {
		t.Fatalf("after starting a router: %d %v", code, q)
	}
	if s.router.Status().Launches != 0 {
		t.Fatal("bundled launched")
	}
}

func TestBundledOnlyIgnoresExisting(t *testing.T) {
	port, _ := fakeMaker(t)
	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.MakerSeshURL = "http://" + testSesh + ":" + port
	s.Cfg.Transport = ModeBundled
	bundledFake(t, s)
	withExisting(t, s)
	restoreTestWallet(t, h)
	if code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", ""); code != 200 {
		t.Fatalf("quote %d %v", code, q)
	}
	if s.router.Status().Launches != 1 {
		t.Fatal("bundled-only mode didn't use the bundled router")
	}
	if st := s.ext.status(); st.DNS != "" || st.Connected {
		t.Fatalf("bundled-only mode touched the existing router: %+v", st)
	}
}

func TestConfiguredDNSTriedFirst(t *testing.T) {
	dns, stop, err := srtest.StartDNS()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	e := &existingRouter{configured: dns, resolvConf: filepath.Join(t.TempDir(), "none"), extra: []string{"127.0.0.1:1"},
		dial: func(context.Context, string) error { return nil }}
	if c := e.candidates(); len(c) != 2 || c[0].dns != dns || c[0].kind != "configured" {
		t.Fatalf("candidates %v", c)
	}
	if _, err := e.lookup(context.Background(), testSesh, "80", true); err != nil {
		t.Fatal(err)
	}
	if st := e.status(); st.DNS != dns || st.Kind != "configured" {
		t.Fatalf("status %+v", st)
	}
}

func TestOnlyLocalNameserversAreTried(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	_ = os.WriteFile(p, []byte("# test\nnameserver 10.0.0.2\nnameserver 127.0.0.53\nnameserver 8.8.8.8\nnameserver ::1\noptions edns0\n"), 0o600)
	got := loopbackNameservers(p)
	want := []string{"127.0.0.53:53", net.JoinHostPort("::1", "53")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v (an off-machine resolver must never see a .sesh name)", got, want)
	}
}
