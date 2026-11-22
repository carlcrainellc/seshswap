package wallet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/chaincfg"
	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/feed"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// Config is the wallet daemon's startup configuration (JSON file + flags).
type Config struct {
	Listen             string        `json:"listen"` // must be 127.0.0.1:port
	DataDir            string        `json:"dataDir"`
	EthRPC             string        `json:"ethRpc"`
	EthRPCs            []string      `json:"ethRpcs,omitempty"`
	Contract           eth.Address   `json:"contract"`
	XmrWalletRPC       string        `json:"xmrWalletRpc"`
	XmrDaemonRPC       string        `json:"xmrDaemonRpc"`
	XmrNet             keys.Network  `json:"xmrNet"`
	XmrBlockSecs       uint64        `json:"xmrBlockSecs,omitempty"`
	XmrRestoreHeight   uint64        `json:"xmrRestoreHeight,omitempty"`  // 0 = current height - 200, via wallet-rpc or daemon
	MakerURL           string        `json:"makerUrl"`                    // direct-mode maker URL (fallback)
	MakerSeshURL       string        `json:"makerSeshUrl,omitempty"`      // private-mode maker URL: http://<name>.sesh
	Registry           eth.Address   `json:"registry,omitempty"`          // SeshRegistry: find bonded makers here (configured maker = fallback)
	NoRegistry         bool          `json:"noRegistry,omitempty"`        // don't use the built-in registry (single configured maker only)
	RegistryFromBlock  uint64        `json:"registryFromBlock,omitempty"` // its deployment block
	ExitLease          eth.Address   `json:"exitLease,omitempty"`         // ExitLease: buy hours on registry exits (tag "exit")
	ExitLeaseFromBlock uint64        `json:"exitLeaseFromBlock,omitempty"`
	ExitRouterRPC      string        `json:"exitRouterRpc,omitempty"`  // your own Session Router's RPC (e.g. tcp://127.0.0.1:1190) for exits
	ExitWarnBefore     string        `json:"exitWarnBefore,omitempty"` // warn and offer a top-up this long before exit time ends (default 5m)
	ExitDNS            string        `json:"exitDns,omitempty"`        // "auto" (default): use the exit's resolver for system DNS while connected; "off"
	Transport          string        `json:"transport,omitempty"`      // "auto" (default with a .sesh maker), "bundled", "existing" or "direct"; "private" = auto
	SessionRouter      *RouterConfig `json:"sessionRouter,omitempty"`  // bundled router location and idle timeout
	USDC               eth.Address   `json:"usdc,omitempty"`           // mock or real USDC on this chain; empty = no USDC
	USDCDecimals       int           `json:"usdcDecimals,omitempty"`   // 0 = 6
	Chain              string        `json:"chain,omitempty"`          // "arbitrum-sepolia" (default) or "sepolia"
	ChainID            int64         `json:"chainId,omitempty"`        // expected EVM chain id; 0 = from the chain table
	ChainLabel         string        `json:"-"`                        // filled by ApplyChain
	Explorer           string        `json:"explorer,omitempty"`       // block-explorer origin, no trailing slash
	XMRUSDFeed         eth.Address   `json:"xmrUsdFeed,omitempty"`
	ETHUSDFeed         eth.Address   `json:"ethUsdFeed,omitempty"`
	XMRUSDNote         string        `json:"xmrUsdNote,omitempty"`  // shown on the quote screen; says "mock" when it is
	FairMaxBps         int           `json:"fairMaxBps,omitempty"`  // warn if the quote is more than this many bps off; 0 = 500
	SatchelTest        string        `json:"satchelTest,omitempty"` // optional 127.0.0.1:port for the Satchel stand-in page
	Relayers           []string      `json:"relayers"`
	MainWallet         string        `json:"mainWallet"`
	SeshDNS            string        `json:"seshDns,omitempty"`
	Poll               string        `json:"poll,omitempty"`
	AllowMainnet       bool          `json:"iUnderstandThisIsUnauditedAndMayLoseFunds"`
	Demo               bool          `json:"demo,omitempty"` // sample data for screenshots; no real funds
	AppVersion         string        `json:"-"`              // shown on the Settings screen
	Policy             *swap.Policy  `json:"policy,omitempty"`
	FaucetURL          string        `json:"faucetUrl,omitempty"` // testnet faucet; "" = DefaultFaucetURL, "off" = hide
}

var mainnets = map[int64]string{1: "Ethereum", 10: "Optimism", 100: "Gnosis", 8453: "Base", 42161: "Arbitrum One"}

// Server is the local wallet process: UI + JSON API on 127.0.0.1, and the
// swap engine (watchtower loop) once the wallet is unlocked.
type Server struct {
	Cfg Config
	Log func(string, ...any)

	token string // per launch; only the wallet page has it (see checkOrigin)

	mu       sync.Mutex
	vault    *Vault
	eng      *swap.Engine
	eth      *eth.Client
	xmrW     *xmr.Wallet
	cancel   context.CancelFunc
	pending  *pending
	http     *http.Client
	pwd      string // in memory only, while unlocked; used as the Monero wallet-rpc password
	proposal *Proposal

	router   *srouter.Router // bundled Session Router (private mode), started on demand
	ext      *existingRouter // a Session Router already running on this computer (never managed by us)
	idleStop time.Duration
	useMu    sync.Mutex
	lastUse  time.Time
	source   string // SourceExisting / SourceBundled: what the private connection last used

	reg  regState
	ex   exitState
	send sendState
}

type pending struct {
	Mnemonic string
	Checks   []int
	At       time.Time
}

