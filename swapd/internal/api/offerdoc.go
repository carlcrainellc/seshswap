package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/store"
)

// OfferDoc is a maker's advertisement. It is transport-agnostic: the signed
// form (SignedOffer) is plain bytes that can travel over HTTP, Nostr, a
// message board, or a Satchel/.sesh session, and anyone can check it against
// the maker's long-term identity key (MakerID) without trusting the carrier.
type OfferDoc struct {
	Version     int          `json:"v"`
	MakerID     string       `json:"makerId"` // hex ed25519 public key
	Endpoints   []string     `json:"endpoints"`
	Direction   string       `json:"direction"` // "eth2xmr": maker sells XMR for the asset
	ChainID     *big.Int     `json:"chainId"`
	Contract    eth.Address  `json:"contract"`
	Asset       eth.Address  `json:"asset"`
	XmrNet      keys.Network `json:"xmrNet"`
	PicoPerUnit *big.Int     `json:"picoPerUnit"` // indicative; the binding price is in the signed quote
	Min         *big.Int     `json:"min"`
	Max         *big.Int     `json:"max"`
	SpreadBps   uint64       `json:"spreadBps"`
	PremiumBps  uint64       `json:"premiumBps"`
	MinConfs    uint64       `json:"minConfs"`
	Grace       uint64       `json:"grace"`
	Issued      int64        `json:"issued"`
	Expires     int64        `json:"expires"`
}

// SignedOffer carries the exact signed bytes, so no re-encoding can break the signature.
type SignedOffer struct {
	Payload string `json:"payload"` // base64 of the OfferDoc JSON
	Sig     string `json:"sig"`     // hex ed25519 signature over "xmrswap-offer-v1|" + payload bytes
}

const offerDomain = "xmrswap-offer-v1|"

// Identity is the maker's long-term ed25519 key, kept in the encrypted store.
type Identity struct{ Key ed25519.PrivateKey }

func (i *Identity) ID() string { return hex.EncodeToString(i.Key.Public().(ed25519.PublicKey)) }

// LoadIdentity loads the maker identity from the store, creating it on first use.
func LoadIdentity(st *store.Store) (*Identity, error) {
	var h string
	if err := st.Get("identity", &h); err == nil {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != ed25519.SeedSize {
			return nil, errors.New("corrupt identity")
		}
		return &Identity{ed25519.NewKeyFromSeed(b)}, nil
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if err := st.Put("identity", hex.EncodeToString(seed)); err != nil {
		return nil, err
	}
	return &Identity{ed25519.NewKeyFromSeed(seed)}, nil
}

func (i *Identity) SignOffer(d *OfferDoc) (*SignedOffer, error) {
	d.MakerID = i.ID()
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(i.Key, append([]byte(offerDomain), b...))
	return &SignedOffer{Payload: base64.StdEncoding.EncodeToString(b), Sig: hex.EncodeToString(sig)}, nil
}

// VerifyOffer checks a signed offer (and, if wantID != "", that it is from that maker).
func VerifyOffer(s *SignedOffer, wantID string, now time.Time) (*OfferDoc, error) {
	b, err := base64.StdEncoding.DecodeString(s.Payload)
	if err != nil {
		return nil, err
	}
	var d OfferDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	pub, err := hex.DecodeString(d.MakerID)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("bad maker id")
	}
	sig, err := hex.DecodeString(s.Sig)
	if err != nil || !ed25519.Verify(pub, append([]byte(offerDomain), b...), sig) {
		return nil, errors.New("offer signature does not verify")
	}
	if wantID != "" && d.MakerID != wantID {
		return nil, errors.New("offer is from a different maker")
	}
	if d.Expires != 0 && now.Unix() > d.Expires {
		return nil, errors.New("offer expired")
	}
	return &d, nil
}

// quoteBytes is what the maker signs in a swap response: it binds the
// per-swap claimer address and every term to the maker's identity, so a
// taker who pinned a MakerID knows the terms came from that maker.
func quoteBytes(r *SwapResponse, value *big.Int, owner eth.Address) []byte {
	b := []byte("xmrswap-quote-v1|" + r.ID + "|")
	b = append(b, r.Claimer[:]...)
	b = append(b, owner[:]...)
	b = append(b, []byte(value.String()+"|"+r.Premium.String()+"|")...)
	for _, x := range []uint64{r.XMR, r.D1, r.D2, r.MinConfs, r.Grace, uint64(r.Expires)} {
		b = binary.BigEndian.AppendUint64(b, x)
	}
	return b
}

func (i *Identity) signQuote(r *SwapResponse, value *big.Int, owner eth.Address) {
	r.MakerID = i.ID()
	r.Sig = hex.EncodeToString(ed25519.Sign(i.Key, quoteBytes(r, value, owner)))
}

func verifyQuote(r *SwapResponse, value *big.Int, owner eth.Address) error {
	pub, err := hex.DecodeString(r.MakerID)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("bad maker id")
	}
	sig, err := hex.DecodeString(r.Sig)
	if err != nil || !ed25519.Verify(pub, quoteBytes(r, value, owner), sig) {
		return errors.New("quote signature does not verify")
	}
	return nil
}
