package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/price"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// Service holds the always-on maker features. All optional: a zero Service
// behaves like the v2 maker (static price from the offer, no limits beyond
// liquidity, unsigned quotes).
type Service struct {
	Oracle *price.Oracle // live price; nil = static Offer.PicoPerUnit
	// AssetDecimals > 0 means the asset is a USD stablecoin with these decimals
	// (priced at 1 USD); 0 means the native coin (ETH).
	AssetDecimals int
	MaxExposure   uint64 // piconero: reserved + locked-unsettled XMR may not exceed this (0 = no limit)
	MaxOpenQuotes uint64 // max swaps in MNegotiated (unfunded quotes); 0 = no limit
	Identity      *Identity
	Endpoints     []string      // advertised in the signed offer document
	DocTTL        time.Duration // signed offer lifetime (default 1 h)

	mu       sync.Mutex
	counters map[string]uint64
	started  time.Time
}

func (s *Service) count(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counters == nil {
		s.counters = map[string]uint64{}
		s.started = time.Now()
	}
	s.counters[k]++
}

// Terms are what the maker would agree to right now for a given value.
type Terms struct {
	Value       *big.Int `json:"value"`
	XMR         uint64   `json:"xmr"`
	PicoPerUnit *big.Int `json:"picoPerUnit"`
	D1          uint64   `json:"d1"`
	D2          uint64   `json:"d2"`
	MinConfs    uint64   `json:"minConfs"`
	Grace       uint64   `json:"grace"`
	Premium     *big.Int `json:"premium"`
	Expires     int64    `json:"expires"`
	BlockSecs   uint64   `json:"blockSecs"` // Monero mean block time used for the windows and estimates
	// Estimates for the taker (seconds): time to XMR arrival in the shared
	// address, and until the XMR is spendable in the taker's own wallet.
	EstReadySecs     uint64 `json:"estReadySecs"`
	EstSpendableSecs uint64 `json:"estSpendableSecs"`
	// Set when this maker is listed in SeshRegistry. The taker checks the
	// listing on-chain; the maker's word for it is not trusted.
	Registry  *eth.Address `json:"registry,omitempty"`
	ListingID uint64       `json:"listingId,omitempty"`
}

// picoPerUnit returns the current rate (piconero per 1e18 base units).
func (m *Maker) picoPerUnit() (*big.Int, error) {
	if m.Oracle == nil {
		return m.Offer.PicoPerUnit, nil
	}
	snap, err := m.Oracle.Current()
	if err != nil {
		return nil, err
	}
	spread := 1 - float64(m.Offer.SpreadBps)/10000
	var pico float64
	if m.AssetDecimals == 0 {
		pico = snap.XmrPerEth() * spread * 1e12 // per 1e18 wei = per ETH
	} else {
		pico = 1e30 / math.Pow10(m.AssetDecimals) / snap.XMRUSD * spread
	}
	if !(pico > 0) || math.IsInf(pico, 0) {
		return nil, errors.New("bad price")
	}
	b, _ := new(big.Float).SetFloat64(pico).Int(nil)
	return b, nil
}

func (m *Maker) currentOffer() (Offer, error) {
	o := m.Offer
	p, err := m.picoPerUnit()
	if err != nil {
		return o, err
	}
	o.PicoPerUnit = p
	o.Expires = time.Now().Unix() + o.TTL
	if o.Direction == "" {
		o.Direction = "eth2xmr"
	}
	return o, nil
}

