package wallet

// Exits: buy hours on a Session Router exit listed in the SeshRegistry
// (tag "exit"), pay through ExitLease, and send this computer's traffic
// through it with MapExit until the paid time ends.
//
// The router rules are the wallet-sr ones. Auto uses your own router when it
// can do exits (patched 1.1.1 with its RPC address in Settings), otherwise the
// bundled one. Bundled always uses the bundled one. Existing only uses yours.
// Direct has no router, so no exits. The wallet maps and unmaps exits on a
// router it did not start; it never stops or reconfigures that router.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/exits"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
)

const (
	msgExitsDirect   = "Exits need Session Router. Choose Auto or Bundled in Settings (Direct has no router)."
	msgExitsExisting = "Your Session Router can't use exits. It needs the exit build of 1.1.1 with its RPC address set in Settings, or choose Auto or Bundled."
)

// exitLeaseRec is one purchase this wallet made, kept in exits.json.
type exitLeaseRec struct {
	exits.Lease
	ExitSesh string `json:"exitSesh"`
	Country  string `json:"country,omitempty"`
	DNS      string `json:"dns,omitempty"`
	Source   string `json:"source"`        // bundled / existing: whose .sesh was bought for
	Via      string `json:"via,omitempty"` // "swap": paid by a swap claim (ExitPay), not from this wallet
}

// exitConn is the exit mapped right now.
type exitConn struct {
	ListingID uint64    `json:"listingId"`
	ExitSesh  string    `json:"exitSesh"`
	BuyerSesh string    `json:"buyerSesh"`
	Source    string    `json:"source"`
	RPC       string    `json:"-"`
	End       uint64    `json:"end"`
	Since     time.Time `json:"since"`
	DNS       string    `json:"dns,omitempty"`
	Asset     string    `json:"asset,omitempty"`    // "usdc" or "eth": what the time was paid in, for top-ups
	DNSState  string    `json:"dnsState,omitempty"` // system DNS: "", "switching", "on", "failed: …"
	// What the router says, checked every few seconds while mapped. Only
	// "up" (this computer's routes are installed) counts as connected, and
	// only then is system DNS switched.
	Phase        string `json:"phase"` // routing, up, noroutes
	SessionLive  bool   `json:"sessionLive"`
	ExitCapable  bool   `json:"exitCapable"`
	RoutesUp     bool   `json:"routesUp"`
	RoutesReason string `json:"routesReason,omitempty"`
	warned       bool
	dnsSet       bool
}

const (
	phaseRouting  = "routing"  // mapped, waiting for this computer's routes
	phaseUp       = "up"       // routes installed: connected
	phaseNoRoutes = "noroutes" // routes didn't come up, or dropped
	phaseTesting  = "testing"  // routes installed; checking that traffic gets through
)

// exitRoutesWait is how long Connect waits for the routes before it says so.
var exitRoutesWait = 30 * time.Second

type exitState struct {
	mu          sync.Mutex
	Leases      []exitLeaseRec `json:"leases"`
	Conn        *exitConn      `json:"conn,omitempty"`
	Last        string         `json:"last,omitempty"` // last event in plain English
	loaded      bool
	busy        bool
	rpcFor      func(endpoint string) *exits.RouterRPC                                        // tests
	activeUntil func(ctx context.Context, listingID uint64, buyerSesh string) (uint64, error) // tests
	buyFor      func(ctx context.Context, listingID uint64, asset string, hours uint32, buyer string) (*exits.Lease, string, error)
	priceFor    func(ctx context.Context, listingID uint64, asset string, hours uint32) (string, error)
	dns         dnsChanger                      // tests
	linkFor     func(ip string) (string, error) // tests
	topPrice    struct {
		key, text string
	}
}

// exitWarnBefore is how long before the paid time ends the wallet warns and
// offers a top-up (config exitWarnBefore, default 5 minutes).
func (s *Server) exitWarnBefore() time.Duration {
	if d, err := time.ParseDuration(s.Cfg.ExitWarnBefore); err == nil && d > 0 {
		return d
	}
	return 5 * time.Minute
}

func (s *Server) exitsEnabled() bool { return !s.Cfg.ExitLease.IsZero() && s.regEnabled() }

func (s *Server) exitsFile() string { return filepath.Join(s.Cfg.DataDir, "exits.json") }

// exitKeyFile is the bundled router's exit identity. It is only used while
// an exit is wanted, so swaps keep a fresh router address each launch.
func (s *Server) exitKeyFile() string { return filepath.Join(s.Cfg.DataDir, "exit-router.key") }

func (s *Server) exitsLoadLocked() {
	if s.ex.loaded {
		return
	}
	s.ex.loaded = true
	b, err := os.ReadFile(s.exitsFile())
	if err != nil {
		return
	}
	var st struct {
		Leases []exitLeaseRec `json:"leases"`
	}
	if json.Unmarshal(b, &st) == nil {
		s.ex.Leases = st.Leases
	}
}

func (s *Server) exitsSaveLocked() {
	b, _ := json.MarshalIndent(struct {
		Leases []exitLeaseRec `json:"leases"`
	}{s.ex.Leases}, "", "  ")
	_ = os.MkdirAll(s.Cfg.DataDir, 0o700)
	tmp := s.exitsFile() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.exitsFile())
	}
}

func (s *Server) exitRPC(endpoint string) *exits.RouterRPC {
	if s.ex.rpcFor != nil {
		return s.ex.rpcFor(endpoint)
	}
	return &exits.RouterRPC{Endpoint: endpoint}
}

