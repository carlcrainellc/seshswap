// Command wallet is the standalone ETH/USDC/XMR swap wallet: a local page on
// 127.0.0.1 that holds the keys, shows balances, and runs ETH→XMR swaps
// through the existing swapd engine (including crash recovery).
//
// Test-only. Not audited. Refuses Ethereum and Monero mainnet unless
// explicitly overridden, same as swapd.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/sysdns"
	"github.com/carlcrainellc/seshswap/swapd/internal/wallet"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == sysdns.HelperArg {
		// Run as root through sudo -n: change or restore system DNS for an
		// exit (see internal/sysdns).
		os.Exit(sysdns.RunHelper(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == srouter.GuardArg {
		// Child mode: keep the bundled Session Router tied to this wallet's
		// lifetime (see srouter.RunGuard). Never returns.
		srouter.RunGuard(os.Args[2:])
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("wallet: ")
	cfgPath := flag.String("config", "", "config JSON (eth RPCs, contract, maker URL, Monero wallet-rpc)")
	listen := flag.String("listen", "", "override listen address (must be 127.0.0.1:port)")
	data := flag.String("data", "", "override wallet data directory")
	demo := flag.Bool("demo", false, "sample quotes and balances for screenshots; no real funds")
	satchel := flag.String("satchel-test", "", "serve a Satchel stand-in page on this 127.0.0.1:port")
	flag.Parse()

	cfg := wallet.Config{Listen: "127.0.0.1:7676", Demo: *demo}
	if *cfgPath != "" {
		b, err := os.ReadFile(*cfgPath)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Fatal(err)
		}
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *data != "" {
		cfg.DataDir = *data
	}
	if *demo {
		cfg.Demo = true
	}
	if *satchel != "" {
		cfg.SatchelTest = *satchel
	}
	if cfg.DataDir == "" {
		cfg.DataDir = wallet.StorePath("")
	}
	cfg.ApplyChain()
	log.Printf("chain %s (id %d) explorer %s contract %s", cfg.Chain, cfg.ChainID, cfg.Explorer, cfg.Contract.Hex())

	s := wallet.NewServer(cfg)
	if err := wallet.WriteAPIFile(cfg.DataDir, cfg.Listen); err != nil {
		log.Print(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.ListenAndServe(ctx); err != nil {
		log.Fatal(err)
	}
}
