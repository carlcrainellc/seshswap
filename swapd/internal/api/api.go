// Package api is the maker's HTTP+JSON interface (meant to be served as a Tor
// onion service or at a Session Router .sesh address)
// and the taker's client for it. Only swap setup goes over
// this channel; after the taker's lock everything is read from the chains.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/carlcrainellc/seshswap/swapd/internal/exitpay"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// HTTP is used by Take (when TakeParams.HTTP is nil) and by NotifyLocked.
// swapd sets this at start so .sesh maker URLs go through Session Router DNS.
// If it is nil, sesh.DefaultClient is used.
var HTTP *http.Client

func httpClient(timeout time.Duration) *http.Client {
	if HTTP != nil {
		return HTTP
	}
	return sesh.DefaultClient()
}

var one18 = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// Offer is what a maker advertises.
type Offer struct {
	ChainID     *big.Int     `json:"chainId"`
	Contract    eth.Address  `json:"contract"`
	Asset       eth.Address  `json:"asset"`       // 0x0 = native coin
	PicoPerUnit *big.Int     `json:"picoPerUnit"` // piconero paid per 1e18 base units of asset
	Min         *big.Int     `json:"min"`
	Max         *big.Int     `json:"max"`
	D1          uint64       `json:"d1"`
	D2          uint64       `json:"d2"`
	MinConfs    uint64       `json:"minConfs"`
	XmrNet      keys.Network `json:"xmrNet"`
	RelayFee    *big.Int     `json:"relayFee"`   // deducted from the asset amount by the relayer
	TTL         int64        `json:"ttl"`        // seconds the taker has to lock (quote expiry)
	Grace       uint64       `json:"grace"`      // intent freeze; 0 = from policy
	PremiumBps  uint64       `json:"premiumBps"` // taker premium (native-coin swaps), forfeited on a cancel after our ack
	WaiveAfter  int          `json:"waiveAfter"` // waive the premium after this many completed swaps with the same taker (0 = never)
	SpreadBps   uint64       `json:"spreadBps"`  // with a price oracle: maker's spread below the mid price
	Direction   string       `json:"direction"`  // "eth2xmr" (only direction built so far)
	Expires     int64        `json:"expires,omitempty"`
}

func (o *Offer) Quote(value *big.Int) uint64 {
	x := new(big.Int).Mul(value, o.PicoPerUnit)
	return x.Div(x, one18).Uint64()
}

type SwapRequest struct {
	Session   string      `json:"session"` // 32 hex chars chosen by the taker
	Asset     eth.Address `json:"asset"`
	Value     *big.Int    `json:"value"`
	Owner     eth.Address `json:"owner"`
	SpendPub  string      `json:"spendPub"`
	ViewShare string      `json:"viewShare"`
	Proof     keys.Proof  `json:"proof"`
}

type SwapResponse struct {
	ID            string      `json:"id"`
	SpendPub      string      `json:"spendPub"`
	ViewShare     string      `json:"viewShare"`
	Proof         keys.Proof  `json:"proof"`
	Claimer       eth.Address `json:"claimer"`
	XMR           uint64      `json:"xmr"`
	D1            uint64      `json:"d1"`
	D2            uint64      `json:"d2"`
	MinConfs      uint64      `json:"minConfs"`
	RestoreHeight uint64      `json:"restoreHeight"`
	StartBlock    uint64      `json:"startBlock"`
	Expires       int64       `json:"expires"` // unix: the taker must not lock after this
	Grace         uint64      `json:"grace"`
	Premium       *big.Int    `json:"premium"`
	MakerID       string      `json:"makerId,omitempty"`
	Sig           string      `json:"sig,omitempty"` // maker identity signature over all terms
	// Registry is the maker's signed SeshRegistry commitment (listed makers only).
	Registry *registry.SignedCommitment `json:"registry,omitempty"`
}

func pokCtx(session, role string) []byte { return []byte("xmrswap-v3|" + session + "|" + role) }

var sessionRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ---------------------------------------------------------------- maker server

