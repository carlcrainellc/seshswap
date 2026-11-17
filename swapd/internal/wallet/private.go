package wallet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
)

// Connection modes (Settings "transport").
const (
	// ModeAuto uses a Session Router that is already running on this
	// computer (system service or one the user started) if it can reach the
	// maker, and otherwise starts the bundled one only while needed.
	ModeAuto = "auto"
	// ModeBundled always uses the bundled router, started only while needed.
	ModeBundled = "bundled"
	// ModeExisting only uses a router that is already running; it never
	// starts the bundled one.
	ModeExisting = "existing"
	ModeDirect   = "direct" // plain HTTP to makerUrl (fallback)
	// ModePrivate is the old name for the private connection (wallet-sr
	// 7dadbab); it now means ModeAuto.
	ModePrivate = "private"
)

// Where the private connection currently comes from.
const (
	SourceNone     = ""
	SourceExisting = "existing" // a router the wallet did not start; never stopped or reconfigured
	SourceBundled  = "bundled"  // the wallet's own child router
)

// normMode maps a config/settings value to a mode, "" if unknown.
func normMode(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case ModeAuto, ModePrivate:
		return ModeAuto
	case ModeBundled:
		return ModeBundled
	case ModeExisting:
		return ModeExisting
	case ModeDirect:
		return ModeDirect
	}
	return ""
}

func isPrivate(mode string) bool { return mode != ModeDirect }

// RouterConfig is the optional "sessionRouter" block in the wallet config.
type RouterConfig struct {
	// Binary is the bundled session-router. Empty = next to the wallet
	// executable: <exe dir>/session-router/bin/session-router (where
	// scripts/fetch-session-router.sh puts it for swapd/bin/wallet).
	Binary string `json:"binary,omitempty"`
	// IdleStop is how long the router stays up after the last maker request
	// when no swap is open (quote screen, no swap). Default 2m.
	IdleStop string `json:"idleStop,omitempty"`
	// Bootstrap is an optional add-node file; empty = built-in seeds.
	Bootstrap string `json:"bootstrap,omitempty"`
	// TempDir is where per-launch data dirs go; empty = the OS temp dir.
	TempDir string `json:"tempDir,omitempty"`
}

func defaultRouterBinary() string {
	name := "session-router"
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		p := filepath.Join(filepath.Dir(exe), "session-router", "bin", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		return p
	}
	return filepath.Join("bin", "session-router", "bin", name)
}

func (s *Server) initPrivate() {
	rc := RouterConfig{}
	if s.Cfg.SessionRouter != nil {
		rc = *s.Cfg.SessionRouter
	}
	if rc.Binary == "" {
		rc.Binary = defaultRouterBinary()
	}
	s.idleStop = 2 * time.Minute
	if d, err := time.ParseDuration(rc.IdleStop); err == nil && d > 0 {
		s.idleStop = d
	}
	if s.router == nil {
		var guard []string
		if exe, err := os.Executable(); err == nil {
			// cmd/wallet answers srouter.GuardArg by running srouter.RunGuard.
			guard = []string{exe, srouter.GuardArg}
		}
		s.router = srouter.New(srouter.Options{
			Binary: rc.Binary, Bootstrap: rc.Bootstrap, BaseDir: rc.TempDir, Guard: guard,
			// RPC on a private socket + auto-routing (inert until an exit is mapped).
			Exits: s.exitsEnabled(),
			Log:   func(f string, a ...any) { s.logf(f, a...) },
		})
	}
	if s.ext == nil {
		s.ext = newExistingRouter(s.Cfg.SeshDNS)
	}
	t := sesh.New(sesh.Config{Lookup: s.seshLookup})
	c := t.Client(60 * time.Second)
	c.Transport = &markRT{base: c.Transport, s: s}
	s.http = c
}

// mode returns the effective connection mode.
func (s *Server) mode() string {
	if m := normMode(s.Cfg.Transport); m != "" {
		return m
	}
	if s.Cfg.MakerSeshURL != "" {
		return ModeAuto
	}
	// Registry makers are .sesh-only. A wallet set up with only a direct
	// maker address stays direct until the user picks a private mode.
	if !s.Cfg.Registry.IsZero() && s.Cfg.MakerURL == "" {
		return ModeAuto
	}
	return ModeDirect
}

// makerURL is the maker address for the current mode.
func (s *Server) makerURL() string {
	if isPrivate(s.mode()) && s.Cfg.MakerSeshURL != "" {
		return s.Cfg.MakerSeshURL
	}
	return s.Cfg.MakerURL
}

func (s *Server) curMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode()
}