// NewServer prepares a server (it does not listen yet).
func NewServer(cfg Config) *Server {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:7676"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = StorePath("")
	}
	if cfg.XmrNet == "" {
		cfg.XmrNet = keys.Stagenet
	}
	if cfg.MainWallet == "" {
		cfg.MainWallet = "swapwallet-main"
	}
	s := &Server{Cfg: cfg, Log: log.Printf}
	var tk [32]byte
	if _, err := rand.Read(tk[:]); err != nil {
		panic(err)
	}
	s.token = hex.EncodeToString(tk[:])
	s.initPrivate()
	// The lock hint (api.NotifyLocked) uses the same client, so it also goes
	// through the bundled router in private mode.
	api.HTTP = s.http
	return s
}

// Handler is the HTTP handler (UI and /v1 API). Used by tests and Listen.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.ui)
	mux.HandleFunc("/v1/status", s.wrap(s.status))
	mux.HandleFunc("/v1/onboard/start", s.wrap(s.onboardStart))
	mux.HandleFunc("/v1/onboard/confirm", s.wrap(s.onboardConfirm))
	mux.HandleFunc("/v1/wallet/restore", s.wrap(s.restore))
	mux.HandleFunc("/v1/wallet/unlock", s.wrap(s.unlock))
	mux.HandleFunc("/v1/wallet/lock", s.wrap(s.lock))
	mux.HandleFunc("/v1/wallet/backup", s.wrap(s.backup))
	mux.HandleFunc("/v1/wallet/send/prepare", s.wrap(s.sendPrepare))
	mux.HandleFunc("/v1/wallet/send/confirm", s.wrap(s.sendConfirm))
	mux.HandleFunc("/v1/wallet/send/cancel", s.wrap(s.sendCancel))
	mux.HandleFunc("/v1/balances", s.wrap(s.balances))
	mux.HandleFunc("/v1/swap-assets", s.wrap(s.swapAssets))
	mux.HandleFunc("/v1/quote", s.wrap(s.quote))
	mux.HandleFunc("/v1/swaps", s.wrap(s.swaps))
	mux.HandleFunc("/v1/swaps/", s.wrap(s.oneSwap))
	mux.HandleFunc("/v1/settings", s.wrap(s.settings))
	mux.HandleFunc("/v1/registry/search", s.wrap(s.registrySearch))
	mux.HandleFunc("/v1/exits", s.wrap(s.exitsList))
	mux.HandleFunc("/v1/exits/status", s.wrap(s.exitsStatus))
	mux.HandleFunc("/v1/exits/buy", s.wrap(s.exitsBuy))
	mux.HandleFunc("/v1/exits/connect", s.wrap(s.exitsConnect))
	mux.HandleFunc("/v1/exits/disconnect", s.wrap(s.exitsDisconnect))
	mux.HandleFunc("/v1/exits/topup", s.wrap(s.exitsTopUp))
	mux.HandleFunc("/v1/exits/quote", s.wrap(s.exitsQuote))
	mux.HandleFunc("/v1/exits/router", s.wrap(s.exitsRouter))
	mux.HandleFunc("/v1/exits/unmap", s.wrap(s.exitsUnmap))
	mux.HandleFunc("/v1/exits/map", s.wrap(s.exitsMap))
	mux.HandleFunc("/v1/satchel/propose", s.wrap(s.satchelPropose))
	mux.HandleFunc("/v1/satchel/pending", s.wrap(s.satchelPending))
	mux.HandleFunc("/v1/satchel/approve", s.wrap(s.satchelApprove))
	mux.HandleFunc("/v1/satchel/reject", s.wrap(s.satchelReject))
	mux.HandleFunc("/v1/faucet/claim", s.wrap(s.faucetClaim))
	mux.HandleFunc("/v1/faucet/status", s.wrap(s.faucetStatus))
	return s.checkOrigin(mux)
}

// ListenAndServe binds 127.0.0.1 only and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	host, _, err := net.SplitHostPort(s.Cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen address: %w", err)
	}
	if host != "127.0.0.1" && host != "localhost" && !s.Cfg.Demo {
		return fmt.Errorf("wallet listens on 127.0.0.1 only, not %q", host)
	}
	if err := os.MkdirAll(s.Cfg.DataDir, 0o700); err != nil {
		return err
	}
	srv := &http.Server{Addr: s.Cfg.Listen, Handler: s.Handler()}
	// bind first: a second copy of the wallet must not touch the DNS of the
	// one that is running
	ln, err := net.Listen("tcp", s.Cfg.Listen)
	if err != nil {
		return err
	}
	s.cleanStaleRouter()
	s.recoverDNS()
	loopDone := make(chan struct{})
	go func() { s.reconcileLoop(ctx); close(loopDone) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		s.LockWallet()
	}()
	if s.Cfg.SatchelTest != "" {
		if err := satchelTestListenOK(s.Cfg.SatchelTest); err != nil {
			return err
		}
		ts := &http.Server{Addr: s.Cfg.SatchelTest, Handler: satchelTestHandler(s.Cfg.Listen)}
		go func() {
			<-ctx.Done()
			_ = ts.Close()
		}()
		go func() {
			s.logf("Satchel stand-in page on http://%s  (not the wallet; trades still need approval here)", s.Cfg.SatchelTest)
			if err := ts.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				s.logf("satchel test page: %v", err)
			}
		}()
	}
	s.logf("swap wallet listening on http://%s  (data %s)", s.Cfg.Listen, s.Cfg.DataDir)
	err = srv.Serve(ln)
	if err == http.ErrServerClosed {
		// The reconcile loop unmaps the exit on its way out: a router the
		// wallet didn't start (Rusty's service keeps running) would otherwise
		// keep this computer's traffic going through it. Wait, but not forever.
		select {
		case <-loopDone:
		case <-time.After(10 * time.Second):
			s.logf("still unmapping the exit at shutdown; leaving it")
		}
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) logf(f string, a ...any) {
	if s.Log != nil {
		s.Log(f, a...)
	}
}

