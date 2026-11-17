package eth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client is a small JSON-RPC client.
//
// URL is the primary endpoint. URLs, if set, are tried in order on each
// attempt; a rate limit (-32005, HTTP 429, or a "rate limit" message) rotates
// to the next URL and retries with backoff. A non-rate-limit error is returned
// immediately, unless Failover is set.
type Client struct {
	URL  string
	URLs []string // optional fallbacks; NewClients fills this
	HTTP *http.Client
	id   atomic.Int64

	// Failover (for read-only users such as the exit watcher): a failure of
	// the node itself (timeout, connection error, HTTP error, unreadable
	// reply, or a server error that isn't a revert) moves straight on to the
	// next URL. The URL that last worked is used first, and the primary is
	// tried again after PrimaryRetry (default 5m). AttemptTimeout bounds each
	// try (0 = the HTTP client's timeout). OnFailover, if set, is told each
	// time a URL is given up on.
	Failover       bool
	AttemptTimeout time.Duration
	PrimaryRetry   time.Duration
	OnFailover     func(from, to string, err error)
	pref           atomic.Int32
	prefAt         atomic.Int64

	// Retries is the number of retries after the first attempt on a rate
	// limit (0 means DefaultRPCRetries). Backoff is the initial wait
	// (0 means DefaultRPCBackoff), doubled each retry, capped at MaxRPCBackoff.
	Retries int
	Backoff time.Duration

	nmu   sync.Mutex
	nonce map[Address]nonceHint
}

// DefaultRPCRetries is how many times Call retries a rate-limited request
// after the first attempt. DefaultRPCBackoff is the first wait; each retry
// doubles it, up to MaxRPCBackoff.
const (
	DefaultRPCRetries = 8
	DefaultRPCBackoff = 500 * time.Millisecond
	MaxRPCBackoff     = 15 * time.Second
)

// nonceHint is the next nonce we handed out for an address, and when. Public
// RPCs are load-balanced: right after we send a tx, another backend may still
// report the old "pending" count, and a second tx built in the same round would
// reuse the nonce (seen on Sepolia: "replacement transaction underpriced").
type nonceHint struct {
	next uint64
	at   time.Time
}

// NonceHintTTL bounds how long a locally handed-out nonce overrides the RPC.
// After that the RPC is trusted again, so a tx that was built but never sent
// cannot leave a permanent gap.
var NonceHintTTL = 2 * time.Minute

// ForgetNonce gives back a dropped nonce for a, after a built transaction was
// discarded without being broadcast. It only shrinks the hint when
// hint.next == dropped+1 (this was the last nonce handed out), so the next
// build can reuse it. If a later nonce was already handed out, the hint is
// left unchanged and the gap is accepted: wiping the whole hint would let a
// second swap, sharing this key, reuse an in-flight nonce when the RPC is lagging.
func (c *Client) ForgetNonce(a Address, dropped uint64) {
	c.nmu.Lock()
	defer c.nmu.Unlock()
	if c.nonce == nil {
		return
	}
	h, ok := c.nonce[a]
	if !ok || time.Since(h.at) >= NonceHintTTL {
		return
	}
	if h.next == dropped+1 {
		h.next = dropped
		h.at = time.Now()
		c.nonce[a] = h
	}
}

// nextNonce returns max(RPC pending count, our recent hint) and records it as used.
func (c *Client) nextNonce(ctx context.Context, a Address) (uint64, error) {
	n, err := c.Nonce(ctx, a)
	if err != nil {
		return 0, err
	}
	c.nmu.Lock()
	defer c.nmu.Unlock()
	if c.nonce == nil {
		c.nonce = map[Address]nonceHint{}
	}
	if h, ok := c.nonce[a]; ok && time.Since(h.at) < NonceHintTTL && h.next > n {
		n = h.next
	}
	c.nonce[a] = nonceHint{next: n + 1, at: time.Now()}
	return n, nil
}

func NewClient(url string) *Client {
	return NewClients(url)
}

