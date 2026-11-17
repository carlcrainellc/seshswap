// testnet runs the v3 scenarios on a public testnet (Sepolia + Monero stagenet)
// with real swapd engines in one process: a maker (no ETH key; relayers only),
// a taker, and an in-process relayer whose refusals simulate censorship.
// TEST-ONLY. It refuses mainnets. Keys come from the environment and are never
// printed; results (public tx hashes, amounts, timings) go to -out.
//
//	testnet -deploy -bytecode contracts/out/XmrSwapV3.sol/XmrSwapV3.json   (prints the address)
//	testnet -contract 0x.. -dir STATE -maker-wallet URL -taker-wallet URL -out results.json
//
// Env: SWAPD_PASSPHRASE, TAKER_KEY (ETH owner), RELAYER_KEY (relayer + deployer).
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/price"
	"github.com/carlcrainellc/seshswap/swapd/internal/relay"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// Scenario is one swap and what we observed about it (persisted for resume).
type Scenario struct {
	Name      string   `json:"name"`
	Offer     string   `json:"offer"` // "A" (no premium, grace 2 h) or "B" (premium 0.5%, grace 30 min)
	ID        string   `json:"id"`
	SwapID    string   `json:"swapId,omitempty"`
	XMR       uint64   `json:"xmr"`
	Value     string   `json:"value"`
	Premium   string   `json:"premium"`
	Confs     uint64   `json:"confs"`
	Grace     uint64   `json:"grace"`
	T1        uint64   `json:"t1,omitempty"`
	T2        uint64   `json:"t2,omitempty"`
	LockTime  int64    `json:"lockTime,omitempty"`  // ETH lock block time (unix)
	DoneTime  int64    `json:"doneTime,omitempty"`  // claimed (key known to taker) or refunded
	SpendTime int64    `json:"spendTime,omitempty"` // receiving wallet can spend the XMR
	Outcome   string   `json:"outcome,omitempty"`   // CLAIMED / REFUNDED
	Notes     []string `json:"notes,omitempty"`
	Started   int64    `json:"started"`
	Released  bool     `json:"released,omitempty"` // delayed-reveal: censorship lifted
}

type State struct {
	Contract  string            `json:"contract"`
	StartBlk  uint64            `json:"startBlock"`
	Start     map[string]string `json:"startBalances"`
	Scenarios []*Scenario       `json:"scenarios"`
	Payout    string            `json:"payout"`
	Extra     map[string]any    `json:"extra,omitempty"`
}