// openPaths are the API calls another local page (a Satchel-style page on
// another port) may make: it can ask for a quote and propose a trade, which
// waits for approval in the wallet. Everything else needs the wallet page's
// token.
var openPaths = map[string]bool{"/v1/quote": true, "/v1/satchel/propose": true}

// tokenOK: the request carries this launch's token, which is only written
// into the wallet page. Other browser pages can't read that page (no CORS on
// it); a program running as you can, so this stops other sites and casual
// local scripts, not malware on your account.
func (s *Server) tokenOK(r *http.Request) bool {
	t := r.Header.Get("X-Wallet-Token")
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1
}

func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := r.Header.Get("Origin")
		if o != "" && !s.originOK(o) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		open := openPaths[r.URL.Path]
		if o != "" && open {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") && !s.tokenOK(r) && (o == "" || !open) {
			writeErr(w, 403, "this request needs the wallet page's token; use the wallet page")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) originOK(o string) bool {
	// Same host as we listen on, loopback only.
	u := o
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "https://")
	host, _, err := net.SplitHostPort(u)
	if err != nil {
		host = u
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (s *Server) wrap(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.logf("panic: %v", p)
				writeErr(w, 500, "internal error")
			}
		}()
		fn(w, r)
	}
}

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write(bytes.Replace(uiHTML, []byte(`name="wallet-token" content=""`), []byte(`name="wallet-token" content="`+s.token+`"`), 1))
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := map[string]any{
		"state":      s.stateLocked(),
		"network":    s.networkName(),
		"chain":      s.Cfg.Chain,
		"chainId":    s.Cfg.ChainID,
		"explorer":   s.explorer(),
		"makerUrl":   s.makerURL(),
		"connection": s.connStatus(),
		"listen":     s.Cfg.Listen,
		"demo":       s.Cfg.Demo,
		"exists":     Exists(s.Cfg.DataDir),
		"testOnly":   true,
		"audited":    false,
		"version":    s.Cfg.AppVersion,
	}
	if s.vault != nil {
		st["ethAddress"] = s.vault.Keys.EthAddr.Hex()
		st["xmrAddress"] = s.vault.Keys.XmrAddr
		if s.eng != nil {
			var open int
			var openID string
			for _, id := range s.eng.IDs() {
				ph, _, _ := s.eng.Get(id)
				if !ph.Terminal() {
					open++
					openID = id
				}
			}
			st["openSwaps"] = open
			st["openId"] = openID
		}
	}
	if s.proposal != nil {
		st["pending"] = s.proposal
	}
	st["exitsEnabled"] = s.exitsEnabled()
	if s.reg.assets != nil {
		st["swapAssets"] = s.reg.assets
	}
	st["faucet"] = s.faucetURL() != ""
	if c := s.exitConnSnapshot(); c != nil {
		st["exit"] = c
	}
	writeJSON(w, 200, st)
}

func (s *Server) stateLocked() string {
	if s.vault != nil {
		return "unlocked"
	}
	if Exists(s.Cfg.DataDir) {
		return "locked"
	}
	return "new"
}

func (s *Server) networkName() string {
	if s.Cfg.XmrNet == keys.Mainnet {
		return "mainnet (disabled unless explicitly allowed)"
	}
	label := s.Cfg.ChainLabel
	if label == "" {
		label = "Sepolia"
	}
	return label + " + Monero stagenet"
}

func (s *Server) explorer() string {
	if s.Cfg.Explorer != "" {
		return strings.TrimRight(s.Cfg.Explorer, "/")
	}
	return "https://sepolia.etherscan.io"
}

func (s *Server) onboardStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if Exists(s.Cfg.DataDir) {
		writeErr(w, 409, "a wallet already exists in this folder")
		return
	}
	m, err := NewMnemonic()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Three random positions for the "write it down" check, not first/last only.
	checks := pickChecks()
	s.pending = &pending{Mnemonic: m, Checks: checks, At: time.Now()}
	writeJSON(w, 200, map[string]any{"words": strings.Fields(m), "checks": checks})
}

