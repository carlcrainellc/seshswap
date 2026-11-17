package main

import (
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/chaincfg"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/exits"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
)

// ExitLeaseConfig is the optional "exitLease" block (exit seller kit).
type ExitLeaseConfig struct {
	Contract  eth.Address `json:"contract"`
	FromBlock uint64      `json:"fromBlock,omitempty"`
}

const exitsUsage = `swapd exits: sell time on a Session Router exit (testnets only)

  swapd exits meta      -price "0.10 USDC/h" -payout ADDR -country US [-dns 10.195.0.53] [-note TEXT]
                        print the registry listing text for an exit (then: swapd registry register -tag exit)
  swapd exits offer     -config F -id N -payout ADDR [-usdc-per-hour 0.10] [-eth-per-hour 0.00002] [-max-hours 24]
  swapd exits clear-offer -config F -id N
  swapd exits list      -config F                 exits in the registry, with their on-chain prices
  swapd exits leases    -config F -id N           purchases of one exit
  swapd exits keygen    -issuer-key PATH          make the admission key; prints the public key for the exit router
  swapd exits router-key -out PATH                make (or read) a router key file; prints its .sesh address
  swapd exits watch     -config F -id N -exit-sesh S.sesh -issuer-key PATH -store PATH [-state PATH]
                        [-payout ADDR] [-confirmations 20] [-rpc-timeout 10s] [-once]
                        admit buyers: a signed admission for each confirmed purchase, until its end time
  swapd exits watchdog  -probe-rpc ADDR -exit-sesh S.sesh -restart CMD -restart-probe CMD [-probe-ns exitprobe]
                        [-dns 10.195.0.53] [-probe-host example.com] [-every 30s] [-fails 10] [-grace 5m]
                        [-min-gap 15m]
                        through a local probe router that maps the exit: look up -probe-host with the
                        exit's resolver and do a TLS handshake with it on 443; restart the exit router
                        (same key) after -fails failed probes in a row; rate-limited, every step logged

The listing owner key comes from SWAPD_REGISTRY_KEY (hex) for offer/clear-offer.
The watcher holds no key that can move funds: it reads the chain and signs admissions.`

