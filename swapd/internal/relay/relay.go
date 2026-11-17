// Package relay is a stateless relayer: it submits claims signed by XMR
// sellers (who hold no ETH) and is paid the signed fee out of the swap.
// It never needs to be trusted with funds: the signature fixes recipient and fee.
//
// It also submits the claimer's secret-free messages (claim intents and
// "XMR locked" acks) for free: they carry no secret, and the relayer is paid
// when it later submits the claim itself.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
)

// HTTP is used by Post and PostIntent. swapd sets this at start so .sesh
// relayer URLs go through Session Router DNS. If it is nil, sesh.DefaultClient is used.
var HTTP *http.Client

func httpClient() *http.Client {
	if HTTP != nil {
		return HTTP
	}
	return sesh.DefaultClient()
}

type Server struct {
	K   *eth.Contract
	Key *eth.Key
	// MinFeeMultiplier: accept only if fee >= estimatedGas * feeCap * multiplier (ETH swaps).
	MinFeeMultiplier float64
	// TokenMinFee: for ERC-20 swaps, minimum fee per token (token units); unknown tokens are refused.
	TokenMinFee map[eth.Address]*big.Int
	// Refuse (optional operator policy) declines some kinds ("claim", "claimIntent",
	// "ack") for some swaps.
	Refuse func(kind string, swapID eth.Hash) bool
	Logf   func(string, ...any)
	mu     sync.Mutex // serialize nonces
}

