package wallet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func callJSON(t *testing.T, h http.HandlerFunc, method, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// Rusty-style setup: your own router, an exit mapped outside the wallet.
func TestExitRouterCardShowsAndUnmapsOutsideMapping(t *testing.T) {
	s, f := exitTestServer(t)
	s.Cfg.Transport = ModeExisting
	s.Cfg.ExitRouterRPC = "tcp://127.0.0.1:1190"
	f.mapped = map[string]bool{testExit: true}

	st := s.readRouterState(context.Background())
	if !st.Reachable || st.Source != SourceExisting || st.Label != ExistingRouterLabel || len(st.Mapped) != 1 {
		t.Fatalf("state %+v", st)
	}
	if m := st.Mapped[0]; m.Address != testExit || m.Wallet || !m.SessionLive || !st.RoutesKnown || !st.RoutesUp {
		t.Fatalf("row %+v state %+v", m, st)
	}
	if code, _ := callJSON(t, s.exitsUnmap, "POST", `{}`); code != 400 {
		t.Fatalf("unmap without an address: %d", code)
	}
	code, out := callJSON(t, s.exitsUnmap, "POST", `{"address":"`+strings.ToUpper(testExit)+`"}`)
	if code != 200 || f.seen("tcp://127.0.0.1:1190 llarp.unmap_exit") != 1 {
		t.Fatalf("unmap: %d %v calls %v", code, out, f.calls)
	}
	r, _ := out["router"].(map[string]any)
	if r == nil || len(r["mapped"].([]any)) != 0 || !strings.Contains(out["last"].(string), "Unmapped ") {
		t.Fatalf("after unmap: %v", out)
	}
}

func TestExitRouterCardBundledNotRunning(t *testing.T) {
	s, _ := exitTestServer(t)
	s.Cfg.Transport = ModeAuto
	st := s.readRouterState(context.Background())
	if st.Reachable || st.Source != SourceBundled || !strings.Contains(st.Error, "isn't running") || st.Mapped == nil {
		t.Fatalf("state %+v", st)
	}
	s.Cfg.Transport = ModeDirect
	if st := s.readRouterState(context.Background()); st.Error != msgExitsDirect {
		t.Fatalf("direct: %+v", st)
	}
}

// Map by address with no paid time: no time limit, nothing expires, and
// Unmap on the card ends the wallet's own connection.
func TestExitMapByAddressWithoutPaidTime(t *testing.T) {
	s, f := exitTestServer(t)
	s.Cfg.Transport = ModeAuto
	if code, _ := callJSON(t, s.exitsMap, "POST", `{"address":"nope.sesh"}`); code != 400 {
		t.Fatalf("bad address: %d", code)
	}
	code, out := callJSON(t, s.exitsMap, "POST", `{"address":" `+testExit+` "}`)
	if code != 200 || out["paid"] != false {
		t.Fatalf("map: %d %v", code, out)
	}
	c, _ := out["connected"].(map[string]any)
	if c == nil || c["exitSesh"] != testExit || c["end"].(float64) != 0 || f.seen("llarp.map_exit") != 1 {
		t.Fatalf("conn %v calls %v", c, f.calls)
	}
	st := s.exitsStatusMap()["connected"].(map[string]any)
	if st["paid"] != false || st["expiring"] != false || !strings.HasPrefix(st["name"].(string), "exit wo7xwuzo…") {
		t.Fatalf("status %v", st)
	}
	if lbl := s.connStatus()["label"].(string); !strings.HasPrefix(lbl, "Connected through exit wo7xwuzo") {
		t.Fatalf("label %q", lbl)
	}
	s.exitTick() // End 0 is not "paid time ended"
	if !s.exitHoldsRouter() || f.seen("llarp.unmap_exit") != 0 {
		t.Fatalf("an unpaid mapping was ended by the clock: calls %v", f.calls)
	}
	code, out = callJSON(t, s.exitsUnmap, "POST", `{"address":"`+testExit+`"}`)
	if code != 200 || out["connected"] != nil || f.seen("llarp.unmap_exit") != 1 || s.exitHoldsRouter() {
		t.Fatalf("unmap own: %d %v calls %v", code, out, f.calls)
	}
	if !strings.Contains(out["last"].(string), "you pressed Unmap") {
		t.Fatalf("last %v", out["last"])
	}
}

// Map by address uses paid time for that exit when there is some.
func TestExitMapByAddressUsesPaidLease(t *testing.T) {
	s, _ := exitTestServer(t)
	s.Cfg.Transport = ModeAuto
	buyer, err := s.buyerSesh(context.Background(), exitRoute{Source: SourceBundled})
	if err != nil {
		t.Fatal(err)
	}
	end := uint64(time.Now().Add(time.Hour).Unix())
	addLease(s, buyer, end)
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) { return end, nil }
	code, out := callJSON(t, s.exitsMap, "POST", `{"address":"`+testExit+`"}`)
	if code != 200 || out["paid"] != true {
		t.Fatalf("map: %d %v", code, out)
	}
	if c := out["connected"].(map[string]any); c["listingId"].(float64) != 4 || uint64(c["end"].(float64)) != end {
		t.Fatalf("conn %v", c)
	}
	if lbl := s.connStatus()["label"].(string); !strings.Contains(lbl, "exit #4") {
		t.Fatalf("label %q", lbl)
	}
}

func TestExitDNSByRouter(t *testing.T) {
	s := &Server{}
	for _, c := range []struct {
		v          string
		on, router bool
	}{{"", true, false}, {"auto", true, false}, {"off", false, false}, {"router", false, true}, {"Router", false, true}} {
		s.Cfg.ExitDNS = c.v
		if s.exitDNSOn() != c.on || s.exitDNSByRouter() != c.router {
			t.Fatalf("%q: on=%v router=%v", c.v, s.exitDNSOn(), s.exitDNSByRouter())
		}
	}
	if short(testExit) != "wo7xwuzo…y.sesh" || short("abc.sesh") != "abc.sesh" {
		t.Fatalf("short %q", short(testExit))
	}
}