func runExits(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, exitsUsage)
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("exits "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", "", "config JSON (chain/ethRpc; optional registry and exitLease blocks)")
	id := fs.Uint64("id", 0, "registry listing id of the exit")
	payout := fs.String("payout", "", "payout address")
	usdcH := fs.String("usdc-per-hour", "", "price per hour in USDC (e.g. 0.10); empty = USDC not accepted")
	ethH := fs.String("eth-per-hour", "", "price per hour in ETH; empty = ETH not accepted")
	maxH := fs.Uint("max-hours", 24, "most hours one purchase may buy (1..744)")
	price := fs.String("price", "", "meta: price text shown in search")
	country := fs.String("country", "", "meta: two-letter country code")
	dns := fs.String("dns", "10.195.0.53", "meta, watchdog: the exit resolver buyers use through the tunnel (the watchdog probes it)")
	note := fs.String("note", "", "meta: short note")
	exitSesh := fs.String("exit-sesh", "", "watch, watchdog: this exit's .sesh address")
	issuer := fs.String("issuer-key", "", "admission key file (32-byte hex seed)")
	store := fs.String("store", "", "watch: admissions.json the exit router reads (SR_EXIT_LEASE_ADMISSIONS)")
	state := fs.String("state", "", "watch: file that remembers the last scanned block")
	confs := fs.Uint64("confirmations", 20, "watch: blocks before a purchase counts")
	outPath := fs.String("out", "", "router-key: key file path")
	rpcTimeout := fs.Duration("rpc-timeout", 10*time.Second, "watch: give up on one RPC after this and try the next")
	once := fs.Bool("once", false, "watch/watchdog: one pass and exit")
	wdNS := fs.String("probe-ns", "exitprobe", "watchdog: network namespace of the probe router")
	wdRPC := fs.String("probe-rpc", "", "watchdog: the probe router's RPC address (e.g. ipc:///opt/srnet-exit/run/probe-rpc.sock)")
	wdHost := fs.String("probe-host", "example.com", "watchdog: site the probe fetches over 443 through the exit")
	wdTimeout := fs.Duration("probe-timeout", 10*time.Second, "watchdog: how long one probe waits for an answer")
	wdRestart := fs.String("restart", "", "watchdog: shell command that restarts the exit router")
	wdRestartProbe := fs.String("restart-probe", "", "watchdog: shell command that restarts the probe router")
	wdFails := fs.Int("fails", 10, "watchdog: failed probes in a row before the exit router is restarted")
	wdGrace := fs.Duration("grace", 5*time.Minute, "watchdog: no restart this soon after start or a restart")
	wdGap := fs.Duration("min-gap", 15*time.Minute, "watchdog: least time between restarts (doubles while they don't help)")
	wdEvery := fs.Duration("every", 30*time.Second, "watchdog: how often to probe")
	_ = fs.Parse(args[1:])

	switch sub {
	case "meta":
		m := exits.Meta{Price: *price, Chain: chaincfg.ArbitrumSepolia, Payout: *payout, Country: *country,
			Policy: "tcp/80,tcp/443,dns", DNS: *dns, Note: *note}
		s, err := m.String()
		check(err)
		fmt.Println(s)
		return
	case "router-key":
		if *outPath == "" {
			log.Fatal("-out is required")
		}
		k, err := exits.RouterKey(*outPath, true)
		check(err)
		fmt.Println(exits.SeshFromPub(k.Public().(ed25519.PublicKey)))
		return
	case "watchdog":
		runWatchdog(watchdogOpts{ns: *wdNS, rpc: *wdRPC, exit: *exitSesh, dns: *dns, host: *wdHost, timeout: *wdTimeout,
			restart: *wdRestart, restartProbe: *wdRestartProbe, fails: *wdFails, grace: *wdGrace, gap: *wdGap,
			every: *wdEvery, once: *once})
		return
	case "keygen":
		if *issuer == "" {
			log.Fatal("-issuer-key is required")
		}
		k, err := exits.IssuerKey(*issuer, true)
		check(err)
		fmt.Println(exits.PubHex(k))
		return
	}

	cfg := mustConfig(*cfgPath)
	ch, _ := chaincfg.Lookup(cfg.Chain)
	regAddr, fromReg := eth.Address{}, uint64(0)
	if cfg.Registry != nil && !cfg.Registry.Contract.IsZero() {
		regAddr, fromReg = cfg.Registry.Contract, cfg.Registry.FromBlock
	} else if ch.Registry != "" {
		regAddr, _ = eth.ParseAddress(ch.Registry)
		fromReg = ch.RegistryFromBlock
	}
	elAddr, fromEL := eth.Address{}, uint64(0)
	if cfg.ExitLease != nil && !cfg.ExitLease.Contract.IsZero() {
		elAddr, fromEL = cfg.ExitLease.Contract, cfg.ExitLease.FromBlock
	} else if ch.ExitLease != "" {
		elAddr, _ = eth.ParseAddress(ch.ExitLease)
		fromEL = ch.ExitLeaseFromBlock
	}
	if elAddr.IsZero() || regAddr.IsZero() {
		log.Fatal("no ExitLease/SeshRegistry for this chain (set exitLease.contract and registry.contract)")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	c := eth.NewClients(append([]string{cfg.EthRPC}, cfg.EthRPCs...)...)
	if sub == "watch" {
		// The watcher only reads: if one RPC hangs or fails, use the next one.
		c.Failover, c.AttemptTimeout = true, *rpcTimeout
		c.OnFailover = func(from, to string, err error) {
			log.Printf("rpc %s failed (%v); trying %s", rpcHost(from), err, rpcHost(to))
		}
		hosts := make([]string, len(c.URLs))
		for i, u := range c.URLs {
			hosts[i] = rpcHost(u)
		}
		log.Printf("RPCs in order: %s (each try times out after %s)", strings.Join(hosts, ", "), *rpcTimeout)
	}
	cid, err := c.ChainID(ctx)
	check(err)
	if name, ok := mainnets[cid.Int64()]; ok {
		log.Fatalf("refusing %s mainnet: ExitLease is test-only", name)
	}
	k := &exits.Contract{C: c, Addr: elAddr}
	rc := &registry.Client{C: c, Addr: regAddr}
	needID := func() uint64 {
		if *id == 0 {
			log.Fatal("-id is required")
		}
		return *id
	}
	usdcDec := 6

	switch sub {
	case "offer", "clear-offer":
		h := os.Getenv("SWAPD_REGISTRY_KEY")
		if h == "" {
			log.Fatal("set SWAPD_REGISTRY_KEY (hex, the listing owner key)")
		}
		key, err := eth.KeyFromHex(h)
		check(err)
		info, err := rc.Info(ctx, needID())
		check(err)
		if info.Owner != key.Address() {
			log.Fatalf("listing %d is owned by %s, not this key", *id, info.Owner.Hex())
		}
		if info.Tag != exits.TagExit {
			log.Fatalf("listing %d has tag %q; register the exit with -tag exit", *id, info.Tag)
		}
		var data []byte
		if sub == "clear-offer" {
			data = exits.EncodeClearOffer(*id)
		} else {
			p, err := eth.ParseAddress(*payout)
			if err != nil {
				log.Fatal("-payout must be an address")
			}
			wei, usdc := new(big.Int), new(big.Int)
			if *ethH != "" {
				wei, err = parseETH(*ethH)
				check(err)
			}
			if *usdcH != "" {
				usdc, err = parseDec(*usdcH, usdcDec)
				check(err)
			}
			if wei.Sign() == 0 && usdc.Sign() == 0 {
				log.Fatal("set -usdc-per-hour and/or -eth-per-hour")
			}
			data = exits.EncodeSetOffer(*id, p, wei, usdc, uint32(*maxH))
		}
		r, err := c.SendAndWait(ctx, key, &elAddr, nil, data)
		if err != nil {
			log.Fatalf("%s: %v", sub, exits.ExplainRevert(err))
		}
		fmt.Printf("%s: tx %s/tx/%s (gas %d)\n", sub, cfg.Explorer, r.TxHash.Hex(), r.Gas())
	case "list":
		ix := registry.OpenIndex("", regAddr, fromReg)
		check(ix.Sync(ctx, c))
		for _, e := range ix.Search("", exits.TagExit, false) {
			o, err := k.Offer(ctx, e.ID)
			sale := "not for sale"
			if err == nil && o.ForSale() {
				var ps []string
				if o.USDCPerHour.Sign() > 0 {
					ps = append(ps, fmtDec(o.USDCPerHour, usdcDec)+" USDC/h")
				}
				if o.WeiPerHour.Sign() > 0 {
					ps = append(ps, registry.FormatWei(o.WeiPerHour)+" ETH/h")
				}
				sale = fmt.Sprintf("%s, up to %d h, pays %s", strings.Join(ps, " or "), o.MaxHours, o.Payout.Hex())
			}
			fmt.Printf("#%d %s\n    %s\n    %s\n", e.ID, e.Sesh, sale, e.Meta)
		}
	case "leases":
		head, err := c.Head(ctx)
		check(err)
		ls, err := leasesChunked(ctx, k, fromEL, head.Number, needID())
		check(err)
		for _, l := range ls {
			fmt.Printf("lease %d: %s, %d h, %s → %s, paid %s %s (tx %s)\n", l.LeaseID, l.BuyerSesh, l.Hours,
				time.Unix(int64(l.Start), 0).UTC().Format(time.RFC3339), time.Unix(int64(l.End), 0).UTC().Format(time.RFC3339),
				l.Amount, assetName(l.Asset), l.Tx.Hex())
		}
	case "watch":
		if *exitSesh == "" || *issuer == "" || *store == "" {
			log.Fatal("watch needs -exit-sesh, -issuer-key and -store")
		}
		key, err := exits.IssuerKey(*issuer, false)
		check(err)
		var pay eth.Address
		if *payout != "" {
			pay, err = eth.ParseAddress(*payout)
			check(err)
		}
		info, err := rc.Info(ctx, needID())
		check(err)
		if info.Tag != exits.TagExit {
			log.Fatalf("listing %d has tag %q, not exit", *id, info.Tag)
		}
		w := &exits.Watcher{K: k, ListingID: *id, ExitSesh: strings.ToLower(strings.TrimSpace(*exitSesh)), Payout: pay,
			Key: key, Store: &exits.Store{Path: *store}, StatePath: *state, FromBlock: fromEL, Confirmations: *confs,
			Log: log.Printf}
		log.Printf("watching ExitLease %s for listing %d (exit %s); admissions in %s; issuer %s",
			elAddr.Hex(), *id, w.ExitSesh, *store, exits.PubHex(key))
		if *once {
			res, err := w.Step(ctx)
			check(err)
			for _, a := range res {
				r := a.Reason
				if r == "" {
					r = "admitted"
				}
				fmt.Printf("lease %d %s: %s\n", a.Lease.LeaseID, a.Lease.BuyerSesh, r)
			}
			return
		}
		_ = w.Run(ctx, 4*time.Second)
	default:
		fmt.Fprintln(os.Stderr, exitsUsage)
		os.Exit(2)
	}
}

func leasesChunked(ctx context.Context, k *exits.Contract, from, to, id uint64) ([]exits.Lease, error) {
	var out []exits.Lease
	for from <= to {
		end := from + 50000 - 1
		if end > to {
			end = to
		}
		ls, err := k.Leases(ctx, from, end, id, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, ls...)
		from = end + 1
	}
	return out, nil
}

func assetName(a eth.Address) string {
	if a.IsZero() {
		return "wei"
	}
	return "units of " + a.Hex()
}

func parseDec(s string, dec int) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("bad amount %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)))
	if !r.IsInt() {
		return nil, fmt.Errorf("too many decimals in %q", s)
	}
	return r.Num(), nil
}

func fmtDec(x *big.Int, dec int) string {
	r := new(big.Rat).SetFrac(x, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil))
	s := r.FloatString(dec)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" {
		return "0"
	}
	return s
}

