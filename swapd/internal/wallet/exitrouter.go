package wallet

// The exits panel's router card: what the router the wallet uses for exits
// says right now (mapped or not, this computer's exit routes, its own .sesh),
// Unmap for any mapped exit (also ones mapped outside the wallet, e.g. with
// rusty-ctl.exe), and Map by address (paid time is used if there is some).

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ExistingRouterLabel is what the page calls your own Session Router. The
// Windows build sets it to "Rusty's Session Router service".
var ExistingRouterLabel = "your own Session Router"

// seshAddr is a Session Router address: 52 z-base-32 letters, then .sesh.
var seshAddr = regexp.MustCompile(`^[ybndrfg8ejkmcpqxot1uwisza345h769]{52}\.sesh$`)

// routerState is GET /v1/exits/router.
type routerState struct {
	Source       string          `json:"source,omitempty"` // bundled / existing
	Label        string          `json:"label"`
	RPC          string          `json:"rpc,omitempty"`
	Reachable    bool            `json:"reachable"`
	Error        string          `json:"error,omitempty"`
	Mapped       []routerExitRow `json:"mapped"`
	RoutesKnown  bool            `json:"routesKnown"`
	RoutesUp     bool            `json:"routesUp"`
	RoutesReason string          `json:"routesReason,omitempty"`
	Self         string          `json:"self,omitempty"` // the router's own .sesh (exit time is bought for it)
}

// routerExitRow is one exit the router has mapped, by anyone.
type routerExitRow struct {
	Address     string   `json:"address"`
	SessionLive bool     `json:"sessionLive"`
	ExitCapable bool     `json:"exitCapable"`
	Ranges      []string `json:"ranges,omitempty"`
	ListingID   uint64   `json:"listingId,omitempty"` // a listing this wallet bought time on for it
	Wallet      bool     `json:"wallet"`              // the wallet's own connection
}

// The bundled router is "the bundled Session Router" on the page and in
// messages; see routerLabel at the end of this file.

// currentRouter is the router the panel shows: your own (Rusty's service on
// Windows) when the wallet would use it for exits, otherwise the bundled one.
// It doesn't probe or start anything: the card only reads, and Map picks the
// router with pickExitRouter like Connect does (which may fall back to the
// bundled one in Auto).
func (s *Server) currentRouter() (exitRoute, string, error) {
	mode := s.curMode()
	s.mu.Lock()
	ext := s.Cfg.ExitRouterRPC
	s.mu.Unlock()
	// Same order as pickExitRouter, without its probe of your router's RPC,
	// so an unreachable router shows as such instead of the bundled one.
	switch {
	case mode == ModeDirect:
		return exitRoute{}, "", errors.New(msgExitsDirect)
	case ext != "" && (mode == ModeExisting || mode == ModeAuto):
		return exitRoute{Source: SourceExisting, RPC: ext}, routerLabel(SourceExisting), nil
	case s.router == nil || !s.router.Running() || s.router.RPC() == "":
		return exitRoute{Source: SourceBundled}, routerLabel(SourceBundled), errors.New("the bundled Session Router isn't running, so nothing is mapped")
	}
	return exitRoute{Source: SourceBundled, RPC: s.router.RPC()}, routerLabel(SourceBundled), nil
}

// readRouterState asks the current router what it has mapped and matches the
// rows with the wallet's connection and its paid leases.
func (s *Server) readRouterState(ctx context.Context) routerState {
	rt, label, err := s.currentRouter()
	st := routerState{Source: rt.Source, Label: label, RPC: rt.RPC, Mapped: []routerExitRow{}}
	if err != nil {
		st.Error = err.Error()
		return st
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	l, err := s.exitRPC(rt.RPC).ListExits(cctx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Reachable = true
	st.RoutesKnown, st.RoutesUp, st.RoutesReason = l.Routes.Known, l.Routes.Up, l.Routes.Reason
	s.ex.mu.Lock()
	s.exitsLoadLocked()
	conn := s.ex.Conn
	leases := append([]exitLeaseRec(nil), s.ex.Leases...)
	s.ex.mu.Unlock()
	for _, e := range l.Exits {
		row := routerExitRow{Address: e.Address, SessionLive: e.SessionLive, ExitCapable: e.ExitCapable, Ranges: e.Ranges}
		if conn != nil && conn.ExitSesh == e.Address {
			row.Wallet, row.ListingID = true, conn.ListingID
		}
		for _, x := range leases {
			if row.ListingID == 0 && x.ExitSesh == e.Address {
				row.ListingID = x.ListingID
			}
		}
		st.Mapped = append(st.Mapped, row)
	}
	if rt.Source == SourceExisting {
		if a, err := s.buyerSesh(ctx, rt); err == nil {
			st.Self = a
		}
	} else if conn != nil && conn.Source == SourceBundled {
		st.Self = conn.BuyerSesh // the exit key; a fresh address otherwise
	}
	return st
}

// exitsRouter is GET /v1/exits/router.
func (s *Server) exitsRouter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "GET")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	writeJSON(w, 200, s.readRouterState(ctx))
}

