package api

import (
	"encoding/json"
	"math/big"
	"net/http"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
)

// A real Maker.ServeHTTP offer is fetched over a Loop .sesh name — the same
// HTTP the taker will use when MakerURL is http://<name>.sesh.
func TestOfferOverSeshLoop(t *testing.T) {
	m := &Maker{Offer: Offer{
		ChainID:     big.NewInt(11155111),
		PicoPerUnit: big.NewInt(4e12),
		Min:         big.NewInt(1e15),
		Max:         big.NewInt(1e17),
		D1:          1800, D2: 1800, MinConfs: 10,
		XmrNet: keys.Stagenet, TTL: 900,
	}}
	loop := sesh.NewLoop()
	defer loop.Close()
	url, err := loop.Serve("maker.sesh", m)
	if err != nil {
		t.Fatal(err)
	}
	old := HTTP
	HTTP = loop.Client()
	t.Cleanup(func() { HTTP = old })

	var o Offer
	if err := getJSON(t.Context(), HTTP, url+"/v1/offer", &o); err != nil {
		t.Fatal(err)
	}
	if o.XmrNet != keys.Stagenet || o.D1 != 1800 || o.Direction != "eth2xmr" {
		b, _ := json.Marshal(o)
		t.Fatalf("unexpected offer %s", b)
	}
	if o.PicoPerUnit == nil || o.PicoPerUnit.Cmp(big.NewInt(4e12)) != 0 {
		t.Fatalf("pico %v", o.PicoPerUnit)
	}
}

func TestNotifyLockedOverSeshLoop(t *testing.T) {
	seen := make(chan string, 1)
	loop := sesh.NewLoop()
	defer loop.Close()
	_, err := loop.Serve("maker.sesh", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":"true"}`))
	}))
	if err != nil {
		t.Fatal(err)
	}
	old := HTTP
	HTTP = loop.Client()
	t.Cleanup(func() { HTTP = old })
	NotifyLocked("abcd", "http://maker.sesh", eth.Hash{})
	select {
	case p := <-seen:
		if p != "/v1/swaps/abcd/locked" {
			t.Fatalf("path %s", p)
		}
	default:
		t.Fatal("NotifyLocked did not reach the .sesh server")
	}
}