// NewClients builds a client that fails over across urls (and any
// comma-separated list inside a single entry). The first URL is the primary.
func NewClients(urls ...string) *Client {
	var all []string
	seen := map[string]bool{}
	for _, u := range urls {
		for _, p := range strings.Split(u, ",") {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			all = append(all, p)
		}
	}
	primary := ""
	if len(all) > 0 {
		primary = all[0]
	}
	return &Client{URL: primary, URLs: all, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) rpcURLs() []string {
	if len(c.URLs) > 0 {
		return c.URLs
	}
	if c.URL != "" {
		return []string{c.URL}
	}
	return nil
}

func (c *Client) retryLimit() int {
	if c.Retries > 0 {
		return c.Retries
	}
	if c.Retries < 0 {
		return 0
	}
	return DefaultRPCRetries
}

func (c *Client) initialBackoff() time.Duration {
	if c.Backoff > 0 {
		return c.Backoff
	}
	return DefaultRPCBackoff
}

// IsRateLimited reports whether err (and an optional HTTP status) is a
// provider rate limit that is safe to retry. Matches JSON-RPC -32005, HTTP
// 429, and messages that say "rate limit" or "too many requests".
func IsRateLimited(err error, httpStatus int) bool {
	if httpStatus == 429 {
		return true
	}
	if err == nil {
		return false
	}
	var re *rpcErr
	if errors.As(err, &re) {
		if re.Code == -32005 || re.Code == 429 {
			return true
		}
		msg := strings.ToLower(re.Message)
		if strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests") {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests") {
		return true
	}
	return false
}

type rpcErr struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *rpcErr) Error() string {
	return fmt.Sprintf("rpc %d: %s %s", e.Code, e.Message, string(e.Data))
}

func (c *Client) Call(ctx context.Context, out any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	urls := c.rpcURLs()
	if len(urls) == 0 {
		return errors.New("eth rpc: no URL")
	}
	retries := c.retryLimit()
	backoff := c.initialBackoff()
	start := 0
	if c.Failover {
		start = c.preferred(len(urls))
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		i := (start + attempt) % len(urls)
		url := urls[i]
		err, status := c.attempt(ctx, url, out, method, params)
		if err == nil {
			if c.Failover {
				c.setPreferred(start, i)
			}
			return nil
		}
		last = err
		if ctx.Err() != nil {
			return err
		}
		failover := c.Failover && NodeFailure(err, status)
		if !failover && !IsRateLimited(err, status) {
			return err
		}
		if attempt == retries {
			break
		}
		if failover && len(urls) > 1 {
			next := urls[(i+1)%len(urls)]
			if c.OnFailover != nil {
				c.OnFailover(url, next, err)
			}
			if attempt < len(urls)-1 {
				continue // first pass over the list: no wait
			}
		}
		d := backoff << attempt
		if d > MaxRPCBackoff {
			d = MaxRPCBackoff
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return last
}

func (c *Client) attempt(ctx context.Context, url string, out any, method string, params []any) (error, int) {
	if c.AttemptTimeout <= 0 {
		return c.callOnce(ctx, url, out, method, params)
	}
	actx, cancel := context.WithTimeout(ctx, c.AttemptTimeout)
	defer cancel()
	err, status := c.callOnce(actx, url, out, method, params)
	if err != nil && ctx.Err() == nil && actx.Err() != nil {
		err = fmt.Errorf("%s: no answer from %s within %s: %w", method, url, c.AttemptTimeout, err)
	}
	return err, status
}

func (c *Client) preferred(n int) int {
	p := int(c.pref.Load())
	if p <= 0 || p >= n {
		return 0
	}
	retry := c.PrimaryRetry
	if retry <= 0 {
		retry = 5 * time.Minute
	}
	if time.Since(time.Unix(0, c.prefAt.Load())) > retry {
		return 0 // give the primary another chance
	}
	return p
}

// setPreferred records that urls[i] answered a call that started at urls[start].
func (c *Client) setPreferred(start, i int) {
	if i != start {
		c.pref.Store(int32(i))
		c.prefAt.Store(time.Now().UnixNano())
	} else if i == 0 {
		c.pref.Store(0)
	}
}

// NodeFailure reports whether err looks like a problem with the RPC node
// rather than with the request: transport errors and timeouts, HTTP errors,
// unreadable replies, and JSON-RPC server errors other than reverts and bad
// parameters. A revert would fail the same way on every node.
func NodeFailure(err error, httpStatus int) bool {
	if err == nil {
		return false
	}
	var re *rpcErr
	if errors.As(err, &re) {
		msg := strings.ToLower(re.Message)
		if re.Code == 3 || re.Code == -32602 || strings.Contains(msg, "revert") {
			return false
		}
		return true
	}
	return true
}

func (c *Client) callOnce(ctx context.Context, url string, out any, method string, params []any) (err error, httpStatus int) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err, 0
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err, 0
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err), resp.StatusCode
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcErr         `json:"error"`
	}
	decErr := json.Unmarshal(raw, &r)
	if resp.StatusCode == 429 {
		if decErr == nil && r.Error != nil {
			return fmt.Errorf("%s: %w", method, r.Error), 429
		}
		return fmt.Errorf("%s: http 429: %s", method, truncate(raw, 200)), 429
	}
	if decErr != nil {
		if resp.StatusCode >= 400 {
			return fmt.Errorf("%s: http %d: %s", method, resp.StatusCode, truncate(raw, 200)), resp.StatusCode
		}
		return fmt.Errorf("%s: %w", method, decErr), resp.StatusCode
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %w", method, r.Error), resp.StatusCode
	}
	if out == nil {
		return nil, resp.StatusCode
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return err, resp.StatusCode
	}
	return nil, resp.StatusCode
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func hexU(x uint64) string    { return fmt.Sprintf("0x%x", x) }
func hexB(x *big.Int) string  { return "0x" + x.Text(16) }
func hexData(b []byte) string { return "0x" + hex.EncodeToString(b) }