var mainnets = map[int64]bool{1: true, 10: true, 100: true, 8453: true, 42161: true}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	ethRPC := flag.String("eth", "https://ethereum-sepolia-rpc.publicnode.com", "Ethereum RPC")
	deploy := flag.Bool("deploy", false, "deploy the contract and exit")
	bytecodePath := flag.String("bytecode", "", "forge artifact JSON")
	contract := flag.String("contract", "", "XmrSwapV3 address")
	dir := flag.String("dir", "state", "state dir")
	mw := flag.String("maker-wallet", "http://127.0.0.1:38090", "maker wallet-rpc")
	xd := flag.String("xmr-daemon", "http://node2.monerodevs.org:38089", "monerod used to measure block times")
	tw := flag.String("taker-wallet", "http://127.0.0.1:38091", "taker wallet-rpc")
	out := flag.String("out", "results.json", "results file")
	valueS := flag.String("value", "5000000000000000", "wei per swap")
	setup := flag.Bool("setup", false, "create the wallets, print the maker and taker addresses, exit")
	only := flag.String("scenarios", "happy,premium-returned,premium-forfeit,delayed-reveal,refund", "scenarios")
	flag.Parse()
	ctx := context.Background()
	c := eth.NewClient(*ethRPC)
	cid, err := c.ChainID(ctx)
	must(err)
	if mainnets[cid.Int64()] {
		log.Fatal("refusing mainnet")
	}
	rk, err := eth.KeyFromHex(strings.TrimPrefix(os.Getenv("RELAYER_KEY"), "0x"))
	must(err)
	if *deploy {
		b, err := os.ReadFile(*bytecodePath)
		must(err)
		var j struct {
			Bytecode struct {
				Object string `json:"object"`
			} `json:"bytecode"`
		}
		must(json.Unmarshal(b, &j))
		code, err := hex.DecodeString(strings.TrimPrefix(j.Bytecode.Object, "0x"))
		must(err)
		rc, err := c.SendAndWait(ctx, rk, nil, nil, code)
		must(err)
		fmt.Printf("deployed %s tx %s gas %d\n", rc.Contract.Hex(), rc.TxHash.Hex(), rc.Gas())
		return
	}
	tk, err := eth.KeyFromHex(strings.TrimPrefix(os.Getenv("TAKER_KEY"), "0x"))
	must(err)
	addr, err := eth.ParseAddress(*contract)
	must(err)
	k := &eth.Contract{C: c, Addr: addr, ChainID: cid}
	pass := os.Getenv("SWAPD_PASSPHRASE")
	must(os.MkdirAll(*dir, 0o700))
	statePath := filepath.Join(*dir, "driver.json")
	var S State
	if b, err := os.ReadFile(statePath); err == nil {
		must(json.Unmarshal(b, &S))
	}
	save := func() {
		b, _ := json.MarshalIndent(&S, "", "  ")
		tmp := statePath + ".tmp"
		must(os.WriteFile(tmp, b, 0o600))
		must(os.Rename(tmp, statePath))
	}

	pol := swap.Policy{Margin: 300, XmrLockBudget: 600, EthConfs: 2, RelayTimeout: 120, RelayFee: big.NewInt(1e12), MinGrace: 1800}
	mst, err := store.Open(filepath.Join(*dir, "maker"), pass)
	must(err)
	tst, err := store.Open(filepath.Join(*dir, "taker"), pass)
	must(err)

	// payout address: a fresh key kept in the maker store (never printed)
	var payHex string
	if err := mst.Get("payout-key", &payHex); err != nil {
		pk, err := eth.NewKey()
		must(err)
		payHex = pk.Hex()
		must(mst.Put("payout-key", payHex))
	}
	pk, err := eth.KeyFromHex(payHex)
	must(err)
	payout := pk.Address()
	S.Payout = payout.Hex()

	// relayer with per-swap censorship controls
	var mu sync.Mutex
	refuse := map[string]map[string]bool{} // swapID -> kinds
	rs := &relay.Server{K: k, Key: rk, MinFeeMultiplier: 1.0, TokenMinFee: map[eth.Address]*big.Int{}, Logf: log.Printf,
		Refuse: func(kind string, id eth.Hash) bool {
			mu.Lock()
			defer mu.Unlock()
			return refuse[id.Hex()][kind]
		}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go http.Serve(ln, rs)
	relayURL := "http://" + ln.Addr().String()

	mEng := &swap.Engine{Role: swap.Maker, Store: mst, K: k, Wallet: xmr.NewWallet(*mw, pass), MainWal: "main",
		XmrNet: keys.Stagenet, Relayers: []string{relayURL}, Policy: pol, Logf: log.Printf}
	tEng := &swap.Engine{Role: swap.Taker, Store: tst, K: k, EthKey: tk, Wallet: xmr.NewWallet(*tw, pass), MainWal: "main",
		XmrNet: keys.Stagenet, Policy: pol, Logf: log.Printf, OnLocked: api.NotifyLocked}
	mEng.Daemon, tEng.Daemon = xmr.NewDaemon(*xd), xmr.NewDaemon(*xd)
	must(mEng.Load())
	must(tEng.Load())
	for _, w := range []*xmr.Wallet{mEng.Wallet, tEng.Wallet} {
		if err := w.Session(ctx, "main", func(s *xmr.Sess) error { return nil }); err != nil {
			must(w.Create(ctx, "main"))
		}
	}

	if *setup {
		for n, w := range map[string]*xmr.Wallet{"maker": mEng.Wallet, "taker": tEng.Wallet} {
			_ = w.Session(ctx, "main", func(s *xmr.Sess) error {
				a, err := s.Address(ctx)
				_ = s.Refresh(ctx)
				t, u, _ := s.Balance(ctx)
				fmt.Printf("%s main wallet %s balance %d unlocked %d\n", n, a, t, u)
				return err
			})
		}
		return
	}

	// maker service: live median price (4 public sources), 1% spread
	or := price.New(price.Config{MinXmrPerEth: 1, MaxXmrPerEth: 20})
	if err := or.Refresh(ctx); err != nil {
		log.Printf("price: %v", err)
	}
	go or.Run(ctx, 30*time.Second, log.Printf)
	id, err := api.LoadIdentity(mst)
	must(err)
	value, _ := new(big.Int).SetString(*valueS, 10)
	mk := func(premiumBps, grace uint64) string {
		m := &api.Maker{Engine: mEng, PayoutTo: payout, Offer: api.Offer{ChainID: cid, Contract: addr,
			PicoPerUnit: big.NewInt(4e12), Min: big.NewInt(1e15), Max: big.NewInt(1e17), D1: 1800, D2: 1800,
			MinConfs: 10, XmrNet: keys.Stagenet, RelayFee: pol.RelayFee, TTL: 900, Grace: grace, PremiumBps: premiumBps, SpreadBps: 100}}
		m.Oracle, m.Identity, m.MaxExposure = or, id, 200_000_000_000
		l, err := net.Listen("tcp", "127.0.0.1:0")
		must(err)
		go http.Serve(l, m)
		return "http://" + l.Addr().String()
	}
	// "C": a 4 h freeze, so a reveal held back until after t2 still lands far
	// inside the claim intent's deadline (intent time + grace).
	urls := map[string]string{"A": mk(0, 7200), "B": mk(50, 1800), "C": mk(0, 4*3600)}

	if S.Contract == "" {
		h, err := c.Head(ctx)
		must(err)
		S.Contract, S.StartBlk, S.Start = addr.Hex(), h.Number, map[string]string{}
		for n, a := range map[string]eth.Address{"taker": tk.Address(), "relayer": rk.Address(), "payout": payout, "contract": addr} {
			b, err := c.Balance(ctx, a)
			must(err)
			S.Start[n] = b.String()
		}
		for n, w := range map[string]*xmr.Wallet{"makerXmr": mEng.Wallet, "takerXmr": tEng.Wallet} {
			_ = w.Session(ctx, "main", func(s *xmr.Sess) error {
				_ = s.Refresh(ctx)
				t, _, err := s.Balance(ctx)
				S.Start[n] = fmt.Sprint(t)
				return err
			})
		}
		offers := map[string]string{"happy": "A", "premium-returned": "B", "premium-forfeit": "B", "delayed-reveal": "C", "refund": "A"}
		for _, n := range strings.Split(*only, ",") {
			S.Scenarios = append(S.Scenarios, &Scenario{Name: n, Offer: offers[n]})
		}
		save()
	}

	start := func(sc *Scenario) {
		rec, err := api.Take(ctx, tEng, api.TakeParams{MakerURL: urls[sc.Offer], Value: value, MinXMR: 1, MinD1: 1800, MinD2: 1800,
			MaxConfs: 40, MinConfs: 10, MakerID: id.ID()})
		if err != nil {
			log.Printf("[%s] take failed: %v", sc.Name, err)
			return
		}
		sc.Notes = append(sc.Notes, fmt.Sprintf("planned with %d s mean Monero block time: t1 = lock + %d s, t2 = t1 + %d s", func() uint64 { s, _ := tEng.BlockSecs(ctx); return s }(), rec.D1, rec.D2))
		sc.ID, sc.XMR, sc.Value, sc.Premium, sc.Confs, sc.Grace, sc.Started = rec.ID, rec.XMR, rec.Value.String(), rec.Premium.String(), rec.MinConfs, rec.Grace, time.Now().Unix()
		if sc.Name == "premium-forfeit" {
			// The taker decides to cancel: it will never consider the XMR confirmed,
			// so its daemon sends a cancel intent before t1 (after the maker's ack).
			_ = tEng.Update(rec.ID, func(r *swap.Record) error { r.MinConfs = 1_000_000; return nil })
			sc.Notes = append(sc.Notes, "taker set to never accept the XMR lock (simulates a taker who walks away after the maker's ack)")
		}
		log.Printf("[%s] swap %s: %s wei -> %d piconero, premium %s, %d confs, grace %ds", sc.Name, rec.ID, rec.Value, rec.XMR, rec.Premium, rec.MinConfs, rec.Grace)
		save()
	}

	// censor installs the relayer refusals for a scenario. It must also run for
	// swaps resumed from driver.json: a restart that forgets it lets the refund
	// scenario's claim intent through (happened on the first Sepolia run).
	censor := func(sc *Scenario) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case sc.Name == "delayed-reveal" && !sc.Released:
			refuse[sc.SwapID] = map[string]bool{"claim": true}
		case sc.Name == "refund":
			refuse[sc.SwapID] = map[string]bool{"claimIntent": true}
		}
	}
	for _, sc := range S.Scenarios {
		if sc.SwapID != "" && sc.Outcome == "" {
			censor(sc)
		}
	}

	for _, sc := range S.Scenarios {
		if sc.ID == "" {
			start(sc)
			time.Sleep(20 * time.Second)
		}
	}

	walletHeight := func(w *xmr.Wallet) uint64 {
		var h uint64
		_ = w.Session(ctx, "main", func(s *xmr.Sess) error { var err error; h, err = s.Height(ctx); return err })
		return h
	}

	for {
		mEng.StepAll(ctx)
		tEng.StepAll(ctx)
		head, err := c.Head(ctx)
		if err != nil {
			log.Printf("head: %v", err)
			time.Sleep(20 * time.Second)
			continue
		}
		allDone := true
		for _, sc := range S.Scenarios {
			if sc.ID == "" {
				start(sc) // e.g. the maker had no unlocked XMR yet: retry
				allDone = false
				continue
			}
			tr, _ := tEng.Snapshot(sc.ID)
			mr, _ := mEng.Snapshot(sc.ID)
			if tr.Swap != nil && sc.SwapID == "" {
				sc.SwapID, sc.T1, sc.T2 = tr.SwapID.Hex(), tr.Swap.T1, tr.Swap.T2
				sc.LockTime = int64(tr.Swap.T1 - tr.D1)
				censor(sc)
				save()
			}
			if sc.SwapID != "" {
				st, err := k.State(ctx, tr.SwapID)
				if err == nil {
					if sc.Name == "delayed-reveal" && !sc.Released && st.Stage == eth.StatusClaiming && releaseNow(head.Timestamp, sc.T2, st.Deadline, pol.Margin) {
						sc.Notes = append(sc.Notes, fmt.Sprintf("reveal held back until %d s after t2 (chain time %d, t2 %d, intent deadline %d); stage still CLAIMING, taker phase %s: the taker could not refund during the freeze", head.Timestamp-sc.T2, head.Timestamp, sc.T2, st.Deadline, tr.Phase))
						mu.Lock()
						delete(refuse, sc.SwapID)
						mu.Unlock()
						sc.Released = true
						save()
					}
					if sc.DoneTime == 0 && (st.Stage == eth.StatusClaimed || st.Stage == eth.StatusRefunded) {
						sc.DoneTime = time.Now().Unix()
						sc.Outcome = map[uint8]string{eth.StatusClaimed: "CLAIMED", eth.StatusRefunded: "REFUNDED"}[st.Stage]
						log.Printf("[%s] %s after %ds", sc.Name, sc.Outcome, sc.DoneTime-sc.LockTime)
						save()
					}
				}
			}
			// spendable: the receiving side's sweep is 10 blocks deep
			recv, w := tr, tEng.Wallet
			if sc.Outcome == "REFUNDED" {
				recv, w = mr, mEng.Wallet
			}
			if sc.SpendTime == 0 && recv.SweepHeight != 0 && walletHeight(w) >= swap.SpendableAt(0, recv.SweepHeight) {
				sc.SpendTime = time.Now().Unix()
				log.Printf("[%s] XMR spendable after %ds", sc.Name, sc.SpendTime-sc.LockTime)
				save()
			}
			if !(tr.Phase.Terminal() && mr.Phase.Terminal() && (sc.SpendTime != 0 || sc.Outcome == "")) {
				allDone = false
			}
		}
		if allDone {
			break
		}
		time.Sleep(20 * time.Second)
	}
	report(ctx, c, k, &S, tEng, mEng, tk.Address(), rk.Address(), payout, *out)
	save()
}

