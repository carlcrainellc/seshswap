// End-to-end tests on LOCAL devnets only: anvil (Ethereum) + monerod --regtest
// --offline + one monero-wallet-rpc per party. Skipped unless E2E=1.
package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// anvil's well-known public dev keys (local devnet only)
const (
	deployerPK = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	takerPK    = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	relayerPK  = "5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a"
	passphrase = "test-only passphrase"
)

var root = func() string { d, _ := filepath.Abs("../.."); return d }()

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type Env struct {
	t          *testing.T
	ctx        context.Context
	work       string
	eth        *eth.Client
	k          *eth.Contract
	daemon     *xmr.Daemon
	walletM    *xmr.Wallet // maker's wallet-rpc
	walletT    *xmr.Wallet // taker's wallet-rpc
	relayURL   string
	minerAddr  string
	procs      []*exec.Cmd
	token      eth.Address
	relaySrv   *relay.Server
	walletMURL string // maker wallet-rpc base URL (hostile tests)
	walletTURL string // taker wallet-rpc base URL (hostile tests)
}

func (e *Env) start(name string, args ...string) {
	f, _ := os.Create(filepath.Join(e.work, filepath.Base(name)+fmt.Sprint(len(e.procs))+".log"))
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = f, f
	if err := c.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.procs = append(e.procs, c)
	e.t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
}