func (s *Server) leaseContract() *exits.Contract {
	return &exits.Contract{C: s.regClient(), Addr: s.Cfg.ExitLease}
}

// exitRoute says which router an exit goes through.
type exitRoute struct {
	Source string // SourceBundled / SourceExisting
	RPC    string // existing: its RPC endpoint
}

// pickExitRouter applies the mode rules. It may probe your own router's RPC.
func (s *Server) pickExitRouter(ctx context.Context) (exitRoute, error) {
	mode := s.curMode()
	s.mu.Lock()
	ext := s.Cfg.ExitRouterRPC
	s.mu.Unlock()
	if mode == ModeDirect {
		return exitRoute{}, errors.New(msgExitsDirect)
	}
	if mode == ModeExisting || (mode == ModeAuto && ext != "") {
		if ext != "" {
			pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			err := s.exitRPC(ext).SupportsExits(pctx)
			cancel()
			if err == nil {
				return exitRoute{Source: SourceExisting, RPC: ext}, nil
			}
			s.logf("own Session Router at %s can't do exits: %v", ext, err)
		}
		if mode == ModeExisting {
			return exitRoute{}, errors.New(msgExitsExisting)
		}
	}
	if s.router == nil {
		return exitRoute{}, errors.New("the bundled Session Router isn't set up")
	}
	return exitRoute{Source: SourceBundled}, nil
}

// buyerSesh is the address paid exit time is bought for: the bundled
// router's exit key, or your own router's address (read from its DNS).
func (s *Server) buyerSesh(ctx context.Context, rt exitRoute) (string, error) {
	if rt.Source == SourceBundled {
		k, err := exits.RouterKey(s.exitKeyFile(), true)
		if err != nil {
			return "", err
		}
		return exits.SeshFromPub(k.Public().(ed25519.PublicKey)), nil
	}
	var last error
	for _, c := range s.ext.candidates() {
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		a, err := exits.OwnSesh(dctx, c.dns)
		cancel()
		if err == nil {
			return a, nil
		}
		last = err
	}
	return "", fmt.Errorf("couldn't read your Session Router's own address from its DNS: %v", last)
}

// --------------------------------------------------------------- HTTP

type exitRow struct {
	ID        uint64            `json:"id"`
	Sesh      string            `json:"sesh"`
	Meta      string            `json:"meta"`
	Info      *exits.Meta       `json:"info,omitempty"`
	BondETH   string            `json:"bondEth"`
	Status    string            `json:"statusText"`
	ForSale   bool              `json:"forSale"`
	Payout    string            `json:"payout,omitempty"`
	PriceETH  string            `json:"ethPerHour,omitempty"`
	PriceUSDC string            `json:"usdcPerHour,omitempty"`
	MaxHours  uint32            `json:"maxHours,omitempty"`
	Why       string            `json:"why,omitempty"`
	Extra     map[string]string `json:"-"`
}

func (s *Server) exitsList(w http.ResponseWriter, r *http.Request) {
	if !s.exitsEnabled() {
		writeErr(w, 404, "exits aren't set up for this network")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	ix, err := s.regIndex(ctx, 15*time.Second)
	if err != nil {
		writeErr(w, 502, "couldn't read the registry: "+err.Error())
		return
	}
	k := s.leaseContract()
	rows := []exitRow{}
	for _, e := range ix.Search(r.URL.Query().Get("q"), exits.TagExit, false) {
		rw := exitRow{ID: e.ID, Sesh: e.Sesh, Meta: e.Meta, BondETH: registry.FormatWei(e.BondWei()), Status: registry.StatusName(e.Status)}
		m := exits.ParseMeta(e.Meta)
		rw.Info = &m
		o, err := k.Offer(ctx, e.ID)
		switch {
		case err != nil:
			rw.Why = "couldn't read its price: " + err.Error()
		case !o.ForSale():
			rw.Why = "not selling time right now"
		case o.Owner != e.Owner:
			rw.Why = "the listing changed hands; the old price no longer applies"
		default:
			rw.ForSale = true
			rw.Payout = o.Payout.Hex()
			rw.MaxHours = o.MaxHours
			if o.WeiPerHour.Sign() > 0 {
				rw.PriceETH = registry.FormatWei(o.WeiPerHour)
			}
			if o.USDCPerHour.Sign() > 0 {
				rw.PriceUSDC = formatUnits(o.USDCPerHour, s.usdcDec())
			}
		}
		rows = append(rows, rw)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ForSale && !rows[j].ForSale })
	s.syncPaidElsewhere(ctx, k, rows)
	s.refreshTopUpPrice(ctx)
	out := s.exitsStatusMap()
	out["exits"] = rows
	out["exitLease"] = s.Cfg.ExitLease.Hex()
	out["exitLeaseUrl"] = s.explorer() + "/address/" + s.Cfg.ExitLease.Hex()
	writeJSON(w, 200, out)
}

func (s *Server) usdcDec() int {
	if s.Cfg.USDCDecimals > 0 {
		return s.Cfg.USDCDecimals
	}
	return 6
}

