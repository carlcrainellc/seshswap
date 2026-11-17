package api

import (
	"crypto/ed25519"
	"math/big"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

func TestOfferDocSignVerify(t *testing.T) {
	_, k, _ := ed25519.GenerateKey(nil)
	id := &Identity{k}
	d := &OfferDoc{Version: 1, Direction: "eth2xmr", ChainID: big.NewInt(31337), Min: big.NewInt(1), Max: big.NewInt(2),
		PicoPerUnit: big.NewInt(3), Expires: time.Now().Add(time.Hour).Unix()}
	so, err := id.SignOffer(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyOffer(so, id.ID(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyOffer(so, "00", time.Now()); err == nil {
		t.Fatal("wrong pinned id accepted")
	}
	if _, err := VerifyOffer(so, "", time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired offer accepted")
	}
	bad := *so
	last := "00" // always a different last byte (the old "00" was a no-op 1 time in 256: flaky CI)
	if so.Sig[len(so.Sig)-2:] == last {
		last = "01"
	}
	bad.Sig = so.Sig[:len(so.Sig)-2] + last
	if _, err := VerifyOffer(&bad, "", time.Now()); err == nil {
		t.Fatal("bad signature accepted")
	}
}

func TestQuoteSignatureBindsTerms(t *testing.T) {
	_, k, _ := ed25519.GenerateKey(nil)
	id := &Identity{k}
	owner := eth.Address{1}
	r := &SwapResponse{ID: "abc", Claimer: eth.Address{2}, XMR: 5, D1: 3600, D2: 3600, MinConfs: 10, Grace: 7200, Premium: big.NewInt(9), Expires: 100}
	id.signQuote(r, big.NewInt(1000), owner)
	if err := verifyQuote(r, big.NewInt(1000), owner); err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*SwapResponse){
		func(x *SwapResponse) { x.Claimer = eth.Address{3} },
		func(x *SwapResponse) { x.XMR = 6 },
		func(x *SwapResponse) { x.Grace = 1 },
		func(x *SwapResponse) { x.Premium = big.NewInt(10) },
		func(x *SwapResponse) { x.MinConfs = 9 },
	} {
		c := *r
		mut(&c)
		if verifyQuote(&c, big.NewInt(1000), owner) == nil {
			t.Fatal("mutated quote verified")
		}
	}
	if verifyQuote(r, big.NewInt(1001), owner) == nil || verifyQuote(r, big.NewInt(1000), eth.Address{9}) == nil {
		t.Fatal("quote not bound to value/owner")
	}
}
