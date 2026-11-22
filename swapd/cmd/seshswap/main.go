// Command seshswap is the one-download build of the SeshSwap wallet, for
// Linux and for Windows.
//
// It runs the wallet page on 127.0.0.1 with the testnet settings built in,
// starts the bundled monero-wallet-rpc (stagenet) and stops it on exit, uses
// a Session Router for private swaps (on Linux the bundled one, on Windows
// Rusty's service; see os_linux.go and os_windows.go), and opens your browser.
// Nothing to edit: an optional config.json in the data dir overrides any setting.
//
// Testnets only (Arbitrum Sepolia, Sepolia, Monero stagenet). Not audited.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	// Mozilla roots, used only when the system has no CA bundle (minimal installs).
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/sysdns"
	"github.com/carlcrainellc/seshswap/swapd/internal/wallet"
)

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

// Public Monero stagenet nodes, tried in order; the first one that answers
// as a synced stagenet node is used. Override with "xmrDaemonRpc" (one node)
// or "xmrDaemons" (your own list) in config.json.
var defaultDaemons = []string{
	"http://node.monerodevs.org:38089",
	"http://node2.monerodevs.org:38089",
	"http://node3.monerodevs.org:38089",
	"http://stagenet.xmr-tw.org:38081",
	"http://xmr-lux.boldsuck.org:38081",
}

// stagenetFloor is a restore height used only when no node answers at first
// start, so a new wallet doesn't scan stagenet from block 1 (about 1 Nov
// 2026; no SeshSwap wallet is older).
const stagenetFloor = 2240000

// config is the wallet config plus a few launcher-only keys.
type config struct {
	wallet.Config
	XmrDaemons      []string `json:"xmrDaemons,omitempty"`      // stagenet nodes to try (default: defaultDaemons)
	MoneroWalletRPC string   `json:"moneroWalletRpc,omitempty"` // path to monero-wallet-rpc (default: the bundled one)
	OpenBrowser     *bool    `json:"openBrowser,omitempty"`     // default true
	MoneroLogLevel  int      `json:"moneroLogLevel,omitempty"`  // monero-wallet-rpc --log-level (0-4), default 0
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case sysdns.HelperArg:
			os.Exit(sysdns.RunHelper(os.Args[2:]))
		case srouter.GuardArg:
			srouter.RunGuard(os.Args[2:])
		case "setup-router":
			os.Exit(setupRouter())
		case "check":
			code := check()
			pauseIfOwnConsole(true)
			os.Exit(code)
		case "stop":
			os.Exit(stopRunning(dataDirFromArgs(os.Args[2:])))
		case "version", "--version", "-version":
			fmt.Println("seshswap", version)
			return
		}
	}
	code := run()
	pauseIfOwnConsole(code != 0)
	os.Exit(code)
}

func bundleDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Dir(exe)
}

func dataDirFromArgs(args []string) string {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	d := fs.String("data", defaultDataDir(), "data directory")
	_ = fs.Parse(args)
	return *d
}

