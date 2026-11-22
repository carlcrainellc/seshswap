package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

// regState is the wallet's view of SeshRegistry: a local index of every
// listing (rebuilt from events, saved in the data dir) used for search and
// for finding makers. The chain stays the source of truth: before a maker
// is used, its bond and owner are read again with info().
type regState struct {
	mu       sync.Mutex
	ix       *registry.Index
	c        *eth.Client
	lastSync time.Time
	syncErr  string
	port     string          // tests only: ":port" appended to listed .sesh names
	assets   map[string]bool // which assets the listed makers take, from their offers (guarded by Server.mu)
}

func (s *Server) seshURL(sesh string) string { return "http://" + sesh + s.reg.port }

func (s *Server) regEnabled() bool { return !s.Cfg.Registry.IsZero() }

func (s *Server) regClient() *eth.Client {
	if s.eth != nil {
		return s.eth
	}
	s.reg.mu.Lock()
	defer s.reg.mu.Unlock()
	if s.reg.c == nil {
		urls := append([]string{}, s.Cfg.EthRPCs...)
		if s.Cfg.EthRPC != "" {
			urls = append([]string{s.Cfg.EthRPC}, urls...)
		}
		if len(urls) == 0 {
			return nil
		}
		s.reg.c = eth.NewClients(urls...)
	}
	return s.reg.c
}

// regIndex returns the index, syncing it if older than maxAge.
func (s *Server) regIndex(ctx context.Context, maxAge time.Duration) (*registry.Index, error) {
	c := s.regClient()
	if c == nil {
		return nil, errors.New("no Ethereum RPC configured")
	}
	s.reg.mu.Lock()
	if s.reg.ix == nil {
		path := ""
		if s.Cfg.DataDir != "" {
			path = filepath.Join(s.Cfg.DataDir, "registry-index-"+strings.ToLower(s.Cfg.Registry.Hex()[2:10])+".json")
		}
		s.reg.ix = registry.OpenIndex(path, s.Cfg.Registry, s.Cfg.RegistryFromBlock)
	}
	ix := s.reg.ix
	fresh := time.Since(s.reg.lastSync) < maxAge
	s.reg.mu.Unlock()
	if fresh {
		return ix, nil
	}
	err := ix.Sync(ctx, c)
	s.reg.mu.Lock()
	if err == nil {
		s.reg.lastSync = time.Now()
		s.reg.syncErr = ""
	} else {
		s.reg.syncErr = err.Error()
	}
	s.reg.mu.Unlock()
	if err != nil && ix.Last() == 0 {
		return nil, err
	}
	return ix, nil // a stale index still searches; the error is shown
}