func (c *Client) bigCall(ctx context.Context, method string, params ...any) (*big.Int, error) {
	var s string
	if err := c.Call(ctx, &s, method, params...); err != nil {
		return nil, err
	}
	return HexBig(s)
}

func (c *Client) ChainID(ctx context.Context) (*big.Int, error) { return c.bigCall(ctx, "eth_chainId") }

func (c *Client) Balance(ctx context.Context, a Address) (*big.Int, error) {
	return c.bigCall(ctx, "eth_getBalance", a.Hex(), "latest")
}

func (c *Client) Nonce(ctx context.Context, a Address) (uint64, error) {
	n, err := c.bigCall(ctx, "eth_getTransactionCount", a.Hex(), "pending")
	if err != nil {
		return 0, err
	}
	return n.Uint64(), nil
}

type Head struct {
	Number    uint64
	Timestamp uint64
	BaseFee   *big.Int
}

// Head returns the latest block. Deadlines are always judged by Head.Timestamp.
func (c *Client) Head(ctx context.Context) (*Head, error) { return c.block(ctx, "latest") }

// BlockAt returns block n.
func (c *Client) BlockAt(ctx context.Context, n uint64) (*Head, error) { return c.block(ctx, hexU(n)) }

func (c *Client) block(ctx context.Context, tag string) (*Head, error) {
	var b *struct {
		Number        string `json:"number"`
		Timestamp     string `json:"timestamp"`
		BaseFeePerGas string `json:"baseFeePerGas"`
	}
	if err := c.Call(ctx, &b, "eth_getBlockByNumber", tag, false); err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("block %s not found", tag)
	}
	n, _ := HexBig(b.Number)
	t, _ := HexBig(b.Timestamp)
	bf := big.NewInt(0)
	if b.BaseFeePerGas != "" {
		bf, _ = HexBig(b.BaseFeePerGas)
	}
	return &Head{Number: n.Uint64(), Timestamp: t.Uint64(), BaseFee: bf}, nil
}

type CallMsg struct {
	From  Address
	To    *Address
	Value *big.Int
	Data  []byte
}

func (m CallMsg) json() map[string]any {
	o := map[string]any{"from": m.From.Hex(), "data": hexData(m.Data)}
	if m.To != nil {
		o["to"] = m.To.Hex()
	}
	if m.Value != nil {
		o["value"] = hexB(m.Value)
	}
	return o
}

