package api

import (
	"math/big"
	"path/filepath"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

func TestCheckOpenQuotes(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "s"), "test")
	if err != nil {
		t.Fatal(err)
	}
	eng := &swap.Engine{Role: swap.Maker, Store: st, Policy: swap.DefaultPolicy()}
	if err := eng.Load(); err != nil {
		t.Fatal(err)
	}
	m := &Maker{Engine: eng, Service: Service{MaxOpenQuotes: 2}}
	if err := m.checkOpenQuotes(); err != nil {
		t.Fatalf("empty: %v", err)
	}
	for _, id := range []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	} {
		if err := eng.Add(&swap.Record{ID: id, Role: swap.Maker, Phase: swap.MNegotiated, Value: big.NewInt(1), Asset: eth.Address{}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.checkOpenQuotes(); err == nil {
		t.Fatal("expected open-quote limit")
	}
	m.MaxOpenQuotes = 0
	if err := m.checkOpenQuotes(); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
}