func pickChecks() []int {
	// 1-based word numbers; three distinct in 1..24.
	var out []int
	seen := map[int]bool{}
	for len(out) < 3 {
		var b [1]byte
		_, _ = rand.Read(b[:])
		n := int(b[0])%24 + 1
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func (s *Server) onboardConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	var req struct {
		Password string            `json:"password"`
		Answers  map[string]int    `json:"-"`
		Words    map[string]string `json:"words"` // "4": "able", ...
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || time.Since(s.pending.At) > 30*time.Minute {
		writeErr(w, 400, "start a new backup first; the previous one expired")
		return
	}
	parts := strings.Fields(s.pending.Mnemonic)
	for _, n := range s.pending.Checks {
		got := strings.ToLower(strings.TrimSpace(req.Words[strconv.Itoa(n)]))
		if n < 1 || n > len(parts) || got != parts[n-1] {
			writeErr(w, 400, fmt.Sprintf("word %d is not right. Look at the backup again.", n))
			return
		}
	}
	v, err := Create(s.Cfg.DataDir, s.pending.Mnemonic, req.Password, s.Cfg.XmrNet)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.pending = nil
	if err := s.attach(v, req.Password); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ethAddress": v.Keys.EthAddr.Hex(),
		"xmrAddress": v.Keys.XmrAddr,
	})
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	var req struct {
		Mnemonic string `json:"mnemonic"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := Create(s.Cfg.DataDir, req.Mnemonic, req.Password, s.Cfg.XmrNet)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.attach(v, req.Password); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ethAddress": v.Keys.EthAddr.Hex(),
		"xmrAddress": v.Keys.XmrAddr,
	})
}

func (s *Server) unlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := OpenVault(s.Cfg.DataDir, req.Password)
	if err != nil {
		writeErr(w, 403, "wrong password, or the wallet file is corrupt")
		return
	}
	if err := s.attach(v, req.Password); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ethAddress": v.Keys.EthAddr.Hex(),
		"xmrAddress": v.Keys.XmrAddr,
		"resumed":    s.openCount(),
	})
}

func (s *Server) lock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	// Locking disconnects an exit first: the route and DNS go back before
	// the keys are dropped.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	s.disconnectExit(ctx, "wallet locked")
	cancel()
	s.LockWallet()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// LockWallet stops the watchtower loop and forgets keys in memory.
func (s *Server) LockWallet() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.vault, s.eng, s.xmrW, s.pwd = nil, nil, nil, ""
	s.send.clear()
	if s.router != nil && s.router.Running() {
		go s.stopRouter()
	}
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vault == nil {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	// Re-open with the given password so a caller who stole the UI session
	// still cannot see the seed without it.
	v, err := OpenVault(s.Cfg.DataDir, req.Password)
	if err != nil {
		writeErr(w, 403, "wrong password")
		return
	}
	writeJSON(w, 200, map[string]any{"words": strings.Fields(v.File.Mnemonic)})
}

func (s *Server) balances(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	v := s.vault
	c := s.eth
	xw := s.xmrW
	cfg := s.Cfg
	s.mu.Unlock()
	if v == nil {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	out := map[string]any{
		"ethAddress": v.Keys.EthAddr.Hex(),
		"xmrAddress": v.Keys.XmrAddr,
		"usdc":       "0",
	}
	if cfg.USDC.IsZero() {
		out["usdcNote"] = "No USDC token is configured."
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if cfg.Demo && c == nil {
		out["eth"] = "0.15"
		out["xmr"] = "0.4"
		out["ethWei"] = "150000000000000000"
		out["xmrPico"] = uint64(400000000000)
		out["demo"] = true
		writeJSON(w, 200, out)
		return
	}
	if c != nil {
		bal, err := c.Balance(ctx, v.Keys.EthAddr)
		if err != nil {
			out["ethError"] = err.Error()
		} else {
			out["eth"] = weiToETH(bal)
			out["ethWei"] = bal.String()
		}
	} else {
		out["ethError"] = "Ethereum RPC is not configured"
	}
	if c != nil && !cfg.USDC.IsZero() {
		tb, err := eth.TokenBalance(ctx, c, cfg.USDC, v.Keys.EthAddr)
		dec := cfg.USDCDecimals
		if dec == 0 {
			dec = 6
		}
		if err != nil {
			out["usdcError"] = err.Error()
		} else {
			out["usdc"] = formatUnits(tb, dec)
			out["usdcRaw"] = tb.String()
			out["usdcAddress"] = cfg.USDC.Hex()
			out["usdcNote"] = "Mock USDC on this testnet. Not the real USD Coin."
		}
	}
	if xw != nil {
		var total, unlocked, blocks uint64
		err := xw.Session(ctx, cfg.MainWallet, func(sess *xmr.Sess) error {
			_ = sess.Refresh(ctx)
			var err error
			total, unlocked, blocks, err = sess.BalanceLock(ctx)
			return err
		})
		if err != nil {
			out["xmrError"] = err.Error()
			if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") {
				out["xmrError"] = "Monero wallet is still catching up with the network. This can take a few minutes on a new wallet."
			}
		} else {
			out["xmr"] = picoToXMR(total)
			out["xmrUnlocked"] = picoToXMR(unlocked)
			out["xmrPico"] = total
			if total > unlocked {
				out["xmrLocked"] = picoToXMR(total - unlocked)
				if blocks == 0 {
					blocks = 10
				}
				out["xmrUnlockMins"] = blocks * 2
			}
		}
	} else {
		out["xmrError"] = "Monero wallet is not connected"
	}
	writeJSON(w, 200, out)
}

func (s *Server) quote(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	wantUSDC := r.URL.Query().Get("usdc") != "" || strings.EqualFold(r.URL.Query().Get("asset"), "usdc")
	var val *big.Int
	var err error
	if usdcStr := r.URL.Query().Get("usdc"); usdcStr != "" {
		val, err = parseUnits(usdcStr, 6)
	} else {
		val, err = parseValue(r.URL.Query().Get("value"), r.URL.Query().Get("eth"))
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	makerURL := s.curMakerURL()
	if s.Cfg.Demo && makerURL == "" {
		// A realistic sample quote, clearly marked.
		if wantUSDC {
			// 1 USDC (1e6) → 0.003 XMR = 3e9 pico: pico = val * 3000.
			pico := new(big.Int).Mul(val, big.NewInt(3000))
			writeJSON(w, 200, map[string]any{
				"value": val.String(), "valueUsdc": formatUnits(val, 6),
				"valueLabel": formatUnits(val, 6) + " USDC", "asset": "USDC",
				"xmr": picoToXMR(pico.Uint64()), "xmrPico": pico.Uint64(),
				"premiumWei": "0", "d1": 2400, "d2": 3600, "grace": 21600,
				"minConfs": 10, "estReadySecs": 1500, "estSpendableSecs": 2700,
				"expires": time.Now().Add(45 * time.Second).Unix(),
				"demo":    true,
			})
			return
		}
		// 1 ETH = 1e18 wei → 3.1 XMR = 3.1e12 pico, so pico = val * 31 / 1e7.
		pico := new(big.Int).Div(new(big.Int).Mul(val, big.NewInt(31)), big.NewInt(10_000_000))
		writeJSON(w, 200, map[string]any{
			"value": val.String(), "valueEth": weiToETH(val),
			"valueLabel": weiToETH(val) + " ETH", "asset": "ETH",
			"xmr": picoToXMR(pico.Uint64()), "xmrPico": pico.Uint64(),
			"premiumWei": "0", "d1": 2400, "d2": 3600, "grace": 21600,
			"minConfs": 10, "estReadySecs": 1500, "estSpendableSecs": 2700,
			"expires": time.Now().Add(45 * time.Second).Unix(),
			"demo":    true,
		})
		return
	}
	// A cold private start is about 3 s, but allow for slow paths.
	ctx, cancel := context.WithTimeout(withUserInitiated(r.Context()), 60*time.Second)
	defer cancel()
	var quotes []*makerQuote
	regErr := ""
	if s.regEnabled() && isPrivate(s.curMode()) {
		qs, err := s.askRegistryMakers(ctx, wantUSDC, val)
		if err != nil {
			regErr = err.Error()
		}
		quotes = qs
		if best := pickBest(qs); best != nil {
			out := s.quoteOut(ctx, best.terms, best.offer, best.URL, wantUSDC)
			out["quotes"], out["listingId"], out["sesh"] = qs, best.ListingID, best.Sesh
			out["source"] = "registry"
			out["bondEth"], out["maxEth"] = best.BondETH, best.MaxETH
			writeJSON(w, 200, out)
			return
		}
	}
	if makerURL == "" {
		msg := "no maker is configured (set one in Settings)"
		if s.regEnabled() {
			msg = "no bonded maker in the registry gave a usable quote"
			if regErr != "" {
				msg += " (" + regErr + ")"
			} else if s.noMakerTakes(wantUSDC) {
				if wantUSDC {
					msg = "No maker takes USDC right now. Swap ETH instead."
				} else {
					msg = "No maker takes ETH right now."
				}
			} else if r := rangeSummary(quotes); r != "" {
				msg = r
			}
		}
		writeJSON(w, 502, map[string]any{"error": msg, "quotes": quotes})
		return
	}
	var terms api.Terms
	u := strings.TrimRight(makerURL, "/") + "/v1/quote?value=" + val.String()
	if err := getJSON(ctx, s.http, u, &terms); err != nil {
		writeErr(w, 502, "could not get a quote: "+friendlyNetErr(err))
		return
	}
	var offer api.Offer
	_ = getJSON(ctx, s.http, strings.TrimRight(makerURL, "/")+"/v1/offer", &offer)
	if wantUSDC && offer.Asset.IsZero() && offer.Contract != (eth.Address{}) {
		// Offer was fetched and is ETH-only.
		writeErr(w, 422, "this maker is offering ETH, not USDC")
		return
	}
	if !wantUSDC && !offer.Asset.IsZero() {
		writeErr(w, 422, "this maker is offering USDC, not ETH")
		return
	}
	out := s.quoteOut(ctx, &terms, &offer, makerURL, wantUSDC)
	out["source"] = "configured"
	if quotes != nil {
		out["quotes"] = quotes
		out["fallback"] = true
	}
	writeJSON(w, 200, out)
}

// quoteOut is the quote screen's JSON for one maker's terms.
func (s *Server) quoteOut(ctx context.Context, terms *api.Terms, offer *api.Offer, makerURL string, wantUSDC bool) map[string]any {
	out := map[string]any{
		"value": terms.Value.String(),
		"xmr":   picoToXMR(terms.XMR), "xmrPico": terms.XMR,
		"premiumWei": bigStr(terms.Premium), "premiumEth": weiToETH(terms.Premium),
		"d1": terms.D1, "d2": terms.D2, "grace": terms.Grace,
		"minConfs": terms.MinConfs, "expires": terms.Expires,
		"estReadySecs": terms.EstReadySecs, "estSpendableSecs": terms.EstSpendableSecs,
		"makerUrl": makerURL,
	}
	if wantUSDC || !offer.Asset.IsZero() {
		out["asset"] = "USDC"
		out["assetAddr"] = offer.Asset.Hex()
		out["valueUsdc"] = formatUnits(terms.Value, 6)
		out["valueLabel"] = formatUnits(terms.Value, 6) + " USDC"
	} else {
		out["asset"] = "ETH"
		out["valueEth"] = weiToETH(terms.Value)
		out["valueLabel"] = weiToETH(terms.Value) + " ETH"
	}
	asset, _ := out["asset"].(string)
	if chk, err := s.fairPrice(ctx, asset, terms.Value, terms.XMR); err != nil {
		out["fairPriceError"] = err.Error()
	} else if chk != nil {
		out["fairPrice"] = chk
	}
	return out
}

func (s *Server) swaps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listSwaps(w, r)
	case http.MethodPost:
		s.startSwap(w, r)
	default:
		writeErr(w, 405, "GET or POST")
	}
}

func (s *Server) listSwaps(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	eng := s.eng
	s.mu.Unlock()
	if s.vault == nil {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	var out []View
	if eng != nil {
		for _, id := range eng.IDs() {
			rec, ok := eng.Snapshot(id)
			if !ok {
				continue
			}
			out = append(out, s.view(rec))
		}
	}
	if out == nil {
		out = []View{}
	}
	writeJSON(w, 200, map[string]any{"swaps": out})
}

func (s *Server) startSwap(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	eng := s.eng
	makerURL := s.makerURL()
	s.mu.Unlock()
	if !s.isWalletPage(r) {
		writeErr(w, 403, "start the swap from the wallet. A Satchel-style page can propose a trade, but you approve it here.")
		return
	}
	if s.vault == nil || eng == nil {
		if s.vault == nil {
			writeErr(w, 401, "unlock the wallet first")
			return
		}
		writeErr(w, 503, "the swap engine is not running (Ethereum or Monero is not connected)")
		return
	}
	var req struct {
		Value     string `json:"value"`
		Eth       string `json:"eth"`
		USDC      string `json:"usdc"`
		Asset     string `json:"asset"`
		MinXMR    uint64 `json:"minXmr"`
		ListingID uint64 `json:"listingId"` // a bonded maker from the quote screen
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	var val *big.Int
	var err error
	if req.USDC != "" {
		val, err = parseUnits(req.USDC, 6)
	} else {
		val, err = parseValue(req.Value, req.Eth)
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	pol := eng.Policy
	tp := takeLimits(pol, val)
	tp.MakerURL, tp.MinXMR, tp.HTTP = makerURL, req.MinXMR, s.http
	if req.ListingID != 0 {
		if !s.regEnabled() {
			writeErr(w, 400, "no registry is configured")
			return
		}
		e, info, err := s.registryMakerForSwap(ctx, req.ListingID)
		if err != nil {
			writeErr(w, 422, err.Error())
			return
		}
		ev := val
		if req.USDC != "" || strings.EqualFold(req.Asset, "usdc") {
			rc := &registry.Client{C: s.regClient(), Addr: s.Cfg.Registry}
			if ev, err = rc.EthValue(ctx, s.Cfg.USDC, val); err != nil {
				writeErr(w, 502, "couldn't value USDC: "+err.Error())
				return
			}
		}
		if ev.Cmp(info.MaxQuote) > 0 {
			writeErr(w, 422, "amount is over this maker's bond cap ("+registry.FormatWei(info.MaxQuote)+" ETH)")
			return
		}
		tp.MakerURL = s.seshURL(e.Sesh)
		tp.Registry, tp.ListingID, tp.ListingOwner, tp.Sesh = s.Cfg.Registry, e.ID, info.Owner, e.Sesh
	}
	if tp.MakerURL == "" {
		writeErr(w, 400, "no maker is configured")
		return
	}
	rec, err := api.Take(ctx, eng, tp)
	if err != nil {
		writeErr(w, 422, friendlyNetErr(err))
		return
	}
	// The watchtower loop will lock on the next step; kick it now.
	go eng.StepAll(context.Background())
	writeJSON(w, 200, s.view(*rec))
}

func (s *Server) oneSwap(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/swaps/")
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		writeErr(w, 404, "unknown swap")
		return
	}
	s.mu.Lock()
	eng := s.eng
	s.mu.Unlock()
	if s.vault == nil {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	if eng == nil {
		writeErr(w, 404, "unknown swap")
		return
	}
	rec, ok := eng.Snapshot(id)
	if !ok {
		writeErr(w, 404, "unknown swap")
		return
	}
	writeJSON(w, 200, s.view(rec))
}

// view is PublicView plus the registry line.
func (s *Server) view(rec swap.Record) View {
	v := PublicView(rec, time.Now())
	v.Registry = regView(rec, s.explorer())
	return v
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"makerUrl":       s.makerURL(),
			"makerUrlDirect": s.Cfg.MakerURL,
			"makerSeshUrl":   s.Cfg.MakerSeshURL,
			"transport":      s.mode(),
			"connection":     s.connStatus(),
			"listen":         s.Cfg.Listen,
			"network":        s.networkName(),
			"chain":          s.Cfg.Chain,
			"chainId":        s.Cfg.ChainID,
			"explorer":       s.explorer(),
			"ethRpcs":        append([]string{s.Cfg.EthRPC}, s.Cfg.EthRPCs...),
			"contract":       s.Cfg.Contract.Hex(),
			"usdc":           s.Cfg.USDC.Hex(),
			"xmrUsdFeed":     s.Cfg.XMRUSDFeed.Hex(),
			"fairMaxBps":     s.Cfg.FairMaxBps,
			"xmrWalletRpc":   s.Cfg.XmrWalletRPC != "",
			"xmrDaemonRpc":   s.Cfg.XmrDaemonRPC != "",
			"relayers":       s.Cfg.Relayers,
			"demo":           s.Cfg.Demo,
			"registry":       addrStr(s.Cfg.Registry),
			"exitLease":      addrStr(s.Cfg.ExitLease),
			"exitRouterRpc":  s.Cfg.ExitRouterRPC,
		})
	case http.MethodPut, http.MethodPost:
		var req struct {
			MakerURL      string  `json:"makerUrl"`
			MakerSeshURL  string  `json:"makerSeshUrl"`
			Transport     string  `json:"transport"`
			ExitRouterRPC *string `json:"exitRouterRpc"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, 400, "bad json")
			return
		}
		if req.Transport != "" && normMode(req.Transport) == "" {
			writeErr(w, 400, "transport must be \"auto\", \"bundled\", \"existing\" or \"direct\"")
			return
		}
		if req.ExitRouterRPC != nil && *req.ExitRouterRPC != "" && !localRPC(*req.ExitRouterRPC) {
			writeErr(w, 400, "your router's RPC address must be on this computer: tcp://127.0.0.1:PORT or ipc:///path")
			return
		}
		if req.MakerSeshURL != "" && !isSeshURL(req.MakerSeshURL) {
			writeErr(w, 400, "the private maker address must be http://<name>.sesh")
			return
		}
		s.mu.Lock()
		if req.MakerURL != "" {
			s.Cfg.MakerURL = req.MakerURL
		}
		if req.MakerSeshURL != "" {
			s.Cfg.MakerSeshURL = req.MakerSeshURL
		}
		if req.Transport != "" {
			s.Cfg.Transport = normMode(req.Transport)
		}
		if req.ExitRouterRPC != nil {
			s.Cfg.ExitRouterRPC = strings.TrimSpace(*req.ExitRouterRPC)
		}
		if isPrivate(s.mode()) && s.Cfg.MakerSeshURL == "" && !s.regEnabled() {
			s.Cfg.Transport = ModeDirect
			s.mu.Unlock()
			writeErr(w, 400, "set a .sesh maker address before choosing the private connection")
			return
		}
		out := map[string]any{"makerUrl": s.makerURL(), "transport": s.mode(), "makerSeshUrl": s.Cfg.MakerSeshURL, "makerUrlDirect": s.Cfg.MakerURL}
		s.mu.Unlock()
		writeJSON(w, 200, out)
	default:
		writeErr(w, 405, "GET or PUT")
	}
}