type Maker struct {
	Offer    Offer
	Engine   *swap.Engine
	PayoutTo eth.Address // where claimed ETH/tokens go
	// ExitPay, if set, makes every claim buy exit time instead: each swap gets
	// its own forwarder (Ref and Address are filled in per swap).
	ExitPay  *exitpay.Target
	Registry *MakerRegistry // optional: bonded SeshRegistry listing
	Service                 // optional always-on service features (price, limits, identity)
}

func (m *Maker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.count("http_" + strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)[0])
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/offer":
		o, err := m.currentOffer()
		if err != nil {
			writeJSON(w, 503, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, o)
	case r.Method == "GET" && r.URL.Path == "/v1/offer.signed":
		d, err := m.OfferDoc()
		if err != nil {
			writeJSON(w, 503, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, d)
	case r.Method == "GET" && r.URL.Path == "/v1/quote":
		v, ok := new(big.Int).SetString(r.URL.Query().Get("value"), 10)
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "value (base units) required"})
			return
		}
		q, err := m.quote(r.Context(), v, eth.Address{})
		if err != nil {
			writeJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, q)
	case r.Method == "GET" && r.URL.Path == "/healthz":
		m.healthz(w, r)
	case r.Method == "GET" && r.URL.Path == "/metrics":
		m.metrics(w, r)
	case r.Method == "POST" && r.URL.Path == "/v1/swaps":
		var req SwapRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		resp, err := m.newSwap(r.Context(), &req)
		if err != nil {
			writeJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, resp)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/swaps/") && strings.HasSuffix(r.URL.Path, "/locked"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/swaps/"), "/locked")
		var body struct {
			TxHash eth.Hash `json:"txHash"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		err := m.Engine.Update(id, func(rec *swap.Record) error {
			if rec.Phase != swap.MNegotiated {
				return nil
			}
			rec.LockHint = body.TxHash
			return nil
		})
		if err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"ok": "true"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/swaps/"):
		rec, ok := m.Engine.Snapshot(strings.TrimPrefix(r.URL.Path, "/v1/swaps/"))
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "unknown swap"})
			return
		}
		writeJSON(w, 200, map[string]any{"phase": string(rec.Phase),
			"xmrLockHeight": rec.XmrLockHeight, "spendableAtHeight": swap.SpendableAt(rec.XmrLockHeight, 0)})
	default:
		http.Error(w, "not found", 404)
	}
}

func (m *Maker) newSwap(ctx context.Context, q *SwapRequest) (*SwapResponse, error) {
	o := &m.Offer
	if !sessionRe.MatchString(q.Session) {
		return nil, errors.New("bad session id")
	}
	if q.Asset != o.Asset || q.Value == nil || q.Value.Cmp(o.Min) < 0 || q.Value.Cmp(o.Max) > 0 {
		return nil, errors.New("asset or amount outside the offer")
	}
	if q.Owner.IsZero() {
		return nil, errors.New("missing owner")
	}
	theirSpend, err := keys.PointFromHex(q.SpendPub)
	if err != nil {
		return nil, fmt.Errorf("spend key: %w", err)
	}
	if err := keys.Verify(theirSpend, pokCtx(q.Session, "taker"), q.Proof); err != nil {
		return nil, err
	}
	theirView, err := keys.ScalarFromHex(q.ViewShare)
	if err != nil {
		return nil, fmt.Errorf("view share: %w", err)
	}
	terms, err := m.quote(ctx, q.Value, q.Owner)
	if err != nil {
		m.count("quote_rejected")
		return nil, err
	}
	xmrAmt := terms.XMR
	// liquidity: unlocked balance must cover this and every swap we may still have to fund
	var unlocked, height uint64
	if err := m.Engine.Wallet.Session(ctx, m.Engine.MainWal, func(s *xmr.Sess) error {
		if err := s.Refresh(ctx); err != nil {
			return err
		}
		_, u, err := s.Balance(ctx)
		unlocked = u
		if err != nil {
			return err
		}
		height, err = s.Height(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	if unlocked < xmrAmt+m.Engine.Reserved() {
		m.count("quote_rejected")
		return nil, errors.New("maker does not have enough unlocked XMR right now")
	}
	if err := m.checkExposure(xmrAmt); err != nil {
		m.count("quote_rejected")
		return nil, err
	}
	if m.Registry != nil {
		if _, _, err := m.Registry.check(ctx, o.Asset, q.Value); err != nil {
			m.count("quote_rejected")
			return nil, err
		}
	}
	head, err := m.Engine.K.C.Head(ctx)
	if err != nil {
		return nil, err
	}
	mySpend, err := keys.RandomScalar()
	if err != nil {
		return nil, err
	}
	myView, err := keys.RandomScalar()
	if err != nil {
		return nil, err
	}
	claimer, err := eth.NewKey() // fresh, never-funded claimer address per swap
	if err != nil {
		return nil, err
	}
	sh, err := keys.SharedAddress(o.XmrNet, theirSpend, mySpend.Public(), theirView, myView)
	if err != nil {
		return nil, err
	}
	proof, err := keys.Prove(mySpend, pokCtx(q.Session, "maker"))
	if err != nil {
		return nil, err
	}
	rh := uint64(0)
	if height > 10 {
		rh = height - 10
	}
	payTo := m.PayoutTo
	var ep *exitpay.Target
	if m.ExitPay != nil {
		t := *m.ExitPay
		if t.Asset != o.Asset {
			return nil, errors.New("exit pay is set up for a different asset")
		}
		t.Ref = exitpay.Ref(q.Session)
		if err := t.Resolve(ctx, m.Engine.K.C); err != nil {
			return nil, err
		}
		ep, payTo = &t, t.Address
	}
	rec := &swap.Record{
		ID: q.Session, Role: swap.Maker, Phase: swap.MNegotiated,
		Created: time.Now().Unix(), Expires: time.Now().Unix() + o.TTL,
		Contract: o.Contract, ChainID: o.ChainID, Asset: o.Asset, Value: q.Value, XMR: xmrAmt,
		D1: terms.D1, D2: terms.D2, MinConfs: terms.MinConfs, XmrNet: o.XmrNet, BlockSecs: terms.BlockSecs,
		Grace: terms.Grace, Premium: terms.Premium, EthConfs: m.Engine.Policy.EthConfs, Direction: "eth2xmr",
		MySpend: mySpend.Hex(), MyView: myView.Hex(), ClaimerKey: claimer.Hex(),
		TheirSpend: theirSpend.Hex(), TheirView: theirView.Hex(), Owner: q.Owner,
		Claimer: claimer.Address(), PayoutTo: payTo, ExitPay: ep, SharedAddr: sh.Address,
		RestoreHeight: rh, StartBlock: head.Number,
	}
	if err := m.Engine.Add(rec); err != nil { // secrets on disk before we answer
		return nil, err
	}
	resp := &SwapResponse{ID: rec.ID, SpendPub: mySpend.Public().Hex(), ViewShare: myView.Hex(), Proof: proof,
		Claimer: rec.Claimer, XMR: xmrAmt, D1: rec.D1, D2: rec.D2, MinConfs: rec.MinConfs,
		RestoreHeight: rh, StartBlock: head.Number, Expires: rec.Expires, Grace: rec.Grace, Premium: terms.Premium}
	if err := m.attachRegistry(ctx, rec, mySpend, resp); err != nil {
		_ = m.Engine.Update(rec.ID, func(x *swap.Record) error { x.Phase = swap.MAborted; return nil })
		return nil, err
	}
	if m.Identity != nil {
		m.Identity.signQuote(resp, q.Value, q.Owner)
	}
	m.count("swaps_accepted")
	return resp, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- taker client

// TakeParams are the taker's own limits; the maker's offer is never trusted for them.
type TakeParams struct {
	MakerURL      string
	Value         *big.Int
	MinXMR        uint64 // refuse quotes below this
	MinD1         uint64
	MinD2         uint64
	MaxConfs      uint64 // refuse offers asking us to wait for more confirmations than this
	MinConfs      uint64 // our own minimum XMR confirmations before marking ready (maker can't lower it)
	MaxGrace      uint64 // refuse an intent freeze longer than this (default 12 h)
	MaxPremiumBps uint64 // refuse a premium above this share of the value (default 100 = 1%)
	MakerID       string // if set (pinned from a signed offer), the quote must be signed by this maker
	HTTP          *http.Client

	// SeshRegistry listing the maker was found in (all from the chain, not
	// from the maker). With Registry set, the maker's signed commitment must
	// verify against ListingOwner; it is stored on the swap so the taker can
	// record it on-chain after the lock.
	Registry     eth.Address
	ListingID    uint64
	ListingOwner eth.Address
	Sesh         string
}

func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(ctx context.Context, c *http.Client, url string, in, out any) error {
	b, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("POST %s: %s %s", url, resp.Status, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Take negotiates a swap with a maker and hands it to the taker engine.
func Take(ctx context.Context, e *swap.Engine, p TakeParams) (*swap.Record, error) {
	c := p.HTTP
	if c == nil {
		c = httpClient(60 * time.Second)
	}
	var o Offer
	if err := getJSON(ctx, c, p.MakerURL+"/v1/offer", &o); err != nil {
		return nil, err
	}
	if o.Contract != e.K.Addr || o.ChainID.Cmp(e.K.ChainID) != 0 {
		return nil, errors.New("maker uses a different contract or chain than ours")
	}
	if o.XmrNet != e.XmrNet {
		return nil, errors.New("maker is on a different Monero network")
	}
	sb := make([]byte, 16)
	if _, err := rand.Read(sb); err != nil {
		return nil, err
	}
	session := hex.EncodeToString(sb)
	mySpend, err := keys.RandomScalar()
	if err != nil {
		return nil, err
	}
	myView, err := keys.RandomScalar()
	if err != nil {
		return nil, err
	}
	proof, err := keys.Prove(mySpend, pokCtx(session, "taker"))
	if err != nil {
		return nil, err
	}
	var salt eth.Hash
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, err
	}
	req := SwapRequest{Session: session, Asset: o.Asset, Value: p.Value, Owner: e.EthKey.Address(),
		SpendPub: mySpend.Public().Hex(), ViewShare: myView.Hex(), Proof: proof}
	var resp SwapResponse
	if err := postJSON(ctx, c, p.MakerURL+"/v1/swaps", req, &resp); err != nil {
		return nil, err
	}
	theirSpend, err := keys.PointFromHex(resp.SpendPub)
	if err != nil {
		return nil, fmt.Errorf("maker spend key: %w", err)
	}
	if err := keys.Verify(theirSpend, pokCtx(session, "maker"), resp.Proof); err != nil {
		return nil, fmt.Errorf("maker proof: %w", err)
	}
	theirView, err := keys.ScalarFromHex(resp.ViewShare)
	if err != nil {
		return nil, err
	}
	confs, err := CheckTerms(ctx, e, p, resp.D1, resp.D2, resp.MinConfs, resp.XMR, resp.Grace, resp.Premium)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.ID != session:
		return nil, errors.New("maker changed the session id")
	case resp.Claimer.IsZero():
		return nil, errors.New("no claimer address")
	case resp.XMR < p.MinXMR:
		return nil, fmt.Errorf("quote %d piconero is below our minimum %d", resp.XMR, p.MinXMR)
	case resp.Expires != 0 && time.Now().Unix() >= resp.Expires:
		return nil, errors.New("quote already expired")
	}
	if resp.Sig != "" || p.MakerID != "" {
		if err := verifyQuote(&resp, p.Value, e.EthKey.Address()); err != nil {
			return nil, err
		}
		if p.MakerID != "" && resp.MakerID != p.MakerID {
			return nil, errors.New("quote signed by a different maker than the one we pinned")
		}
	}
	sh, err := keys.SharedAddress(e.XmrNet, mySpend.Public(), theirSpend, myView, theirView)
	if err != nil {
		return nil, err
	}
	head, err := e.K.C.Head(ctx)
	if err != nil {
		return nil, err
	}
	// never trust the maker's wallet restore height: a too-high one would hide the XMR from us
	rh := resp.RestoreHeight
	if err := e.Wallet.Session(ctx, e.MainWal, func(s *xmr.Sess) error {
		h, err := s.Height(ctx)
		if err == nil && h > 10 && h-10 < rh {
			rh = h - 10
		}
		return err
	}); err != nil {
		return nil, err
	}
	var link *swap.RegLink
	if !p.Registry.IsZero() {
		if resp.Registry == nil {
			return nil, errors.New("this maker is listed in the registry but didn't send its signed commitment")
		}
		if err := VerifyCommitment(resp.Registry, e.K.ChainID, p.Registry, p.ListingOwner, p.ListingID,
			e.EthKey.Address(), resp.Claimer, theirSpend.Int(), o.Asset, p.Value); err != nil {
			return nil, err
		}
		if resp.Registry.Commitment.XMRAtomic != resp.XMR {
			return nil, errors.New("maker's commitment names a different XMR amount")
		}
		raw, _ := json.Marshal(resp.Registry)
		link = &swap.RegLink{Registry: p.Registry, ListingID: p.ListingID, Sesh: p.Sesh, Signed: raw}
	}
	rec := &swap.Record{
		ID: session, Role: swap.Taker, Phase: swap.TNegotiated, Created: time.Now().Unix(),
		Contract: e.K.Addr, ChainID: e.K.ChainID, Asset: o.Asset, Value: p.Value, XMR: resp.XMR,
		D1: resp.D1, D2: resp.D2, MinConfs: confs, XmrNet: e.XmrNet,
		MySpend: mySpend.Hex(), MyView: myView.Hex(), TheirSpend: theirSpend.Hex(), TheirView: theirView.Hex(),
		Owner: e.EthKey.Address(), Claimer: resp.Claimer, SharedAddr: sh.Address,
		RestoreHeight: rh, StartBlock: head.Number, Salt: salt, MakerURL: p.MakerURL,
		Grace: resp.Grace, Premium: resp.Premium, Expires: resp.Expires, Direction: "eth2xmr",
		Registry: link,
	}
	if err := e.Add(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// NotifyLocked is the taker's OnLocked hook: tell the maker which tx locked the funds.
func NotifyLocked(id, makerURL string, tx eth.Hash) {
	if makerURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = postJSON(ctx, httpClient(20*time.Second), makerURL+"/v1/swaps/"+id+"/locked",
		map[string]any{"txHash": tx}, nil)
}

// CheckTerms runs the taker's checks on a maker's timeouts, confirmations,
// intent freeze and premium. Take runs it on the maker's answer; the wallet
// also runs it on quotes, so a quote it would refuse is never shown as usable.
// It returns the Monero confirmations the swap will wait for.
func CheckTerms(ctx context.Context, e *swap.Engine, p TakeParams, d1, d2, minConfs, xmr, grace uint64, premium *big.Int) (uint64, error) {
	confs := max(minConfs, p.MinConfs, swap.XmrConfs(xmr, e.ReorgDepth(ctx)))
	bs, err := e.BlockSecs(ctx)
	if err != nil {
		return 0, err
	}
	maxGrace := p.MaxGrace
	if maxGrace == 0 {
		maxGrace = 12 * 3600
	}
	pbps := p.MaxPremiumBps
	if pbps == 0 {
		pbps = 100
	}
	maxPremium := new(big.Int).Div(new(big.Int).Mul(p.Value, new(big.Int).SetUint64(pbps)), big.NewInt(10000))
	minGrace := max(e.Policy.MinGrace, swap.ContractMinGrace)
	switch {
	case d1 < p.MinD1 || d2 < p.MinD2:
		return 0, TermsError{fmt.Sprintf("maker's timeouts are shorter than our minimums (%d s and %d s; we need %d s and %d s)", d1, d2, p.MinD1, p.MinD2)}
	case minConfs > p.MaxConfs:
		return 0, TermsError{"unacceptable confirmation count"}
	case d1 < 2*e.Policy.Margin+swap.ConfirmSecs(confs, bs)+300:
		return 0, TermsError{fmt.Sprintf("t1 too short for our safety margin and %d confirmations", confs)}
	case grace < minGrace || grace > maxGrace:
		return 0, TermsError{fmt.Sprintf("intent freeze %ds outside our range [%d, %d]", grace, minGrace, maxGrace)}
	case premium == nil || premium.Sign() < 0 || premium.Cmp(maxPremium) > 0:
		return 0, TermsError{fmt.Sprintf("premium %v above our limit %v", premium, maxPremium)}
	}
	return confs, nil
}

// TermsError is a maker's terms failing the taker's limits (as opposed to the
// check itself not being able to run).
type TermsError struct{ Msg string }

func (e TermsError) Error() string { return e.Msg }