// quote computes the binding terms for value (owner may be zero for a preview).
// want is the confirmations the taker asked for (0 = no preference).
func (m *Maker) quote(ctx context.Context, value *big.Int, owner eth.Address, want uint64) (*Terms, error) {
	o := &m.Offer
	if value == nil || value.Cmp(o.Min) < 0 || value.Cmp(o.Max) > 0 {
		return nil, errors.New("asset or amount outside the offer")
	}
	ppu, err := m.picoPerUnit()
	if err != nil {
		return nil, fmt.Errorf("not quoting: %w", err)
	}
	x := new(big.Int).Mul(value, ppu)
	x.Div(x, one18)
	if x.Sign() <= 0 || !x.IsUint64() {
		return nil, errors.New("amount too small")
	}
	xmrAmt := x.Uint64()
	pol := m.Engine.Policy
	maxConfs := o.MaxConfs
	if maxConfs == 0 {
		maxConfs = swap.XmrConfMax
	}
	if want > maxConfs {
		return nil, fmt.Errorf("this maker waits for at most %d Monero confirmations; you asked for %d", maxConfs, want)
	}
	confs := max(o.MinConfs, swap.XmrConfs(xmrAmt, m.Engine.ReorgDepth(ctx)), want)
	bs, err := m.Engine.BlockSecs(ctx)
	if err != nil {
		return nil, err
	}
	d1, d2 := swap.Windows(confs, bs, pol)
	d1, d2 = max(d1, o.D1), max(d2, o.D2)
	grace := max(o.Grace, swap.Grace(pol))
	premium := new(big.Int)
	if o.PremiumBps > 0 && o.Asset.IsZero() {
		if o.WaiveAfter == 0 || owner.IsZero() || m.Engine.CompletedBy(owner) < o.WaiveAfter {
			premium.Mul(value, new(big.Int).SetUint64(o.PremiumBps))
			premium.Div(premium, big.NewInt(10000))
		}
	}
	t := &Terms{Value: value, XMR: xmrAmt, PicoPerUnit: ppu, D1: d1, D2: d2, MinConfs: confs, Grace: grace, BlockSecs: bs,
		Premium: premium, Expires: time.Now().Unix() + o.TTL,
		EstReadySecs:     pol.XmrLockBudget/3 + (confs+1)*bs,
		EstSpendableSecs: pol.XmrLockBudget/3 + (confs+1)*bs + (swap.XmrSpendLock+1)*bs,
	}
	if m.Registry != nil {
		a := m.Registry.Client.Addr
		t.Registry, t.ListingID = &a, m.Registry.ListingID
	}
	return t, nil
}

func (m *Maker) checkOpenQuotes() error {
	if m.MaxOpenQuotes == 0 {
		return nil
	}
	n := uint64(0)
	for _, id := range m.Engine.IDs() {
		ph, _, ok := m.Engine.Get(id)
		if ok && ph == swap.MNegotiated {
			n++
		}
	}
	if n >= m.MaxOpenQuotes {
		return fmt.Errorf("maker has too many open quotes (%d); try again shortly", n)
	}
	return nil
}

func (m *Maker) checkExposure(xmrAmt uint64) error {
	if m.MaxExposure == 0 {
		return nil
	}
	res, locked := m.Engine.Exposure()
	if res+locked+xmrAmt > m.MaxExposure {
		return fmt.Errorf("maker exposure limit reached (%d + %d piconero in flight)", res+locked, xmrAmt)
	}
	return nil
}

// OfferDoc builds and signs the current offer document.
func (m *Maker) OfferDoc() (*SignedOffer, error) {
	if m.Identity == nil {
		return nil, errors.New("maker has no identity key")
	}
	o, err := m.currentOffer()
	if err != nil {
		return nil, err
	}
	ttl := m.DocTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	now := time.Now()
	d := &OfferDoc{Version: 1, Endpoints: m.Endpoints, Direction: o.Direction, ChainID: o.ChainID,
		Contract: o.Contract, Asset: o.Asset, XmrNet: o.XmrNet, PicoPerUnit: o.PicoPerUnit, Min: o.Min, Max: o.Max,
		SpreadBps: o.SpreadBps, PremiumBps: o.PremiumBps, MinConfs: max(o.MinConfs, swap.XmrConfFloor),
		Grace: max(o.Grace, swap.Grace(m.Engine.Policy)), Issued: now.Unix(), Expires: now.Add(ttl).Unix()}
	return m.Identity.SignOffer(d)
}