// attachNet does attach's node calls without s.mu. cid == nil (and no
// error) means: carry on without swaps for now (already logged). xw is the
// Monero wallet for the engine; xmrOK says our wallet file opened in it.
func (s *Server) attachNet(c *eth.Client, v *Vault, password string) (cid *big.Int, xw *xmr.Wallet, xmrOK bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cid, err = c.ChainID(ctx)
	cancel()
	if err != nil {
		s.logf("ethereum RPC not reachable yet: %v", err)
		return nil, nil, false, nil
	}
	if name, ok := mainnets[cid.Int64()]; ok && !s.Cfg.AllowMainnet {
		return nil, nil, false, fmt.Errorf("refusing to run on %s mainnet: this software is unaudited", name)
	}
	if s.Cfg.ChainID != 0 && cid.Int64() != s.Cfg.ChainID {
		return nil, nil, false, fmt.Errorf("RPC chain id %d does not match configured %d (%s)", cid.Int64(), s.Cfg.ChainID, s.Cfg.Chain)
	}
	if s.Cfg.XmrNet == keys.Mainnet && !s.Cfg.AllowMainnet {
		return nil, nil, false, errors.New("refusing Monero mainnet: this software is unaudited")
	}
	if s.Cfg.XmrWalletRPC == "" && !s.Cfg.Demo {
		s.logf("no Monero wallet-rpc; ETH balances will work, swaps will not")
		return nil, nil, false, nil
	}
	if s.Cfg.XmrWalletRPC == "" {
		return cid, nil, false, nil
	}
	xw = xmr.NewWallet(s.Cfg.XmrWalletRPC, password)
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rh := s.restoreHeight(ctx)
	if err := xw.FromKeys(ctx, s.Cfg.MainWallet, v.Keys.XmrAddr, v.Keys.XmrView.Hex(), v.Keys.XmrSpend.Hex(), rh); err != nil {
		s.logf("monero wallet from keys: %v (swaps need this)", err)
		// Still start the engine if we can; sweep/balance may fail.
		return cid, xw, false, nil
	}
	return cid, xw, true, nil
}