// exitsUnmap unmaps one exit by address: the wallet's own connection (as the
// Unmap button on it does) or one mapped outside the wallet.
func (s *Server) exitsUnmap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST")
		return
	}
	var req struct {
		Address string `json:"address"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Address) == "" {
		writeErr(w, 400, "say which exit to unmap")
		return
	}
	addr := strings.ToLower(strings.TrimSpace(req.Address))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// The wallet's own connection goes through disconnectExit so DNS and the
	// lease state go back too; anything else is only unmapped on the router.
	mapped := func(rpc string) bool { // is addr still mapped on that router?
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		l, err := s.exitRPC(rpc).ListExits(cctx)
		if err != nil {
			return false
		}
		for _, e := range l.Exits {
			if e.Address == addr {
				return true
			}
		}
		return false
	}
	s.ex.mu.Lock()
	c := s.ex.Conn
	s.ex.mu.Unlock()
	if c != nil && c.ExitSesh == addr {
		s.disconnectExit(ctx, "you pressed Unmap")
	} else {
		rt, label, err := s.currentRouter()
		if err != nil {
			writeErr(w, 409, err.Error())
			return
		}
		if err := s.exitRPC(rt.RPC).UnmapExit(ctx, addr); err != nil {
			writeErr(w, 502, "couldn't unmap: "+err.Error())
			return
		}
		// The router drops it a moment later; wait so the card shows it gone
		// (up to 5 s; Rusty's service usually takes about a second).
		for i := 0; i < 10 && ctx.Err() == nil && mapped(rt.RPC); i++ {
			time.Sleep(500 * time.Millisecond)
		}
		s.routesChanged()
		s.logf("unmapped %s (not the wallet's connection) on %s", addr, label)
		s.ex.mu.Lock()
		s.ex.Last = "Unmapped " + addr + " on " + label + "."
		s.ex.mu.Unlock()
	}
	out := s.exitsStatusMap()
	out["router"] = s.readRouterState(ctx)
	writeJSON(w, 200, out)
}

// exitsMap maps an exit by its .sesh address through the router the wallet
// uses for exits. Paid time for that address and this router is used (like
// Connect); without any, the exit is mapped with no time limit, which works
// on an open exit or one whose operator allowed this computer's .sesh.
func (s *Server) exitsMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST")
		return
	}
	var req struct {
		Address string `json:"address"` // the exit's .sesh
	}
	// Empty or not JSON: the address check below says what is wrong.
	_ = readJSON(r, &req)
	// Accept it as copied from anywhere: spaces around it, upper case.
	addr := strings.ToLower(strings.TrimSpace(req.Address))
	addr = strings.TrimSuffix(addr, ".") // a trailing dot from a sentence or DNS
	if !seshAddr.MatchString(addr) {
		writeErr(w, 400, "that isn't a .sesh address (52 letters and digits, then .sesh)")
		return
	}
	s.mu.Lock()
	locked := s.vault == nil
	s.mu.Unlock()
	if locked {
		writeErr(w, 403, "unlock the wallet first")
		return
	}
	if !s.setBusy(true) {
		writeErr(w, 409, "another exit action is running")
		return
	}
	defer s.setBusy(false)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	rt, err := s.pickExitRouter(ctx)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	buyer, err := s.buyerSesh(ctx, rt)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	now := uint64(time.Now().Unix())
	var paid uint64
	s.ex.mu.Lock()
	s.exitsLoadLocked()
	for _, l := range s.ex.Leases {
		if l.ExitSesh == addr && l.BuyerSesh == buyer && l.End > now {
			paid = l.ListingID
		}
	}
	s.ex.mu.Unlock()
	var conn *exitConn
	if paid != 0 {
		conn, err = s.connectExit(ctx, paid)
	} else {
		conn, err = s.mapOn(ctx, rt, &exitConn{ExitSesh: addr, BuyerSesh: buyer, Source: rt.Source})
	}
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"connected": conn, "paid": paid != 0})
}

// short shortens a .sesh address for messages, like the page does.
func short(a string) string {
	if len(a) > 16 {
		return a[:8] + "…" + a[len(a)-6:]
	}
	return a
}

// routerLabel is how the page names the router for a source.
func routerLabel(source string) string {
	if source == SourceExisting {
		return ExistingRouterLabel
	}
	return "the bundled Session Router"
}
