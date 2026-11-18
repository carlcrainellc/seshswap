// Package xmr is a small client for monerod and monero-wallet-rpc JSON-RPC.
package xmr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type rpc struct {
	url  string
	http *http.Client
	auth *digest // from user:pass@ in the URL (monero-wallet-rpc --rpc-login); nil = none
}

func newRPC(rawURL string, timeout time.Duration) rpc {
	u, d := splitLogin(rawURL)
	return rpc{url: u, http: &http.Client{Timeout: timeout}, auth: d}
}

func (r *rpc) call(ctx context.Context, out any, method string, params any) error {
	if params == nil {
		params = map[string]any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "0", "method": method, "params": params})
	resp, err := r.post(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%s: monero RPC login refused", method)
	}
	var res struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if res.Error != nil {
		return fmt.Errorf("%s: %d %s", method, res.Error.Code, res.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(res.Result, out)
	}
	return nil
}

// post sends one JSON-RPC body, answering a Digest challenge if the URL
// carried a login.
func (r *rpc) post(ctx context.Context, body []byte) (*http.Response, error) {
	send := func(authz string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", r.url+"/json_rpc", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		return r.http.Do(req)
	}
	if r.auth == nil {
		return send("")
	}
	// Reuse the last challenge; on a fresh start or a stale nonce, answer the new one.
	resp, err := send(r.auth.header("POST", "/json_rpc"))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	r.auth.challenge(resp.Header.Get("WWW-Authenticate"))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return send(r.auth.header("POST", "/json_rpc"))
}

// Daemon is monerod.
type Daemon struct{ r rpc }

func NewDaemon(url string) *Daemon {
	return &Daemon{newRPC(url, 60*time.Second)}
}

func (d *Daemon) Height(ctx context.Context) (uint64, error) {
	var o struct {
		Height uint64 `json:"height"`
	}
	err := d.r.call(ctx, &o, "get_info", nil)
	return o.Height, err
}

// MaxAltChainDepth is the length of the longest alternative chain monerod has
// seen (a proxy for the deepest recent reorg attempt).
func (d *Daemon) MaxAltChainDepth(ctx context.Context) (uint64, error) {
	var o struct {
		Chains []struct {
			Length uint64 `json:"length"`
		} `json:"chains"`
	}
	if err := d.r.call(ctx, &o, "get_alternate_chains", nil); err != nil {
		return 0, err
	}
	var m uint64
	for _, c := range o.Chains {
		m = max(m, c.Length)
	}
	return m, nil
}

// AvgBlockSecs is the mean block interval over the last n blocks.
func (d *Daemon) AvgBlockSecs(ctx context.Context, n uint64) (uint64, error) {
	h, err := d.Height(ctx)
	if err != nil {
		return 0, err
	}
	if h < n+2 {
		return 0, fmt.Errorf("chain too short")
	}
	var o struct {
		Headers []struct {
			Timestamp uint64 `json:"timestamp"`
		} `json:"headers"`
	}
	if err := d.r.call(ctx, &o, "get_block_headers_range", map[string]any{"start_height": h - 1 - n, "end_height": h - 1}); err != nil {
		return 0, err
	}
	if len(o.Headers) < 2 {
		return 0, fmt.Errorf("no headers")
	}
	a, b := o.Headers[0].Timestamp, o.Headers[len(o.Headers)-1].Timestamp
	if b <= a {
		return 0, fmt.Errorf("bad timestamps")
	}
	return (b - a) / uint64(len(o.Headers)-1), nil
}

// GenerateBlocks mines n blocks (regtest only).
func (d *Daemon) GenerateBlocks(ctx context.Context, n int, addr string) error {
	return d.r.call(ctx, nil, "generateblocks", map[string]any{"amount_of_blocks": n, "wallet_address": addr, "starting_nonce": 0})
}

// Wallet is one monero-wallet-rpc process. It can have one wallet file open at
// a time, so every operation takes the mutex and opens the wallet it needs.
type Wallet struct {
	r        rpc
	mu       sync.Mutex
	open     string
	password string
}

func NewWallet(url, password string) *Wallet {
	return &Wallet{r: newRPC(url, 300*time.Second), password: password}
}

// Session runs fn with wallet `name` open.
func (w *Wallet) Session(ctx context.Context, name string, fn func(s *Sess) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.open != name {
		_ = w.r.call(ctx, nil, "close_wallet", nil)
		w.open = ""
		if err := w.r.call(ctx, nil, "open_wallet", map[string]any{"filename": name, "password": w.password}); err != nil {
			return err
		}
		w.open = name
	}
	return fn(&Sess{w})
}

// Create makes a new random wallet file (fails if it exists).
func (w *Wallet) Create(ctx context.Context, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.r.call(ctx, nil, "close_wallet", nil)
	w.open = ""
	err := w.r.call(ctx, nil, "create_wallet", map[string]any{"filename": name, "password": w.password, "language": "English"})
	if err == nil {
		w.open = name
	}
	return err
}

// FromKeys creates (or reuses, if it already exists) a wallet from keys.
// spendKeyHex == "" makes a view-only wallet.
func (w *Wallet) FromKeys(ctx context.Context, name, address, viewKeyHex, spendKeyHex string, restoreHeight uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.r.call(ctx, nil, "close_wallet", nil)
	w.open = ""
	if err := w.r.call(ctx, nil, "open_wallet", map[string]any{"filename": name, "password": w.password}); err == nil {
		w.open = name
		return nil
	}
	p := map[string]any{"filename": name, "address": address, "viewkey": viewKeyHex, "password": w.password,
		"restore_height": restoreHeight, "autosave_current": true}
	if spendKeyHex != "" {
		p["spendkey"] = spendKeyHex
	}
	if err := w.r.call(ctx, nil, "generate_from_keys", p); err != nil {
		return err
	}
	w.open = name
	return nil
}

