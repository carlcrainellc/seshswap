package wallet

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

//go:embed web/satchel-test.html
var satchelTestHTML []byte

// Proposal is a trade a Satchel-style page asked for. It does not lock funds
// until the user approves it in this wallet.
type Proposal struct {
	ID      string `json:"id"`
	Asset   string `json:"asset"` // "ETH" or "USDC"
	Amount  string `json:"amount"`
	Value   string `json:"value"` // base units
	MinXMR  uint64 `json:"minXmr"`
	Origin  string `json:"origin"`
	Created int64  `json:"created"`
}

// isWalletPage is true when the request comes from our own UI (or from a
// non-browser tool with no Origin). A page on another port — including the
// Satchel test page — is not the wallet.
func (s *Server) isWalletPage(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	return s.walletOrigin(o)
}

func (s *Server) walletOrigin(o string) bool {
	u := strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://")
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		return false
	}
	lh, lp, err := net.SplitHostPort(s.Cfg.Listen)
	if err != nil {
		return false
	}
	if lp == "0" {
		return false // test listener; browser Origin will not be :0
	}
	okHost := (host == "127.0.0.1" || host == "localhost" || host == "::1") &&
		(lh == "127.0.0.1" || lh == "localhost" || lh == "::1" || lh == "")
	return okHost && port == lp
}

func (s *Server) satchelPropose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	if s.vault == nil {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	var req struct {
		Eth    string `json:"eth"`
		USDC   string `json:"usdc"`
		Value  string `json:"value"`
		Asset  string `json:"asset"`
		MinXMR uint64 `json:"minXmr"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	asset := "ETH"
	if req.USDC != "" || strings.EqualFold(req.Asset, "usdc") {
		asset = "USDC"
	}
	var (
		val *big.Int
		err error
		amt string
	)
	switch asset {
	case "USDC":
		amt = req.USDC
		if req.Value != "" {
			val, err = parseValue(req.Value, "")
		} else {
			val, err = parseUnits(amt, 6)
		}
		if amt == "" && val != nil {
			amt = formatUnits(val, 6)
		}
	default:
		amt = req.Eth
		val, err = parseValue(req.Value, req.Eth)
		if amt == "" && val != nil {
			amt = weiToETH(val)
		}
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		writeErr(w, 500, "could not create a proposal")
		return
	}
	p := &Proposal{
		ID:      hex.EncodeToString(b),
		Asset:   asset,
		Amount:  amt,
		Value:   val.String(),
		MinXMR:  req.MinXMR,
		Origin:  r.Header.Get("Origin"),
		Created: time.Now().Unix(),
	}
	s.mu.Lock()
	s.proposal = p
	s.mu.Unlock()
	s.logf("satchel proposal %s: %s %s (from %s) waiting for approval in the wallet", p.ID, p.Amount, p.Asset, p.Origin)
	writeJSON(w, 200, map[string]any{
		"id":      p.ID,
		"asset":   p.Asset,
		"amount":  p.Amount,
		"value":   p.Value,
		"minXmr":  p.MinXMR,
		"status":  "waiting for approval in the wallet",
		"message": "The trade will only start after you approve it in the wallet. This page cannot skip that step.",
	})
}

func (s *Server) satchelPending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "GET only")
		return
	}
	s.mu.Lock()
	p := s.proposal
	s.mu.Unlock()
	if p == nil {
		writeJSON(w, 200, map[string]any{"pending": nil})
		return
	}
	writeJSON(w, 200, map[string]any{"pending": p})
}

func (s *Server) satchelReject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	if !s.isWalletPage(r) {
		writeErr(w, 403, "reject the proposal in the wallet")
		return
	}
	s.mu.Lock()
	s.proposal = nil
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) satchelApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	if !s.isWalletPage(r) {
		writeErr(w, 403, "approve the proposal in the wallet")
		return
	}
	if s.vault == nil || s.eng == nil {
		if s.vault == nil {
			writeErr(w, 401, "unlock the wallet first")
			return
		}
		writeErr(w, 503, "the swap engine is not running (Ethereum or Monero is not connected)")
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	_ = readJSON(r, &req)
	s.mu.Lock()
	p := s.proposal
	s.mu.Unlock()
	if p == nil || (req.ID != "" && req.ID != p.ID) {
		writeErr(w, 404, "no matching proposal")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	makerURL := s.curMakerURL()
	if makerURL == "" {
		writeErr(w, 400, "no maker is configured")
		return
	}
	amount, err := parseValue(p.Value, "")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	pol := s.eng.Policy
	rec, err := api.Take(ctx, s.eng, api.TakeParams{
		MakerURL: makerURL, Value: amount, MinXMR: p.MinXMR,
		MinD1: 2*pol.Margin + 1800, MinD2: 2 * pol.Margin,
		MaxConfs: 60, MinConfs: swap.XmrConfFloor, HTTP: s.http,
	})
	if err != nil {
		writeErr(w, 422, friendlyNetErr(err))
		return
	}
	s.mu.Lock()
	s.proposal = nil
	s.mu.Unlock()
	go s.eng.StepAll(context.Background())
	writeJSON(w, 200, PublicView(*rec, time.Now()))
}

func (s *Server) requireWalletPage(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodOptions && !s.isWalletPage(r) {
			writeErr(w, 403, "this step has to happen in the wallet")
			return
		}
		next(w, r)
	}
}

func satchelTestListenOK(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("satchel test listen: %w", err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("satchel test page listens on 127.0.0.1 only, not %q", host)
	}
	return nil
}

func satchelTestHandler(walletListen string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src http://127.0.0.1:* http://localhost:* http://[::1]:*")
		_, _ = w.Write(satchelTestHTML)
	})
}