// exitsStatusMap is the connection + purchases block for the UI.
func (s *Server) exitsStatusMap() map[string]any {
	s.ex.mu.Lock()
	defer s.ex.mu.Unlock()
	s.exitsLoadLocked()
	now := uint64(time.Now().Unix())
	out := map[string]any{"now": now, "mode": s.curMode()}
	leases := make([]map[string]any, 0, len(s.ex.Leases))
	for i := len(s.ex.Leases) - 1; i >= 0; i-- {
		l := s.ex.Leases[i]
		m := map[string]any{"leaseId": l.LeaseID, "listingId": l.ListingID, "exitSesh": l.ExitSesh, "buyerSesh": l.BuyerSesh,
			"hours": l.Hours, "start": l.Start, "end": l.End, "active": l.End > now, "source": l.Source, "country": l.Country,
			"paid": s.paidText(l.Lease), "txUrl": s.explorer() + "/tx/" + l.Tx.Hex(), "via": l.Via}
		leases = append(leases, m)
	}
	out["leases"] = leases
	if c := s.ex.Conn; c != nil {
		left := int64(c.End) - int64(now)
		if left < 0 {
			left = 0
		}
		warn := int64(s.exitWarnBefore() / time.Second)
		conn := map[string]any{"listingId": c.ListingID, "exitSesh": c.ExitSesh, "buyerSesh": c.BuyerSesh,
			"source": c.Source, "end": c.End, "secondsLeft": left, "since": c.Since.Unix(), "dns": c.DNS,
			"asset": c.Asset, "warnSeconds": warn, "expiring": left <= warn, "dnsState": c.DNSState,
			"phase": c.Phase, "sessionLive": c.SessionLive, "exitCapable": c.ExitCapable, "routesUp": c.RoutesUp, "routesReason": c.RoutesReason}
		if s.ex.topPrice.key == fmt.Sprintf("%d/%s/%s", c.ListingID, c.BuyerSesh, c.Asset) {
			conn["topUpPrice"] = s.ex.topPrice.text
		}
		out["connected"] = conn
	}
	if s.ex.Last != "" {
		out["last"] = s.ex.Last
	}
	out["busy"] = s.ex.busy
	return out
}

func (s *Server) paidText(l exits.Lease) string {
	v, _ := new(big.Int).SetString(l.Amount, 10)
	if v == nil {
		v = new(big.Int)
	}
	if l.Asset.IsZero() {
		return registry.FormatWei(v) + " ETH"
	}
	return formatUnits(v, s.usdcDec()) + " USDC"
}

func (s *Server) exitsStatus(w http.ResponseWriter, r *http.Request) {
	if !s.exitsEnabled() {
		writeErr(w, 404, "exits aren't set up for this network")
		return
	}
	s.refreshTopUpPrice(r.Context())
	writeJSON(w, 200, s.exitsStatusMap())
}

func (s *Server) setBusy(b bool) bool {
	s.ex.mu.Lock()
	defer s.ex.mu.Unlock()
	if b && s.ex.busy {
		return false
	}
	s.ex.busy = b
	return true
}

// exitsBuy pays for hours: approve USDC if needed, then ExitLease.buy.
func (s *Server) exitsBuy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST")
		return
	}
	if !s.exitsEnabled() {
		writeErr(w, 404, "exits aren't set up for this network")
		return
	}
	var req struct {
		ListingID uint64 `json:"listingId"`
		Asset     string `json:"asset"`
		Hours     uint32 `json:"hours"`
	}
	if err := readJSON(r, &req); err != nil || req.ListingID == 0 || req.Hours == 0 {
		writeErr(w, 400, "pick an exit and a number of hours")
		return
	}
	s.mu.Lock()
	v, c := s.vault, s.eth
	s.mu.Unlock()
	if v == nil || c == nil {
		writeErr(w, 403, "unlock the wallet first")
		return
	}
	if !s.setBusy(true) {
		writeErr(w, 409, "another exit action is running")
		return
	}
	defer s.setBusy(false)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	rt, err := s.pickExitRouter(ctx)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	buyer, err := s.buyerSesh(ctx, rt)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	ix, err := s.regIndex(ctx, 15*time.Second)
	if err != nil {
		writeErr(w, 502, "couldn't read the registry: "+err.Error())
		return
	}
	var entry *registry.Entry
	for _, e := range ix.Search("", exits.TagExit, false) {
		if e.ID == req.ListingID {
			e := e
			entry = &e
		}
	}
	if entry == nil {
		writeErr(w, 404, "that exit isn't listed any more")
		return
	}
	lease, txURL, err := s.purchase(ctx, req.ListingID, req.Asset, req.Hours, buyer)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	rec := exitLeaseRec{Lease: *lease, ExitSesh: entry.Sesh, Source: rt.Source}
	m := exits.ParseMeta(entry.Meta)
	rec.Country, rec.DNS = m.Country, m.DNS
	s.ex.mu.Lock()
	s.exitsLoadLocked()
	s.ex.Leases = append(s.ex.Leases, rec)
	s.ex.Last = fmt.Sprintf("Bought %d hour(s) on exit #%d for %s.", lease.Hours, lease.ListingID, s.paidText(*lease))
	s.exitsSaveLocked()
	s.ex.mu.Unlock()
	s.logf("exit lease %d: %d h on listing %d, ends %s", lease.LeaseID, lease.Hours, lease.ListingID, time.Unix(int64(lease.End), 0).Format(time.RFC3339))
	writeJSON(w, 200, map[string]any{"lease": rec, "txUrl": txURL})
}

func (s *Server) assetAddr(name string) (eth.Address, error) {
	if strings.EqualFold(name, "usdc") {
		if s.Cfg.USDC.IsZero() {
			return eth.Address{}, errors.New("no USDC on this network")
		}
		return s.Cfg.USDC, nil
	}
	return eth.Address{}, nil
}

func (s *Server) assetName(a eth.Address) string {
	if a.IsZero() {
		return "eth"
	}
	return "usdc"
}

