//go:build hostile

// Command hostile is a TEST-ONLY adversary for hostile-peer testnet runs. It is
// excluded from every normal build (build tag "hostile") and never shipped.
//
//	go build -tags hostile -o hostile ./cmd/hostile
//
//	hostile maker      -config F -mode ack-no-lock|short|wrong-addr|lock-silent
//	hostile taker      -config F -maker URL -value N -min-xmr P -mode vanish|cancel-lockin
//	hostile relayproxy -listen A -upstream URL -mode drop-claim|dup|delay|reorder|pass
//	hostile spam       -url URL -kind junk|quotes|slow -n N [-value N]
//
// Secrets come from the environment like swapd: SWAPD_ETH_KEY, SWAPD_PASSPHRASE.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/relay"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

type Config struct {
	EthRPC       string       `json:"ethRpc"`
	EthRPCs      []string     `json:"ethRpcs"`
	Contract     eth.Address  `json:"contract"`
	XmrWalletRPC string       `json:"xmrWalletRpc"`
	XmrDaemonRPC string       `json:"xmrDaemonRpc"`
	XmrNet       keys.Network `json:"xmrNet"`
	StoreDir     string       `json:"storeDir"`
	Listen       string       `json:"listen"`
	PayoutTo     eth.Address  `json:"payoutTo"`
	Policy       *swap.Policy `json:"policy"`
	Offer        *api.Offer   `json:"offer"`
	Events       string       `json:"events"` // JSON-lines log of every hostile action
	TakerMargin  uint64       `json:"takerMargin"`
	AckLead      uint64       `json:"ackLead"` // ack-no-lock: ack this many seconds before the taker's cancel point
}

var (
	cfg    Config
	evMu   sync.Mutex
	logf   = log.Printf
	poll   = 8 * time.Second
	ctxAll = context.Background()
)

