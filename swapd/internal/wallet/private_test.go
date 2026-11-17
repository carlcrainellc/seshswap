package wallet

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/srouter/srtest"
)

func TestMain(m *testing.M) {
	srtest.MaybeRun()
	os.Exit(m.Run())
}

const testSesh = "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwo.sesh"

func fakeMaker(t *testing.T) (port string, hosts *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Host)
		switch r.URL.Path {
		case "/v1/quote":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": 10000000000000000, "xmr": 1230000000000,
				"premium": 0, "picoPerUnit": 1, "d1": 2400, "d2": 3600, "grace": 21600,
				"minConfs": 10, "estReadySecs": 1500, "estSpendableSecs": 2700,
				"expires": 2000000000,
			})
		case "/v1/offer":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	return p, &seen
}

func restoreTestWallet(t *testing.T, h http.Handler) {
	t.Helper()
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	if code, r := do(t, h, "POST", "/v1/wallet/restore", string(body), ""); code != 200 {
		t.Fatalf("restore %d %v", code, r)
	}
}

func TestPrivateQuoteStartsRouterAndStopsWhenIdle(t *testing.T) {
	port, hosts := fakeMaker(t)
	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.MakerURL = "http://127.0.0.1:1" // direct fallback, unused here
	s.Cfg.MakerSeshURL = "http://" + testSesh + ":" + port
	o := srtest.Options("ok")
	o.BaseDir = t.TempDir()
	o.Log = t.Logf
	s.router = srouter.New(o)
	noExisting(t, s)
	s.idleStop = 200 * time.Millisecond
	restoreTestWallet(t, h)

	_, st := do(t, h, "GET", "/v1/status", "", "")
	conn := st["connection"].(map[string]any)
	if conn["mode"] != "auto" || conn["state"] != "off" {
		t.Fatalf("before quote %v", conn)
	}
	if s.router.Running() {
		t.Fatal("router started before it was needed")
	}

	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 200 || q["xmr"] != "1.23" {
		t.Fatalf("quote %d %v", code, q)
	}
	if len(*hosts) == 0 || !strings.HasPrefix((*hosts)[0], testSesh) {
		t.Fatalf("maker saw Host %v, want the .sesh name", *hosts)
	}
	_, st = do(t, h, "GET", "/v1/status", "", "")
	conn = st["connection"].(map[string]any)
	if conn["state"] != "connected" || conn["label"] != "Connected privately (bundled)" || conn["source"] != "bundled" {
		t.Fatalf("after quote %v", conn)
	}
	pid := s.router.Status().PID

	// No swap open: idle timeout closes it.
	time.Sleep(300 * time.Millisecond)
	s.reconcile()
	if s.router.Running() {
		t.Fatal("router still running with no swap and idle")
	}
	if srtest.Alive(pid) {
		t.Fatalf("router pid %d still alive", pid)
	}
	_, st = do(t, h, "GET", "/v1/status", "", "")
	if c := st["connection"].(map[string]any); c["state"] != "off" {
		t.Fatalf("after idle %v", c)
	}
	// It comes back on demand, with a fresh launch.
	if code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", ""); code != 200 {
		t.Fatalf("second quote %d %v", code, q)
	}
	if n := s.router.Status().Launches; n != 2 {
		t.Fatalf("launches %d", n)
	}
	// Locking the wallet closes it.
	s.LockWallet()
	deadline := time.Now().Add(6 * time.Second)
	for s.router.Running() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if s.router.Running() {
		t.Fatal("router running after lock")
	}
}

func TestDirectModeNeverStartsRouter(t *testing.T) {
	port, hosts := fakeMaker(t)
	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.MakerURL = "http://127.0.0.1:" + port
	s.Cfg.MakerSeshURL = "http://" + testSesh + ":" + port
	s.Cfg.Transport = ModeDirect
	o := srtest.Options("ok")
	o.BaseDir = t.TempDir()
	s.router = srouter.New(o)
	restoreTestWallet(t, h)
	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 200 || q["makerUrl"] != s.Cfg.MakerURL {
		t.Fatalf("quote %d %v", code, q)
	}
	if s.router.Status().Launches != 0 {
		t.Fatal("direct mode launched the router")
	}
	if !strings.HasPrefix((*hosts)[0], "127.0.0.1:") {
		t.Fatalf("host %v", *hosts)
	}
	_, st := do(t, h, "GET", "/v1/status", "", "")
	if c := st["connection"].(map[string]any); c["mode"] != "direct" || !strings.Contains(c["label"].(string), "can see your IP") {
		t.Fatalf("status %v", c)
	}
}

func TestPrivateErrorIsPlain(t *testing.T) {
	s, h := newTestServer(t)
	s.Cfg.Demo = false
	s.Cfg.MakerSeshURL = "http://" + testSesh
	s.router = srouter.New(srouter.Options{Binary: filepath.Join(t.TempDir(), "missing"), BaseDir: t.TempDir()})
	noExisting(t, s)
	restoreTestWallet(t, h)
	code, q := do(t, h, "GET", "/v1/quote?eth=0.01", "", "")
	if code != 502 {
		t.Fatalf("code %d %v", code, q)
	}
	msg, _ := q["error"].(string)
	if !strings.Contains(msg, "Session Router isn't installed") || strings.Contains(msg, "dial tcp") {
		t.Fatalf("error not plain: %q", msg)
	}
	_, st := do(t, h, "GET", "/v1/status", "", "")
	c := st["connection"].(map[string]any)
	if c["state"] != "error" || c["label"] != "Private connection failed" || c["detail"] == "" {
		t.Fatalf("status %v", c)
	}
}

func TestSettingsSwitchTransport(t *testing.T) {
	s, h := newTestServer(t)
	restoreTestWallet(t, h)
	if s.mode() != ModeDirect {
		t.Fatal("no .sesh maker should mean direct")
	}
	if code, r := do(t, h, "PUT", "/v1/settings", `{"transport":"private"}`, ""); code != 400 {
		t.Fatalf("private without .sesh accepted: %d %v", code, r)
	}
	if code, r := do(t, h, "PUT", "/v1/settings", `{"makerSeshUrl":"http://example.com"}`, ""); code != 400 {
		t.Fatalf("non-.sesh accepted: %d %v", code, r)
	}
	code, r := do(t, h, "PUT", "/v1/settings", `{"makerSeshUrl":"http://`+testSesh+`","transport":"private","makerUrl":"http://127.0.0.1:19000"}`, "")
	if code != 200 || r["transport"] != "auto" || r["makerUrl"] != "http://"+testSesh {
		t.Fatalf("switch (old name \"private\" = auto) %d %v", code, r)
	}
	for _, m := range []string{"bundled", "existing", "auto"} {
		code, r = do(t, h, "PUT", "/v1/settings", `{"transport":"`+m+`"}`, "")
		if code != 200 || r["transport"] != m || r["makerUrl"] != "http://"+testSesh {
			t.Fatalf("switch to %s: %d %v", m, code, r)
		}
	}
	if code, r = do(t, h, "PUT", "/v1/settings", `{"transport":"tor"}`, ""); code != 400 {
		t.Fatalf("unknown transport accepted: %d %v", code, r)
	}
	code, r = do(t, h, "PUT", "/v1/settings", `{"transport":"direct"}`, "")
	if code != 200 || r["makerUrl"] != "http://127.0.0.1:19000" {
		t.Fatalf("back to direct %d %v", code, r)
	}
}
