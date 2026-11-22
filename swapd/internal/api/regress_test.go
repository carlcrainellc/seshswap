package api

import (
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
)

// Found by FuzzSwapResponse: a maker could send a commitment whose penalty
// is over 256 bits; the taker's VerifyCommitment then panicked while hashing
// it (Take would crash instead of refusing the maker).
func TestVerifyCommitmentHugePenalty(t *testing.T) {
	sc := &registry.SignedCommitment{Commitment: registry.Commitment{
		ClaimKey: big.NewInt(5), Value: big.NewInt(7), Penalty: new(big.Int).Lsh(big.NewInt(1), 300)}}
	if err := VerifyCommitment(sc, big.NewInt(1), eth.Address{}, eth.Address{}, 0, eth.Address{}, eth.Address{},
		big.NewInt(5), eth.Address{}, big.NewInt(7)); err == nil {
		t.Fatal("want error")
	}
}