func (s *Server) touch() {
	s.useMu.Lock()
	s.lastUse = time.Now()
	s.useMu.Unlock()
}

func (s *Server) idleFor() time.Duration {
	s.useMu.Lock()
	defer s.useMu.Unlock()
	if s.lastUse.IsZero() {
		return time.Hour
	}
	return time.Since(s.lastUse)
}

// seshLookup turns a .sesh host into a dial address.
//
//   - direct: a .sesh maker still works through seshDns (old behaviour).
//   - existing: only a router already running on this computer.
//   - auto: an already-running router if it reaches the maker, else the
//     bundled one (started now, stopped when no longer needed).
//   - bundled: only the bundled one.
//
// A router the wallet did not start is only ever used through its DNS; the
// wallet never stops, restarts or reconfigures it.
func (s *Server) seshLookup(ctx context.Context, host, port string) (string, error) {
	s.touch()
	mode := s.curMode()
	switch mode {
	case ModeDirect:
		dns := s.Cfg.SeshDNS
		if dns == "" {
			dns = sesh.DefaultDNS
		}
		return sesh.ResolveVia(ctx, dns, host, port)
	case ModeExisting:
		addr, err := s.ext.lookup(ctx, host, port, true)
		if err != nil {
			return "", &srouter.UserError{Msg: msgNoExisting, Err: err}
		}
		s.setSource(SourceExisting)
		return addr, nil
	case ModeAuto:
		// The bundled router already up (we started it for this swap): keep it.
		if !s.router.Running() {
			if addr, err := s.ext.lookup(ctx, host, port, false); err == nil {
				s.setSource(SourceExisting)
				return addr, nil
			}
		}
	}
	s.setSource(SourceBundled)
	return s.bundledLookup(ctx, host, port)
}