type Sess struct{ w *Wallet }

func (s *Sess) call(ctx context.Context, out any, m string, p any) error {
	return s.w.r.call(ctx, out, m, p)
}

func (s *Sess) Refresh(ctx context.Context) error { return s.call(ctx, nil, "refresh", nil) }

func (s *Sess) Address(ctx context.Context) (string, error) {
	var o struct {
		Address string `json:"address"`
	}
	err := s.call(ctx, &o, "get_address", map[string]any{"account_index": 0})
	return o.Address, err
}

func (s *Sess) Balance(ctx context.Context) (total, unlocked uint64, err error) {
	var o struct {
		Balance  uint64 `json:"balance"`
		Unlocked uint64 `json:"unlocked_balance"`
	}
	err = s.call(ctx, &o, "get_balance", nil)
	return o.Balance, o.Unlocked, err
}

// BalanceLock is Balance plus how many blocks until all of it can be spent
// (0 when everything is unlocked).
func (s *Sess) BalanceLock(ctx context.Context) (total, unlocked, blocksToUnlock uint64, err error) {
	var o struct {
		Balance  uint64 `json:"balance"`
		Unlocked uint64 `json:"unlocked_balance"`
		Blocks   uint64 `json:"blocks_to_unlock"`
	}
	err = s.call(ctx, &o, "get_balance", nil)
	return o.Balance, o.Unlocked, o.Blocks, err
}

type Transfer struct {
	TxID          string `json:"txid"`
	Amount        uint64 `json:"amount"`
	Confirmations uint64 `json:"confirmations"`
	Height        uint64 `json:"height"`
	UnlockTime    uint64 `json:"unlock_time"`
	Locked        bool   `json:"locked"`
}

// Incoming lists confirmed and pool incoming transfers.
func (s *Sess) Incoming(ctx context.Context) (confirmed, pool []Transfer, err error) {
	var o struct {
		In   []Transfer `json:"in"`
		Pool []Transfer `json:"pool"`
	}
	err = s.call(ctx, &o, "get_transfers", map[string]any{"in": true, "pool": true})
	return o.In, o.Pool, err
}

// Prepared is a signed but not yet relayed transaction; storing Metadata
// lets us relay exactly the same transaction again after a crash.
type Prepared struct {
	TxHash   string `json:"tx_hash"`
	Metadata string `json:"tx_metadata"`
	Fee      uint64 `json:"fee"`
	Amount   uint64 `json:"amount"`
}

// PrepareTransfer builds and signs a transfer without broadcasting it.
func (s *Sess) PrepareTransfer(ctx context.Context, to string, amount uint64) (*Prepared, error) {
	var o Prepared
	err := s.call(ctx, &o, "transfer", map[string]any{
		"destinations": []map[string]any{{"amount": amount, "address": to}},
		"do_not_relay": true, "get_tx_metadata": true, "priority": 0,
	})
	return &o, err
}

// PrepareSweep builds and signs a sweep_all without broadcasting it.
func (s *Sess) PrepareSweep(ctx context.Context, to string) ([]Prepared, error) {
	var o struct {
		TxHashList   []string `json:"tx_hash_list"`
		MetadataList []string `json:"tx_metadata_list"`
		FeeList      []uint64 `json:"fee_list"`
		AmountList   []uint64 `json:"amount_list"`
	}
	if err := s.call(ctx, &o, "sweep_all", map[string]any{"address": to, "do_not_relay": true, "get_tx_metadata": true}); err != nil {
		return nil, err
	}
	var out []Prepared
	for i := range o.TxHashList {
		out = append(out, Prepared{TxHash: o.TxHashList[i], Metadata: o.MetadataList[i], Fee: o.FeeList[i], Amount: o.AmountList[i]})
	}
	return out, nil
}

// Relay broadcasts a prepared transaction. Relaying the same one twice is harmless.
func (s *Sess) Relay(ctx context.Context, p *Prepared) error {
	return s.call(ctx, nil, "relay_tx", map[string]any{"hex": p.Metadata})
}

// TxConfirmations returns confirmations of one of our outgoing/incoming txs (0 if in pool, -1 if unknown).
func (s *Sess) TxConfirmations(ctx context.Context, txid string) (int64, error) {
	var o struct {
		Transfer struct {
			Confirmations uint64 `json:"confirmations"`
			Type          string `json:"type"`
		} `json:"transfer"`
	}
	if err := s.call(ctx, &o, "get_transfer_by_txid", map[string]any{"txid": txid}); err != nil {
		return -1, nil
	}
	if o.Transfer.Type == "pending" || o.Transfer.Type == "pool" {
		return 0, nil
	}
	return int64(o.Transfer.Confirmations), nil
}

func (s *Sess) Height(ctx context.Context) (uint64, error) {
	var o struct {
		Height uint64 `json:"height"`
	}
	err := s.call(ctx, &o, "get_height", nil)
	return o.Height, err
}

// Version is monero-wallet-rpc's get_version (used as a readiness check).
func (w *Wallet) Version(ctx context.Context) (uint64, error) {
	var o struct {
		Version uint64 `json:"version"`
	}
	err := w.r.call(ctx, &o, "get_version", nil)
	return o.Version, err
}

// StopWallet asks monero-wallet-rpc to save the open wallet and exit.
func (w *Wallet) StopWallet(ctx context.Context) error {
	return w.r.call(ctx, nil, "stop_wallet", nil)
}
