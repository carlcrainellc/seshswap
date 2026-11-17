package wallet

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/exits"
)

// A Leased log as ExitLease emits it, for buyer `who` and .sesh `sesh`.
func leasedLog(t *testing.T, leaseID, listingID uint64, who eth.Address, sesh string, end uint64) map[string]any {
	t.Helper()
	pad := func(b []byte) []byte {
		if r := len(b) % 32; r != 0 {
			b = append(b, make([]byte, 32-r)...)
		}
		return b
	}
	var d []byte
	d = append(d, eth.WordAddr(eth.Address{1})...) // payout
	d = append(d, eth.WordAddr(eth.Address{})...)  // asset: ETH
	d = append(d, eth.WordU(20_000_000_000_000)...)
	d = append(d, eth.WordU(1)...)
	d = append(d, eth.WordU(end-3600)...)
	d = append(d, eth.WordU(end)...)
	d = append(d, eth.WordU(7*32)...)
	d = append(d, eth.WordU(uint64(len(sesh)))...)
	d = append(d, pad([]byte(sesh))...)
	h := func(b []byte) string { return "0x" + hex.EncodeToString(b) }
	return map[string]any{
		"address": "0x145049b20f382ca9d0b281a986dc02deb653e65c",
		"topics":  []string{h(exits.TopicLeased[:]), h(eth.WordU(leaseID)), h(eth.WordU(listingID)), h(eth.WordAddr(who))},
		"data":    h(d), "blockNumber": "0x10", "transactionHash": h(eth.WordU(leaseID + 1000)), "removed": false,
	}
}

func fakeChain(t *testing.T, logs []map[string]any) *eth.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(b, &req)
		var res any
		switch req.Method {
		case "eth_getBlockByNumber":
			res = map[string]string{"number": "0x20", "timestamp": fmt.Sprintf("0x%x", time.Now().Unix())}
		case "eth_getLogs":
			res = logs
		default:
			res = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}))
	t.Cleanup(srv.Close)
	return eth.NewClient(srv.URL)
}

// Time bought for the wallet's exit key by a swap claim shows up as its own
// purchase, so the wallet can connect with it.
func TestExitTimePaidBySwapIsFound(t *testing.T) {
	s, _ := exitTestServer(t)
	k, err := exits.RouterKey(s.exitKeyFile(), true)
	if err != nil {
		t.Fatal(err)
	}
	sesh := exits.SeshFromPub(k.Public().(ed25519.PublicKey))
	end := uint64(time.Now().Add(50 * time.Minute).Unix())
	forwarder := eth.Address{0x5e, 0x8a}
	c := fakeChain(t, []map[string]any{
		leasedLog(t, 1, 4, forwarder, sesh, end),
		leasedLog(t, 2, 4, forwarder, "someoneelse0000000000000000000000000000000000000000.sesh", end),
	})
	s.ex.activeUntil = func(_ context.Context, id uint64, b string) (uint64, error) {
		if id == 4 && b == sesh {
			return end, nil
		}
		return 0, nil
	}
	lc := &exits.Contract{C: c, Addr: s.Cfg.ExitLease}
	rows := []exitRow{{ID: 4, Sesh: testExit, Meta: "exit; country=DE; dns=10.195.0.53"}, {ID: 5, Sesh: testExit}}
	s.syncPaidElsewhere(context.Background(), lc, rows)
	st := s.exitsStatusMap()
	ls := st["leases"].([]map[string]any)
	if len(ls) != 1 {
		t.Fatalf("want 1 lease, got %d", len(ls))
	}
	l := ls[0]
	if l["via"] != "swap" || l["active"] != true || l["buyerSesh"] != sesh || l["listingId"] != uint64(4) {
		t.Fatalf("bad lease row: %v", l)
	}
	s.syncPaidElsewhere(context.Background(), lc, rows) // already known: no duplicate
	if n := len(s.exitsStatusMap()["leases"].([]map[string]any)); n != 1 {
		t.Fatalf("duplicate lease rows: %d", n)
	}
}

// Without an exit key nothing is looked up (the key isn't created by listing).
func TestExitPaidElsewhereNeedsKey(t *testing.T) {
	s, _ := exitTestServer(t)
	s.ex.activeUntil = func(context.Context, uint64, string) (uint64, error) {
		t.Fatal("looked up time without an exit key")
		return 0, nil
	}
	s.syncPaidElsewhere(context.Background(), &exits.Contract{}, []exitRow{{ID: 4}})
	if _, err := exits.RouterKey(s.exitKeyFile(), false); err == nil {
		t.Fatal("listing created an exit key")
	}
}