// attach is called with s.mu held, after a successful Create or OpenVault.
// It releases s.mu during node calls and holds it again when it returns.
func (s *Server) attach(v *Vault, password string) error {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.vault = v
	s.pwd = password
	s.eng = nil
	s.xmrW = nil

	urls := append([]string{}, s.Cfg.EthRPCs...)
	if s.Cfg.EthRPC != "" {
		urls = append([]string{s.Cfg.EthRPC}, urls...)
	}
	if len(urls) == 0 && !s.Cfg.Demo {
		s.logf("no Ethereum RPC configured; balances and swaps will wait")
		return nil
	}
	if len(urls) > 0 {
		c := eth.NewClients(urls...)
		c.HTTP = newChainHTTP()
		// The node calls below can take up to 75 s on a bad link; don't
		// hold s.mu across them, so status and the other handlers answer.
		s.mu.Unlock()
		cid, xw, xmrOK, err := s.attachNet(c, v, password)
		s.mu.Lock()
		if s.vault != v {
			return errors.New("the wallet was locked while it was connecting")
		}
		s.eth = c
		if err != nil {
			s.eth = nil
			return err
		}
		if cid == nil {
			return nil // RPC not reachable yet, or no Monero wallet-rpc (logged)
		}
		if xmrOK {
			s.xmrW = xw
		}
		pol := swap.DefaultPolicy()
		if s.Cfg.Policy != nil {
			pol = *s.Cfg.Policy
		}
		k := &eth.Contract{C: c, Addr: s.Cfg.Contract, ChainID: cid}
		eng := &swap.Engine{
			Role: swap.Taker, Store: v.Store, K: k, EthKey: v.Keys.EthKey,
			Wallet: xw, MainWal: s.Cfg.MainWallet, XmrNet: s.Cfg.XmrNet,
			Relayers: s.Cfg.Relayers, Policy: pol, OnLocked: api.NotifyLocked,
		}
		if s.Cfg.XmrDaemonRPC != "" {
			eng.Daemon = xmr.NewDaemon(s.Cfg.XmrDaemonRPC)
		}
		eng.AssumeBlockSecs = s.Cfg.XmrBlockSecs
		if err := eng.Load(); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		s.eng = eng
		ctxRun, stop := context.WithCancel(context.Background())
		s.cancel = stop
		poll := 10 * time.Second
		if s.Cfg.Poll != "" {
			if d, err := time.ParseDuration(s.Cfg.Poll); err == nil {
				poll = d
			}
		}
		go eng.Run(ctxRun, poll)
		if s.regEnabled() {
			go s.regLoop(ctxRun, eng, 5*time.Second)
		}
		n := 0
		for _, id := range eng.IDs() {
			if ph, _, _ := eng.Get(id); !ph.Terminal() {
				n++
			}
		}
		if n > 0 {
			s.logf("resumed %d unfinished swap(s)", n)
			s.startForResume()
		}
	}
	return nil
}

