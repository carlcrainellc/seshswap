package wallet

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// stallingXmr is a monero-wallet-rpc whose "refresh" hangs (node link
// stalled) until the test ends or the client gives up.
func stallingXmr(t *testing.T) (*xmr.Wallet, <-chan struct{}) {
	ent := make(chan struct{}, 64)
	rel := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "refresh" {
			select {
			case ent <- struct{}{}:
			default:
			}
			select {
			case <-rel:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"address": "x", "balance": 0, "unlocked_balance": 0}})
	}))
	t.Cleanup(func() { close(rel); srv.Close() })
	return xmr.NewWallet(srv.URL, "pw"), ent
}

// sweepingRecord is a taker swap whose step goes straight to the Monero
// wallet (sweep after the maker's claim): no chain calls first.
func sweepingRecord(t *testing.T) *swap.Record {
	my, _ := keys.RandomScalar()
	their, _ := keys.RandomScalar()
	v1, _ := keys.RandomScalar()
	v2, _ := keys.RandomScalar()
	return &swap.Record{ID: "cc00000000000000000000000000000c", Role: swap.Taker, Phase: swap.PSweeping,
		XmrNet: keys.Stagenet, MySpend: my.Hex(), MyView: v1.Hex(), TheirView: v2.Hex(),
		TheirSpend: their.Public().Hex(), TheirSecret: their.Hex(), SweepDest: "dest",
		Value: big1(), XMR: 1, Swap: &eth.Swap{Value: big1()}}
}

// v0.1.5 freeze: the engine holds its mutex for a whole swap step, and the
// step blocks on a stalled Monero call (no deadline; 300 s HTTP timeout,
// then the next step stalls again). Every wallet screen that reads swaps
// (the swap list, Send's "held for open swaps") then hangs.
func TestWalletNotFrozenByStalledMonero(t *testing.T) {
	s, h, _, _ := sendServer(t)
	xw, entered := stallingXmr(t)
	r := sweepingRecord(t)
	addSwap(t, s, r)
	s.mu.Lock()
	eng := s.eng
	eng.Wallet, eng.MainWal = xw, "main"
	eng.K = &eth.Contract{C: s.eth, ChainID: big.NewInt(421614)}
	s.xmrW = xw
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.StepAll(ctx)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the swap step never reached the Monero wallet")
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/swaps", ""},
		{"POST", "/v1/wallet/send/prepare", jsonBody(map[string]any{"coin": "ETH", "to": eth.Address{0x99, 1}.Hex(), "amount": "0.01"})},
		{"GET", "/v1/status", ""},
	} {
		done := make(chan int, 1)
		go func() { code, _ := do(t, h, c.method, c.path, c.body, ""); done <- code }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("FREEZE: %s %s still waiting after 3 s while a Monero call is stalled", c.method, c.path)
		}
	}
}

func big1() *big.Int { return big.NewInt(1) }
