package registry

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// Entry is one listing as rebuilt from events.
type Entry struct {
	ID         uint64      `json:"id"`
	Owner      eth.Address `json:"owner"`
	Tag        string      `json:"tag"`
	Sesh       string      `json:"sesh"`
	Meta       string      `json:"meta"`
	Bond       string      `json:"bond"` // wei, decimal
	Status     uint8       `json:"status"`
	Challenged bool        `json:"challenged,omitempty"`
	Block      uint64      `json:"block"`
	Tx         eth.Hash    `json:"tx"`
	Slashes    int         `json:"slashes,omitempty"`
}

func (e *Entry) BondWei() *big.Int {
	x, ok := new(big.Int).SetString(e.Bond, 10)
	if !ok {
		return new(big.Int)
	}
	return x
}

// Index is a local copy of every listing, kept up to date from events.
// It is a cache: the chain is the source of truth, and the wallet re-reads
// a maker's bond with Info before trusting it for a swap.
type Index struct {
	Registry  eth.Address       `json:"registry"`
	FromBlock uint64            `json:"fromBlock"` // deployment block
	LastBlock uint64            `json:"lastBlock"`
	Listings  map[uint64]*Entry `json:"listings"`

	mu   sync.Mutex
	path string
}

// OpenIndex loads the index file at path (or starts empty). path "" = memory only.
func OpenIndex(path string, reg eth.Address, fromBlock uint64) *Index {
	ix := &Index{Registry: reg, FromBlock: fromBlock, Listings: map[uint64]*Entry{}, path: path}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var old Index
			if json.Unmarshal(b, &old) == nil && old.Registry == reg && old.Listings != nil {
				ix.LastBlock = old.LastBlock
				ix.Listings = old.Listings
				if old.FromBlock != 0 && fromBlock == 0 {
					ix.FromBlock = old.FromBlock
				}
			}
		}
	}
	return ix
}

func (ix *Index) save() error {
	if ix.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(ix, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ix.path), 0o700); err != nil {
		return err
	}
	tmp := ix.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ix.path)
}

// Sync reads new events up to the head. Ranges shrink on RPC errors (public
// endpoints cap eth_getLogs ranges).
func (ix *Index) Sync(ctx context.Context, c *eth.Client) error {
	head, err := c.Head(ctx)
	if err != nil {
		return err
	}
	ix.mu.Lock()
	from := ix.LastBlock + 1
	if ix.LastBlock == 0 {
		from = ix.FromBlock
	}
	ix.mu.Unlock()
	step := uint64(500_000)
	for from <= head.Number {
		to := from + step - 1
		if to > head.Number {
			to = head.Number
		}
		logs, err := c.Logs(ctx, ix.Registry, from, to)
		if err != nil {
			if step > 2_000 && ctx.Err() == nil {
				step /= 4
				continue
			}
			return err
		}
		ix.mu.Lock()
		for i := range logs {
			if !logs[i].Removed {
				ix.apply(&logs[i])
			}
		}
		ix.LastBlock = to
		ix.mu.Unlock()
		from = to + 1
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.save()
}

func (ix *Index) get(id uint64) *Entry {
	e := ix.Listings[id]
	if e == nil {
		e = &Entry{ID: id, Bond: "0"}
		ix.Listings[id] = e
	}
	return e
}

func (ix *Index) apply(l *eth.Log) {
	if len(l.Topics) == 0 {
		return
	}
	d := l.Data()
	switch l.Topics[0] {
	case TopicListed:
		if len(l.Topics) < 4 {
			return
		}
		e := ix.get(topicU(l.Topics[1]))
		copy(e.Owner[:], l.Topics[2][12:])
		e.Tag = TagString(l.Topics[3])
		off1 := int(wBig(d, 0).Int64())
		off2 := int(wBig(d, 1).Int64())
		e.Sesh, _ = decodeString(d, off1)
		e.Meta, _ = decodeString(d, off2)
		e.Bond = wBig(d, 2).String()
		e.Status = StatusActive
		e.Block = l.Block()
		e.Tx = l.TxHash
	case TopicMetaUpdated:
		e := ix.get(topicU(l.Topics[1]))
		e.Meta, _ = decodeString(d, int(wBig(d, 0).Int64()))
	case TopicToppedUp:
		ix.get(topicU(l.Topics[1])).Bond = wBig(d, 1).String()
	case TopicExitReq:
		ix.get(topicU(l.Topics[1])).Status = StatusExiting
	case TopicExitCancel:
		ix.get(topicU(l.Topics[1])).Status = StatusActive
	case TopicWithdrawn:
		e := ix.get(topicU(l.Topics[1]))
		if e.Status != StatusRemoved {
			e.Status = StatusWithdrawn
		}
		e.Bond = "0"
	case TopicChallenged:
		ix.get(topicU(l.Topics[1])).Challenged = true
	case TopicAnswered:
		ix.get(topicU(l.Topics[1])).Challenged = false
	case TopicUpheld:
		e := ix.get(topicU(l.Topics[1]))
		e.Challenged = false
		e.Status = StatusRemoved
		b := e.BondWei()
		b.Sub(b, wBig(d, 1))
		if b.Sign() < 0 {
			b.SetInt64(0)
		}
		e.Bond = b.String()
	case TopicSlashed:
		if len(l.Topics) < 3 {
			return
		}
		e := ix.get(topicU(l.Topics[2]))
		b := e.BondWei()
		b.Sub(b, wBig(d, 1))
		if b.Sign() < 0 {
			b.SetInt64(0)
		}
		e.Bond = b.String()
		e.Slashes++
	}
}

// Search returns active listings matching tag (exact, "" = any) and every
// word of q (case-insensitive, in the name, tag or description), makers
// with the biggest bond first, then newest.
func (ix *Index) Search(q, tag string, includeInactive bool) []Entry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	words := strings.Fields(strings.ToLower(q))
	tag = strings.ToLower(strings.TrimSpace(tag))
	var out []Entry
	for _, e := range ix.Listings {
		if e.Sesh == "" {
			continue
		}
		if !includeInactive && e.Status != StatusActive {
			continue
		}
		if tag != "" && e.Tag != tag {
			continue
		}
		hay := strings.ToLower(e.Sesh + " " + e.Tag + " " + e.Meta)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		bi, bj := out[i].BondWei(), out[j].BondWei()
		if c := bi.Cmp(bj); c != 0 {
			return c > 0
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// Makers returns active maker listings.
// Get returns a copy of one listing.
func (ix *Index) Get(id uint64) (Entry, bool) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	e, ok := ix.Listings[id]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

func (ix *Index) Makers() []Entry { return ix.Search("", TagMaker, false) }

// Tags counts active listings per tag.
func (ix *Index) Tags() map[string]int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	m := map[string]int{}
	for _, e := range ix.Listings {
		if e.Status == StatusActive && e.Sesh != "" {
			m[e.Tag]++
		}
	}
	return m
}

func (ix *Index) Last() uint64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.LastBlock
}