func (s *Server) openCount() int {
	if s.eng == nil {
		return 0
	}
	n := 0
	for _, id := range s.eng.IDs() {
		if ph, _, _ := s.eng.Get(id); !ph.Terminal() {
			n++
		}
	}
	return n
}

func parseValue(wei, ethStr string) (*big.Int, error) {
	if wei != "" {
		v, ok := new(big.Int).SetString(wei, 10)
		if !ok || v.Sign() <= 0 {
			return nil, errors.New("value must be a positive number of wei")
		}
		return v, nil
	}
	if ethStr == "" {
		return nil, errors.New("enter an amount")
	}
	return parseETH(ethStr)
}

func parseETH(s string) (*big.Int, error) {
	return parseUnits(s, 18)
}

func parseUnits(s string, dec int) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("enter an amount")
	}
	if strings.HasPrefix(s, "-") {
		return nil, errors.New("amount must be positive")
	}
	if dec <= 0 {
		return nil, errors.New("bad decimals")
	}
	parts := strings.SplitN(s, ".", 2)
	whole := parts[0]
	if whole == "" {
		whole = "0"
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) > dec {
		frac = frac[:dec]
	}
	for len(frac) < dec {
		frac += "0"
	}
	v, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok || v.Sign() <= 0 {
		return nil, errors.New("amount must be a positive number")
	}
	return v, nil
}

