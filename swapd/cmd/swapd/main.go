// swapd: ETH/ERC-20 <-> XMR atomic swap daemon (TEST-ONLY, NOT AUDITED).
//
//	swapd maker   -config maker.json            run an always-on maker (XMR seller): offer API,
//	                                            live price, limits, health/metrics + watchtower
//	swapd offer-doc -config maker.json          print the maker's signed offer document (JSON)
//	swapd take    -config taker.json -maker URL -value WEI -min-xmr PICO [-maker-id HEX]
//	swapd relayer -config relayer.json          run a gas relayer
//	swapd status  -config any.json              list swaps in the store
//	swapd exit-pay-settle -config F SWAP_ID     buy the exit time a claimed swap paid for
//	swapd resume  -config taker.json            keep driving saved swaps until all are finished
//	swapd registry SUBCOMMAND -config F ...     register / top up / withdraw a SeshRegistry listing
//	swapd exits SUBCOMMAND -config F ...        sell exit time: offer, admission watcher, list (see exits_cmd.go)
//
// Secrets come from the environment, never from config files:
//
//	SWAPD_PASSPHRASE   store encryption passphrase (also the Monero wallet-file password)
//	SWAPD_ETH_KEY      hex secp256k1 key (taker, maker gas key and relayer: required)
//	SWAPD_REGISTRY_KEY hex key that owns a SeshRegistry listing (maker with registry.listingId; registry CLI)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/carlcrainellc/seshswap/swapd/internal/exitpay"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/chaincfg"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/price"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
	"github.com/carlcrainellc/seshswap/swapd/internal/relay"
	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

type Config struct {
	EthRPC       string              `json:"ethRpc"`
	EthRPCs      []string            `json:"ethRpcs,omitempty"` // fallback endpoints; tried on rate limit
	Contract     eth.Address         `json:"contract"`
	Chain        string              `json:"chain,omitempty"`   // "arbitrum-sepolia" (default) or "sepolia"
	ChainID      int64               `json:"chainId,omitempty"` // expected EVM chain id; 0 = from the chain table
	USDC         eth.Address         `json:"usdc,omitempty"`
	XMRUSDFeed   eth.Address         `json:"xmrUsdFeed,omitempty"`
	ETHUSDFeed   eth.Address         `json:"ethUsdFeed,omitempty"`
	Explorer     string              `json:"explorer,omitempty"`
	XmrWalletRPC string              `json:"xmrWalletRpc"`
	XmrNet       keys.Network        `json:"xmrNet"`
	MainWallet   string              `json:"mainWallet"`
	StoreDir     string              `json:"storeDir"`
	Listen       string              `json:"listen"`  // maker/relayer; bind to 127.0.0.1 (Tor) or the Session Router fd2e:… interface (.sesh)
	SeshDNS      string              `json:"seshDns"` // Session Router DNS for .sesh URLs (default 127.0.0.1:53); never the system resolver
	Relayers     []string            `json:"relayers"`
	PayoutTo     eth.Address         `json:"payoutTo"`
	Policy       *swap.Policy        `json:"policy"`
	Offer        *api.Offer          `json:"offer"`
	RelayMinFeeX float64             `json:"relayMinFeeMultiplier"`
	RelayTokens  map[string]*big.Int `json:"relayTokenMinFee"`
	Poll         string              `json:"poll"`
	XmrDaemonRPC string              `json:"xmrDaemonRpc"`        // monerod: measured block time for windows, and reorg depth
	XmrBlockSecs uint64              `json:"xmrBlockSecs"`        // explicit mean block seconds if no daemon; 0 refuses to assume 120 s
	Service      *ServiceConfig      `json:"service"`             // maker: always-on service options
	Registry     *RegistryConfig     `json:"registry,omitempty"`  // SeshRegistry (maker listing, CLI)
	ExitLease    *ExitLeaseConfig    `json:"exitLease,omitempty"` // ExitLease (exit seller kit, CLI)
	ExitPay      *exitpay.Target     `json:"exitPay,omitempty"`   // maker: claims buy exit time (ExitPay)
	AllowMainnet bool                `json:"iUnderstandThisIsUnauditedAndMayLoseFunds"`
}