type Response struct {
	TxHash string `json:"txHash,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

// Intent is a claimer-signed message without a secret: a claim intent, or an
// "XMR locked" ack naming where a forfeited premium goes (To).
type Intent struct {
	Kind string      `json:"kind"` // "claimIntent" or "ack"
	Swap *eth.Swap   `json:"swap"`
	To   eth.Address `json:"to,omitempty"`
	Sig  eth.Sig     `json:"sig"`
}

// FeeQuote is a token-denominated (or wei, for ETH) minimum fee this relayer accepts.
type FeeQuote struct {
	Asset string `json:"asset"`
	Fee   string `json:"fee"`   // integer string, in Units
	Units string `json:"units"` // "token" or "wei"
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/fee" {
		asset, err := eth.ParseAddress(r.URL.Query().Get("asset"))
		if err != nil && r.URL.Query().Get("asset") != "" {
			writeJSON(w, 400, FeeQuote{Error: "bad asset"})
			return
		}
		q, status := s.quoteFee(asset)
		writeJSON(w, status, q)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/intent" {
		var in Intent
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSON(w, 400, Response{Error: "bad json"})
			return
		}
		h, err := s.SubmitIntent(r.Context(), &in)
		if err != nil {
			writeJSON(w, 422, Response{Error: err.Error()})
			return
		}
		writeJSON(w, 200, Response{TxHash: h.Hex()})
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/relay" {
		http.Error(w, "not found", 404)
		return
	}
	var c eth.RelayedClaim
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&c); err != nil {
		writeJSON(w, 400, Response{Error: "bad json"})
		return
	}
	h, err := s.Submit(r.Context(), &c)
	if err != nil {
		writeJSON(w, 422, Response{Error: err.Error()})
		return
	}
	writeJSON(w, 200, Response{TxHash: h.Hex()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Submit validates and broadcasts a relayed claim.
func (s *Server) Submit(ctx context.Context, c *eth.RelayedClaim) (eth.Hash, error) {
	if c.Swap == nil || c.Secret == nil || c.Fee == nil || c.Swap.Value == nil || c.Swap.ClaimKey == nil || c.Swap.RefundKey == nil {
		return eth.Hash{}, errors.New("missing fields")
	}
	if s.Refuse != nil && s.Refuse("claim", c.Swap.ID()) {
		return eth.Hash{}, errors.New("relayer policy: not relaying this")
	}
	if !c.Relayer.IsZero() && c.Relayer != s.Key.Address() {
		return eth.Hash{}, errors.New("claim is bound to another relayer")
	}
	if err := eth.VerifyClaimSig(s.K.ChainID, s.K.Addr, c); err != nil {
		return eth.Hash{}, err
	}
	data := eth.EncodeClaimRelayed(c)
	// Token fees can be checked before simulation so a leftover ETH wei fee
	// (fee >= value) is refused with a clear error instead of BadParams.
	if !c.Swap.Asset.IsZero() {
		if err := s.checkFee(c, nil); err != nil {
			return eth.Hash{}, err
		}
	}
	msg := eth.CallMsg{From: s.Key.Address(), To: &s.K.Addr, Data: data}
	// simulate first: a reverted claim would publish the secret for nothing
	if _, err := s.K.C.EthCall(ctx, msg); err != nil {
		return eth.Hash{}, fmt.Errorf("simulation failed: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.K.C.BuildTx(ctx, s.Key, &s.K.Addr, nil, data)
	if err != nil {
		return eth.Hash{}, err
	}
	if err := s.checkFee(c, tx); err != nil {
		return eth.Hash{}, err
	}
	raw, h := tx.Sign(s.Key)
	if _, err := s.K.C.SendRaw(ctx, raw); err != nil {
		return eth.Hash{}, err
	}
	s.logf("relay: submitted claim for swap %s fee %s tx %s", c.Swap.ID().Hex(), c.Fee, h.Hex())
	return h, nil
}

// SubmitIntent validates and broadcasts a claim intent or an ack. Free.
func (s *Server) SubmitIntent(ctx context.Context, in *Intent) (eth.Hash, error) {
	if s.Refuse != nil && s.Refuse(in.Kind, in.Swap.ID()) {
		return eth.Hash{}, errors.New("relayer policy: not relaying this")
	}
	if in.Swap == nil || in.Swap.Value == nil || in.Swap.ClaimKey == nil || in.Swap.RefundKey == nil {
		return eth.Hash{}, errors.New("missing fields")
	}
	var data []byte
	switch in.Kind {
	case "claimIntent":
		if err := eth.VerifyIntentSig(s.K.ChainID, s.K.Addr, in.Swap, in.Sig); err != nil {
			return eth.Hash{}, err
		}
		data = eth.EncodeClaimIntent(in.Swap, in.Sig)
	case "ack":
		if in.Sig.V != 27 && in.Sig.V != 28 {
			return eth.Hash{}, errors.New("bad v")
		}
		a, err := eth.Recover(eth.AckDigest(s.K.ChainID, s.K.Addr, in.Swap.ID(), in.To), in.Sig.V-27, [32]byte(in.Sig.R), [32]byte(in.Sig.S))
		if err != nil || a != in.Swap.Claimer {
			return eth.Hash{}, errors.New("ack signature is not from the swap's claimer")
		}
		data = eth.EncodeAckLocked(in.Swap, in.To, in.Sig)
	default:
		return eth.Hash{}, errors.New("unknown intent kind")
	}
	if _, err := s.K.C.EthCall(ctx, eth.CallMsg{From: s.Key.Address(), To: &s.K.Addr, Data: data}); err != nil {
		return eth.Hash{}, fmt.Errorf("simulation failed: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.K.C.BuildTx(ctx, s.Key, &s.K.Addr, nil, data)
	if err != nil {
		return eth.Hash{}, err
	}
	raw, h := tx.Sign(s.Key)
	if _, err := s.K.C.SendRaw(ctx, raw); err != nil {
		return eth.Hash{}, err
	}
	s.logf("relay: submitted %s for swap %s tx %s", in.Kind, in.Swap.ID().Hex(), h.Hex())
	return h, nil
}

func (s *Server) checkFee(c *eth.RelayedClaim, tx *eth.Tx) error {
	if c.Fee == nil {
		return errors.New("missing fee")
	}
	if c.Swap.Value != nil && c.Fee.Cmp(c.Swap.Value) >= 0 {
		return fmt.Errorf("fee %s is not less than locked value %s (fee must be in asset units)", c.Fee, c.Swap.Value)
	}
	if c.Swap.Asset.IsZero() {
		if tx == nil {
			return errors.New("missing gas estimate for ETH fee")
		}
		cost := new(big.Float).SetInt(new(big.Int).Mul(new(big.Int).SetUint64(tx.Gas), tx.FeeCap))
		need, _ := cost.Mul(cost, big.NewFloat(s.MinFeeMultiplier)).Int(nil)
		if c.Fee.Cmp(need) < 0 {
			return fmt.Errorf("fee too low: need >= %s wei", need)
		}
		return nil
	}
	min, ok := s.TokenMinFee[c.Swap.Asset]
	if !ok {
		return errors.New("token not accepted by this relayer")
	}
	if c.Fee.Cmp(min) < 0 {
		return fmt.Errorf("fee too low: need >= %s token units", min)
	}
	return nil
}

// quoteFee returns the advertised minimum fee for asset, in token units (or wei).
func (s *Server) quoteFee(asset eth.Address) (FeeQuote, int) {
	q := FeeQuote{Asset: asset.Hex()}
	if asset.IsZero() {
		q.Fee = "0"
		q.Units = "wei"
		q.Note = "ETH minimum is estimated at submit time from gas and MinFeeMultiplier"
		return q, 200
	}
	min, ok := s.TokenMinFee[asset]
	if !ok || min == nil {
		q.Units = "token"
		q.Error = "token not accepted by this relayer"
		return q, 422
	}
	q.Fee = min.String()
	q.Units = "token"
	return q, 200
}

// QuoteFee asks a relayer for its minimum fee on asset (token units or wei).
func QuoteFee(ctx context.Context, url string, asset eth.Address) (*big.Int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url+"/v1/fee?asset="+asset.Hex(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var q FeeQuote
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		return nil, "", err
	}
	if q.Error != "" {
		return nil, q.Units, errors.New(q.Error)
	}
	fee, ok := new(big.Int).SetString(q.Fee, 10)
	if !ok {
		return nil, q.Units, errors.New("bad fee in quote")
	}
	return fee, q.Units, nil
}

// Post posts a claim to a relayer URL.
func Post(ctx context.Context, url string, c *eth.RelayedClaim) (eth.Hash, error) {
	return post(ctx, url+"/v1/relay", c)
}

// PostIntent posts a claim intent or an ack to a relayer URL.
func PostIntent(ctx context.Context, url string, in *Intent) (eth.Hash, error) {
	return post(ctx, url+"/v1/intent", in)
}

func post(ctx context.Context, url string, v any) (eth.Hash, error) {
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return eth.Hash{}, err
	}
	defer resp.Body.Close()
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return eth.Hash{}, err
	}
	if out.Error != "" {
		return eth.Hash{}, errors.New(out.Error)
	}
	return eth.ParseHash(out.TxHash)
}