func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// restoreHeight is the Monero wallet restore height. Height 0 makes
// monero-wallet-rpc scan the whole chain, which hangs the first balance
// call, so we prefer an explicit config value, then the daemon tip minus
// a short buffer, then 1.
func (s *Server) restoreHeight(ctx context.Context) uint64 {
	if s.Cfg.XmrRestoreHeight != 0 {
		return s.Cfg.XmrRestoreHeight
	}
	if s.Cfg.XmrDaemonRPC != "" {
		if h, err := xmr.NewDaemon(s.Cfg.XmrDaemonRPC).Height(ctx); err == nil && h > 200 {
			return h - 200
		}
	}
	return 1
}

// WriteAPIFile writes a user-only file Satchel (later) can use to find the port.
func WriteAPIFile(dir, listen string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, "api.json")
	b, _ := json.MarshalIndent(map[string]string{"listen": listen, "url": "http://" + listen}, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

// fairPrice compares a maker quote to the on-chain XMR/USD feed (a mock
// AggregatorV3 on Arbitrum Sepolia — there is no official Chainlink XMR/USD
// on that chain). Returns (nil, nil) when no feed is configured.
func (s *Server) fairPrice(ctx context.Context, asset string, value *big.Int, xmrPico uint64) (*feed.Check, error) {
	if s.Cfg.XMRUSDFeed.IsZero() || s.eth == nil {
		return nil, nil
	}
	xmr, err := feed.Read(ctx, s.eth, s.Cfg.XMRUSDFeed)
	if err != nil {
		return nil, fmt.Errorf("XMR/USD feed: %w", err)
	}
	fair := xmr.Price()
	var ethUSD float64
	ethSrc := ""
	if asset != "USDC" {
		if s.Cfg.ETHUSDFeed.IsZero() {
			return nil, fmt.Errorf("ETH/USD feed is not configured")
		}
		ethf, err := feed.Read(ctx, s.eth, s.Cfg.ETHUSDFeed)
		if err != nil {
			return nil, fmt.Errorf("ETH/USD feed: %w", err)
		}
		ethUSD = ethf.Price()
		ethSrc = ethf.Desc
		if ethSrc == "" {
			ethSrc = "ETH/USD"
		}
	}
	implied, err := feed.ImpliedXMRUSD(asset, value, xmrPico, ethUSD)
	if err != nil {
		return nil, err
	}
	src := s.Cfg.XMRUSDNote
	if src == "" {
		src = "the XMR/USD price feed"
	}
	max := int64(s.Cfg.FairMaxBps)
	if max <= 0 {
		max = int64(chaincfg.DefaultFairMaxBps)
	}
	chk := feed.Evaluate(fair, implied, max, src, s.Cfg.XMRUSDFeed.Hex(), xmr.UpdatedAt)
	chk.ETHUSD = ethUSD
	chk.ETHUSDSource = ethSrc
	return &chk, nil
}

func (s *Server) curMakerURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.makerURL()
}

func isSeshURL(u string) bool {
	pu, err := url.Parse(u)
	return err == nil && pu.Scheme == "http" && sesh.IsSesh(pu.Hostname())
}