type watchdogOpts struct {
	ns, rpc, exit, dns, host, restart, restartProbe string
	timeout, grace, gap, every                      time.Duration
	fails                                           int
	once                                            bool
}

func runWatchdog(o watchdogOpts) {
	if o.rpc == "" || o.exit == "" || o.restart == "" || o.restartProbe == "" {
		log.Fatal("watchdog needs -probe-rpc, -exit-sesh, -restart and -restart-probe")
	}
	shell := func(what, cmd string) func(context.Context) error {
		return func(ctx context.Context) error {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			out, err := exec.CommandContext(cctx, "/bin/sh", "-c", cmd).CombinedOutput()
			if len(out) > 0 {
				log.Printf("watchdog: %s output: %s", what, strings.TrimSpace(string(out)))
			}
			return err
		}
	}
	rr := &exits.RouterRPC{Endpoint: o.rpc}
	wp := &exits.WebProbe{
		DNS: o.dns, Host: o.host, Timeout: o.timeout,
		Dial: func(network, addr string, t time.Duration) (net.Conn, error) {
			return exits.DialInNetNS(o.ns, network, addr, t)
		},
		Map: func(ctx context.Context, ranges []string) error {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			_, err := rr.MapRanges(cctx, o.exit, ranges)
			return err
		},
	}
	w := &exits.Watchdog{
		Probe:        wp.Check,
		Remap:        wp.Remap,
		RestartProbe: shell("probe restart", o.restartProbe),
		Restart:      shell("restart", o.restart),
		Fails:        o.fails, Grace: o.grace, MinGap: o.gap, Log: log.Printf,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Printf("watchdog: fetching https://%s/ through %s (resolver %s) from namespace %s every %s (restart after %d failures, grace %s, min gap %s)",
		o.host, o.exit, o.dns, o.ns, o.every, o.fails, o.grace, o.gap)
	if o.once {
		fmt.Println(w.Step(ctx))
		return
	}
	_ = w.Run(ctx, o.every)
}

// rpcHost is an RPC URL without its path or query, which may hold an API key.
func rpcHost(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Scheme + "://" + p.Host
	}
	return "rpc"
}