// ServiceConfig is the always-on maker's pricing and limits.
type ServiceConfig struct {
	LivePrice     bool     `json:"livePrice"`    // quote from public price sources (Kraken, CoinGecko, Bitfinex, KuCoin)
	MinSources    int      `json:"minSources"`   // sources that must agree (>= 2)
	MaxDeviation  float64  `json:"maxDeviation"` // e.g. 0.02
	MaxJump       float64  `json:"maxJump"`      // e.g. 0.10
	MinXmrPerEth  float64  `json:"minXmrPerEth"` // sanity range
	MaxXmrPerEth  float64  `json:"maxXmrPerEth"`
	AssetDecimals int      `json:"assetDecimals"` // >0: asset is a USD stablecoin with these decimals
	MaxExposure   uint64   `json:"maxExposure"`   // piconero across all unfinished swaps (0 = no limit)
	MaxOpenQuotes uint64   `json:"maxOpenQuotes"` // concurrent MNegotiated quotes (0 = no limit)
	Endpoints     []string `json:"endpoints"`     // advertised in the signed offer document
	PriceEvery    string   `json:"priceEvery"`    // refresh period (default 30s)
}

// production chains this prototype refuses unless explicitly overridden
var mainnets = map[int64]string{1: "Ethereum", 10: "Optimism", 100: "Gnosis", 8453: "Base", 42161: "Arbitrum One"}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: swapd maker|offer-doc|take|relayer|status|resume -config FILE [flags]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	if cmd == "registry" {
		runRegistry(os.Args[2:])
		return
	}
	if cmd == "exits" {
		runExits(os.Args[2:])
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "", "config JSON")
	makerURL := fs.String("maker", "", "take: maker URL")
	value := fs.String("value", "", "take: amount in asset base units (wei)")
	minXmr := fs.Uint64("min-xmr", 0, "take: refuse quotes below this many piconero")
	minConfs := fs.Uint64("min-confs", swap.XmrConfFloor, "take: least XMR confirmations before marking ready; asked of the maker (never below 10; scales up with amount and reorg depth)")
	makerID := fs.String("maker-id", "", "take: pin the maker identity (hex ed25519 key from its signed offer document)")
	_ = fs.Parse(os.Args[2:])
	cfg := mustConfig(*cfgPath)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// .sesh MakerURL and relayers entries are dialled only through Session Router DNS.
	ht := sesh.New(sesh.Config{DNS: cfg.SeshDNS}).Client(60 * time.Second)
	api.HTTP, relay.HTTP = ht, ht

	c := eth.NewClients(append([]string{cfg.EthRPC}, cfg.EthRPCs...)...)
	cid, err := c.ChainID(ctx)
	check(err)
	if name, ok := mainnets[cid.Int64()]; ok && !cfg.AllowMainnet {
		log.Fatalf("refusing to run on %s mainnet: this software is unaudited", name)
	}
	if cfg.ChainID != 0 && cid.Int64() != cfg.ChainID {
		log.Fatalf("RPC chain id %d does not match configured %d (%s)", cid.Int64(), cfg.ChainID, cfg.Chain)
	}
	if cfg.XmrNet == keys.Mainnet && !cfg.AllowMainnet {
		log.Fatal("refusing Monero mainnet: this software is unaudited")
	}
	k := &eth.Contract{C: c, Addr: cfg.Contract, ChainID: cid}
	var ethKey *eth.Key
	if h := os.Getenv("SWAPD_ETH_KEY"); h != "" {
		ethKey, err = eth.KeyFromHex(h)
		check(err)
	}
	poll := 10 * time.Second
	if cfg.Poll != "" {
		poll, err = time.ParseDuration(cfg.Poll)
		check(err)
	}

	if cmd == "relayer" {
		if ethKey == nil {
			log.Fatal("relayer needs SWAPD_ETH_KEY")
		}
		tm := map[eth.Address]*big.Int{}
		for a, v := range cfg.RelayTokens {
			addr, err := eth.ParseAddress(a)
			check(err)
			tm[addr] = v
		}
		mult := cfg.RelayMinFeeX
		if mult == 0 {
			mult = 1.2
		}
		srv := &relay.Server{K: k, Key: ethKey, MinFeeMultiplier: mult, TokenMinFee: tm}
		log.Printf("relayer %s listening on %s", ethKey.Address().Hex(), cfg.Listen)
		check(newServer(cfg.Listen, logRemote(srv)).ListenAndServe())
		return
	}

	pass := os.Getenv("SWAPD_PASSPHRASE")
	st, err := store.Open(cfg.StoreDir, pass)
	check(err)
	pol := swap.DefaultPolicy()
	if cfg.Policy != nil {
		pol = *cfg.Policy
	}
	if cmd == "maker" || cmd == "offer-doc" {
		if cfg.Offer == nil || cfg.PayoutTo.IsZero() {
			log.Fatal("maker needs offer and payoutTo")
		}
		swap.ApplyOfferRelayFee(&pol, cfg.Offer.Asset, cfg.Offer.RelayFee)
		if err := swap.ValidateMakerRelay(pol, cfg.Offer.Asset, cfg.Offer.Min, ethKey != nil); err != nil {
			log.Fatal(err)
		}
		cfg.Offer.RelayFee = pol.FeeFor(cfg.Offer.Asset)
	}
	role := swap.Taker
	if cmd == "maker" {
		role = swap.Maker
	}
	main := cfg.MainWallet
	if main == "" {
		main = "main"
	}
	eng := &swap.Engine{Role: role, Store: st, K: k, EthKey: ethKey, Wallet: xmr.NewWallet(cfg.XmrWalletRPC, pass),
		MainWal: main, XmrNet: cfg.XmrNet, Relayers: cfg.Relayers, Policy: pol, OnLocked: api.NotifyLocked}
	if cfg.XmrDaemonRPC != "" {
		eng.Daemon = xmr.NewDaemon(cfg.XmrDaemonRPC)
	}
	eng.AssumeBlockSecs = cfg.XmrBlockSecs
	check(eng.Load()) // crash recovery: resume every unfinished swap

	switch cmd {
	case "status":
		for _, id := range eng.IDs() {
			ph, lg, _ := eng.Get(id)
			last := ""
			if len(lg) > 0 {
				last = lg[len(lg)-1]
			}
			fmt.Printf("%s  %-12s %s\n", id, ph, last)
		}
	case "exit-pay-settle":
		sid := fs.Arg(0)
		if sid == "" {
			log.Fatal("usage: swapd exit-pay-settle -config F SWAP_ID")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		check(eng.SettleExitPay(ctx, sid))
		ph, lg, _ := eng.Get(sid)
		fmt.Println(ph, lg[len(lg)-1])
	case "maker", "offer-doc":
		if cfg.Offer == nil || cfg.PayoutTo.IsZero() {
			log.Fatal("maker needs offer and payoutTo")
		}
		cfg.Offer.ChainID, cfg.Offer.Contract, cfg.Offer.XmrNet = cid, cfg.Contract, cfg.XmrNet
		m := &api.Maker{Offer: *cfg.Offer, Engine: eng, PayoutTo: cfg.PayoutTo}
		if ep := cfg.ExitPay; ep != nil {
			if ep.RefundTo.IsZero() {
				ep.RefundTo = cfg.PayoutTo
			}
			ep.Asset = cfg.Offer.Asset
			check(ep.Check())
			m.ExitPay = ep
			log.Printf("claims buy %d h of exit time on listing %d for %s (max %s, change to %s)",
				ep.Hours, ep.ListingID, ep.BuyerSesh, ep.MaxCost, ep.RefundTo.Hex())
		}
		id, err := api.LoadIdentity(st)
		check(err)
		m.Identity = id
		if sc := cfg.Service; sc != nil {
			m.AssetDecimals, m.MaxExposure, m.MaxOpenQuotes, m.Endpoints = sc.AssetDecimals, sc.MaxExposure, sc.MaxOpenQuotes, sc.Endpoints
			if sc.LivePrice {
				m.Oracle = price.New(price.Config{MinSources: sc.MinSources, MaxDeviation: sc.MaxDeviation, MaxJump: sc.MaxJump,
					MinXmrPerEth: sc.MinXmrPerEth, MaxXmrPerEth: sc.MaxXmrPerEth})
				every := 30 * time.Second
				if sc.PriceEvery != "" {
					every, err = time.ParseDuration(sc.PriceEvery)
					check(err)
				}
				if err := m.Oracle.Refresh(ctx); err != nil {
					log.Printf("price: %v (quotes stay off until sources agree)", err)
				}
				go m.Oracle.Run(ctx, every, log.Printf)
			}
		}
		if cmd == "offer-doc" {
			d, err := m.OfferDoc()
			check(err)
			_ = json.NewEncoder(os.Stdout).Encode(d)
			return
		}
		log.Printf("maker identity %s", id.ID())
		if rcfg := cfg.Registry; rcfg != nil && rcfg.ListingID != 0 {
			h := os.Getenv("SWAPD_REGISTRY_KEY")
			if h == "" {
				log.Fatal("registry.listingId is set: the maker needs SWAPD_REGISTRY_KEY (the listing owner key)")
			}
			rk, err := eth.KeyFromHex(h)
			check(err)
			m.Registry = &api.MakerRegistry{Client: &registry.Client{C: c, Addr: rcfg.Contract}, ChainID: cid,
				ListingID: rcfg.ListingID, Key: rk, AttestWindow: rcfg.AttestWindow, Log: log.Printf}
			info, err := m.Registry.Client.Info(ctx, rcfg.ListingID)
			check(err)
			if info.Owner != rk.Address() || info.Tag != registry.TagMaker {
				log.Fatalf("registry listing %d is not a maker listing owned by %s", rcfg.ListingID, rk.Address().Hex())
			}
			log.Printf("registry %s listing %d: bond %s ETH, max swap %s ETH", rcfg.Contract.Hex(), rcfg.ListingID,
				registry.FormatWei(info.Bond), registry.FormatWei(info.MaxQuote))
			go m.Registry.RunAcker(ctx, eng, poll)
		}
		go eng.Run(ctx, poll)
		log.Printf("maker listening on %s (publish via a Tor onion service or a Session Router .sesh address)", cfg.Listen)
		srv := newServer(cfg.Listen, logRemote(m))
		go func() { <-ctx.Done(); _ = srv.Close() }()
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			check(err)
		}
	case "resume":
		for {
			eng.StepAll(ctx)
			open := 0
			for _, id := range eng.IDs() {
				if ph, _, _ := eng.Get(id); !ph.Terminal() {
					open++
				}
			}
			if open == 0 {
				log.Print("all swaps finished")
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(poll):
			}
		}
	case "take":
		if ethKey == nil {
			log.Fatal("taker needs SWAPD_ETH_KEY")
		}
		v, ok := new(big.Int).SetString(*value, 10)
		if !ok || *makerURL == "" || *minXmr == 0 {
			log.Fatal("take needs -maker, -value and -min-xmr")
		}
		rec, err := api.Take(ctx, eng, api.TakeParams{MakerURL: *makerURL, Value: v, MinXMR: *minXmr,
			MinD1: 2*pol.Margin + 1800, MinD2: 2 * pol.Margin, MaxConfs: 60, MinConfs: max(*minConfs, swap.XmrConfFloor),
			MakerID: *makerID})
		check(err)
		log.Printf("swap %s agreed: %s units -> %d piconero (premium %s wei, %d Monero confirmations, freeze %ds); shared address %s",
			rec.ID, v, rec.XMR, rec.Premium, rec.MinConfs, rec.Grace, rec.SharedAddr)
		for {
			eng.StepAll(ctx)
			ph, _, _ := eng.Get(rec.ID)
			if ph.Terminal() {
				log.Printf("swap %s finished: %s", rec.ID, ph)
				return
			}
			select {
			case <-ctx.Done():
				log.Printf("interrupted; state is saved — run `swapd resume` — state is saved")
				return
			case <-time.After(poll):
			}
		}
	default:
		log.Fatalf("unknown command %q", cmd)
	}
}