func run() int {
	cfgPath := flag.String("config", "", "config JSON with overrides (default: <data>/config.json if it exists)")
	data := flag.String("data", "", "data directory (default "+defaultDataDir()+")")
	listen := flag.String("listen", "", "page address, 127.0.0.1:port (default 127.0.0.1:7676)")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser; just print the address")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "SeshSwap %s (testnet)\n\n%s\nOptions:\n", version, usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	dir := *data
	if dir == "" {
		dir = defaultDataDir()
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "seshswap:", err)
		return 1
	}
	logf, err := os.OpenFile(filepath.Join(dir, "logs", "seshswap.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		defer logf.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, logf))
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("seshswap: ")

	bdir := bundleDir()
	cfg := config{Config: wallet.Config{Listen: "127.0.0.1:7676"}}
	path := *cfgPath
	if path == "" {
		if p := filepath.Join(dir, "config.json"); fileExists(p) {
			path = p
		}
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			log.Print(err)
			return 1
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Printf("%s: %v", path, err)
			return 1
		}
		log.Printf("settings from %s", path)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	cfg.DataDir = dir
	open := !*noBrowser && (cfg.OpenBrowser == nil || *cfg.OpenBrowser)
	pageURL := "http://" + cfg.Listen + "/"

	// Already running? Open that one instead of starting a second copy.
	if ln, err := net.Listen("tcp", cfg.Listen); err != nil {
		if pageUp(pageURL, 2*time.Second) {
			fmt.Println("SeshSwap is already running at", pageURL)
			if open {
				openBrowser(pageURL)
			}
			return 0
		}
		log.Printf("can't use %s: %v (another program has that port; start with -listen 127.0.0.1:<port>)", cfg.Listen, err)
		return 1
	} else {
		ln.Close()
	}

	platformRouter(&cfg, bdir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	watchStop(ctx, filepath.Join(dir, "stop-request"), stop)

	// Monero: the bundled monero-wallet-rpc, unless the config points at one.
	var mon *moneroRPC
	if cfg.XmrWalletRPC == "" {
		bin := cfg.MoneroWalletRPC
		if bin == "" {
			bin = filepath.Join(bdir, "monero", "monero-wallet-rpc"+exeSuffix)
		}
		if cfg.XmrNet == "" {
			cfg.XmrNet = "stagenet"
		}
		daemons := cfg.XmrDaemons
		if cfg.XmrDaemonRPC != "" {
			daemons = []string{cfg.XmrDaemonRPC}
		} else if len(daemons) == 0 {
			daemons = defaultDaemons
		}
		d, err := pickDaemon(ctx, daemons, string(cfg.XmrNet))
		if err != nil {
			log.Printf("Monero: %v; trying %s anyway (XMR balance stays empty until a node answers)", err, daemons[0])
			d = daemons[0]
			if cfg.XmrRestoreHeight == 0 && cfg.XmrNet == "stagenet" {
				cfg.XmrRestoreHeight = stagenetFloor
			}
		} else {
			log.Printf("Monero %s node: %s", cfg.XmrNet, d)
		}
		cfg.XmrDaemonRPC = d
		mon, err = startMonero(ctx, bin, dir, d, string(cfg.XmrNet), cfg.MoneroLogLevel)
		if err != nil {
			log.Printf("Monero wallet: %v (ETH and USDC still work; XMR and swaps won't)", err)
		} else {
			cfg.XmrWalletRPC = mon.URL()
			defer mon.Stop()
		}
	}

	cfg.ApplyChain()
	log.Printf("SeshSwap %s, %s (chain id %d)", version, cfg.ChainLabel, cfg.ChainID)
	cfg.AppVersion = version
	consoleTitle("SeshSwap " + version)
	s := wallet.NewServer(cfg.Config)
	if err := wallet.WriteAPIFile(cfg.DataDir, cfg.Listen); err != nil {
		log.Print(err)
	}
	pidFile := filepath.Join(dir, "seshswap.pid")
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	defer os.Remove(pidFile)

	go func() {
		if !pageUp(pageURL, 20*time.Second) {
			return
		}
		fmt.Println()
		fmt.Println("SeshSwap is running: " + pageURL)
		fmt.Println("Keep this running while you use the wallet. Ctrl-C (or `seshswap stop`) quits.")
		if open {
			openBrowser(pageURL)
		}
	}()
	err = s.ListenAndServe(ctx)
	s.LockWallet()
	mon.Stop() // also deferred, for early returns; a second call does nothing
	if err != nil {
		log.Print(err)
		return 1
	}
	log.Print("stopped")
	return 0
}

func selfCommand() string {
	exe, err := os.Executable()
	if err != nil {
		return "./seshswap"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func pageUp(u string, wait time.Duration) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	end := time.Now().Add(wait)
	for {
		if resp, err := c.Get(u); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

var errNoDaemon = errors.New("no Monero node answered")