func (c *Client) EthCall(ctx context.Context, m CallMsg) ([]byte, error) {
	var s string
	if err := c.Call(ctx, &s, "eth_call", m.json(), "latest"); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

func (c *Client) EstimateGas(ctx context.Context, m CallMsg) (uint64, error) {
	g, err := c.bigCall(ctx, "eth_estimateGas", m.json())
	if err != nil {
		return 0, err
	}
	return g.Uint64(), nil
}

func (c *Client) SendRaw(ctx context.Context, raw []byte) (Hash, error) {
	var s string
	if err := c.Call(ctx, &s, "eth_sendRawTransaction", hexData(raw)); err != nil {
		return Hash{}, err
	}
	return ParseHash(s)
}

type Log struct {
	Address     Address `json:"address"`
	Topics      []Hash  `json:"topics"`
	DataHex     string  `json:"data"`
	BlockNumber string  `json:"blockNumber"`
	TxHash      Hash    `json:"transactionHash"`
	Removed     bool    `json:"removed"`
}

func (l *Log) Data() []byte { b, _ := hex.DecodeString(strings.TrimPrefix(l.DataHex, "0x")); return b }
func (l *Log) Block() uint64 {
	x, _ := HexBig(l.BlockNumber)
	return x.Uint64()
}

type Receipt struct {
	Status      string   `json:"status"`
	BlockNumber string   `json:"blockNumber"`
	GasUsed     string   `json:"gasUsed"`
	Contract    *Address `json:"contractAddress"`
	Logs        []Log    `json:"logs"`
	GasPrice    string   `json:"effectiveGasPrice"`
	FromAddr    Address  `json:"from"`
	TxHash      Hash     `json:"transactionHash"`
}

// From is the sender of the transaction.
func (r *Receipt) From() Address { return r.FromAddr }

// Fee is gasUsed * effectiveGasPrice in wei.
func (r *Receipt) Fee() *big.Int {
	p, _ := HexBig(r.GasPrice)
	if p == nil {
		p = new(big.Int)
	}
	return new(big.Int).Mul(p, new(big.Int).SetUint64(r.Gas()))
}

func (r *Receipt) OK() bool { return r.Status == "0x1" }
func (r *Receipt) Block() uint64 {
	x, _ := HexBig(r.BlockNumber)
	return x.Uint64()
}
func (r *Receipt) Gas() uint64 { x, _ := HexBig(r.GasUsed); return x.Uint64() }

// Receipt returns nil, nil if the tx is not mined yet.
func (c *Client) Receipt(ctx context.Context, h Hash) (*Receipt, error) {
	var r *Receipt
	if err := c.Call(ctx, &r, "eth_getTransactionReceipt", h.Hex()); err != nil {
		return nil, err
	}
	return r, nil
}

func (c *Client) WaitReceipt(ctx context.Context, h Hash) (*Receipt, error) {
	for {
		r, err := c.Receipt(ctx, h)
		if err != nil {
			return nil, err
		}
		if r != nil {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (c *Client) Logs(ctx context.Context, contract Address, from, to uint64, topics ...any) ([]Log, error) {
	q := map[string]any{"address": contract.Hex(), "fromBlock": hexU(from), "toBlock": hexU(to)}
	if len(topics) > 0 {
		q["topics"] = topics
	}
	var out []Log
	err := c.Call(ctx, &out, "eth_getLogs", q)
	return out, err
}

// ---------------------------------------------------------------- transactions

// Tx is an EIP-1559 transaction.
type Tx struct {
	ChainID *big.Int
	Nonce   uint64
	TipCap  *big.Int
	FeeCap  *big.Int
	Gas     uint64
	To      *Address
	Value   *big.Int
	Data    []byte
}

func (t *Tx) fields() [][]byte {
	to := []byte{0x80}
	if t.To != nil {
		to = rlpBytes(t.To[:])
	}
	v := t.Value
	if v == nil {
		v = new(big.Int)
	}
	return [][]byte{rlpInt(t.ChainID), rlpUint(t.Nonce), rlpInt(t.TipCap), rlpInt(t.FeeCap), rlpUint(t.Gas), to, rlpInt(v), rlpBytes(t.Data), rlpList()}
}

// Sign returns the raw signed transaction and its hash.
func (t *Tx) Sign(k *Key) (raw []byte, h Hash) {
	sigHash := Keccak(append([]byte{0x02}, rlpList(t.fields()...)...))
	v, r, s := k.Sign(sigHash)
	f := append(t.fields(), rlpUint(uint64(v)), rlpInt(new(big.Int).SetBytes(r[:])), rlpInt(new(big.Int).SetBytes(s[:])))
	raw = append([]byte{0x02}, rlpList(f...)...)
	return raw, Hash(Keccak32(raw))
}

// Fees suggests (tip, feeCap): tip from the node, cap = 2*base + tip.
func (c *Client) Fees(ctx context.Context) (*big.Int, *big.Int, error) {
	h, err := c.Head(ctx)
	if err != nil {
		return nil, nil, err
	}
	tip, err := c.bigCall(ctx, "eth_maxPriorityFeePerGas")
	if err != nil {
		tip = big.NewInt(1_000_000) // 0.001 gwei fallback
	}
	cap := new(big.Int).Add(new(big.Int).Mul(h.BaseFee, big.NewInt(2)), tip)
	return tip, cap, nil
}

// BuildTx fills nonce, fees and gas (estimate * 1.25) for a call from k.
func (c *Client) BuildTx(ctx context.Context, k *Key, to *Address, value *big.Int, data []byte) (*Tx, error) {
	cid, err := c.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	tip, cap, err := c.Fees(ctx)
	if err != nil {
		return nil, err
	}
	g, err := c.EstimateGas(ctx, CallMsg{From: k.Address(), To: to, Value: value, Data: data})
	if err != nil {
		return nil, err
	}
	n, err := c.nextNonce(ctx, k.Address()) // last, so a failed estimate uses no nonce
	if err != nil {
		return nil, err
	}
	return &Tx{ChainID: cid, Nonce: n, TipCap: tip, FeeCap: cap, Gas: g*5/4 + 10000, To: to, Value: value, Data: data}, nil
}

// SendAndWait builds, signs, sends and waits; returns the receipt (error if reverted).
func (c *Client) SendAndWait(ctx context.Context, k *Key, to *Address, value *big.Int, data []byte) (*Receipt, error) {
	tx, err := c.BuildTx(ctx, k, to, value, data)
	if err != nil {
		return nil, err
	}
	raw, h := tx.Sign(k)
	if _, err := c.SendRaw(ctx, raw); err != nil {
		return nil, err
	}
	r, err := c.WaitReceipt(ctx, h)
	if err != nil {
		return nil, err
	}
	if !r.OK() {
		return r, errors.New("transaction reverted")
	}
	return r, nil
}

// BuildTxFixedGas is BuildTx with a fixed gas limit and NO eth_estimateGas call.
// Used for transactions that carry a secret (claim/refund): estimating would hand
// the secret to the RPC node before we decided to broadcast.
func (c *Client) BuildTxFixedGas(ctx context.Context, k *Key, to *Address, value *big.Int, data []byte, gas uint64) (*Tx, error) {
	cid, err := c.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	tip, cap, err := c.Fees(ctx)
	if err != nil {
		return nil, err
	}
	n, err := c.nextNonce(ctx, k.Address())
	if err != nil {
		return nil, err
	}
	return &Tx{ChainID: cid, Nonce: n, TipCap: tip, FeeCap: cap, Gas: gas, To: to, Value: value, Data: data}, nil
}

// TxKnown reports whether the node knows a transaction (pending or mined).
func (c *Client) TxKnown(ctx context.Context, h Hash) (bool, error) {
	var v *struct {
		Hash string `json:"hash"`
	}
	if err := c.Call(ctx, &v, "eth_getTransactionByHash", h.Hex()); err != nil {
		return false, err
	}
	return v != nil, nil
}