// purchase pays ExitLease for hours of time for buyer: approve the exact
// USDC amount if needed, then buy. ExitLease adds the hours to the end of any
// time the buyer still has, so the same call buys new time and tops it up.
func (s *Server) purchase(ctx context.Context, listingID uint64, assetName string, hours uint32, buyer string) (*exits.Lease, string, error) {
	if s.ex.buyFor != nil {
		return s.ex.buyFor(ctx, listingID, assetName, hours, buyer)
	}
	s.mu.Lock()
	v, c := s.vault, s.eth
	s.mu.Unlock()
	if v == nil || c == nil {
		return nil, "", errors.New("unlock the wallet first")
	}
	asset, err := s.assetAddr(assetName)
	if err != nil {
		return nil, "", err
	}
	k := &exits.Contract{C: c, Addr: s.Cfg.ExitLease}
	amount, err := k.Quote(ctx, listingID, asset, hours)
	if err != nil {
		return nil, "", exits.ExplainRevert(err)
	}
	key := v.Keys.EthKey
	if !asset.IsZero() {
		al, err := allowance(ctx, c, asset, key.Address(), s.Cfg.ExitLease)
		if err != nil {
			return nil, "", fmt.Errorf("couldn't read your USDC allowance: %v", err)
		}
		if al.Cmp(amount) < 0 {
			if _, err := c.SendAndWait(ctx, key, &asset, nil, eth.EncodeApprove(s.Cfg.ExitLease, amount)); err != nil {
				return nil, "", fmt.Errorf("USDC approval failed: %v", err)
			}
		}
	}
	var value *big.Int
	if asset.IsZero() {
		value = amount
	}
	rcpt, err := c.SendAndWait(ctx, key, &s.Cfg.ExitLease, value, exits.EncodeBuy(listingID, asset, hours, amount, buyer))
	if err != nil {
		return nil, "", fmt.Errorf("buying time failed: %v", exits.ExplainRevert(err))
	}
	txURL := s.explorer() + "/tx/" + rcpt.TxHash.Hex()
	for i := range rcpt.Logs {
		if l, err := exits.DecodeLeased(&rcpt.Logs[i]); err == nil && l != nil {
			return l, txURL, nil
		}
	}
	return nil, txURL, errors.New("the purchase went through but its receipt had no Leased event; tx " + rcpt.TxHash.Hex())
}

// exitPrice is the price of hours on the connected exit, as text ("0.10 USDC").
func (s *Server) exitPrice(ctx context.Context, listingID uint64, assetName string, hours uint32) (string, error) {
	if s.ex.priceFor != nil {
		return s.ex.priceFor(ctx, listingID, assetName, hours)
	}
	asset, err := s.assetAddr(assetName)
	if err != nil {
		return "", err
	}
	amt, err := s.leaseContract().Quote(ctx, listingID, asset, hours)
	if err != nil {
		return "", exits.ExplainRevert(err)
	}
	if asset.IsZero() {
		return registry.FormatWei(amt) + " ETH", nil
	}
	return formatUnits(amt, s.usdcDec()) + " USDC", nil
}

// wrongCurrency reports whether err means the exit doesn't take that
// currency (as opposed to a network or chain error).
func wrongCurrency(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "doesn't take that currency") || strings.Contains(err.Error(), "no USDC"))
}