func (s *Server) bundledLookup(ctx context.Context, host, port string) (string, error) {
	dns, err := s.router.Ensure(ctx)
	if err != nil {
		var ue *srouter.UserError
		if errors.As(err, &ue) {
			return "", err
		}
		if ctx.Err() != nil {
			s.router.MarkFailed(msgPrivateTimeout)
			return "", &srouter.UserError{Msg: msgPrivateTimeout, Err: err}
		}
		return "", &srouter.UserError{Msg: "Couldn't start the private connection: " + err.Error(), Err: err}
	}
	// Paths take a moment after a cold start: retry the name until the
	// request's own deadline.
	var last error
	for {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addr, err := sesh.ResolveVia(lctx, dns, host, port)
		cancel()
		if err == nil {
			return addr, nil
		}
		last = err
		select {
		case <-ctx.Done():
			s.router.MarkFailed(msgPrivateTimeout)
			return "", &srouter.UserError{Msg: msgPrivateTimeout, Err: last}
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (s *Server) setSource(src string) {
	s.useMu.Lock()
	s.source = src
	s.useMu.Unlock()
}

func (s *Server) curSource() string {
	s.useMu.Lock()
	defer s.useMu.Unlock()
	return s.source
}

const msgNoExisting = "Couldn't find a working Session Router on this computer. Start yours, or choose Auto or Bundled in Settings."

const msgPrivateTimeout = "Couldn't reach the maker privately in time. The maker may be offline, or your internet is slow. Try again, or switch to Direct connection in Settings."

// markRT notes maker traffic and marks the private connection as connected
// after the first successful reply from a .sesh host.
type markRT struct {
	base http.RoundTripper
	s    *Server
}

func (m *markRT) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Hostname()
	isSesh := sesh.IsSesh(host)
	if isSesh {
		m.s.touch()
	}
	resp, err := m.base.RoundTrip(r)
	if isSesh && isPrivate(m.s.curMode()) {
		switch m.s.curSource() {
		case SourceBundled:
			if err == nil {
				m.s.router.MarkConnected()
			}
		case SourceExisting:
			m.s.ext.markResult(err)
		}
		if err == nil {
			m.s.touch()
		}
	}
	return resp, err
}

// CloseIdleConnections drops kept-alive maker connections (they run over the
// router's adapter, which is gone once the router stops).
func (m *markRT) CloseIdleConnections() {
	if c, ok := m.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// stopRouter stops the bundled router and forgets connections that used it.
func (s *Server) stopRouter() {
	if s.router == nil {
		return
	}
	s.router.Stop()
	if s.http != nil {
		s.http.CloseIdleConnections()
	}
}

// connStatus is what the UI shows as the connection line.
func (s *Server) connStatus() map[string]any {
	mode := s.mode()
	out := map[string]any{"mode": mode}
	if mode == ModeDirect {
		out["state"] = "direct"
		out["label"] = "Direct connection (the maker can see your IP address)"
		return out
	}
	if c := s.exitConnSnapshot(); c != nil {
		out["source"] = c["source"]
		out["exit"] = c["listingId"]
		router := "bundled"
		if c["source"] == SourceExisting {
			router = "your Session Router"
		}
		switch c["phase"] {
		case phaseUp:
			out["state"] = string(srouter.Connected)
			out["label"] = fmt.Sprintf("Connected through exit #%v (%s)", c["listingId"], router)
			out["detail"] = "Stays up until you press Disconnect. Locking or closing the wallet disconnects it first."
		case phaseNoRoutes:
			out["state"] = string(srouter.Failed)
			out["label"] = fmt.Sprintf("Exit #%v mapped, routes not up", c["listingId"])
			out["detail"] = fmt.Sprint(c["routesReason"])
		default:
			out["state"] = string(srouter.Connecting)
			out["label"] = fmt.Sprintf("Exit #%v: waiting for this computer's routes…", c["listingId"])
		}
		return out
	}
	src := s.curSource()
	if mode == ModeExisting || (src == SourceExisting && !s.router.Running()) {
		es := s.ext.status()
		out["source"] = SourceExisting
		if es.DNS != "" {
			out["dns"] = es.DNS
		}
		switch {
		case es.Connected:
			out["state"] = string(srouter.Connected)
			out["label"] = "Connected privately (using your Session Router)"
		case es.Err != "" && mode == ModeExisting:
			out["state"] = string(srouter.Failed)
			out["label"] = "Private connection failed"
			out["detail"] = msgNoExisting
		default:
			out["state"] = string(srouter.Off)
			out["label"] = "Private connection through your own Session Router (checked when you get a quote or swap)"
		}
		return out
	}
	st := s.router.Status()
	out["state"] = string(st.State)
	out["launches"] = st.Launches
	if st.State != srouter.Off {
		out["source"] = SourceBundled
	}
	if st.StartupMs > 0 {
		out["startupMs"] = st.StartupMs
	}
	switch st.State {
	case srouter.Off:
		if mode == ModeAuto {
			out["label"] = "Private connection (uses your Session Router if it's running, otherwise starts the bundled one when you get a quote or swap)"
		} else {
			out["label"] = "Private connection (starts when you get a quote or swap)"
		}
	case srouter.Connecting:
		out["label"] = "Connecting privately… (bundled)"
	case srouter.Connected:
		out["label"] = "Connected privately (bundled)"
	case srouter.Failed:
		out["label"] = "Private connection failed"
		out["detail"] = st.Message
	}
	return out
}

// reconcile stops the router when nothing needs it: wallet locked, direct
// mode, or no open swap and no maker request for idleStop.
func (s *Server) reconcile() {
	if s.router == nil || !s.router.Running() {
		return
	}
	s.mu.Lock()
	locked := s.vault == nil
	mode := s.mode()
	open := s.openCount()
	s.mu.Unlock()
	held := s.exitHoldsRouter()
	switch {
	case locked:
		if held {
			s.disconnectExit(context.Background(), "wallet locked")
		}
		s.logf("wallet locked; closing the private connection")
	case mode == ModeDirect || mode == ModeExisting:
		if held {
			s.disconnectExit(context.Background(), "bundled router not selected")
		}
		s.logf("bundled router not selected; closing it")
	case held:
		return // a mapped exit keeps the bundled router up until its time ends
	case open == 0 && s.idleFor() > s.idleStop:
		s.logf("no swap running; closing the private connection")
	default:
		return
	}
	s.stopRouter()
}

func (s *Server) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.disconnectExit(dctx, "wallet closing")
			cancel()
			s.stopRouter()
			return
		case <-t.C:
			s.exitTick()
			s.reconcile()
		}
	}
}

// startForResume brings the private connection up after unlock when a swap
// is still open (crash-resume), so the lock hint can still reach the maker
// and the user sees the connection state. Swap safety does not depend on it.
func (s *Server) startForResume() {
	if !isPrivate(s.mode()) || s.router == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.touch()
		u := s.makerURL()
		if u == "" {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u, "/")+"/v1/offer", nil)
		if err != nil {
			return
		}
		resp, err := s.http.Do(req)
		if err != nil {
			s.logf("private connection after resume: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

// friendlyNetErr unwraps a router UserError from an HTTP error chain.
func friendlyNetErr(err error) string {
	var ue *srouter.UserError
	if errors.As(err, &ue) {
		return ue.Msg
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "the maker didn't answer in time"
	}
	return err.Error()
}