// logRemote logs the remote address of every HTTP request so we can verify
// .sesh clients arrive via Session Router (fd2e:… / 172.16.x) and not a public IP.
func logRemote(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("http %s %s remote=%s xff=%q host=%s", r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("X-Forwarded-For"), r.Host)
		h.ServeHTTP(w, r)
	})
}

func mustConfig(p string) *Config {
	if p == "" {
		log.Fatal("-config is required")
	}
	b, err := os.ReadFile(p)
	check(err)
	var c Config
	check(json.Unmarshal(b, &c))
	applyChain(&c)
	return &c
}

// applyChain fills empty RPC and contract fields from the built-in chain
// table. The default is Arbitrum Sepolia; a legacy Sepolia config is kept.
func applyChain(c *Config) {
	ch := chaincfg.Resolve(chaincfg.Override{
		Chain:      c.Chain,
		EthRPC:     c.EthRPC,
		EthRPCs:    c.EthRPCs,
		Contract:   addrIf(c.Contract),
		USDC:       addrIf(c.USDC),
		XMRUSDFeed: addrIf(c.XMRUSDFeed),
		ETHUSDFeed: addrIf(c.ETHUSDFeed),
		Explorer:   c.Explorer,
	})
	c.Chain = ch.Key
	if c.ChainID == 0 {
		c.ChainID = ch.ID
	}
	if c.Explorer == "" {
		c.Explorer = ch.Explorer
	}
	if c.EthRPC == "" {
		c.EthRPC = ch.EthRPC
		if len(c.EthRPCs) == 0 {
			c.EthRPCs = append([]string{}, ch.EthRPCs...)
		}
	}
	if c.Contract.IsZero() && ch.Contract != "" {
		c.Contract, _ = eth.ParseAddress(ch.Contract)
	}
	if c.USDC.IsZero() && ch.USDC != "" {
		c.USDC, _ = eth.ParseAddress(ch.USDC)
	}
	if c.XMRUSDFeed.IsZero() && ch.XMRUSDFeed != "" {
		c.XMRUSDFeed, _ = eth.ParseAddress(ch.XMRUSDFeed)
	}
	if c.ETHUSDFeed.IsZero() && ch.ETHUSDFeed != "" {
		c.ETHUSDFeed, _ = eth.ParseAddress(ch.ETHUSDFeed)
	}
	log.Printf("chain %s (id %d) contract %s", c.Chain, c.ChainID, c.Contract.Hex())
}

func addrIf(a eth.Address) string {
	if a.IsZero() {
		return ""
	}
	return a.Hex()
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
