// Command seshswap is the one-download Linux build of the SeshSwap wallet.
//
// It runs the wallet page on 127.0.0.1 with the testnet settings built in,
// starts the bundled monero-wallet-rpc (stagenet) and stops it on exit, uses
// the bundled Session Router for private swaps, and opens your browser.
// Nothing to edit: an optional config.json in the data dir (~/.seshswap)
// overrides any setting.
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
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
			os.Exit(check())
		case "stop":
			os.Exit(stopRunning(dataDirFromArgs(os.Args[2:])))
		case "version", "--version", "-version":
			fmt.Println("seshswap", version)
			return
		}
	}
	os.Exit(run())
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

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".seshswap"
	}
	return filepath.Join(home, ".seshswap")
}

func dataDirFromArgs(args []string) string {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	d := fs.String("data", defaultDataDir(), "data directory")
	_ = fs.Parse(args)
	return *d
}

func run() int {
	cfgPath := flag.String("config", "", "config JSON with overrides (default: <data>/config.json if it exists)")
	data := flag.String("data", "", "data directory (default ~/.seshswap)")
	listen := flag.String("listen", "", "page address, 127.0.0.1:port (default 127.0.0.1:7676)")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser; just print the address")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `SeshSwap %s (testnet)

  seshswap                 start the wallet and open it in your browser
  seshswap setup-router    one-time permission for private swaps (asks for sudo)
  seshswap stop            stop a running SeshSwap
  seshswap check           check that the bundled programs run on this computer
  seshswap version

Options:
`, version)
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

	// Bundled Session Router.
	srDir := filepath.Join(bdir, "session-router")
	if cfg.SessionRouter == nil {
		cfg.SessionRouter = &wallet.RouterConfig{}
	}
	if cfg.SessionRouter.Binary == "" && fileExists(filepath.Join(srDir, "bin", "session-router")) {
		cfg.SessionRouter.Binary = filepath.Join(srDir, "bin", "session-router")
		if _, err := relocRouter(srDir); err != nil {
			log.Printf("Session Router: %v", err)
		}
		if ok, _ := srouter.FileHasNetAdmin(cfg.SessionRouter.Binary); !ok && !srouter.CanCreateTUN() {
			fmt.Println("Private swaps need a one-time permission for Session Router. Run:")
			fmt.Println("  " + selfCommand() + " setup-router")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// Monero: the bundled monero-wallet-rpc, unless the config points at one.
	var mon *moneroRPC
	if cfg.XmrWalletRPC == "" {
		bin := cfg.MoneroWalletRPC
		if bin == "" {
			bin = filepath.Join(bdir, "monero", "monero-wallet-rpc")
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

// openBrowser uses xdg-open (or the Debian/Ubuntu fallbacks). Without one
// that works, the address printed above is enough.
func openBrowser(u string) {
	for _, c := range []string{"xdg-open", "sensible-browser", "x-www-browser"} {
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		cmd := exec.Command(p, u)
		if err := cmd.Start(); err != nil {
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil {
				return
			}
		case <-time.After(3 * time.Second):
			return // still running: a browser is opening
		}
	}
	fmt.Println("Open this address in your browser: " + u)
}

func stopRunning(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "seshswap.pid"))
	if err != nil {
		fmt.Println("SeshSwap isn't running.")
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		fmt.Println("SeshSwap isn't running.")
		return 0
	}
	// Only signal it if it really is seshswap.
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err != nil || !strings.HasPrefix(filepath.Base(exe), "seshswap") {
		fmt.Println("SeshSwap isn't running.")
		return 0
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintln(os.Stderr, "seshswap stop:", err)
		return 1
	}
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			fmt.Println("SeshSwap stopped.")
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("SeshSwap is still stopping.")
	return 0
}

// setupRouter gives the bundled Session Router cap_net_admin so it can make
// its own network adapter while running as you. Needs sudo once (and again
// if the folder moves).
func setupRouter() int {
	srDir := filepath.Join(bundleDir(), "session-router")
	bin := filepath.Join(srDir, "bin", "session-router")
	if !fileExists(bin) {
		fmt.Fprintln(os.Stderr, "No bundled Session Router in", srDir)
		return 1
	}
	if _, err := relocRouter(srDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if ok, _ := srouter.FileHasNetAdmin(bin); ok {
		fmt.Println("Already set up. Private swaps are ready.")
		return 0
	}
	fmt.Println("Session Router needs permission to create its own network adapter.")
	fmt.Println("This runs once with sudo:")
	var cmd *exec.Cmd
	if p, err := exec.LookPath("setcap"); err == nil {
		fmt.Println("  sudo setcap cap_net_admin+ep " + bin)
		cmd = exec.Command("sudo", p, "cap_net_admin+ep", bin)
	} else if p, err := exec.LookPath("python3"); err == nil {
		// No libcap tools: write the same xattr setcap would (v2, cap_net_admin, +ep).
		fmt.Println("  (setcap isn't installed; writing the same capability with python3)")
		cmd = exec.Command("sudo", p, "-c", `import os,struct,sys; os.setxattr(sys.argv[1],"security.capability",struct.pack("<IIIII",0x02000001,1<<12,0,0,0))`, bin)
	} else {
		fmt.Println("  sudo apt install libcap2-bin && sudo setcap cap_net_admin+ep " + bin)
		return 1
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "setup-router:", err)
		return 1
	}
	if ok, _ := srouter.FileHasNetAdmin(bin); !ok {
		fmt.Fprintln(os.Stderr, "The permission didn't stick. The folder may be on a drive that doesn't support it; move it to your home folder and try again.")
		return 1
	}
	fmt.Println("Done. Session Router still runs as you, not root.")
	return 0
}

var errNoDaemon = errors.New("no Monero node answered")

// check runs the bundled programs once to show they work on this computer.
func check() int {
	bdir := bundleDir()
	srDir := filepath.Join(bdir, "session-router")
	bad := 0
	if _, err := relocRouter(srDir); err != nil {
		fmt.Println("session-router: ", err)
		bad++
	}
	for _, c := range []struct{ name, bin, arg string }{
		{"session-router", filepath.Join(srDir, "bin", "session-router"), "--version"},
		{"monero-wallet-rpc", filepath.Join(bdir, "monero", "monero-wallet-rpc"), "--version"},
	} {
		out, err := exec.Command(c.bin, c.arg).CombinedOutput()
		line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if err != nil {
			fmt.Printf("%-18s FAILED: %v %s\n", c.name, err, line)
			bad++
			continue
		}
		fmt.Printf("%-18s ok  %s\n", c.name, line)
	}
	bin := filepath.Join(srDir, "bin", "session-router")
	if ok, _ := srouter.FileHasNetAdmin(bin); ok {
		fmt.Printf("%-18s ok  private swaps are set up\n", "permission")
	} else {
		fmt.Printf("%-18s not set up yet: run %s setup-router\n", "permission", selfCommand())
	}
	if bad > 0 {
		return 1
	}
	return 0
}
