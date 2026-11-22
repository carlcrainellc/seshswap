package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// fakeChain answers what Submit/SubmitIntent need. eth_call always succeeds
// (the earlier tx is not mined yet, so "latest" still lets it pass) and
// every eth_sendRawTransaction is counted.
func fakeChain(t *testing.T, sent *atomic.Int32) *eth.Client { return fakeChainFail(t, sent, nil) }

func fakeChainFail(t *testing.T, sent *atomic.Int32, failSend *atomic.Bool) *eth.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var res any
		switch req.Method {
		case "eth_call":
			res = "0x"
		case "eth_chainId":
			res = "0x66eee"
		case "eth_estimateGas":
			res = "0x30000"
		case "eth_getTransactionCount":
			res = "0x0"
		case "eth_maxPriorityFeePerGas":
			res = "0x1"
		case "eth_getBlockByNumber":
			res = map[string]string{"number": "0x10", "timestamp": "0x6553f100", "baseFeePerGas": "0x5f5e100"}
		case "eth_sendRawTransaction":
			if failSend != nil && failSend.Swap(false) {
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": "node hiccup"}})
				return
			}
			n := sent.Add(1)
			res = fmt.Sprintf("0x%064x", n)
		default:
			res = "0x0"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}))
	t.Cleanup(srv.Close)
	return eth.NewClient(srv.URL)
}

func signedAck(t *testing.T, k *eth.Contract) *Intent {
	claimer, _ := eth.NewKey()
	owner, _ := eth.NewKey()
	sw := &eth.Swap{Owner: owner.Address(), Claimer: claimer.Address(), ClaimKey: big.NewInt(5), RefundKey: big.NewInt(7),
		T1: 1_700_003_600, T2: 1_700_007_200, Grace: 3600, Value: big.NewInt(1e16), Premium: big.NewInt(1e14)}
	to := claimer.Address()
	v, r, s := claimer.Sign(eth.AckDigest(k.ChainID, k.Addr, sw.ID(), to))
	return &Intent{Kind: "ack", Swap: sw, To: to, Sig: eth.Sig{V: v + 27, R: r, S: s}}
}

// The same ack submitted many times at once (by the maker's retries or by
// anyone who saw it) must go on chain once: every copy after the first
// reverts on chain and the relayer pays its gas.
func TestRelayDuplicateIntentSentOnce(t *testing.T) {
	var sent atomic.Int32
	key, _ := eth.NewKey()
	k := &eth.Contract{C: fakeChain(t, &sent), Addr: eth.Address{0xf2, 0x96}, ChainID: big.NewInt(421614)}
	s := &Server{K: k, Key: key}
	in := signedAck(t, k)
	if _, err := s.SubmitIntent(context.Background(), in); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.SubmitIntent(context.Background(), in) }()
	}
	wg.Wait()
	if n := sent.Load(); n != 1 {
		t.Fatalf("DUPLICATE RELAY: one ack was broadcast %d times (the copies revert and burn the relayer's gas)", n)
	}
}

// A submission that failed to broadcast can be sent again; a repeat after
// success gets the first hash back.
func TestRelayRetryAfterFailure(t *testing.T) {
	var sent atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	key, _ := eth.NewKey()
	k := &eth.Contract{C: fakeChainFail(t, &sent, &fail), Addr: eth.Address{0xf2, 0x96}, ChainID: big.NewInt(421614)}
	s := &Server{K: k, Key: key}
	in := signedAck(t, k)
	if _, err := s.SubmitIntent(context.Background(), in); err == nil {
		t.Fatal("first submit should fail")
	}
	h1, err := s.SubmitIntent(context.Background(), in)
	if err != nil || sent.Load() != 1 {
		t.Fatalf("retry: %v, sent %d", err, sent.Load())
	}
	h2, err := s.SubmitIntent(context.Background(), in)
	if err != nil || h2 != h1 || sent.Load() != 1 {
		t.Fatalf("repeat: %v %s %s, sent %d", err, h1.Hex(), h2.Hex(), sent.Load())
	}
}
