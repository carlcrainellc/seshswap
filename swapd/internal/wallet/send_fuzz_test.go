package wallet

import (
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

// sendModel drives the Send endpoints with a random sequence of operations
// against fake chains and checks after every confirmed send that
//   - no prepared id is ever sent twice, and every 200 is exactly one tx;
//   - once every sent tx is mined, the wallet still holds what open taker
//     swaps need (value + premium of swaps not locked yet, and their gas).
type sendModel struct {
	t       *testing.T
	s       *Server
	fe      *fakeEth
	ids     []string
	view    map[string]map[string]any
	sent    map[string]bool
	outETH  *big.Int // sent but not mined yet (amount + fee upper bound)
	outUSDC *big.Int
	nswap   int
	locked  map[string]bool
	ok      int
	stale   bool // a send was still unmined when this one was confirmed
}

func newSendModel(t *testing.T) *sendModel {
	s, _, _, fe := sendServer(t)
	return &sendModel{t: t, s: s, fe: fe, sent: map[string]bool{}, view: map[string]map[string]any{}, outETH: new(big.Int), outUSDC: new(big.Int),
		locked: map[string]bool{}}
}

func (m *sendModel) step(op, arg byte) {
	t := m.t
	h := m.s.Handler()
	to := eth.Address{0x99, 0x01}.Hex()
	switch op % 8 {
	case 0, 1: // prepare ETH or USDC
		coin := "ETH"
		amt := fmt.Sprintf("0.%03d", 1+int(arg)%120) // 0.001 .. 0.120 ETH
		if op%8 == 1 {
			coin, amt = "USDC", fmt.Sprintf("%d", 1+int(arg)%60)
		}
		body := map[string]any{"coin": coin, "to": to, "amount": amt}
		if arg%7 == 0 {
			body = map[string]any{"coin": coin, "to": to, "all": true}
		}
		if code, p := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(body), ""); code == 200 {
			m.ids = append(m.ids, p["id"].(string))
			m.view[p["id"].(string)] = p
		}
	case 2, 3: // confirm one of the prepared ids (maybe one already used)
		if len(m.ids) == 0 {
			return
		}
		id := m.ids[int(arg)%len(m.ids)]
		m.confirm(h, id)
	case 4: // cancel
		if len(m.ids) > 0 {
			do(t, h, "POST", "/v1/wallet/send/cancel", jsonBody(map[string]any{"id": m.ids[int(arg)%len(m.ids)]}), "")
		}
	case 5: // the chain mines everything sent so far
		m.fe.mu.Lock()
		m.fe.bal.Sub(m.fe.bal, m.outETH)
		m.fe.usdc.Sub(m.fe.usdc, m.outUSDC)
		m.fe.mined = len(m.fe.raws)
		m.fe.mu.Unlock()
		m.outETH.SetInt64(0)
		m.outUSDC.SetInt64(0)
	case 6: // a new taker swap starts (ETH or USDC), not locked yet
		m.nswap++
		r := &swap.Record{ID: fmt.Sprintf("%032x", 0xdd00+m.nswap), Role: swap.Taker, Phase: swap.TNegotiated,
			Value: big.NewInt(int64(1+int(arg)%40) * 1e15), XMR: 1}
		if arg%3 == 0 {
			r.Asset, r.Value = m.s.Cfg.USDC, big.NewInt(int64(1+int(arg)%20)*1e6)
		}
		addSwap(t, m.s, r)
	case 7: // an id expires
		m.s.send.mu.Lock()
		for _, p := range m.s.send.m {
			if arg%2 == 0 {
				p.Expires = time.Now().Add(-time.Second)
				break
			}
		}
		m.s.send.mu.Unlock()
	}
}

func (m *sendModel) confirm(h interface{}, id string) {
	t := m.t
	m.fe.mu.Lock()
	before := len(m.fe.raws)
	m.fe.mu.Unlock()
	code, c := do(t, m.s.Handler(), "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": id}), "")
	m.fe.mu.Lock()
	after := len(m.fe.raws)
	m.fe.mu.Unlock()
	if code != 200 {
		if after != before {
			t.Fatalf("confirm %s failed (%d %v) but sent a tx", id, code, c)
		}
		return
	}
	if m.sent[id] {
		t.Fatalf("id %s confirmed twice", id)
	}
	m.sent[id] = true
	m.ok++
	m.stale = m.outETH.Sign() > 0 || m.outUSDC.Sign() > 0
	if after != before+1 {
		t.Fatalf("one confirm sent %d txs", after-before)
	}
	// What this send costs at most (the reviewed amount and fee bound).
	v := m.view[id]
	fee, _ := parseUnits(fmt.Sprint(v["fee"]), 18)
	m.outETH.Add(m.outETH, fee)
	if v["coin"] == "USDC" {
		u, _ := parseUnits(fmt.Sprint(v["amount"]), 6)
		m.outUSDC.Add(m.outUSDC, u)
	} else {
		a, _ := parseUnits(fmt.Sprint(v["amount"]), 18)
		m.outETH.Add(m.outETH, a)
	}
	m.check(v["coin"] == "USDC")
}

