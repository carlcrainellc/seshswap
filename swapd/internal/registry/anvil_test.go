package registry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// anvil's well-known public dev keys (local devnet only)
const (
	devDeployer = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	devTaker    = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"
	devMaker    = "5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a"
	devSite     = "7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6"
)

var root = func() string { d, _ := filepath.Abs("../../.."); return d }()

func artifact(t *testing.T, file, name string) []byte {
	p := filepath.Join(root, "contracts", "out", file, name+".json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("no forge artifact %s (run forge build in contracts/)", p)
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

type devnet struct {
	ctx context.Context
	c   *eth.Client
	url string
}

// StartAnvil runs a throwaway local anvil (exported for the wallet tests via
// a copy; kept here for this package).
func startAnvil(t *testing.T) *devnet {
	p := os.Getenv("ANVIL")
	if p == "" {
		if lp, err := exec.LookPath("anvil"); err == nil {
			p = lp
		} else {
			p = filepath.Join(os.Getenv("HOME"), ".foundry/bin/anvil")
		}
	}
	if _, err := os.Stat(p); err != nil {
		t.Skip("anvil not installed")
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cmd := exec.Command(p, "--port", fmt.Sprint(port), "--silent")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	d := &devnet{ctx: context.Background(), url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	d.c = eth.NewClient(d.url)
	for i := 0; i < 100; i++ {
		if _, err := d.c.ChainID(d.ctx); err == nil {
			return d
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("anvil did not start")
	return nil
}

func (d *devnet) send(t *testing.T, k *eth.Key, to eth.Address, value *big.Int, data []byte) *eth.Receipt {
	t.Helper()
	rc, err := d.c.SendAndWait(d.ctx, k, &to, value, data)
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
	return rc
}

func (d *devnet) warp(t *testing.T, secs uint64) {
	var out any
	if err := d.c.Call(d.ctx, &out, "evm_increaseTime", secs); err != nil {
		t.Fatal(err)
	}
	if err := d.c.Call(d.ctx, &out, "evm_mine"); err != nil {
		t.Fatal(err)
	}
}

func key(t *testing.T, h string) *eth.Key {
	k, err := eth.KeyFromHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

const testSesh = "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwo.sesh"

// Full round trip through the real contract: register, index from events,
// search, commit against a real V3 lock, ack, slash, top up, exit.
func TestRegistryOnAnvil(t *testing.T) {
	d := startAnvil(t)
	dk, tk, mk, sk := key(t, devDeployer), key(t, devTaker), key(t, devMaker), key(t, devSite)
	rc, err := d.c.SendAndWait(d.ctx, dk, nil, nil, artifact(t, "XmrSwapV3.sol", "XmrSwapV3"))
	if err != nil {
		t.Fatal(err)
	}
	v3 := *rc.Contract
	p := PlaceholderParams(v3, eth.Address{}, eth.Address{})
	p.USDCDecimals = 0
	p.MaxFeedAge = 0
	code := append(artifact(t, "SeshRegistry.sol", "SeshRegistry"), p.ConstructorArgs()...)
	rc, err = d.c.SendAndWait(d.ctx, dk, nil, nil, code)
	if err != nil {
		t.Fatal("deploy registry:", err)
	}
	reg := *rc.Contract
	deployBlock := rc.Block()
	cl := &Client{C: d.c, Addr: reg}
	cid, _ := d.c.ChainID(d.ctx)

	pp, err := cl.Params(d.ctx)
	if err != nil || pp.MakerMinBond.Cmp(ether("0.002")) != 0 || pp.CapFactor.Uint64() != 5 || pp.ExitDelay != 3*86400 {
		t.Fatalf("params %+v %v", pp, err)
	}

	makerTag, _ := Tag("maker")
	siteTag, _ := Tag("forum")
	d.send(t, mk, reg, ether("0.002"), EncodeRegister(testSesh, makerTag, "ETH→XMR test maker, Arbitrum Sepolia"))
	d.send(t, sk, reg, ether("0.0002"), EncodeRegister("testforum.sesh", siteTag, "A test forum about Monero"))
	if _, err := d.c.SendAndWait(d.ctx, sk, &reg, ether("0.0002"), EncodeRegister("testforum.sesh", siteTag, "dupe")); err == nil {
		t.Fatal("duplicate name accepted")
	}

	dir := t.TempDir()
	ix := OpenIndex(filepath.Join(dir, "ix.json"), reg, deployBlock)
	if err := ix.Sync(d.ctx, d.c); err != nil {
		t.Fatal(err)
	}
	if m := ix.Makers(); len(m) != 1 || m[0].Sesh != testSesh || m[0].Owner != mk.Address() || m[0].Meta != "ETH→XMR test maker, Arbitrum Sepolia" {
		t.Fatalf("makers %+v", m)
	}
	if r := ix.Search("monero FORUM", "", false); len(r) != 1 || r[0].Sesh != "testforum.sesh" {
		t.Fatalf("search %+v", r)
	}
	if r := ix.Search("", "store", false); len(r) != 0 {
		t.Fatalf("tag filter %+v", r)
	}
	// Reopen from disk: same content, nothing re-read.
	ix2 := OpenIndex(filepath.Join(dir, "ix.json"), reg, deployBlock)
	if len(ix2.Listings) != 2 || ix2.LastBlock != ix.LastBlock {
		t.Fatalf("reload %d %d", len(ix2.Listings), ix2.LastBlock)
	}

	info, err := cl.Info(d.ctx, 1)
	if err != nil || info.Tag != "maker" || info.Status != StatusActive || info.MaxQuote.Cmp(ether("0.01")) != 0 {
		t.Fatalf("info %+v %v", info, err)
	}

	// --- a swap the maker acks
	lock := func(salt byte, value *big.Int) (*eth.Swap, eth.Hash) {
		claimer, _ := eth.NewKey()
		var s eth.Hash
		s[31] = salt
		ck := new(big.Int).SetUint64(uint64(1000 + int(salt)))
		rk := new(big.Int).SetUint64(uint64(2000 + int(salt)))
		r := d.send(t, tk, v3, value, eth.EncodeLock(claimer.Address(), ck, rk, 3600, 3600, 1800, eth.Address{}, value, nil, s))
		h, _ := d.c.BlockAt(d.ctx, r.Block())
		sw := &eth.Swap{Owner: tk.Address(), Claimer: claimer.Address(), ClaimKey: ck, RefundKey: rk, T1: h.Timestamp + 3600,
			T2: h.Timestamp + 7200, Grace: 1800, Value: value, Premium: new(big.Int), Salt: s}
		return sw, sw.ID()
	}
	commit := func(sw *eth.Swap, penalty *big.Int) *SignedCommitment {
		h, _ := d.c.Head(d.ctx)
		sc := &SignedCommitment{Registry: reg, Commitment: Commitment{ListingID: 1, Taker: sw.Owner, Claimer: sw.Claimer,
			ClaimKey: sw.ClaimKey, Asset: sw.Asset, Value: sw.Value, XMRAtomic: 3_663_126_998, Penalty: penalty,
			AttestWindow: 600, Expiry: h.Timestamp + 900}}
		sc.Sig = SignCommitment(mk, cid, reg, &sc.Commitment)
		if a, err := sc.Signer(cid); err != nil || a != mk.Address() {
			t.Fatalf("signer %v %v", a, err)
		}
		if err := sc.CheckAgainstSwap(sw); err != nil {
			t.Fatal(err)
		}
		d.send(t, tk, reg, nil, EncodeCommit(sw, sc))
		return sc
	}
	val := ether("0.001")
	pen, err := cl.RequiredPenalty(d.ctx, eth.Address{}, val)
	if err != nil || pen.Cmp(ether("0.00005")) != 0 {
		t.Fatalf("penalty %v %v", pen, err)
	}
	sw1, id1 := lock(1, val)
	commit(sw1, pen)
	cs, _ := cl.Commit(d.ctx, id1)
	if cs.Stage != CommitOpen || cs.ListingID != 1 || cs.Penalty.Cmp(pen) != 0 {
		t.Fatalf("commit %+v", cs)
	}
	d.send(t, dk, reg, nil, EncodeAck(id1, SignAck(mk, cid, reg, id1, 1))) // a watchtower submits
	if cs, _ = cl.Commit(d.ctx, id1); cs.Stage != CommitAcked {
		t.Fatalf("after ack %+v", cs)
	}

	// --- over the cap is refused by the contract
	swBig, _ := lock(2, ether("0.011"))
	h, _ := d.c.Head(d.ctx)
	bad := &SignedCommitment{Registry: reg, Commitment: Commitment{ListingID: 1, Taker: swBig.Owner, Claimer: swBig.Claimer,
		ClaimKey: swBig.ClaimKey, Value: swBig.Value, XMRAtomic: 1, Penalty: ether("0.001"), AttestWindow: 600, Expiry: h.Timestamp + 900}}
	bad.Sig = SignCommitment(mk, cid, reg, &bad.Commitment)
	if _, err := d.c.SendAndWait(d.ctx, tk, &reg, nil, EncodeCommit(swBig, bad)); err == nil {
		t.Fatal("over-cap commit accepted")
	}

	// --- a swap the maker never acks: slashed to the taker
	sw3, id3 := lock(3, val)
	commit(sw3, pen)
	d.warp(t, 601)
	before, _ := d.c.Balance(d.ctx, tk.Address())
	d.send(t, dk, reg, nil, EncodeSlash(id3))
	after, _ := d.c.Balance(d.ctx, tk.Address())
	if new(big.Int).Sub(after, before).Cmp(pen) != 0 {
		t.Fatalf("taker got %v, want %v", new(big.Int).Sub(after, before), pen)
	}
	_ = sw3

	// --- top up, exit; the index follows
	d.send(t, mk, reg, ether("0.001"), EncodeTopUp(1))
	d.send(t, sk, reg, nil, EncodeRequestExit(2))
	if err := ix.Sync(d.ctx, d.c); err != nil {
		t.Fatal(err)
	}
	m := ix.Listings[1]
	want := new(big.Int).Sub(ether("0.003"), pen)
	if m.BondWei().Cmp(want) != 0 || m.Slashes != 1 {
		t.Fatalf("maker after slash+topup %+v want bond %v", m, want)
	}
	if ix.Listings[2].Status != StatusExiting || len(ix.Search("", "", false)) != 1 {
		t.Fatalf("exit not indexed %+v", ix.Listings[2])
	}
	info, _ = cl.Info(d.ctx, 1)
	if info.Bond.Cmp(want) != 0 {
		t.Fatalf("chain bond %v index %v", info.Bond, want)
	}
	// withdraw is refused before the delay
	if _, err := d.c.SendAndWait(d.ctx, sk, &reg, nil, EncodeWithdraw(2, sk.Address())); err == nil {
		t.Fatal("withdraw before delay accepted")
	}
	d.warp(t, 3*86400)
	d.send(t, sk, reg, nil, EncodeWithdraw(2, sk.Address()))
	_ = ix.Sync(d.ctx, d.c)
	if ix.Listings[2].Status != StatusWithdrawn {
		t.Fatalf("withdraw not indexed %+v", ix.Listings[2])
	}
}

func TestTagAndNames(t *testing.T) {
	h, err := Tag(" Maker ")
	if err != nil || TagString(h) != "maker" {
		t.Fatal(h, err)
	}
	if _, err := Tag(strings.Repeat("x", 33)); err == nil {
		t.Fatal("long tag")
	}
	for n, ok := range map[string]bool{testSesh: true, "a.sesh": true, "a-b.sesh": true, "A.sesh": false, "-a.sesh": false, "a.b.sesh": false, "x.onion": false} {
		if ValidSesh(n) != ok {
			t.Fatalf("%s: %v", n, !ok)
		}
	}
	if FormatWei(ether("0.00205")) != "0.00205" || FormatWei(ether("2")) != "2" {
		t.Fatal(FormatWei(ether("0.00205")))
	}
}