// exitsQuote prices a purchase for the confirm step: listingId 0 means
// another hour on the connected exit, in the currency it was paid in last
// (or the other one if the exit no longer takes it).
func (s *Server) exitsQuote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST")
		return
	}
	var req struct {
		ListingID uint64 `json:"listingId"`
		Asset     string `json:"asset"`
		Hours     uint32 `json:"hours"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	if req.Hours == 0 {
		req.Hours = 1
	}
	tries := []string{strings.ToLower(req.Asset)}
	end := uint64(0)
	if req.ListingID == 0 {
		s.ex.mu.Lock()
		c := s.ex.Conn
		s.ex.mu.Unlock()
		if c == nil {
			writeErr(w, 409, "you're not connected to an exit")
			return
		}
		req.ListingID, end = c.ListingID, c.End
		if req.Asset == "" {
			tries = []string{c.Asset, map[string]string{"usdc": "eth", "eth": "usdc"}[c.Asset]}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var err error
	for _, a := range tries {
		if a == "" {
			continue
		}
		var txt string
		txt, err = s.exitPrice(ctx, req.ListingID, a, req.Hours)
		if err != nil && !wrongCurrency(err) && ctx.Err() == nil {
			// One retry: right after DNS switches, the first chain call can miss.
			time.Sleep(time.Second)
			txt, err = s.exitPrice(ctx, req.ListingID, a, req.Hours)
		}
		if err == nil {
			out := map[string]any{"listingId": req.ListingID, "asset": a, "hours": req.Hours, "price": txt}
			if end > 0 {
				out["newEnd"] = max(end, uint64(time.Now().Unix())) + uint64(req.Hours)*3600
			}
			writeJSON(w, 200, out)
			return
		}
		if !wrongCurrency(err) { // only switch currency when the exit doesn't take this one
			break
		}
	}
	if err == nil {
		err = errors.New("pick a currency")
	}
	writeErr(w, 409, err.Error())
}

// exitsTopUp adds hours (default 1) to the connected exit, paid like the
// last purchase (USDC or ETH) unless the request says otherwise. The seller
// admits the new time once the purchase is confirmed; the connection stays up.
func (s *Server) exitsTopUp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST")
		return
	}
	var req struct {
		Hours uint32 `json:"hours"`
		Asset string `json:"asset"`
	}
	_ = readJSON(r, &req)
	if req.Hours == 0 {
		req.Hours = 1
	}
	s.ex.mu.Lock()
	c := s.ex.Conn
	s.ex.mu.Unlock()
	if c == nil {
		writeErr(w, 409, "you're not connected to an exit")
		return
	}
	if !s.setBusy(true) {
		writeErr(w, 409, "another exit action is running")
		return
	}
	defer s.setBusy(false)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	asset := strings.ToLower(req.Asset)
	if asset == "" {
		asset = c.Asset
	}
	tries := []string{asset}
	if req.Asset == "" { // paid like last time if the exit still takes it, else the other one
		other := "usdc"
		if asset == "usdc" {
			other = "eth"
		}
		tries = append(tries, other)
	}
	var lease *exits.Lease
	var txURL string
	var err error
	for _, a := range tries {
		if a == "" {
			continue
		}
		if a == "usdc" && s.Cfg.USDC.IsZero() && s.ex.buyFor == nil {
			continue
		}
		lease, txURL, err = s.purchase(ctx, c.ListingID, a, req.Hours, c.BuyerSesh)
		if err == nil {
			asset = a
			break
		}
		if !wrongCurrency(err) {
			break
		}
	}
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	s.ex.mu.Lock()
	s.exitsLoadLocked()
	rec := exitLeaseRec{Lease: *lease, ExitSesh: c.ExitSesh, Source: c.Source, DNS: c.DNS}
	for _, l := range s.ex.Leases {
		if l.ListingID == c.ListingID && l.Country != "" {
			rec.Country = l.Country
		}
	}
	s.ex.Leases = append(s.ex.Leases, rec)
	if s.ex.Conn == c && lease.End > c.End {
		c.End, c.warned, c.Asset = lease.End, false, asset
	}
	s.ex.topPrice.key = ""
	s.ex.Last = fmt.Sprintf("Added %d hour(s) on exit #%d for %s. Your time now ends %s.", lease.Hours, lease.ListingID,
		s.paidText(*lease), time.Unix(int64(lease.End), 0).Format("15:04 MST"))
	s.exitsSaveLocked()
	s.ex.mu.Unlock()
	s.logf("exit lease %d: topped up %d h on listing %d, now ends %s", lease.LeaseID, lease.Hours, lease.ListingID,
		time.Unix(int64(lease.End), 0).Format(time.RFC3339))
	out := s.exitsStatusMap()
	out["txUrl"] = txURL
	writeJSON(w, 200, out)
}

// refreshTopUpPrice quotes one hour on the connected exit for the UI's
// top-up button; quoted once per connection and asset.
func (s *Server) refreshTopUpPrice(ctx context.Context) {
	s.ex.mu.Lock()
	c := s.ex.Conn
	s.ex.mu.Unlock()
	if c == nil {
		return
	}
	key := fmt.Sprintf("%d/%s/%s", c.ListingID, c.BuyerSesh, c.Asset)
	s.ex.mu.Lock()
	known := s.ex.topPrice.key == key
	s.ex.mu.Unlock()
	if known {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	txt, err := s.exitPrice(qctx, c.ListingID, c.Asset, 1)
	if err != nil {
		return
	}
	s.ex.mu.Lock()
	s.ex.topPrice.key, s.ex.topPrice.text = key, txt
	s.ex.mu.Unlock()
}

func allowance(ctx context.Context, c *eth.Client, token, owner, spender eth.Address) (*big.Int, error) {
	data := append([]byte{0xdd, 0x62, 0xed, 0x3e}, eth.WordAddr(owner)...)
	data = append(data, eth.WordAddr(spender)...)
	out, err := c.EthCall(ctx, eth.CallMsg{To: &token, Data: data})
	if err != nil {
		return nil, err
	}
	if len(out) < 32 {
		return nil, errors.New("short reply")
	}
	return new(big.Int).SetBytes(out[:32]), nil
}

// exitsConnect maps the exit for a paid lease through the chosen router.
func (s *Server) exitsConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST")
		return
	}
	var req struct {
		ListingID uint64 `json:"listingId"`
	}
	if err := readJSON(r, &req); err != nil || req.ListingID == 0 {
		writeErr(w, 400, "pick an exit")
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
	conn, err := s.connectExit(ctx, req.ListingID)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"connected": conn})
}

func (s *Server) connectExit(ctx context.Context, listingID uint64) (*exitConn, error) {
	rt, err := s.pickExitRouter(ctx)
	if err != nil {
		return nil, err
	}
	buyer, err := s.buyerSesh(ctx, rt)
	if err != nil {
		return nil, err
	}
	s.ex.mu.Lock()
	s.exitsLoadLocked()
	var rec *exitLeaseRec
	for i := range s.ex.Leases {
		l := &s.ex.Leases[i]
		if l.ListingID == listingID && l.BuyerSesh == buyer {
			rec = l
		}
	}
	prev := s.ex.Conn
	s.ex.mu.Unlock()
	if rec == nil {
		return nil, errors.New("you have no time on this exit for this router's address. Buy hours first.")
	}
	activeUntil := s.leaseContract().ActiveUntil
	if s.ex.activeUntil != nil {
		activeUntil = s.ex.activeUntil
	}
	end, err := activeUntil(ctx, listingID, buyer)
	if err != nil {
		return nil, fmt.Errorf("couldn't check your paid time: %v", err)
	}
	if end <= uint64(time.Now().Unix()) {
		return nil, errors.New("your paid time on this exit has ended. Buy more hours to connect.")
	}
	if prev != nil && prev.ListingID != listingID {
		s.disconnectExit(context.Background(), "switching exits")
	}
	rpc := rt.RPC
	if rt.Source == SourceBundled {
		if s.router.LaunchedKeyFile() != s.exitKeyFile() && s.router.Running() {
			s.mu.Lock()
			open := s.openCount()
			s.mu.Unlock()
			if open > 0 {
				return nil, errors.New("finish your open swap first: connecting an exit restarts the bundled router with your exit address")
			}
			s.stopRouter()
		}
		s.router.SetKeyFile(s.exitKeyFile())
		if _, err := s.router.Ensure(ctx); err != nil {
			return nil, err
		}
		rpc = s.router.RPC()
		if rpc == "" {
			return nil, errors.New("the bundled router has no RPC (exits are off in its options)")
		}
	}
	rr := s.exitRPC(rpc)
	if err := rr.SupportsExits(ctx); err != nil {
		if rt.Source == SourceBundled {
			return nil, errors.New("the bundled Session Router can't do exits; run scripts/fetch-session-router.sh to get the exit build of 1.1.1")
		}
		return nil, errors.New(msgExitsExisting)
	}
	// Paths to a fresh exit take a moment: retry until the request's deadline.
	var last error
	var routes exits.RouteStatus
	for {
		mctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		routes, last = rr.MapExit(mctx, rec.ExitSesh)
		cancel()
		if last == nil {
			break
		}
		s.logf("map exit %s: %v", rec.ExitSesh, last)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("couldn't reach the exit: %v. The seller may be offline, or hasn't admitted your purchase yet (it waits for confirmations). Try again in a minute.", last)
		case <-time.After(3 * time.Second):
		}
	}
	if rt.Source == SourceBundled {
		s.router.MarkConnected()
	}
	c := &exitConn{ListingID: listingID, ExitSesh: rec.ExitSesh, BuyerSesh: buyer, Source: rt.Source, RPC: rpc, End: end, Since: time.Now(), DNS: rec.DNS,
		Asset: s.assetName(rec.Asset), Phase: phaseRouting}
	if routes.Known && !routes.Up {
		c.RoutesReason = routes.Reason
	}
	s.ex.mu.Lock()
	s.ex.Conn = c
	s.ex.Last = "Exit #" + strconv.FormatUint(listingID, 10) + " mapped. Waiting for this computer's routes…"
	s.ex.mu.Unlock()
	s.touch()
	s.logf("exit %s mapped via %s router until %s; waiting for routes", rec.ExitSesh, rt.Source, time.Unix(int64(end), 0).Format(time.RFC3339))
	deadline := time.Now().Add(exitRoutesWait)
	for {
		if s.checkExitRoutes(ctx, c) {
			// Routes are in. Only say Connected once a request actually gets
			// through the exit; otherwise put the network back and say so.
			s.ex.mu.Lock()
			c.Phase = phaseTesting
			s.ex.Last = "Exit #" + strconv.FormatUint(listingID, 10) + ": routes are up. Testing a request through it…"
			s.ex.mu.Unlock()
			s.routesChanged()
			if perr := exitProbe(ctx); perr != nil {
				s.logf("exit %s: no traffic through it: %v", rec.ExitSesh, perr)
				s.disconnectExit(context.Background(), "no traffic got through it")
				msg := "Couldn't reach the internet through exit #" + strconv.FormatUint(listingID, 10) +
					", so the wallet disconnected and your network is back to normal. The seller may be offline. Your paid time keeps running; try again later."
				s.ex.mu.Lock()
				s.ex.Last = msg
				s.ex.mu.Unlock()
				return nil, errors.New(msg)
			}
			s.exitRoutesUp(c)
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			s.exitRoutesDown(c, "")
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	s.ex.mu.Lock()
	cp := *c
	s.ex.mu.Unlock()
	return &cp, nil
}

// exitProbe checks that web traffic gets through a just-connected exit. It
// asks Cloudflare's trace page by IP address (port 443, then 80), because
// system DNS may not be switched yet. Tests replace it.
var exitProbe = probeThroughExit

func probeThroughExit(ctx context.Context) error {
	t := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 8 * time.Second}
	cl := &http.Client{Timeout: 10 * time.Second, Transport: t}
	targets := []string{"https://1.1.1.1/cdn-cgi/trace", "http://1.0.0.1/cdn-cgi/trace"}
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for i := 0; time.Now().Before(deadline) && ctx.Err() == nil; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targets[i%len(targets)], nil)
		if err != nil {
			return err
		}
		resp, err := cl.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		last = err
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if last == nil {
		last = ctx.Err()
	}
	return last
}

// newChainHTTP is the chain RPC client. It has its own transport so the wallet
// can drop its connections when an exit changes the routes, and HTTP/2 pings
// so a connection that was busy at that moment is found dead quickly instead
// of holding every later request until it times out.
func newChainHTTP() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 10 * time.Second}
	return &http.Client{Timeout: 30 * time.Second, Transport: t}
}

// routesChanged drops kept-alive connections: connecting an exit moves the
// default route onto the router's adapter (and disconnecting moves it back),
// so connections opened before carry the old source address and never answer.
func (s *Server) routesChanged() {
	s.mu.Lock()
	c := s.eth
	s.mu.Unlock()
	if c != nil && c.HTTP != nil {
		c.HTTP.CloseIdleConnections()
	}
	if s.http != nil {
		s.http.CloseIdleConnections()
	}
	http.DefaultClient.CloseIdleConnections()
}

func (s *Server) exitsDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s.disconnectExit(ctx, "you disconnected")
	writeJSON(w, 200, s.exitsStatusMap())
}

// disconnectExit unmaps the current exit. The router stays as it was: a
// router the wallet did not start keeps running; the bundled one goes back
// to the normal idle rules.
func (s *Server) disconnectExit(ctx context.Context, why string) {
	s.ex.mu.Lock()
	c := s.ex.Conn
	s.ex.Conn = nil
	s.ex.mu.Unlock()
	if c == nil {
		return
	}
	s.restoreDNS("exit disconnected") // first, while the exit's resolver still answers
	alive := c.Source == SourceExisting || (s.router != nil && s.router.Running() && s.router.RPC() == c.RPC)
	var err error
	if alive {
		err = s.exitRPC(c.RPC).UnmapExit(ctx, c.ExitSesh)
	}
	if c.Source == SourceBundled && s.router != nil {
		s.router.SetKeyFile("") // next launch: a fresh address again
	}
	s.routesChanged()
	msg := "Disconnected from exit #" + strconv.FormatUint(c.ListingID, 10) + " (" + why + ")."
	if err != nil {
		msg += " Unmapping reported: " + err.Error()
	}
	s.ex.mu.Lock()
	s.ex.Last = msg
	s.ex.mu.Unlock()
	s.logf("%s", msg)
}

// exitTick runs with reconcile: unmap at the end of paid time, and notice
// when the bundled router went away underneath a mapped exit.
func (s *Server) exitTick() {
	s.ex.mu.Lock()
	c := s.ex.Conn
	s.ex.mu.Unlock()
	if c == nil {
		return
	}
	now := time.Now()
	if uint64(now.Unix()) >= c.End {
		// Time may have been added since (a top-up from another device, or
		// a swap claim through ExitPay): ask the chain before disconnecting.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		activeUntil := s.leaseContract().ActiveUntil
		if s.ex.activeUntil != nil {
			activeUntil = s.ex.activeUntil
		}
		if end, err := activeUntil(ctx, c.ListingID, c.BuyerSesh); err == nil && end > uint64(now.Unix()) {
			s.ex.mu.Lock()
			if s.ex.Conn == c {
				c.End, c.warned = end, false
				s.ex.Last = "Your time on exit #" + strconv.FormatUint(c.ListingID, 10) + " was extended to " + time.Unix(int64(end), 0).Format("15:04 MST") + "."
			}
			s.ex.mu.Unlock()
			s.logf("exit #%d: paid time extended to %s", c.ListingID, time.Unix(int64(end), 0).Format(time.RFC3339))
			return
		}
		s.disconnectExit(ctx, "paid time ended")
		return
	}
	if left := time.Unix(int64(c.End), 0).Sub(now); left <= s.exitWarnBefore() {
		s.ex.mu.Lock()
		first := s.ex.Conn == c && !c.warned
		if first {
			c.warned = true
			s.ex.Last = fmt.Sprintf("Your time on exit #%d ends at %s (in %s). Add an hour to stay connected.",
				c.ListingID, time.Unix(int64(c.End), 0).Format("15:04 MST"), left.Round(time.Minute))
		}
		s.ex.mu.Unlock()
		if first {
			s.logf("exit #%d: paid time ends in %s; offering a top-up", c.ListingID, left.Round(time.Second))
		}
	}
	if c.Source == SourceBundled && (s.router == nil || !s.router.Running()) {
		s.ex.mu.Lock()
		if s.ex.Conn == c {
			s.ex.Conn = nil
			s.ex.Last = "The bundled Session Router stopped, so the exit is no longer in use."
		}
		s.ex.mu.Unlock()
		if s.router != nil {
			s.router.SetKeyFile("")
		}
		s.restoreDNS("the bundled Session Router stopped")
		s.routesChanged()
		return
	}
	s.ex.mu.Lock()
	busy := s.ex.busy
	s.ex.mu.Unlock()
	if busy {
		return // Connect is checking the routes itself
	}
	if s.checkExitRoutes(context.Background(), c) {
		s.exitRoutesUp(c)
	} else {
		s.exitRoutesDown(c, "")
	}
}

// checkExitRoutes asks the router about the mapped exit and records the
// three things the page shows. It returns whether this computer's routes
// are up. A build that doesn't report routes is judged by whether the
// exit's resolver is routed through the tunnel.
func (s *Server) checkExitRoutes(ctx context.Context, c *exitConn) bool {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	l, err := s.exitRPC(c.RPC).ListExits(cctx)
	var live, capable, found, up bool
	reason := ""
	if err != nil {
		reason = "the router didn't answer: " + err.Error()
	} else {
		for _, e := range l.Exits {
			if e.Address == c.ExitSesh {
				found, live, capable = true, e.SessionLive, e.ExitCapable
			}
		}
		switch {
		case !found:
			reason = "the exit is not mapped on the router"
		case l.Routes.Known:
			up, reason = l.Routes.Up, l.Routes.Reason
		case c.DNS != "":
			linkFor := tunnelLink
			if s.ex.linkFor != nil {
				linkFor = s.ex.linkFor
			}
			if _, lerr := linkFor(c.DNS); lerr != nil {
				reason = lerr.Error()
			} else {
				up = true
			}
		default:
			up = true
		}
	}
	if !up && reason == "" {
		reason = "the router says the routes are not installed"
	}
	s.ex.mu.Lock()
	if s.ex.Conn == c {
		c.SessionLive, c.ExitCapable, c.RoutesUp = live, capable, up
		c.RoutesReason = ""
		if !up {
			c.RoutesReason = reason
		}
	}
	s.ex.mu.Unlock()
	return up
}

// exitRoutesUp: the routes are in. Now the page may say Connected and the
// wallet switches system DNS.
func (s *Server) exitRoutesUp(c *exitConn) {
	s.ex.mu.Lock()
	if s.ex.Conn != c || c.Phase == phaseUp {
		s.ex.mu.Unlock()
		return
	}
	c.Phase = phaseUp
	s.ex.Last = "Connected to exit #" + strconv.FormatUint(c.ListingID, 10) + "."
	s.ex.mu.Unlock()
	s.logf("exit %s: this computer's routes are up; connected", c.ExitSesh)
	s.routesChanged()
	go s.switchDNS(c)
}

// exitRoutesDown: the routes didn't come up or went away. System DNS goes
// back (lookups would go nowhere); the wallet keeps checking.
func (s *Server) exitRoutesDown(c *exitConn, _ string) {
	s.ex.mu.Lock()
	if s.ex.Conn != c || c.Phase == phaseNoRoutes {
		s.ex.mu.Unlock()
		return
	}
	was := c.Phase
	c.Phase = phaseNoRoutes
	why := c.RoutesReason
	c.DNSState, c.dnsSet = "", false
	if was == phaseUp {
		s.ex.Last = "This computer's routes to exit #" + strconv.FormatUint(c.ListingID, 10) + " dropped (" + why + "). DNS is back to normal; the wallet keeps checking."
	} else {
		s.ex.Last = "Exit #" + strconv.FormatUint(c.ListingID, 10) + " is mapped, but this computer's routes didn't come up (" + why + "). DNS was not changed."
	}
	s.ex.mu.Unlock()
	s.logf("exit %s: routes not up (%s)", c.ExitSesh, why)
	if was == phaseUp {
		s.restoreDNS("the exit's routes dropped")
		s.routesChanged()
	}
}

// exitHoldsRouter reports whether a mapped exit needs the bundled router up.
func (s *Server) exitHoldsRouter() bool {
	s.ex.mu.Lock()
	defer s.ex.mu.Unlock()
	return s.ex.Conn != nil && s.ex.Conn.Source == SourceBundled
}

func (s *Server) exitConnSnapshot() map[string]any {
	s.ex.mu.Lock()
	defer s.ex.mu.Unlock()
	c := s.ex.Conn
	if c == nil {
		return nil
	}
	left := int64(c.End) - time.Now().Unix()
	if left < 0 {
		left = 0
	}
	return map[string]any{"listingId": c.ListingID, "exitSesh": c.ExitSesh, "source": c.Source, "end": c.End, "secondsLeft": left, "dns": c.DNS,
		"phase": c.Phase, "routesReason": c.RoutesReason}
}

// localRPC accepts only RPC addresses on this computer.
func localRPC(ep string) bool {
	if strings.HasPrefix(ep, "ipc://") {
		return len(ep) > len("ipc://")
	}
	if strings.HasPrefix(ep, "tcp://") {
		h := strings.TrimPrefix(ep, "tcp://")
		return strings.HasPrefix(h, "127.0.0.1:") || strings.HasPrefix(h, "localhost:") || strings.HasPrefix(h, "[::1]:")
	}
	return false
}

// syncPaidElsewhere picks up time bought for the bundled exit key by someone
// else, such as a swap claim through ExitPay. One activeUntil call per listed
// exit; the Leased events are only read when there is time we don't know of.
func (s *Server) syncPaidElsewhere(ctx context.Context, k *exits.Contract, rows []exitRow) {
	key, err := exits.RouterKey(s.exitKeyFile(), false)
	if err != nil {
		return // no exit key yet: nothing can have been bought for it
	}
	sesh := exits.SeshFromPub(key.Public().(ed25519.PublicKey))
	activeUntil := k.ActiveUntil
	if s.ex.activeUntil != nil {
		activeUntil = s.ex.activeUntil
	}
	var me eth.Address
	s.mu.Lock()
	if s.vault != nil && s.vault.Keys.EthKey != nil {
		me = s.vault.Keys.EthKey.Address()
	}
	s.mu.Unlock()
	for _, rw := range rows {
		until, err := activeUntil(ctx, rw.ID, sesh)
		if err != nil || until <= uint64(time.Now().Unix()) {
			continue
		}
		s.ex.mu.Lock()
		s.exitsLoadLocked()
		known := false
		for _, l := range s.ex.Leases {
			if l.ListingID == rw.ID && l.BuyerSesh == sesh && l.End >= until {
				known = true
			}
		}
		s.ex.mu.Unlock()
		if known || k.C == nil {
			continue
		}
		head, err := k.C.Head(ctx)
		if err != nil {
			continue
		}
		var found []exits.Lease
		for from := s.Cfg.ExitLeaseFromBlock; from <= head.Number; from += 50000 {
			to := min(from+50000-1, head.Number)
			ls, err := k.Leases(ctx, from, to, rw.ID, nil)
			if err != nil {
				found = nil
				break
			}
			for _, l := range ls {
				if l.BuyerSesh == sesh {
					found = append(found, l)
				}
			}
		}
		m := exits.ParseMeta(rw.Meta)
		s.ex.mu.Lock()
		for _, l := range found {
			dup := false
			for _, x := range s.ex.Leases {
				if x.LeaseID == l.LeaseID {
					dup = true
				}
			}
			if dup {
				continue
			}
			rec := exitLeaseRec{Lease: l, ExitSesh: rw.Sesh, Source: SourceBundled, Country: m.Country, DNS: m.DNS}
			if l.Buyer != me {
				rec.Via = "swap"
			}
			s.ex.Leases = append(s.ex.Leases, rec)
			s.logf("found exit time paid elsewhere: %d h on exit #%d until %s", l.Hours, rw.ID, time.Unix(int64(l.End), 0).Format(time.RFC3339))
		}
		s.exitsSaveLocked()
		s.ex.mu.Unlock()
	}
}