// check: after all sent txs are mined, open swaps still have what they need.
func (m *sendModel) check(usdc bool) {
	hd := m.s.heldBySwaps()
	m.fe.mu.Lock()
	ethLeft := new(big.Int).Sub(m.fe.bal, m.outETH)
	usdcLeft := new(big.Int).Sub(m.fe.usdc, m.outUSDC)
	m.fe.mu.Unlock()
	if ethLeft.Cmp(hd.eth) < 0 {
		m.t.Fatalf("SPENT HELD ETH: after the sends are mined %s wei is left but open swaps need %s (earlier send unmined: %v)", ethLeft, hd.eth, m.stale)
	}
	if usdc && usdcLeft.Cmp(hd.usdc) < 0 {
		m.t.Fatalf("SPENT HELD USDC: after the sends are mined %s is left but open swaps need %s (earlier send unmined: %v)", usdcLeft, hd.usdc, m.stale)
	}
}

// Go native fuzz over operation sequences (2 bytes per operation).
func FuzzSendSequence(f *testing.F) {
	f.Add([]byte{6, 10, 0, 50, 0, 50, 2, 0, 2, 1})            // swap; two sends; confirm both before they're mined
	f.Add([]byte{0, 5, 2, 0, 5, 0, 0, 5, 2, 1, 2, 1})         // send, mine, send, double confirm
	f.Add([]byte{1, 30, 6, 3, 1, 30, 3, 0, 3, 1, 4, 0, 7, 0}) // USDC
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 80 {
			ops = ops[:80]
		}
		m := newSendModel(t)
		for i := 0; i+1 < len(ops); i += 2 {
			m.step(ops[i], ops[i+1])
		}
	})
}

// Randomized sequences with a fixed seed set (runs in plain go test).
func TestSendSequencesRandom(t *testing.T) {
	seeds := 150
	if testing.Short() {
		seeds = 20
	}
	for seed := 0; seed < seeds; seed++ {
		ops := make([]byte, 60)
		x := uint32(seed*2654435761 + 1)
		for i := range ops {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			ops[i] = byte(x)
		}
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			m := newSendModel(t)
			for i := 0; i+1 < len(ops); i += 2 {
				m.step(ops[i], ops[i+1])
			}
		})
	}
}

// Many clicks on the same review at once: exactly one send.
func TestSendConfirmConcurrentOnce(t *testing.T) {
	s, h, _, fe := sendServer(t)
	_, p := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "ETH", "to": eth.Address{0x99, 1}.Hex(), "amount": "0.01"}), "")
	var wg sync.WaitGroup
	var mu sync.Mutex
	oks := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _ := do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": p["id"]}), ""); code == 200 {
				mu.Lock()
				oks++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if oks != 1 || len(fe.raws) != 1 {
		t.Fatalf("%d confirms succeeded, %d txs", oks, len(fe.raws))
	}
	_ = s
}

// Two different reviews confirmed back to back, before the first is mined:
// together they must not dig into what an open swap needs.
func TestSendTwoConfirmsBeforeMinedKeepHeld(t *testing.T) {
	s, h, _, fe := sendServer(t)
	// 0.1 ETH; a swap holds 0.05 (+0.0002 gas): 0.0498 is free.
	addSwap(t, s, &swap.Record{ID: "ee00000000000000000000000000000e", Role: swap.Taker, Phase: swap.TNegotiated,
		Value: big.NewInt(5e16), XMR: 1})
	to := eth.Address{0x99, 1}.Hex()
	_, a := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "ETH", "to": to, "amount": "0.04"}), "")
	_, b := do(t, h, "POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "ETH", "to": to, "amount": "0.04"}), "")
	ca, _ := do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": a["id"]}), "")
	cb, rb := do(t, h, "POST", "/v1/wallet/send/confirm", jsonBody(map[string]any{"id": b["id"]}), "")
	if ca != 200 {
		t.Fatalf("first confirm: %d", ca)
	}
	if cb == 200 {
		t.Fatalf("SPENT HELD ETH: second send confirmed (%v); 0.08 ETH + fees leaves less than the swap's 0.0502 (raws %d)", rb, len(fe.raws))
	}
	if !strings.Contains(fmt.Sprint(rb["error"]), "Review") {
		t.Fatalf("second confirm error: %v", rb)
	}
}