func waitFor(t *testing.T, what string, fn func() error) {
	var err error
	for i := 0; i < 240; i++ {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s: %v", what, err)
}

func bytecode(t *testing.T, file, name string) []byte {
	p := filepath.Join(root, "contracts", "out", file, name+".json")
	if _, err := os.Stat(p); err != nil {
		cmd := exec.Command(filepath.Join(os.Getenv("HOME"), ".foundry/bin/forge"), "build")
		cmd.Dir = filepath.Join(root, "contracts")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("forge build: %v %s", err, out)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var j struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	_ = json.Unmarshal(b, &j)
	code, err := hex.DecodeString(strings.TrimPrefix(j.Bytecode.Object, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func deploy(t *testing.T, ctx context.Context, c *eth.Client, k *eth.Key, code []byte) eth.Address {
	rc, err := c.SendAndWait(ctx, k, nil, nil, code)
	if err != nil {
		t.Fatal("deploy:", err)
	}
	return *rc.Contract
}

// anvilPath: $ANVIL, then anvil on PATH (CI), then ~/.foundry/bin/anvil.
func anvilPath() string {
	if p := os.Getenv("ANVIL"); p != "" {
		return p
	}
	if p, err := exec.LookPath("anvil"); err == nil {
		return p
	}
	return filepath.Join(os.Getenv("HOME"), ".foundry/bin/anvil")
}

func NewEnv(t *testing.T) *Env {
	if os.Getenv("E2E") != "1" {
		t.Skip("set E2E=1 to run local devnet end-to-end tests")
	}
	mon := os.Getenv("MONERO_BIN")
	if mon == "" {
		mon = filepath.Join(root, ".monero", "monero-x86_64-linux-gnu-v0.18.5.4")
	}
	e := &Env{t: t, ctx: context.Background(), work: t.TempDir()}
	ap, dp, mp, tp := freePort(t), freePort(t), freePort(t), freePort(t)
	e.start(anvilPath(), "--port", fmt.Sprint(ap), "--silent")
	e.start(filepath.Join(mon, "monerod"), "--regtest", "--offline", "--fixed-difficulty", "1",
		"--data-dir", filepath.Join(e.work, "monerod"), "--p2p-bind-port", fmt.Sprint(freePort(t)),
		"--rpc-bind-port", fmt.Sprint(dp), "--no-zmq", "--non-interactive", "--log-level", "0")
	for i, p := range []int{mp, tp} {
		wd := filepath.Join(e.work, fmt.Sprintf("wallets%d", i))
		_ = os.MkdirAll(wd, 0o700)
		e.start(filepath.Join(mon, "monero-wallet-rpc"), "--wallet-dir", wd, "--rpc-bind-port", fmt.Sprint(p),
			"--disable-rpc-login", "--daemon-address", fmt.Sprintf("127.0.0.1:%d", dp), "--trusted-daemon",
			"--allow-mismatched-daemon-version", "--log-level", "0")
	}
	e.eth = eth.NewClient(fmt.Sprintf("http://127.0.0.1:%d", ap))
	e.daemon = xmr.NewDaemon(fmt.Sprintf("http://127.0.0.1:%d", dp))
	e.walletM = xmr.NewWallet(fmt.Sprintf("http://127.0.0.1:%d", mp), passphrase)
	e.walletMURL = fmt.Sprintf("http://127.0.0.1:%d", mp)
	e.walletTURL = fmt.Sprintf("http://127.0.0.1:%d", tp)
	e.walletT = xmr.NewWallet(fmt.Sprintf("http://127.0.0.1:%d", tp), passphrase)
	waitFor(t, "anvil", func() error { _, err := e.eth.ChainID(e.ctx); return err })
	waitFor(t, "monerod", func() error { _, err := e.daemon.Height(e.ctx); return err })
	for _, w := range []*xmr.Wallet{e.walletM, e.walletT} {
		waitFor(t, "wallet-rpc", func() error { return w.Create(e.ctx, "main") })
	}
	dk, _ := eth.KeyFromHex(deployerPK)
	addr := deploy(t, e.ctx, e.eth, dk, bytecode(t, "XmrSwapV3.sol", "XmrSwapV3"))
	cid, _ := e.eth.ChainID(e.ctx)
	e.k = &eth.Contract{C: e.eth, Addr: addr, ChainID: cid}
	// fund the maker with regtest XMR: mine 80 blocks to it (coinbase unlocks after 60)
	_ = e.walletM.Session(e.ctx, "main", func(s *xmr.Sess) error { a, err := s.Address(e.ctx); e.minerAddr = a; return err })
	if err := e.daemon.GenerateBlocks(e.ctx, 80, e.minerAddr); err != nil {
		t.Fatal(err)
	}
	// from now on mine to a throwaway address so balances only reflect swaps
	s1, err := keys.RandomScalar()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := keys.RandomScalar()
	if err != nil {
		t.Fatal(err)
	}
	e.minerAddr, _ = keys.Address(keys.Regtest, s1.Public(), s2.Public())
	// relayer
	rk, _ := eth.KeyFromHex(relayerPK)
	rs := &relay.Server{K: e.k, Key: rk, MinFeeMultiplier: 1.0, TokenMinFee: map[eth.Address]*big.Int{}, Logf: t.Logf}
	srv := httptest.NewServer(rs)
	t.Cleanup(srv.Close)
	e.relayURL = srv.URL
	e.relaySrv = rs
	return e
}

func (e *Env) policy() swap.Policy {
	return swap.Policy{Margin: 300, XmrLockBudget: 600, EthConfs: 1, RelayTimeout: 60, RelayFee: big.NewInt(1e15), MinGrace: 3600}
}

// Party is one daemon (engine + store dir) that can be "crashed" and restarted.
type Party struct {
	e        *Env
	role     swap.Role
	dir      string
	eng      *swap.Engine
	ethKey   *eth.Key
	wallet   *xmr.Wallet
	makerSrv *httptest.Server
	maker    *api.Maker
	payout   eth.Address
	relayers []string
	// maker service options (set before Restart)
	premiumBps  uint64
	oracle      *price.Oracle
	maxExposure uint64
}

func (e *Env) NewParty(role swap.Role, ethKey *eth.Key) *Party {
	p := &Party{e: e, role: role, dir: filepath.Join(e.work, "state-"+string(role)), ethKey: ethKey}
	p.wallet = e.walletT
	if role == swap.Maker {
		p.wallet = e.walletM
		pk, _ := eth.NewKey()
		p.payout = pk.Address() // fresh payout address with zero balance
		p.relayers = []string{e.relayURL}
	}
	p.Restart()
	return p
}

// Restart simulates a crash: drops all in-memory state and reloads from disk.
func (p *Party) Restart() {
	st, err := store.Open(p.dir, passphrase)
	if err != nil {
		p.e.t.Fatal(err)
	}
	p.eng = &swap.Engine{Role: p.role, Store: st, K: p.e.k, EthKey: p.ethKey, Wallet: p.wallet, MainWal: "main",
		XmrNet: keys.Regtest, Relayers: p.relayers, Policy: p.e.policy(), Logf: p.e.t.Logf,
		AssumeBlockSecs: 120} // regtest blocks are mined on demand, so there is no real block time to measure
	if p.role == swap.Taker {
		p.eng.OnLocked = api.NotifyLocked
	}
	if err := p.eng.Load(); err != nil {
		p.e.t.Fatal(err)
	}
	if p.role == swap.Maker {
		if p.maker == nil {
			p.maker = &api.Maker{PayoutTo: p.payout}
			p.makerSrv = httptest.NewServer(p.maker)
			p.e.t.Cleanup(p.makerSrv.Close)
		}
		p.maker.Engine = p.eng
		p.maker.Offer = api.Offer{ChainID: p.e.k.ChainID, Contract: p.e.k.Addr, Asset: p.e.token,
			PicoPerUnit: big.NewInt(2_000_000_000_000), // 2 XMR per 1e18 units
			Min:         big.NewInt(1e15), Max: new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18)),
			D1: 3600, D2: 3600, MinConfs: 2, XmrNet: keys.Regtest, RelayFee: big.NewInt(1e15), TTL: 600,
			PremiumBps: p.premiumBps}
		p.maker.Service.Oracle, p.maker.Service.MaxExposure = p.oracle, p.maxExposure
	}
}

func (e *Env) mineXMR(n int) {
	if err := e.daemon.GenerateBlocks(e.ctx, n, e.minerAddr); err != nil {
		e.t.Fatal(err)
	}
}

func (e *Env) warp(sec uint64) {
	_ = e.eth.Call(e.ctx, nil, "evm_increaseTime", sec)
	_ = e.eth.Call(e.ctx, nil, "evm_mine")
}

func (e *Env) mineETH() { _ = e.eth.Call(e.ctx, nil, "evm_mine") }

// rounds steps parties n times (with blocks) without a goal.
func (e *Env) rounds(n int, parties ...*Party) {
	for i := 0; i < n; i++ {
		for _, p := range parties {
			p.eng.StepAll(e.ctx)
		}
		e.mineXMR(1)
		e.mineETH()
	}
}

func (p *Party) phase(id string) swap.Phase { ph, _, _ := p.eng.Get(id); return ph }

// drive steps the given (online) parties, mining a Monero and an ETH block per round,
// until cond() holds.
func (e *Env) drive(what string, maxRounds int, cond func() bool, parties ...*Party) {
	for i := 0; i < maxRounds; i++ {
		if cond() {
			return
		}
		for _, p := range parties {
			p.eng.StepAll(e.ctx)
		}
		e.mineXMR(1)
		e.mineETH()
	}
	if !cond() {
		for _, p := range parties {
			for _, id := range p.eng.IDs() {
				ph, lg, _ := p.eng.Get(id)
				e.t.Logf("%s %s phase=%s log:\n  %s", p.role, id[:8], ph, strings.Join(lg, "\n  "))
			}
		}
		e.t.Fatalf("timeout: %s", what)
	}
}

func (p *Party) xmrBalance() uint64 {
	var b uint64
	_ = p.wallet.Session(p.e.ctx, "main", func(s *xmr.Sess) error {
		_ = s.Refresh(p.e.ctx)
		t, _, err := s.Balance(p.e.ctx)
		b = t
		return err
	})
	return b
}