var sentRe = regexp.MustCompile(`(\S+) sent: (0x[0-9a-f]{64})`)

type TxInfo struct {
	Kind string `json:"kind"`
	Hash string `json:"hash"`
	Gas  uint64 `json:"gasUsed"`
	Fee  string `json:"feeWei"`
	From string `json:"from"`
}

func report(ctx context.Context, c *eth.Client, k *eth.Contract, S *State, tEng, mEng *swap.Engine, taker, relayer, payout eth.Address, out string) {
	head, _ := c.Head(ctx)
	res := map[string]any{"contract": S.Contract, "chainId": k.ChainID.String(), "startBlock": S.StartBlk, "endBlock": head.Number,
		"start": S.Start, "payout": S.Payout}
	end := map[string]string{}
	for n, a := range map[string]eth.Address{"taker": taker, "relayer": relayer, "payout": payout, "contract": k.Addr} {
		b, _ := c.Balance(ctx, a)
		end[n] = b.String()
	}
	for n, w := range map[string]*xmr.Wallet{"makerXmr": mEng.Wallet, "takerXmr": tEng.Wallet} {
		_ = w.Session(ctx, "main", func(s *xmr.Sess) error {
			_ = s.Refresh(ctx)
			t, _, err := s.Balance(ctx)
			end[n] = fmt.Sprint(t)
			return err
		})
	}
	res["end"] = end
	takerFees, relayerFees := new(big.Int), new(big.Int)
	var scen []map[string]any
	expPayout, expTaker, expRelayer := new(big.Int), new(big.Int), new(big.Int)
	var expTakerXmr, makerXmrOut, makerXmrBack uint64
	for _, sc := range S.Scenarios {
		tr, _ := tEng.Snapshot(sc.ID)
		mr, _ := mEng.Snapshot(sc.ID)
		var txs []TxInfo
		seen := map[string]bool{}
		// all contract events of this swap (any sender: taker, relayer)
		var evs []eth.Event
		if tr.Swap != nil {
			evs, _ = k.Events(ctx, tr.SwapID, S.StartBlk, head.Number)
			dt, dp, dr := ethDeltas(evs, taker, payout, relayer)
			expTaker.Add(expTaker, dt)
			expPayout.Add(expPayout, dp)
			expRelayer.Add(expRelayer, dr)
			for _, ev := range evs {
				h := ev.TxHash.Hex()
				if seen[h] {
					continue
				}
				seen[h] = true
				ti := TxInfo{Kind: ev.Kind, Hash: h}
				if rc, err := c.Receipt(ctx, ev.TxHash); err == nil && rc != nil {
					ti.Gas, ti.Fee = rc.Gas(), rc.Fee().String()
					if rc.From() == taker {
						takerFees.Add(takerFees, rc.Fee())
					}
					if rc.From() == relayer {
						relayerFees.Add(relayerFees, rc.Fee())
					}
					ti.From = rc.From().Hex()
				}
				txs = append(txs, ti)
			}
		}
		// taker txs that emitted no event (e.g. a reverted one) are found via the log
		for _, l := range tr.Log {
			if m := sentRe.FindStringSubmatch(l); m != nil && !seen[m[2]] {
				h, _ := eth.ParseHash(m[2])
				if rc, err := c.Receipt(ctx, h); err == nil && rc != nil {
					seen[m[2]] = true
					if rc.From() == taker {
						takerFees.Add(takerFees, rc.Fee())
					}
					if rc.From() == relayer {
						relayerFees.Add(relayerFees, rc.Fee())
					}
					txs = append(txs, TxInfo{Kind: m[1] + " (no event)", Hash: m[2], Gas: rc.Gas(), Fee: rc.Fee().String(), From: rc.From().Hex()})
				}
			}
		}
		v, _ := new(big.Int).SetString(sc.Value, 10)
		prem, _ := new(big.Int).SetString(sc.Premium, 10)
		if v == nil {
			v = new(big.Int)
		}
		if prem == nil {
			prem = new(big.Int)
		}
		var sweepFee uint64
		var sweeps []string
		recv := tr
		if sc.Outcome == "REFUNDED" {
			recv = mr
		}
		for _, s := range recv.Sweep {
			sweepFee += s.Fee
			sweeps = append(sweeps, s.TxHash)
		}
		lockFee := uint64(0)
		lockTx := ""
		if mr.XmrLock != nil {
			lockFee, lockTx = mr.XmrLock.Fee, mr.XmrLock.TxHash
			makerXmrOut += sc.XMR + lockFee
		}
		st, _ := k.State(ctx, tr.SwapID)
		switch sc.Outcome {
		case "CLAIMED":
			expTakerXmr += sc.XMR - sweepFee
		case "REFUNDED":
			if mr.XmrLock != nil {
				makerXmrBack += sc.XMR - sweepFee
			}
		}
		e := map[string]any{"scenario": sc, "ethTxs": txs, "xmrLockTx": lockTx, "xmrLockFee": lockFee, "sweepTxs": sweeps, "sweepFee": sweepFee,
			"takerPhase": tr.Phase, "makerPhase": mr.Phase, "premiumTo": st.PremiumTo.Hex(), "xmrLockHeight": mr.XmrLockHeight, "sweepHeight": recv.SweepHeight}
		if sc.LockTime != 0 && sc.DoneTime != 0 {
			e["lockToDoneSec"] = sc.DoneTime - sc.LockTime
		}
		if sc.LockTime != 0 && sc.SpendTime != 0 {
			e["lockToSpendableSec"] = sc.SpendTime - sc.LockTime
		}
		scen = append(scen, e)
	}
	res["scenarios"] = scen
	sb := func(n string) *big.Int { x, _ := new(big.Int).SetString(S.Start[n], 10); return x }
	eb := func(n string) *big.Int { x, _ := new(big.Int).SetString(end[n], 10); return x }
	takerWant := new(big.Int).Add(sb("taker"), expTaker)
	takerWant.Sub(takerWant, takerFees)
	payWant := new(big.Int).Add(sb("payout"), expPayout)
	relayWant := relayerEthWant(sb("relayer"), expRelayer, relayerFees)
	su := func(n string) uint64 { var x uint64; fmt.Sscan(S.Start[n], &x); return x }
	eu := func(n string) uint64 { var x uint64; fmt.Sscan(end[n], &x); return x }
	checks := map[string]any{
		"takerEth":      map[string]any{"want": takerWant.String(), "got": end["taker"], "ok": takerWant.Cmp(eb("taker")) == 0, "feesWei": takerFees.String()},
		"payoutEth":     map[string]any{"want": payWant.String(), "got": end["payout"], "ok": payWant.Cmp(eb("payout")) == 0},
		"relayerEth":    map[string]any{"want": relayWant.String(), "got": end["relayer"], "ok": relayWant.Cmp(eb("relayer")) == 0, "feesWei": relayerFees.String(), "feesReceived": expRelayer.String()},
		"contractEmpty": map[string]any{"got": end["contract"], "ok": eb("contract").Sign() == 0},
		"takerXmr":      map[string]any{"want": su("takerXmr") + expTakerXmr, "got": eu("takerXmr"), "ok": su("takerXmr")+expTakerXmr == eu("takerXmr")},
		"makerXmr":      map[string]any{"want": su("makerXmr") - makerXmrOut + makerXmrBack, "got": eu("makerXmr"), "ok": su("makerXmr")-makerXmrOut+makerXmrBack == eu("makerXmr")},
	}
	res["checks"] = checks
	b, _ := json.MarshalIndent(res, "", "  ")
	must(os.WriteFile(out, b, 0o644))
	log.Printf("results written to %s", out)
}

// relayFeeOf reads the relayer fee paid out of a claimed swap from its Claimed event.
func relayFeeOf(ctx context.Context, k *eth.Contract, id eth.Hash, from, to uint64) *big.Int {
	evs, _ := k.Events(ctx, id, from, to)
	for _, ev := range evs {
		if ev.Kind == "Claimed" && ev.Fee != nil {
			return ev.Fee
		}
	}
	return new(big.Int)
}
