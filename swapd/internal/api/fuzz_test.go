package api

import (
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/registry"
)

// A maker's signed offer document (fetched over .sesh) is untrusted.
func FuzzVerifyOffer(f *testing.F) {
	f.Add([]byte(`{"payload":"e30=","sig":"00"}`), "")
	f.Fuzz(func(t *testing.T, b []byte, want string) {
		var s SignedOffer
		if json.Unmarshal(b, &s) != nil {
			return
		}
		_, _ = VerifyOffer(&s, want, time.Now())
	})
}

// A maker's swap response (quote, signed terms, registry commitment).
func FuzzSwapResponse(f *testing.F) {
	f.Add([]byte(`{"id":"x","makerId":"00","sig":"00","premium":"1"}`), "1")
	f.Add([]byte(`{"registry":{"commitment":{"claimKey":"5","value":"7","penalty":"1157920892373161954235709850086879078532699846656405640394575840079131296399360"}}}`), "7")
	f.Fuzz(func(t *testing.T, b []byte, v string) {
		var r SwapResponse
		if json.Unmarshal(b, &r) != nil {
			return
		}
		value, ok := new(big.Int).SetString(v, 10)
		if !ok {
			value = big.NewInt(1)
		}
		_ = verifyQuote(&r, value, eth.Address{})
		if r.Registry != nil {
			_ = VerifyCommitment(r.Registry, big.NewInt(11155111), r.Registry.Registry, eth.Address{}, r.Registry.Commitment.ListingID,
				r.Registry.Commitment.Taker, r.Registry.Commitment.Claimer, big.NewInt(5), r.Registry.Commitment.Asset, big.NewInt(7))
		}
	})
}

var _ = registry.SignedCommitment{}

// The maker's public read-only routes, with random paths and query values.
func FuzzMakerGET(f *testing.F) {
	f.Add("/v1/quote", "1000000000000000")
	f.Add("/v1/offer", "")
	f.Add("/v1/quote", "-5")
	f.Fuzz(func(t *testing.T, path, value string) {
		m := &Maker{Offer: Offer{
			ChainID: big.NewInt(11155111), PicoPerUnit: big.NewInt(4e12),
			Min: big.NewInt(1e15), Max: big.NewInt(1e17),
			D1: 1800, D2: 1800, MinConfs: 10, XmrNet: keys.Stagenet, TTL: 900,
		}}
		switch path {
		case "/v1/offer", "/v1/quote", "/v1/offer.signed":
		default:
			return // other routes need a swap engine
		}
		if v, ok := new(big.Int).SetString(value, 10); ok && path == "/v1/quote" && v.Cmp(m.Offer.Min) >= 0 && v.Cmp(m.Offer.Max) <= 0 {
			return // an in-range quote needs a swap engine
		}
		req := httptest.NewRequest("GET", path+"?value="+url.QueryEscape(value), nil)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
	})
}