func event(kind string, kv ...any) {
	m := map[string]any{"t": time.Now().Format(time.RFC3339), "kind": kind}
	for i := 0; i+1 < len(kv); i += 2 {
		m[fmt.Sprint(kv[i])] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	logf("EVENT %s", b)
	if cfg.Events == "" {
		return
	}
	evMu.Lock()
	defer evMu.Unlock()
	f, err := os.OpenFile(cfg.Events, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.Write(append(b, '\n'))
		f.Close()
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: hostile maker|taker|relayproxy|spam ...")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "", "config JSON")
	mode := fs.String("mode", "", "behaviour")
	makerURL := fs.String("maker", "", "taker: maker URL")
	value := fs.String("value", "", "taker/spam: value in base units")
	minXmr := fs.Uint64("min-xmr", 1, "taker: min piconero")
	listen := fs.String("listen", "", "relayproxy: listen address")
	upstream := fs.String("upstream", "", "relayproxy: honest relayer URL")
	delay := fs.Duration("delay", 3*time.Minute, "relayproxy delay/reorder hold")
	url := fs.String("url", "", "spam: target base URL")
	kind := fs.String("kind", "junk", "spam: junk|quotes|slow")
	n := fs.Int("n", 100, "spam: requests / connections")
	asset := fs.String("asset", "", "spam quotes: asset address")
	leakFile := fs.String("leak", "", "relayproxy leak-claim / taker collude-leak: file the leaked claim is written to / read from")
	_ = fs.Parse(os.Args[2:])
	if *cfgPath != "" {
		b, err := os.ReadFile(*cfgPath)
		check(err)
		check(json.Unmarshal(b, &cfg))
	}
	switch cmd {
	case "maker":
		runMaker(*mode)
	case "taker":
		runTaker(*mode, *makerURL, *value, *minXmr, *leakFile)
	case "relayproxy":
		runProxy(*listen, *upstream, *mode, *delay, *leakFile)
	case "spam":
		runSpam(*url, *kind, *n, *value, *asset)
	default:
		log.Fatalf("unknown command %q", cmd)
	}
}

func engine(role swap.Role) (*swap.Engine, *eth.Contract) {
	c := eth.NewClients(append([]string{cfg.EthRPC}, cfg.EthRPCs...)...)
	cid, err := c.ChainID(ctxAll)
	check(err)
	k := &eth.Contract{C: c, Addr: cfg.Contract, ChainID: cid}
	var key *eth.Key
	if h := os.Getenv("SWAPD_ETH_KEY"); h != "" {
		key, err = eth.KeyFromHex(h)
		check(err)
	}
	pass := os.Getenv("SWAPD_PASSPHRASE")
	st, err := store.Open(cfg.StoreDir, pass)
	check(err)
	pol := swap.DefaultPolicy()
	if cfg.Policy != nil {
		pol = *cfg.Policy
	}
	e := &swap.Engine{Role: role, Store: st, K: k, EthKey: key, Wallet: xmr.NewWallet(cfg.XmrWalletRPC, pass),
		MainWal: "main", XmrNet: cfg.XmrNet, Policy: pol, Logf: log.Printf}
	if cfg.XmrDaemonRPC != "" {
		e.Daemon = xmr.NewDaemon(cfg.XmrDaemonRPC)
	}
	if role == swap.Taker {
		e.OnLocked = api.NotifyLocked
	}
	check(e.Load())
	return e, k
}

func send(k *eth.Contract, key *eth.Key, data []byte, label string) (eth.Hash, error) {
	rc, err := k.C.SendAndWait(ctxAll, key, &k.Addr, nil, data)
	h := eth.Hash{}
	if rc != nil {
		h = rc.TxHash
	}
	event("tx", "label", label, "tx", h.Hex(), "err", errString(err))
	return h, err
}

// sendForced broadcasts with a fixed gas limit (no estimate), so a call the
// contract refuses still lands on-chain as a reverted transaction (evidence).
func sendForced(k *eth.Contract, key *eth.Key, data []byte, label string) {
	tx, err := k.C.BuildTxFixedGas(ctxAll, key, &k.Addr, nil, data, 300_000)
	if err != nil {
		event("tx", "label", label, "err", err.Error())
		return
	}
	raw, h := tx.Sign(key)
	if _, err := k.C.SendRaw(ctxAll, raw); err != nil {
		event("tx", "label", label, "err", err.Error())
		return
	}
	rc, err := k.C.WaitReceipt(ctxAll, h)
	ok := rc != nil && rc.OK()
	event("tx", "label", label, "tx", h.Hex(), "ok", ok, "err", errString(err))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func head(k *eth.Contract) uint64 {
	for {
		h, err := k.C.Head(ctxAll)
		if err == nil {
			return h.Timestamp
		}
		time.Sleep(3 * time.Second)
	}
}

// ---------------------------------------------------------------- maker

func runMaker(mode string) {
	e, k := engine(swap.Maker)
	cfg.Offer.ChainID, cfg.Offer.Contract, cfg.Offer.XmrNet = k.ChainID, cfg.Contract, cfg.XmrNet
	m := &api.Maker{Offer: *cfg.Offer, Engine: e, PayoutTo: cfg.PayoutTo}
	go func() {
		srv := &http.Server{Addr: cfg.Listen, Handler: m, ReadHeaderTimeout: 10 * time.Second}
		check(srv.ListenAndServe())
	}()
	logf("hostile maker (%s) listening on %s", mode, cfg.Listen)
	tm := cfg.TakerMargin
	if tm == 0 {
		tm = swap.DefaultPolicy().Margin
	}
	done := map[string]bool{}
	for {
		for _, id := range e.IDs() {
			if done[id] {
				continue
			}
			ph, _, _ := e.Get(id)
			if ph == swap.MNegotiated {
				e.StepAll(ctxAll) // negotiation and lock validation are the honest code
				continue
			}
			if ph != swap.MEthLocked {
				continue
			}
			r, _ := e.Snapshot(id)
			done[id] = true
			event("taker-locked", "swap", r.SwapID.Hex(), "id", id, "t1", r.Swap.T1, "t2", r.Swap.T2, "xmr", r.XMR, "shared", r.SharedAddr)
			go hostileMaker(e, k, r, mode, tm)
		}
		time.Sleep(poll)
	}
}

func hostileMaker(e *swap.Engine, k *eth.Contract, r swap.Record, mode string, tm uint64) {
	ck, _ := eth.KeyFromHex(r.ClaimerKey)
	switch mode {
	case "short", "wrong-addr", "lock-silent":
		to, amt := r.SharedAddr, r.XMR
		if mode == "short" {
			amt = r.XMR - 1_000_000 // one micro-XMR short
		}
		err := e.Wallet.Session(ctxAll, "main", func(s *xmr.Sess) error {
			_ = s.Refresh(ctxAll)
			if mode == "wrong-addr" {
				a, err := s.Address(ctxAll) // pays itself instead of the shared address
				if err != nil {
					return err
				}
				to = a
			}
			p, err := s.PrepareTransfer(ctxAll, to, amt)
			if err != nil {
				return err
			}
			if err := s.Relay(ctxAll, p); err != nil {
				return err
			}
			event("xmr-sent", "mode", mode, "to", to, "amount", amt, "txid", p.TxHash, "fee", p.Fee)
			return nil
		})
		if err != nil {
			event("xmr-send-failed", "err", err.Error())
		}
	case "ack-no-lock":
		at := r.Swap.T1 - 2*tm - cfg.AckLead
		event("plan", "ackAt", at)
		for head(k) < at {
			time.Sleep(poll)
		}
		sig := eth.SignAck(ck, k.ChainID, k.Addr, r.SwapID, cfg.PayoutTo)
		_, _ = send(k, e.EthKey, eth.EncodeAckLocked(r.Swap, cfg.PayoutTo, sig), "ack-without-xmr")
	}
	// Opportunistic theft: if the taker is still PENDING at t1 (asleep), claim.
	for {
		st, err := k.State(ctxAll, r.SwapID)
		if err != nil {
			time.Sleep(poll)
			continue
		}
		if st.Stage != eth.StatusPending && st.Stage != eth.StatusReady {
			event("final-stage", "stage", st.Stage)
			if mode == "lock-silent" || st.Stage == eth.StatusCancelling {
				// watch for the end
				for {
					st2, err := k.State(ctxAll, r.SwapID)
					if err == nil && (st2.Stage == eth.StatusRefunded || st2.Stage == eth.StatusClaimed) {
						event("settled", "stage", st2.Stage)
						return
					}
					time.Sleep(poll)
				}
			}
			return
		}
		now := head(k)
		if mode != "lock-silent" && st.Stage == eth.StatusPending && now >= r.Swap.T1 && now < r.Swap.T2 {
			event("taker-asleep", "now", now)
			if _, err := send(k, e.EthKey, eth.EncodeClaimIntent(r.Swap, eth.SignIntent(ck, k.ChainID, k.Addr, r.SwapID)), "theft-claim-intent"); err == nil {
				time.Sleep(30 * time.Second)
				my, _ := keys.ScalarFromHex(r.MySpend)
				c := eth.SignClaim(ck, k.ChainID, k.Addr, r.Swap, my.Int(), cfg.PayoutTo, e.EthKey.Address(), big.NewInt(0))
				_, _ = send(k, e.EthKey, eth.EncodeClaimRelayed(c), "theft-claim")
			}
			return
		}
		time.Sleep(poll)
	}
}

// ---------------------------------------------------------------- taker

func runTaker(mode, makerURL, value string, minXmr uint64, leakFile string) {
	e, k := engine(swap.Taker)
	v, ok := new(big.Int).SetString(value, 10)
	if !ok {
		log.Fatal("bad -value")
	}
	pol := e.Policy
	rec, err := api.Take(ctxAll, e, api.TakeParams{MakerURL: makerURL, Value: v, MinXMR: minXmr,
		MinD1: 1800, MinD2: 1800, MaxConfs: 60, MinConfs: swap.XmrConfFloor, MaxGrace: 48 * 3600})
	check(err)
	_ = pol
	event("agreed", "id", rec.ID, "xmr", rec.XMR, "d1", rec.D1, "d2", rec.D2, "grace", rec.Grace)
	for { // the honest engine locks (approve + lock); then we take over
		e.StepAll(ctxAll)
		ph, _, _ := e.Get(rec.ID)
		if ph == swap.TLocked {
			break
		}
		if ph.Terminal() {
			log.Fatalf("lock failed: %s", ph)
		}
		time.Sleep(poll)
	}
	r, _ := e.Snapshot(rec.ID)
	event("locked", "swap", r.SwapID.Hex(), "lockTx", r.LockTx.Hex(), "t1", r.Swap.T1, "t2", r.Swap.T2)
	switch mode {
	case "collude-leak":
		// honest engine (marks ready etc.) until the relayer leaks the maker's
		// claim; then sweep the XMR with the leaked secret, wait out the intent
		// freeze and refund the ETH too.
		var c eth.RelayedClaim
		for {
			if b, err := os.ReadFile(leakFile); err == nil && json.Unmarshal(b, &c) == nil && c.Secret != nil {
				break
			}
			e.StepAll(ctxAll)
			time.Sleep(poll)
		}
		sc, err := keys.ScalarFromInt(c.Secret)
		check(err)
		event("got-leaked-secret", "swap", r.SwapID.Hex())
		check(e.Update(rec.ID, func(x *swap.Record) error { x.TheirSecret, x.Phase = sc.Hex(), swap.PSweeping; return nil }))
		for {
			e.StepAll(ctxAll)
			ph, lg, _ := e.Get(rec.ID)
			if ph == swap.PDone {
				event("swept-with-leaked-secret", "log", lg[len(lg)-1])
				break
			}
			time.Sleep(poll)
		}
		for {
			st, err := k.State(ctxAll, r.SwapID)
			if err == nil && st.Stage == eth.StatusClaimed {
				event("maker-claimed-after-all")
				return
			}
			if err == nil && st.Stage == eth.StatusClaiming && head(k) > st.Deadline+30 {
				my, _ := keys.ScalarFromHex(r.MySpend)
				if _, err := send(k, e.EthKey, eth.EncodeRefund(r.Swap, my.Int()), "refund after the freeze (XMR already swept)"); err == nil {
					return
				}
			}
			if err == nil && st.Stage == eth.StatusReady && head(k) >= r.Swap.T2+30 {
				my, _ := keys.ScalarFromHex(r.MySpend)
				if _, err := send(k, e.EthKey, eth.EncodeRefund(r.Swap, my.Int()), "refund after t2 (XMR already swept)"); err == nil {
					return
				}
			}
			time.Sleep(poll)
		}
	case "vanish":
		event("vanishing", "note", "never marks ready, never cancels, never refunds")
		return
	case "cancel-lockin":
		sendForced(k, e.EthKey, eth.EncodeCancelIntent(r.Swap), "cancel-inside-lock-in (must revert)")
		st, _ := k.State(ctxAll, r.SwapID)
		for head(k) < st.CancelFrom()+5 {
			time.Sleep(poll)
		}
		// wait until the maker has acked or locked, up to 10 more minutes
		for i := 0; i < 75; i++ {
			st, _ = k.State(ctxAll, r.SwapID)
			if st.PremiumTo != (eth.Address{}) {
				break
			}
			time.Sleep(poll)
		}
		if _, err := send(k, e.EthKey, eth.EncodeCancelIntent(r.Swap), "cancel-after-lock-in"); err != nil {
			return
		}
		time.Sleep(20 * time.Second)
		my, _ := keys.ScalarFromHex(r.MySpend)
		_, _ = send(k, e.EthKey, eth.EncodeRefund(r.Swap, my.Int()), "refund (reveals our key share)")
	}
}

// ---------------------------------------------------------------- relayer proxy

func runProxy(listen, upstream, mode string, hold time.Duration, leakFile string) {
	var hits atomic.Int64
	type held struct {
		path string
		body []byte
	}
	var mu sync.Mutex
	var queue []held
	fwd := func(path string, body []byte) (int, []byte) {
		resp, err := http.Post(upstream+path, "application/json", bytes.NewReader(body))
		if err != nil {
			return 502, []byte(`{"error":"upstream"}`)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	fake := func(w http.ResponseWriter) {
		var b [32]byte
		_, _ = rand.Read(b[:])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(relay.Response{TxHash: "0x" + hex.EncodeToString(b[:])})
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if r.Method == "GET" {
			resp, err := http.Get(upstream + r.URL.RequestURI())
			if err != nil {
				w.WriteHeader(502)
				return
			}
			defer resp.Body.Close()
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var probe struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(body, &probe)
		kind := probe.Kind
		if r.URL.Path == "/v1/relay" {
			kind = "claim"
		}
		switch {
		case mode == "leak-claim" && kind == "claim":
			_ = os.WriteFile(leakFile, body, 0o600)
			event("proxy", "n", n, "action", "withheld and leaked the signed claim (secret) to "+leakFile, "kind", kind)
			fake(w)
		case mode == "drop-claim" && kind == "claim":
			event("proxy", "n", n, "action", "dropped (fake hash)", "kind", kind)
			fake(w)
		case mode == "dup":
			c1, b1 := fwd(r.URL.Path, body)
			c2, b2 := fwd(r.URL.Path, body)
			event("proxy", "n", n, "action", "duplicated", "kind", kind, "first", string(b1), "second", string(b2), "codes", fmt.Sprint(c1, c2))
			w.WriteHeader(c1)
			_, _ = w.Write(b1)
		case mode == "delay":
			event("proxy", "n", n, "action", "delaying", "kind", kind, "by", hold.String())
			time.Sleep(hold)
			c, b := fwd(r.URL.Path, body)
			event("proxy", "n", n, "action", "delayed-forward", "kind", kind, "resp", string(b))
			w.WriteHeader(c)
			_, _ = w.Write(b)
		case mode == "reorder":
			mu.Lock()
			queue = append(queue, held{r.URL.Path, body})
			mu.Unlock()
			event("proxy", "n", n, "action", "held for reorder", "kind", kind)
			fake(w)
		default:
			c, b := fwd(r.URL.Path, body)
			event("proxy", "n", n, "action", "forward", "kind", kind, "resp", string(b))
			w.WriteHeader(c)
			_, _ = w.Write(b)
		}
	})
	if mode == "reorder" {
		go func() {
			for range time.Tick(hold) {
				mu.Lock()
				q := queue
				queue = nil
				mu.Unlock()
				for i := len(q) - 1; i >= 0; i-- {
					_, b := fwd(q[i].path, q[i].body)
					event("proxy", "action", "released (reversed order)", "path", q[i].path, "resp", string(b))
				}
			}
		}()
	}
	logf("hostile relay proxy (%s) on %s -> %s", mode, listen, upstream)
	check(http.ListenAndServe(listen, h))
}

// ---------------------------------------------------------------- spam

func runSpam(target, kind string, n int, value, asset string) {
	start := time.Now()
	codes := map[int]int{}
	var mu sync.Mutex
	count := func(c int) { mu.Lock(); codes[c]++; mu.Unlock() }
	switch kind {
	case "junk":
		bodies := [][]byte{bytes.Repeat([]byte("A"), 4<<20), []byte("{"), []byte(`{"session":"zz"}`),
			[]byte(strings.Repeat("[", 200000)), []byte(`{"swap":{"value":"-1"},"sig":"0x00"}`), {0xff, 0xfe, 0x00}}
		paths := []string{"/v1/swaps", "/v1/swaps/00000000000000000000000000000000/locked", "/v1/relay", "/v1/intent", "/v1/quote?value=" + strings.Repeat("9", 5000)}
		var wg sync.WaitGroup
		sem := make(chan struct{}, 32)
		for i := 0; i < n; i++ {
			for _, p := range paths {
				for _, b := range bodies {
					wg.Add(1)
					sem <- struct{}{}
					go func(p string, b []byte) {
						defer wg.Done()
						defer func() { <-sem }()
						method := "POST"
						if strings.HasPrefix(p, "/v1/quote") {
							method = "GET"
						}
						req, _ := http.NewRequest(method, target+p, bytes.NewReader(b))
						req.Header.Set("Content-Type", "application/json")
						c := &http.Client{Timeout: 20 * time.Second}
						resp, err := c.Do(req)
						if err != nil {
							count(-1)
							return
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						count(resp.StatusCode)
					}(p, b)
				}
			}
		}
		wg.Wait()
	case "quotes":
		v, _ := new(big.Int).SetString(value, 10)
		var a eth.Address
		if asset != "" {
			a, _ = eth.ParseAddress(asset)
		}
		for i := 0; i < n; i++ {
			sb := make([]byte, 16)
			_, _ = rand.Read(sb)
			session := hex.EncodeToString(sb)
			sp, _ := keys.RandomScalar()
			vw, _ := keys.RandomScalar()
			pr, _ := keys.Prove(sp, []byte("xmrswap-v3|"+session+"|taker"))
			k, _ := eth.NewKey()
			b, _ := json.Marshal(api.SwapRequest{Session: session, Asset: a, Value: v, Owner: k.Address(),
				SpendPub: sp.Public().Hex(), ViewShare: vw.Hex(), Proof: pr})
			resp, err := http.Post(target+"/v1/swaps", "application/json", bytes.NewReader(b))
			if err != nil {
				count(-1)
				continue
			}
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			count(resp.StatusCode)
			if resp.StatusCode != 200 {
				logf("quote %d: %d %s", i, resp.StatusCode, strings.TrimSpace(string(rb)))
			}
		}
	case "slow":
		// open n connections that send a partial request and then stall
		host := strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
		var conns []net.Conn
		for i := 0; i < n; i++ {
			c, err := net.DialTimeout("tcp", host, 5*time.Second)
			if err != nil {
				count(-1)
				continue
			}
			fmt.Fprintf(c, "POST /v1/relay HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n{")
			conns = append(conns, c)
			count(0)
		}
		logf("holding %d stalled connections for 120 s", len(conns))
		time.Sleep(120 * time.Second)
		alive := 0
		for _, c := range conns {
			_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			buf := make([]byte, 1)
			_, err := c.Read(buf)
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				alive++ // server still holding the connection open
			}
			c.Close()
		}
		event("spam-slow", "held", len(conns), "stillOpenAfter120s", alive)
	}
	event("spam", "kind", kind, "target", target, "codes", fmt.Sprint(codes), "secs", time.Since(start).Seconds())
}