// GET /v1/registry/search?q=&tag=&all=1
func (s *Server) registrySearch(w http.ResponseWriter, r *http.Request) {
	if !s.regEnabled() {
		writeErr(w, 404, "no registry is configured for this network")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	ix, err := s.regIndex(ctx, 15*time.Second)
	if err != nil {
		writeErr(w, 502, "couldn't read the registry: "+err.Error())
		return
	}
	q := r.URL.Query()
	res := ix.Search(q.Get("q"), q.Get("tag"), q.Get("all") != "")
	type row struct {
		registry.Entry
		BondETH    string `json:"bondEth"`
		StatusText string `json:"statusText"`
		URL        string `json:"url"`
		TxURL      string `json:"txUrl,omitempty"`
	}
	out := make([]row, 0, len(res))
	for _, e := range res {
		rw := row{Entry: e, BondETH: registry.FormatWei(e.BondWei()), StatusText: registry.StatusName(e.Status), URL: "http://" + e.Sesh}
		if s.explorer() != "" {
			rw.TxURL = s.explorer() + "/tx/" + e.Tx.Hex()
		}
		out = append(out, rw)
	}
	s.reg.mu.Lock()
	syncErr := s.reg.syncErr
	s.reg.mu.Unlock()
	tags := ix.Tags()
	tagList := make([]string, 0, len(tags))
	for t := range tags {
		tagList = append(tagList, t)
	}
	sort.Strings(tagList)
	writeJSON(w, 200, map[string]any{
		"registry": s.Cfg.Registry.Hex(), "registryUrl": s.explorer() + "/address/" + s.Cfg.Registry.Hex(),
		"listings": out, "tags": tags, "tagList": tagList, "lastBlock": ix.Last(), "syncError": syncErr,
	})
}

// makerQuote is one maker's answer on the quote screen.
type makerQuote struct {
	ListingID uint64      `json:"listingId,omitempty"`
	Sesh      string      `json:"sesh,omitempty"`
	URL       string      `json:"url"`
	Source    string      `json:"source"` // "registry" or "configured"
	BondETH   string      `json:"bondEth,omitempty"`
	MaxETH    string      `json:"maxEth,omitempty"`
	OK        bool        `json:"ok"`
	Reason    string      `json:"reason,omitempty"`
	XMRPico   uint64      `json:"xmrPico,omitempty"`
	XMR       string      `json:"xmr,omitempty"`
	OffBps    int64       `json:"offBps,omitempty"`
	Ms        int64       `json:"ms"`
	Best      bool        `json:"best,omitempty"`
	terms     *api.Terms  `json:"-"`
	offer     *api.Offer  `json:"-"`
	seen      *api.Offer  `json:"-"` // the maker's offer, also when its quote failed
	outRange  bool        `json:"-"` // refused only because the amount is outside its offer
	owner     eth.Address `json:"-"`
}

// rangeReason says, in plain words, that val is outside the maker's offer
// range; "" when it isn't (or the offer is unknown).
func rangeReason(o *api.Offer, val *big.Int) string {
	if o == nil || o.Min == nil || o.Max == nil || (val.Cmp(o.Min) >= 0 && val.Cmp(o.Max) <= 0) {
		return ""
	}
	unit, dec := "ETH", 18
	if !o.Asset.IsZero() {
		unit, dec = "USDC", 6
	}
	return "this maker takes " + formatUnits(o.Min, dec) + " to " + formatUnits(o.Max, dec) + " " + unit + " per swap"
}

// rangeSummary is the line under the quote table when every maker refused
// only because of the amount: the combined range they take, e.g. "Makers
// here take 0.0001 to 0.005 ETH per swap." "" if any maker failed for
// another reason (or there were none).
func rangeSummary(qs []*makerQuote) string {
	var lo, hi *big.Int
	var asset *eth.Address
	for _, q := range qs {
		if q == nil || !q.outRange || q.seen == nil {
			return ""
		}
		o := q.seen
		if asset != nil && *asset != o.Asset {
			return "" // mixed ETH and USDC offers: no single range
		}
		a := o.Asset
		asset = &a
		if lo == nil || o.Min.Cmp(lo) < 0 {
			lo = o.Min
		}
		if hi == nil || o.Max.Cmp(hi) > 0 {
			hi = o.Max
		}
	}
	if asset == nil {
		return ""
	}
	unit, dec := "ETH", 18
	if !asset.IsZero() {
		unit, dec = "USDC", 6
	}
	who := "Makers here take "
	if len(qs) == 1 {
		who = "This maker takes "
	}
	return who + formatUnits(lo, dec) + " to " + formatUnits(hi, dec) + " " + unit + " per swap."
}

// capMax shows the most this maker will actually take: the smaller of its
// own offer max (when it told us, for an ETH offer) and the on-chain bond
// limit. A USDC offer's max is in USDC, so only the bond limit is shown.
func (mq *makerQuote) capMax(bondLimit *big.Int) {
	o := mq.seen
	if o == nil || !o.Asset.IsZero() || o.Max == nil || o.Max.Sign() <= 0 {
		return
	}
	if bondLimit == nil || o.Max.Cmp(bondLimit) < 0 {
		mq.MaxETH = registry.FormatWei(o.Max)
	}
}

// askRegistryMakers asks every bonded maker for a quote, in parallel, and
// checks each one against the bond cap and the price feed.
func (s *Server) askRegistryMakers(ctx context.Context, wantUSDC bool, val *big.Int) ([]*makerQuote, error) {
	ix, err := s.regIndex(ctx, 15*time.Second)
	if err != nil {
		return nil, err
	}
	makers := ix.Makers()
	rc := &registry.Client{C: s.regClient(), Addr: s.Cfg.Registry}
	asset := eth.Address{}
	if wantUSDC {
		asset = s.Cfg.USDC
	}
	ethVal := val
	if wantUSDC {
		if ethVal, err = rc.EthValue(ctx, asset, val); err != nil {
			return nil, fmt.Errorf("registry can't value USDC right now: %w", err)
		}
	}
	out := make([]*makerQuote, len(makers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, m := range makers {
		mq := &makerQuote{ListingID: m.ID, Sesh: m.Sesh, URL: s.seshURL(m.Sesh), Source: "registry"}
		out[i] = mq
		wg.Add(1)
		go func(m registry.Entry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			defer func() { mq.Ms = time.Since(t0).Milliseconds() }()
			info, err := rc.Info(ctx, m.ID)
			if err != nil {
				mq.Reason = "couldn't read its bond: " + err.Error()
				return
			}
			mq.owner = info.Owner
			mq.BondETH, mq.MaxETH = registry.FormatWei(info.Bond), registry.FormatWei(info.MaxQuote)
			if info.Status != registry.StatusActive || info.MaxQuote.Sign() == 0 {
				mq.Reason = "listing is not active or its bond is below the minimum"
				return
			}
			if ethVal.Cmp(info.MaxQuote) > 0 {
				var o api.Offer
				if getJSON(ctx, s.http, strings.TrimRight(mq.URL, "/")+"/v1/offer", &o) == nil {
					mq.seen = &o
					mq.capMax(info.MaxQuote)
				}
				mq.Reason = "amount is over this maker's bond cap (" + registry.FormatWei(info.MaxQuote) + " ETH)"
				if r := rangeReason(mq.seen, val); r != "" && !wantUSDC {
					mq.Reason, mq.outRange = r, true
				}
				return
			}
			s.fillQuote(ctx, mq, wantUSDC, val)
			mq.capMax(info.MaxQuote)
			if mq.terms != nil && mq.OK {
				if mq.terms.Registry == nil || *mq.terms.Registry != s.Cfg.Registry || mq.terms.ListingID != m.ID {
					mq.OK, mq.Reason = false, "the maker at this address says it is a different listing"
				}
			}
		}(m)
	}
	wg.Wait()
	s.noteAssets(out)
	return out, nil
}

func assetReason(wantUSDC bool) string {
	if wantUSDC {
		return "takes ETH only, not USDC"
	}
	return "takes USDC only, not ETH"
}

// noteAssets remembers which assets the listed makers take, from the offers
// seen while quoting, so the swap screen can stop offering one nobody takes.
func (s *Server) noteAssets(qs []*makerQuote) {
	seen := map[string]bool{}
	any := false
	for _, q := range qs {
		if q.seen == nil {
			continue
		}
		any = true
		if q.seen.Asset.IsZero() {
			seen["eth"] = true
		} else {
			seen["usdc"] = true
		}
	}
	if !any {
		return
	}
	s.mu.Lock()
	s.reg.assets = map[string]bool{"eth": seen["eth"], "usdc": seen["usdc"]}
	s.mu.Unlock()
}

// noMakerTakes is true when the makers' offers are known and none takes the asset.
func (s *Server) noMakerTakes(wantUSDC bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg.assets == nil {
		return false
	}
	if wantUSDC {
		return !s.reg.assets["usdc"]
	}
	return !s.reg.assets["eth"]
}

// fillQuote asks one maker and runs the asset and fair-price checks.
func (s *Server) fillQuote(ctx context.Context, mq *makerQuote, wantUSDC bool, val *big.Int) {
	var terms api.Terms
	base := strings.TrimRight(mq.URL, "/")
	if err := getJSON(ctx, s.http, base+"/v1/quote?value="+val.String(), &terms); err != nil {
		mq.Reason = "no answer: " + friendlyNetErr(err)
		// A maker refuses amounts outside its offer; say so in plain words.
		var o api.Offer
		if getJSON(ctx, s.http, base+"/v1/offer", &o) == nil {
			mq.seen = &o
		}
		if mq.seen != nil && wantUSDC != !mq.seen.Asset.IsZero() {
			mq.Reason = assetReason(wantUSDC)
			return
		}
		if r := rangeReason(mq.seen, val); r != "" {
			mq.Reason, mq.outRange = r, true
		}
		return
	}
	var offer api.Offer
	if err := getJSON(ctx, s.http, base+"/v1/offer", &offer); err != nil {
		mq.Reason = "no offer: " + friendlyNetErr(err)
		return
	}
	mq.terms, mq.offer, mq.seen = &terms, &offer, &offer
	if wantUSDC != !offer.Asset.IsZero() {
		mq.Reason = assetReason(wantUSDC)
		return
	}
	if offer.Contract != s.Cfg.Contract {
		mq.Reason = "uses a different swap contract"
		return
	}
	if terms.Value == nil || terms.Value.Cmp(val) != 0 || terms.XMR == 0 {
		mq.Reason = "quoted a different amount"
		return
	}
	mq.XMRPico, mq.XMR = terms.XMR, picoToXMR(terms.XMR)
	// The same timeout and confirmation checks the swap itself runs, so a
	// quote the wallet would refuse at confirm time is never offered.
	s.mu.Lock()
	eng := s.eng
	s.mu.Unlock()
	if eng != nil {
		var te api.TermsError
		if _, err := api.CheckTerms(ctx, eng, takeLimits(eng.Policy, val), terms.D1, terms.D2, terms.MinConfs, terms.XMR, terms.Grace, terms.Premium); errors.As(err, &te) {
			mq.Reason = "its swap terms don't meet this wallet's safety limits: " + err.Error()
			return
		}
	}
	asset := "ETH"
	if wantUSDC {
		asset = "USDC"
	}
	chk, err := s.fairPrice(ctx, asset, terms.Value, terms.XMR)
	if err == nil && chk != nil {
		mq.OffBps = chk.OffBps
		if chk.Warn && chk.Implied > chk.Fair {
			// The maker charges more than the feed allows.
			mq.Reason = fmt.Sprintf("price is %.1f%% worse than the price feed", float64(chk.OffBps)/100)
			return
		}
	}
	mq.OK = true
}

func pickBest(qs []*makerQuote) *makerQuote {
	var best *makerQuote
	for _, q := range qs {
		if q.OK && (best == nil || q.XMRPico > best.XMRPico) {
			best = q
		}
	}
	if best != nil {
		best.Best = true
	}
	return best
}

// registryMakerForSwap re-reads a chosen listing from the chain right
// before the swap.
func (s *Server) registryMakerForSwap(ctx context.Context, id uint64) (*registry.Entry, *registry.Info, error) {
	ix, err := s.regIndex(ctx, 30*time.Second)
	if err != nil {
		return nil, nil, err
	}
	e, ok := ix.Get(id)
	if !ok || e.Tag != registry.TagMaker {
		return nil, nil, errors.New("that maker isn't in the registry")
	}
	rc := &registry.Client{C: s.regClient(), Addr: s.Cfg.Registry}
	info, err := rc.Info(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if info.Status != registry.StatusActive || info.MaxQuote.Sign() == 0 {
		return nil, nil, errors.New("that maker's listing is no longer active")
	}
	return &e, info, nil
}

// ---------------------------------------------------------------- after the lock

// regLoop records each maker commitment on-chain right after our ETH lock,
// and slashes the maker's penalty to us if it never acks in time. Swap
// safety does not depend on it: the refund path is the same either way.
func (s *Server) regLoop(ctx context.Context, eng *swap.Engine, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, id := range eng.IDs() {
			rec, ok := eng.Snapshot(id)
			if !ok || rec.Registry == nil || rec.Role != swap.Taker {
				continue
			}
			l := rec.Registry
			if l.Acked || l.Released || l.Slashed || l.Skipped {
				continue
			}
			s.regStep(ctx, eng, &rec)
		}
	}
}

func (s *Server) regStep(ctx context.Context, eng *swap.Engine, rec *swap.Record) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	l := rec.Registry
	rc := &registry.Client{C: eng.K.C, Addr: l.Registry}
	upd := func(f func(l *swap.RegLink)) {
		_ = eng.Update(rec.ID, func(x *swap.Record) error {
			if x.Registry != nil {
				f(x.Registry)
			}
			return nil
		})
	}
	if rec.Swap == nil || rec.SwapID == (eth.Hash{}) {
		return // not locked yet
	}
	cs, err := rc.Commit(ctx, rec.SwapID)
	if err != nil {
		return
	}
	switch cs.Stage {
	case registry.CommitAcked:
		upd(func(l *swap.RegLink) { l.Committed, l.Acked, l.AttestBy = true, true, cs.AttestBy })
		eng.Note(rec.ID, "the maker acknowledged its XMR lock in the registry")
		return
	case registry.CommitReleased:
		upd(func(l *swap.RegLink) { l.Committed, l.Released = true, true })
		return
	case registry.CommitSlashed:
		upd(func(l *swap.RegLink) { l.Committed, l.Slashed = true, true })
		return
	case registry.CommitOpen:
		if !l.Committed {
			upd(func(l *swap.RegLink) { l.Committed, l.AttestBy = true, cs.AttestBy })
		}
		h, err := eng.K.C.Head(ctx)
		if err != nil || h.Timestamp < cs.AttestBy {
			return
		}
		st, err := eng.K.State(ctx, rec.SwapID)
		if err != nil || st.Stage == eth.StatusReady || st.Stage == eth.StatusClaimed {
			return // we saw the XMR or the swap finished: nothing to slash
		}
		if st.Stage == eth.StatusRefunded {
			// The registry refuses a slash once the swap is refunded (its refund
			// time is not on-chain), so stop trying.
			upd(func(l *swap.RegLink) { l.Skipped, l.LastError = true, "refunded before the penalty was claimed" })
			return
		}
		reg := l.Registry
		r, err := eng.K.C.SendAndWait(ctx, eng.EthKey, &reg, nil, registry.EncodeSlash(rec.SwapID))
		if err != nil {
			upd(func(l *swap.RegLink) { l.Tries++; l.LastError = err.Error() })
			return
		}
		upd(func(l *swap.RegLink) { l.Slashed, l.SlashTx = true, r.TxHash })
		eng.Note(rec.ID, "the maker never acknowledged in time: its registry penalty was paid to you (tx "+r.TxHash.Hex()+")")
		return
	}
	// CommitNone: record it, unless it is too late or the swap moved on.
	var sc registry.SignedCommitment
	if err := json.Unmarshal(l.Signed, &sc); err != nil {
		upd(func(l *swap.RegLink) { l.Skipped, l.LastError = true, "bad stored commitment" })
		return
	}
	h, err := eng.K.C.Head(ctx)
	if err != nil {
		return
	}
	st, err := eng.K.State(ctx, rec.SwapID)
	if err != nil {
		return
	}
	if h.Timestamp > sc.Commitment.Expiry || st.Stage != eth.StatusPending {
		upd(func(l *swap.RegLink) { l.Skipped, l.LastError = true, "too late to record the maker's commitment" })
		return
	}
	if err := sc.CheckAgainstSwap(rec.Swap); err != nil {
		upd(func(l *swap.RegLink) { l.Skipped, l.LastError = true, err.Error() })
		return
	}
	reg := l.Registry
	r, err := eng.K.C.SendAndWait(ctx, eng.EthKey, &reg, nil, registry.EncodeCommit(rec.Swap, &sc))
	if err != nil {
		upd(func(l *swap.RegLink) { l.Tries++; l.LastError = err.Error() })
		if l.Tries >= 5 {
			upd(func(l *swap.RegLink) { l.Skipped = true })
		}
		return
	}
	cs2, _ := rc.Commit(ctx, rec.SwapID)
	upd(func(l *swap.RegLink) {
		l.Committed, l.CommitTx = true, r.TxHash
		if cs2 != nil {
			l.AttestBy = cs2.AttestBy
		}
	})
	eng.Note(rec.ID, "recorded the maker's bonded commitment in the registry (tx "+r.TxHash.Hex()+")")
}

// regView is the swap screen's registry line.
func regView(r swap.Record, explorer string) map[string]any {
	l := r.Registry
	if l == nil {
		return nil
	}
	out := map[string]any{"listingId": l.ListingID, "sesh": l.Sesh, "committed": l.Committed, "acked": l.Acked,
		"slashed": l.Slashed, "released": l.Released}
	tx := func(k string, h eth.Hash) {
		if h != (eth.Hash{}) {
			out[k] = h.Hex()
			if explorer != "" {
				out[k+"Url"] = explorer + "/tx/" + h.Hex()
			}
		}
	}
	tx("commitTx", l.CommitTx)
	tx("ackTx", l.AckTx)
	tx("slashTx", l.SlashTx)
	switch {
	case l.Slashed:
		out["text"] = "The maker didn't acknowledge in time; its penalty was paid to you."
	case l.Acked:
		out["text"] = "Bonded maker #" + strconv.FormatUint(l.ListingID, 10) + " acknowledged its XMR lock on-chain."
	case l.Committed:
		out["text"] = "Bonded maker #" + strconv.FormatUint(l.ListingID, 10) + " has until " + formatCT(int64(l.AttestBy)) + " to acknowledge, or its penalty goes to you."
	case l.Skipped:
		out["text"] = "The maker's commitment wasn't recorded (" + l.LastError + "). The swap itself is unaffected."
	default:
		out["text"] = "Bonded maker #" + strconv.FormatUint(l.ListingID, 10) + ". Its commitment is recorded right after your ETH lock."
	}
	return out
}

// takeLimits are the taker's own limits for a swap of val, used both when
// checking quotes and when starting a swap.
func takeLimits(pol swap.Policy, val *big.Int) api.TakeParams {
	return api.TakeParams{Value: val, MinD1: 2*pol.Margin + 1800, MinD2: 2 * pol.Margin,
		MaxConfs: 60, MinConfs: swap.XmrConfFloor}
}

// swapAssets reads each listed maker's offer (no quote) and says which assets
// can be swapped right now: {"eth": true, "usdc": false}.
func (s *Server) swapAssets(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	if !s.regEnabled() || !isPrivate(s.curMode()) {
		writeJSON(w, 200, map[string]any{"known": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	ix, err := s.regIndex(ctx, 15*time.Second)
	if err != nil {
		writeJSON(w, 200, map[string]any{"known": false, "error": err.Error()})
		return
	}
	makers := ix.Makers()
	qs := make([]*makerQuote, len(makers))
	var wg sync.WaitGroup
	for i, m := range makers {
		qs[i] = &makerQuote{URL: s.seshURL(m.Sesh)}
		wg.Add(1)
		go func(mq *makerQuote) {
			defer wg.Done()
			var o api.Offer
			if getJSON(ctx, s.http, strings.TrimRight(mq.URL, "/")+"/v1/offer", &o) == nil && o.Contract == s.Cfg.Contract {
				mq.seen = &o
			}
		}(qs[i])
	}
	wg.Wait()
	s.noteAssets(qs)
	s.mu.Lock()
	a := s.reg.assets
	s.mu.Unlock()
	if a == nil {
		writeJSON(w, 200, map[string]any{"known": false})
		return
	}
	writeJSON(w, 200, map[string]any{"known": true, "eth": a["eth"], "usdc": a["usdc"]})
}
