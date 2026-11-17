package e2e

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

func mustPoint(t *testing.T, h string) *big.Int {
	p, err := keys.PointFromHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return p.Int()
}

func mustScalarPub(t *testing.T, h string) *big.Int {
	s, err := keys.ScalarFromHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return s.Public().Int()
}

// feesPaid sums gasUsed*effectiveGasPrice over every transaction sent by addr in
// blocks [from, head], so balance checks can be exact (no tolerance).
func (e *Env) feesPaid(addr eth.Address, from uint64) *big.Int {
	h, err := e.eth.Head(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	sum := new(big.Int)
	for b := from; b <= h.Number; b++ {
		var blk struct {
			Transactions []struct {
				Hash eth.Hash    `json:"hash"`
				From eth.Address `json:"from"`
			} `json:"transactions"`
		}
		if err := e.eth.Call(e.ctx, &blk, "eth_getBlockByNumber", fmt.Sprintf("0x%x", b), true); err != nil {
			e.t.Fatal(err)
		}
		for _, tx := range blk.Transactions {
			if tx.From != addr {
				continue
			}
			rc, err := e.eth.Receipt(e.ctx, tx.Hash)
			if err != nil || rc == nil {
				e.t.Fatalf("receipt %s: %v", tx.Hash.Hex(), err)
			}
			sum.Add(sum, rc.Fee())
		}
	}
	return sum
}

func (e *Env) ethBalance(a eth.Address) *big.Int {
	b, err := e.eth.Balance(e.ctx, a)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func headNumber(e *Env) uint64 { h, _ := e.eth.Head(e.ctx); return h.Number }

// sweepFees is the Monero fee of the sweep(s) a party made for a swap.
func sweepFees(p *Party, id string) uint64 {
	var f uint64
	for _, s := range lockedRecord(p, id).Sweep {
		f += s.Fee
	}
	return f
}

var _ = swap.Taker