type health struct {
	OK        bool   `json:"ok"`
	Price     string `json:"price"`
	Wallet    string `json:"wallet"`
	Eth       string `json:"eth"`
	OpenSwaps int    `json:"openSwaps"`
}

func (m *Maker) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	h := health{OK: true, Price: "static", Wallet: "ok", Eth: "ok"}
	if m.Oracle != nil {
		if _, err := m.Oracle.Current(); err != nil {
			h.OK, h.Price = false, err.Error()
		} else {
			h.Price = "ok"
		}
	}
	if err := m.Engine.Wallet.Session(ctx, m.Engine.MainWal, func(s *xmr.Sess) error { _, err := s.Height(ctx); return err }); err != nil {
		h.OK, h.Wallet = false, err.Error()
	}
	if _, err := m.Engine.K.C.Head(ctx); err != nil {
		h.OK, h.Eth = false, err.Error()
	}
	for ph, n := range m.Engine.PhaseCounts() {
		if !ph.Terminal() {
			h.OpenSwaps += n
		}
	}
	code := 200
	if !h.OK {
		code = 503
	}
	writeJSON(w, code, h)
}

// metrics serves Prometheus text format.
func (m *Maker) metrics(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	g := func(name, help string, v float64, labels string) {
		if help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		}
		fmt.Fprintf(&b, "%s%s %g\n", name, labels, v)
	}
	ph := m.Engine.PhaseCounts()
	keys := make([]string, 0, len(ph))
	for p := range ph {
		keys = append(keys, string(p))
	}
	sort.Strings(keys)
	fmt.Fprint(&b, "# HELP swapd_swaps Swaps in the store by phase.\n# TYPE swapd_swaps gauge\n")
	for _, k := range keys {
		g("swapd_swaps", "", float64(ph[swap.Phase(k)]), fmt.Sprintf(`{phase=%q}`, k))
	}
	res, locked := m.Engine.Exposure()
	g("swapd_xmr_reserved_piconero", "XMR promised to swaps not yet locked.", float64(res), "")
	g("swapd_xmr_locked_piconero", "XMR locked in swaps not yet settled.", float64(locked), "")
	g("swapd_xmr_exposure_limit_piconero", "Configured exposure limit (0 = none).", float64(m.MaxExposure), "")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var bal, unl uint64
	if err := m.Engine.Wallet.Session(ctx, m.Engine.MainWal, func(s *xmr.Sess) error {
		var err error
		bal, unl, err = s.Balance(ctx)
		return err
	}); err == nil {
		g("swapd_xmr_balance_piconero", "Maker wallet balance.", float64(bal), "")
		g("swapd_xmr_unlocked_piconero", "Maker wallet unlocked (spendable) balance.", float64(unl), "")
	}
	if m.Oracle != nil {
		s := m.Oracle.Status()
		g("swapd_price_xmr_usd", "Median XMR price (USD).", s.XMRUSD, "")
		g("swapd_price_eth_usd", "Median ETH price (USD).", s.ETHUSD, "")
		g("swapd_price_sources", "Sources that agreed at the last refresh.", float64(len(s.Sources)), "")
		g("swapd_price_age_seconds", "Age of the last accepted price.", time.Since(s.At).Seconds(), "")
		ok := 1.0
		if _, err := m.Oracle.Current(); err != nil {
			ok = 0
		}
		g("swapd_price_ok", "1 if quoting is allowed by the price circuit breaker.", ok, "")
	}
	g("swapd_reorg_depth_blocks", "Deepest Monero alternative chain seen by our daemon.", float64(m.Engine.ReorgDepth(ctx)), "")
	m.Service.mu.Lock()
	ck := make([]string, 0, len(m.counters))
	for k := range m.counters {
		ck = append(ck, k)
	}
	sort.Strings(ck)
	fmt.Fprint(&b, "# HELP swapd_events_total Counters.\n# TYPE swapd_events_total counter\n")
	for _, k := range ck {
		fmt.Fprintf(&b, "swapd_events_total{event=%q} %d\n", k, m.counters[k])
	}
	m.Service.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}
