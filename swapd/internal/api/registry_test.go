package api

import (
	"math/big"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
)

func TestVerifyCommitmentRefusesAnythingThatDoesntMatch(t *testing.T) {
	owner, _ := eth.KeyFromHex("5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a")
	other, _ := eth.KeyFromHex("7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6")
	reg, _ := eth.ParseAddress("0x00000000000000000000000000000000000000aa")
	taker, _ := eth.ParseAddress("0x00000000000000000000000000000000000000bb")
	claimer, _ := eth.ParseAddress("0x00000000000000000000000000000000000000cc")
	cid := big.NewInt(421614)
	mk := func(k *eth.Key, f func(c *registry.Commitment)) *registry.SignedCommitment {
		c := registry.Commitment{ListingID: 7, Taker: taker, Claimer: claimer, ClaimKey: big.NewInt(12345),
			Value: big.NewInt(1e16), XMRAtomic: 1e11, Penalty: big.NewInt(5e14), AttestWindow: 600, Expiry: 2e9}
		if f != nil {
			f(&c)
		}
		return &registry.SignedCommitment{Registry: reg, Commitment: c, Sig: registry.SignCommitment(k, cid, reg, &c)}
	}
	check := func(sc *registry.SignedCommitment) error {
		return VerifyCommitment(sc, cid, reg, owner.Address(), 7, taker, claimer, big.NewInt(12345), eth.Address{}, big.NewInt(1e16))
	}
	if err := check(mk(owner, nil)); err != nil {
		t.Fatalf("good commitment refused: %v", err)
	}
	bad := map[string]*registry.SignedCommitment{
		"not signed by":     mk(other, nil),
		"different listing": mk(owner, func(c *registry.Commitment) { c.ListingID = 8 }),
		"different amount":  mk(owner, func(c *registry.Commitment) { c.Value = big.NewInt(2e16) }),
		"Monero key":        mk(owner, func(c *registry.Commitment) { c.ClaimKey = big.NewInt(1) }),
		"doesn't match":     mk(owner, func(c *registry.Commitment) { c.Claimer = taker }),
		"no penalty":        mk(owner, func(c *registry.Commitment) { c.Penalty = big.NewInt(0) }),
	}
	for want, sc := range bad {
		if err := check(sc); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	// A signature over other terms than the ones shown is refused too.
	sc := mk(owner, nil)
	sc.Commitment.XMRAtomic = 1
	if err := check(sc); err == nil {
		t.Error("tampered commitment accepted")
	}
}
