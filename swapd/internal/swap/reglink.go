package swap

import (
	"encoding/json"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
)

// RegLink ties a swap to a bonded maker listing in SeshRegistry. The maker
// signs a commitment for the swap; the taker records it on-chain right after
// the ETH lock; the maker (or any watchtower) acks once the XMR lock is
// broadcast; with no ack by attestBy anyone may slash the maker's penalty to
// the taker. None of this changes the swap itself (XmrSwapV3): the taker's
// refund path is the same with or without it.
type RegLink struct {
	Registry  eth.Address     `json:"registry"`
	ListingID uint64          `json:"listingId"`
	Sesh      string          `json:"sesh,omitempty"`
	Signed    json.RawMessage `json:"signed"` // registry.SignedCommitment
	CommitTx  eth.Hash        `json:"commitTx,omitempty"`
	Committed bool            `json:"committed,omitempty"`
	AttestBy  uint64          `json:"attestBy,omitempty"`
	AckTx     eth.Hash        `json:"ackTx,omitempty"`
	Acked     bool            `json:"acked,omitempty"`
	SlashTx   eth.Hash        `json:"slashTx,omitempty"`
	Slashed   bool            `json:"slashed,omitempty"`
	Released  bool            `json:"released,omitempty"`
	Skipped   bool            `json:"skipped,omitempty"` // taker gave up recording it (too late / failed)
	Tries     int             `json:"tries,omitempty"`
	LastError string          `json:"lastError,omitempty"`
}
