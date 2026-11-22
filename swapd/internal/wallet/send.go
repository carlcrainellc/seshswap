package wallet

// Send: move coins out of this wallet to an address the user types.
//
// Two steps so the user sees exactly what will happen before anything leaves:
//
//	POST /v1/wallet/send/prepare {coin, to, amount | all}
//	  checks the address and the amount, keeps back what open swaps still
//	  need, works out the network fee, and returns a one-time id plus the
//	  amount, fee and total to show on the review screen.
//	POST /v1/wallet/send/confirm {id}
//	  broadcasts that exact send (once) and returns the txid and a link.
//
// A prepared send expires after sendTTL and is forgotten when the wallet
// locks. Confirming takes the id out of the table first, so a double click
// or a replayed request can't send twice.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// sendTTL is how long a reviewed send stays valid. Fees and balances move;
// after this the user reviews again.
var sendTTL = 3 * time.Minute

// sendGasKeep is ETH kept back per unfinished swap so it can still pay for
// its refund or claim transaction (Arbitrum gas is cheap; this is generous).
var sendGasKeep = big.NewInt(200_000_000_000_000) // 0.0002 ETH

type sendState struct {
	mu sync.Mutex
	m  map[string]*sendPrep

	// bmu serializes ETH/USDC broadcasts; out is what those sends take once
	// mined. The node's "latest" balance doesn't show a sent tx until it is
	// mined, so without this two sends confirmed back to back could each
	// pass the balance check and together spend what open swaps hold.
	bmu sync.Mutex
	out []sendOutflow
}

type sendOutflow struct {
	from  eth.Address
	nonce uint64
	eth   *big.Int // value + fee bound
	usdc  *big.Int
	at    time.Time
}

// sendOutflowTTL: a send not mined after this long was dropped or replaced.
var sendOutflowTTL = 30 * time.Minute

// unmined is what our own sent-but-unmined txs from `from` will still take
// (call with bmu held). mined is the account's latest-block nonce.
func (st *sendState) unmined(from eth.Address, mined uint64) (ethWei, usdc *big.Int) {
	ethWei, usdc = new(big.Int), new(big.Int)
	keep := st.out[:0]
	for _, o := range st.out {
		if o.from != from {
			keep = append(keep, o)
			continue
		}
		if o.nonce < mined || time.Since(o.at) > sendOutflowTTL {
			continue
		}
		keep = append(keep, o)
		ethWei.Add(ethWei, o.eth)
		usdc.Add(usdc, o.usdc)
	}
	st.out = keep
	return
}

type sendPrep struct {
	ID      string
	Coin    string // XMR, ETH, USDC
	To      string
	Amount  *big.Int // base units of Coin
	Fee     *big.Int // XMR: piconero; ETH/USDC: wei (upper bound)
	All     bool
	Expires time.Time

	xmrTxs []xmr.Prepared // XMR: signed, not relayed

	ethTo   eth.Address // ETH/USDC: the transaction's to (token contract for USDC)
	value   *big.Int
	data    []byte
	gas     uint64
	tip     *big.Int
	feeCap  *big.Int
	demo    bool
	created time.Time
}

func (st *sendState) put(p *sendPrep) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.m == nil {
		st.m = map[string]*sendPrep{}
	}
	now := time.Now()
	for id, q := range st.m {
		if now.After(q.Expires) {
			delete(st.m, id)
		}
	}
	st.m[p.ID] = p
}

// take removes and returns a prepared send; ok=false if unknown or expired.
func (st *sendState) take(id string) (*sendPrep, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := st.m[id]
	if !ok {
		return nil, errors.New("this send was already confirmed or cancelled. Start again from the Send screen")
	}
	delete(st.m, id)
	if time.Now().After(p.Expires) {
		return nil, errors.New("this review expired (fees and balances move). Review it again")
	}
	return p, nil
}

func (st *sendState) drop(id string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.m, id)
}

func (st *sendState) clear() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.m = nil
}

func (s *Server) usdcDecimals() int {
	if s.Cfg.USDCDecimals > 0 {
		return s.Cfg.USDCDecimals
	}
	return 6
}

func coinDecimals(s *Server, coin string) int {
	switch coin {
	case "XMR":
		return 12
	case "USDC":
		return s.usdcDecimals()
	}
	return 18
}

// parseSendAmount is parseUnits but refuses more decimals than the coin has
// instead of silently cutting them off.
func parseSendAmount(v string, dec int) (*big.Int, error) {
	v = strings.TrimSpace(strings.ReplaceAll(v, ",", ""))
	if i := strings.IndexByte(v, '.'); i >= 0 && len(v)-i-1 > dec {
		return nil, fmt.Errorf("too many decimals (this coin has %d)", dec)
	}
	for _, c := range v {
		if (c < '0' || c > '9') && c != '.' {
			return nil, errors.New("enter an amount like 0.25")
		}
	}
	if strings.Count(v, ".") > 1 {
		return nil, errors.New("enter an amount like 0.25")
	}
	return parseUnits(v, dec)
}

// checkXmrAddress validates a Monero address for this wallet's network.
func checkXmrAddress(addr string, want keys.Network) error {
	net, _, err := keys.ParseAddress(addr)
	if err != nil {
		return err
	}
	if want == keys.Regtest {
		want = keys.Mainnet // regtest uses mainnet prefixes
	}
	if net != want {
		return fmt.Errorf("that's a %s address; this wallet is on %s", net, want)
	}
	return nil
}

func newSendID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// heldBySwaps is what unfinished swaps still need from this wallet.
type held struct {
	xmr  uint64
	eth  *big.Int // value + premium of unlocked taker swaps, plus gas per open swap
	usdc *big.Int
	open int
}

func (s *Server) heldBySwaps() held {
	s.mu.Lock()
	e := s.eng
	usdc := s.Cfg.USDC
	s.mu.Unlock()
	h := held{eth: new(big.Int), usdc: new(big.Int)}
	if e == nil {
		return h
	}
	h.xmr = e.XmrHeld()
	w, toks, open := e.TakerHeld()
	h.open = open
	h.eth.Add(w, new(big.Int).Mul(sendGasKeep, big.NewInt(int64(open))))
	if t := toks[usdc]; t != nil && !usdc.IsZero() {
		h.usdc.Set(t)
	}
	return h
}

type sendReq struct {
	Coin   string `json:"coin"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	All    bool   `json:"all"`
	ID     string `json:"id"`
}

func (s *Server) sendPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	var req sendReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad json")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	p, code, err := s.prepareSend(ctx, req)
	if err != nil {
		writeErr(w, code, err.Error())
		return
	}
	s.send.put(p)
	writeJSON(w, 200, s.sendView(p))
}

func (s *Server) sendView(p *sendPrep) map[string]any {
	dec := coinDecimals(s, p.Coin)
	feeCoin, feeDec := "ETH", 18
	if p.Coin == "XMR" {
		feeCoin, feeDec = "XMR", 12
	}
	out := map[string]any{
		"id":        p.ID,
		"coin":      p.Coin,
		"to":        p.To,
		"amount":    formatUnits(p.Amount, dec),
		"fee":       formatUnits(p.Fee, feeDec),
		"feeCoin":   feeCoin,
		"all":       p.All,
		"expiresIn": int(time.Until(p.Expires).Seconds()),
	}
	if p.Coin == feeCoin {
		out["total"] = formatUnits(new(big.Int).Add(p.Amount, p.Fee), dec)
	} else {
		out["total"] = formatUnits(p.Amount, dec) + " " + p.Coin + " + " + formatUnits(p.Fee, feeDec) + " " + feeCoin
	}
	if p.Coin != "XMR" {
		out["feeNote"] = "Most likely less; this is the most the network can charge."
	}
	if p.Coin == "XMR" && len(p.xmrTxs) > 1 {
		out["feeNote"] = fmt.Sprintf("Sent as %d transactions.", len(p.xmrTxs))
	}
	return out
}

func (s *Server) prepareSend(ctx context.Context, req sendReq) (*sendPrep, int, error) {
	s.mu.Lock()
	v, c, xw, cfg := s.vault, s.eth, s.xmrW, s.Cfg
	s.mu.Unlock()
	if v == nil {
		return nil, 401, errors.New("unlock the wallet first")
	}
	coin := strings.ToUpper(strings.TrimSpace(req.Coin))
	to := strings.TrimSpace(req.To)
	if to == "" {
		return nil, 400, errors.New("enter the address to send to")
	}
	p := &sendPrep{ID: newSendID(), Coin: coin, All: req.All, created: time.Now(), Expires: time.Now().Add(sendTTL)}
	var amt *big.Int
	if !req.All {
		var err error
		if amt, err = parseSendAmount(req.Amount, coinDecimals(s, coin)); err != nil {
			return nil, 400, err
		}
	}
	h := s.heldBySwaps()
	switch coin {
	case "XMR":
		if err := checkXmrAddress(to, cfg.XmrNet); err != nil {
			return nil, 400, err
		}
		if to == v.Keys.XmrAddr {
			return nil, 400, errors.New("that's this wallet's own address")
		}
		p.To = to
		if cfg.Demo && xw == nil {
			return demoPrep(p, amt, 400000000000, 31_000_000)
		}
		if xw == nil {
			return nil, 503, errors.New("the Monero wallet isn't running yet. Try again in a minute")
		}
		err := xw.Session(ctx, cfg.MainWallet, func(sess *xmr.Sess) error {
			_ = sess.Refresh(ctx)
			_, unlocked, err := sess.Balance(ctx)
			if err != nil {
				return err
			}
			if req.All {
				if h.xmr > 0 {
					return userErr{"an open swap is using part of your XMR, so Send all is off until it finishes. Enter an amount instead"}
				}
				if unlocked == 0 {
					return userErr{"no spendable XMR yet (new coins unlock after about 20 minutes)"}
				}
				txs, err := sess.PrepareSweep(ctx, to)
				if err != nil {
					return xmrSendErr(err)
				}
				if len(txs) == 0 {
					return userErr{"nothing to send"}
				}
				p.xmrTxs = txs
				p.Amount, p.Fee = new(big.Int), new(big.Int)
				for _, t := range txs {
					p.Amount.Add(p.Amount, new(big.Int).SetUint64(t.Amount))
					p.Fee.Add(p.Fee, new(big.Int).SetUint64(t.Fee))
				}
				return nil
			}
			if !amt.IsUint64() {
				return userErr{"amount is too large"}
			}
			a := amt.Uint64()
			if unlocked < h.xmr || a > unlocked-h.xmr {
				free := uint64(0)
				if unlocked > h.xmr {
					free = unlocked - h.xmr
				}
				msg := "only " + picoToXMR(free) + " XMR can be sent right now"
				if h.xmr > 0 {
					msg += " (" + picoToXMR(h.xmr) + " XMR is held for an open swap)"
				}
				return userErr{msg}
			}
			t, err := sess.PrepareTransfer(ctx, to, a)
			if err != nil {
				return xmrSendErr(err)
			}
			if t.Fee+a > unlocked-h.xmr {
				return userErr{"that plus the network fee (" + picoToXMR(t.Fee) + " XMR) is more than you can send right now. Try a little less"}
			}
			p.xmrTxs = []xmr.Prepared{*t}
			p.Amount, p.Fee = amt, new(big.Int).SetUint64(t.Fee)
			return nil
		})
		if err != nil {
			var ue userErr
			if errors.As(err, &ue) {
				return nil, 400, ue
			}
			return nil, 502, fmt.Errorf("Monero wallet: %v", err)
		}
		return p, 200, nil

	case "ETH", "USDC":
		dst, err := eth.ParseUserAddress(to)
		if err != nil {
			return nil, 400, err
		}
		if dst == v.Keys.EthAddr {
			return nil, 400, errors.New("that's this wallet's own address")
		}
		if coin == "USDC" && cfg.USDC.IsZero() {
			return nil, 400, errors.New("no USDC token is set up on this network")
		}
		if coin == "USDC" && dst == cfg.USDC {
			return nil, 400, errors.New("that's the USDC token contract itself; coins sent there are lost")
		}
		p.To = dst.ChecksumHex()
		if cfg.Demo && c == nil {
			bal := int64(150_000_000_000_000_000)
			if coin == "USDC" {
				bal = 25_000_000
			}
			return demoPrep(p, amt, bal, 3_200_000_000_000)
		}
		if c == nil {
			return nil, 503, errors.New("Ethereum RPC is not configured")
		}
		if err := s.planEthSend(ctx, p, v.Keys.EthAddr, dst, amt, h); err != nil {
			var ue userErr
			if errors.As(err, &ue) {
				return nil, 400, ue
			}
			return nil, 502, fmt.Errorf("Ethereum: %v", err)
		}
		return p, 200, nil
	}
	return nil, 400, errors.New("pick XMR, ETH or USDC")
}

// userErr is a refusal the user can act on (400), not a backend failure.
type userErr struct{ msg string }

func (e userErr) Error() string { return e.msg }

func xmrSendErr(err error) error {
	m := err.Error()
	switch {
	case strings.Contains(m, "not enough"):
		return userErr{"not enough unlocked XMR for that plus the network fee. New coins unlock after about 20 minutes"}
	case strings.Contains(m, "No unlocked balance"):
		return userErr{"no spendable XMR yet (new coins unlock after about 20 minutes)"}
	}
	return err
}

func demoPrep(p *sendPrep, amt *big.Int, bal, fee int64) (*sendPrep, int, error) {
	p.demo = true
	p.Fee = big.NewInt(fee)
	if p.All {
		amt = big.NewInt(bal)
		if p.Coin != "USDC" {
			amt.Sub(amt, p.Fee)
		}
	}
	if amt.Cmp(big.NewInt(bal)) > 0 {
		return nil, 400, userErr{"that's more than this wallet holds"}
	}
	p.Amount = amt
	return p, 200, nil
}

// planEthSend fills an ETH or USDC send: gas, fee cap, and the amount, with
// what open swaps need kept back.
func (s *Server) planEthSend(ctx context.Context, p *sendPrep, from, dst eth.Address, amt *big.Int, h held) error {
	c := s.eth
	ethBal, err := c.Balance(ctx, from)
	if err != nil {
		return err
	}
	// Our own earlier sends that aren't mined yet aren't in the balance.
	mined, err := c.MinedNonce(ctx, from)
	if err != nil {
		return err
	}
	s.send.bmu.Lock()
	outETH, outUSDC := s.send.unmined(from, mined)
	s.send.bmu.Unlock()
	ethBal = maxZero(new(big.Int).Sub(ethBal, outETH))
	tip, feeCap, err := c.Fees(ctx)
	if err != nil {
		return err
	}
	var tokBal *big.Int
	msg := eth.CallMsg{From: from, To: &dst, Value: big.NewInt(0)}
	if p.Coin == "USDC" {
		tokBal, err = eth.TokenBalance(ctx, c, s.Cfg.USDC, from)
		if err != nil {
			return err
		}
		tokBal = maxZero(new(big.Int).Sub(tokBal, outUSDC))
		if p.All {
			amt = new(big.Int).Sub(tokBal, h.usdc)
			if amt.Sign() <= 0 {
				if h.usdc.Sign() > 0 {
					return userErr{"all your USDC is held for an open swap"}
				}
				return userErr{"no USDC to send"}
			}
		}
		free := new(big.Int).Sub(tokBal, h.usdc)
		if amt.Cmp(free) > 0 {
			m := "only " + formatUnits(maxZero(free), s.usdcDecimals()) + " USDC can be sent right now"
			if h.usdc.Sign() > 0 {
				m += " (" + formatUnits(h.usdc, s.usdcDecimals()) + " USDC is held for an open swap)"
			}
			return userErr{m}
		}
		usdc := s.Cfg.USDC
		p.ethTo, p.value = usdc, new(big.Int)
		p.data = append(append(eth.Selector("transfer(address,uint256)"), eth.WordAddr(dst)...), eth.WordBig(amt)...)
		msg = eth.CallMsg{From: from, To: &usdc, Value: new(big.Int), Data: p.data}
	}
	g, err := c.EstimateGas(ctx, msg)
	if err != nil {
		return err
	}
	gas := g * 5 / 4
	if gas < 21000 {
		gas = 21000
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(gas), feeCap)
	needEth := new(big.Int).Add(fee, h.eth)
	if p.Coin == "ETH" {
		if p.All {
			amt = new(big.Int).Sub(ethBal, needEth)
			if amt.Sign() <= 0 {
				if h.eth.Sign() > 0 {
					return userErr{"what's left is held for open swaps and their network fees"}
				}
				return userErr{"not enough ETH to cover the network fee"}
			}
		}
		needEth.Add(needEth, amt)
		p.ethTo, p.value = dst, amt
	}
	if needEth.Cmp(ethBal) > 0 {
		free := new(big.Int).Sub(ethBal, new(big.Int).Add(fee, h.eth))
		m := "not enough ETH for that plus the network fee (up to " + weiToETH(fee) + " ETH)"
		if p.Coin == "ETH" {
			m = "only " + weiToETH(maxZero(free)) + " ETH can be sent after the network fee"
		}
		if h.eth.Sign() > 0 {
			m += "; " + weiToETH(h.eth) + " ETH is kept for open swaps"
		}
		return userErr{m}
	}
	p.Amount, p.Fee, p.gas, p.tip, p.feeCap = amt, fee, gas, tip, feeCap
	return nil
}

func maxZero(x *big.Int) *big.Int {
	if x.Sign() < 0 {
		return new(big.Int)
	}
	return x
}

func (s *Server) sendConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	var req sendReq
	if err := readJSON(r, &req); err != nil || req.ID == "" {
		writeErr(w, 400, "bad json")
		return
	}
	s.mu.Lock()
	unlocked := s.vault != nil
	s.mu.Unlock()
	if !unlocked {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	p, err := s.send.take(req.ID)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	txid, url, err := s.broadcastSend(ctx, p)
	if err != nil {
		var ue userErr
		if errors.As(err, &ue) {
			writeErr(w, 400, ue.Error())
			return
		}
		writeErr(w, 502, err.Error())
		return
	}
	s.logf("sent %s %s to %s (tx %s)", formatUnits(p.Amount, coinDecimals(s, p.Coin)), p.Coin, p.To, txid)
	writeJSON(w, 200, map[string]any{"ok": true, "txid": txid, "url": url, "coin": p.Coin,
		"amount": formatUnits(p.Amount, coinDecimals(s, p.Coin)), "to": p.To})
}

func (s *Server) sendCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	var req sendReq
	_ = readJSON(r, &req)
	s.send.drop(req.ID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func xmrExplorer(n keys.Network) string {
	switch n {
	case keys.Stagenet:
		return "https://stagenet.xmrchain.net/tx/"
	case keys.Testnet:
		return "https://testnet.xmrchain.net/tx/"
	case keys.Mainnet:
		return "https://xmrchain.net/tx/"
	}
	return ""
}

func (s *Server) broadcastSend(ctx context.Context, p *sendPrep) (string, string, error) {
	s.mu.Lock()
	v, c, xw, cfg := s.vault, s.eth, s.xmrW, s.Cfg
	s.mu.Unlock()
	if v == nil {
		return "", "", userErr{"unlock the wallet first"}
	}
	if p.demo {
		var b [32]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		if p.Coin == "XMR" {
			return id, xmrExplorer(cfg.XmrNet) + id, nil
		}
		return "0x" + id, s.explorer() + "/tx/0x" + id, nil
	}
	h := s.heldBySwaps()
	if p.Coin == "XMR" {
		if xw == nil {
			return "", "", errors.New("the Monero wallet isn't running")
		}
		// A swap may have started since the review: check again.
		need := new(big.Int).Add(p.Amount, p.Fee)
		err := xw.Session(ctx, cfg.MainWallet, func(sess *xmr.Sess) error {
			_, unlocked, err := sess.Balance(ctx)
			if err != nil {
				return err
			}
			if h.xmr > 0 && (p.All || need.Cmp(new(big.Int).SetUint64(unlocked-min(unlocked, h.xmr))) > 0) {
				return userErr{"a swap started since you reviewed this and needs part of that XMR. Review the send again"}
			}
			for i := range p.xmrTxs {
				if err := sess.Relay(ctx, &p.xmrTxs[i]); err != nil {
					if i == 0 {
						return err
					}
					return fmt.Errorf("sent %d of %d transactions; the rest failed: %v", i, len(p.xmrTxs), err)
				}
			}
			return nil
		})
		if err != nil {
			return "", "", err
		}
		id := p.xmrTxs[0].TxHash
		return id, xmrExplorer(cfg.XmrNet) + id, nil
	}
	if c == nil {
		return "", "", errors.New("Ethereum RPC is not configured")
	}
	from := v.Keys.EthAddr
	s.send.bmu.Lock()
	defer s.send.bmu.Unlock()
	mined, err := c.MinedNonce(ctx, from)
	if err != nil {
		return "", "", err
	}
	outETH, outUSDC := s.send.unmined(from, mined)
	ethBal, err := c.Balance(ctx, from)
	if err != nil {
		return "", "", err
	}
	ethBal = new(big.Int).Sub(ethBal, outETH) // what is left once our earlier sends are mined
	need := new(big.Int).Add(p.Fee, h.eth)
	need.Add(need, p.value)
	if need.Cmp(ethBal) > 0 {
		return "", "", userErr{"a swap started since you reviewed this, or the balance changed. Review the send again"}
	}
	if p.Coin == "USDC" {
		tb, err := eth.TokenBalance(ctx, c, cfg.USDC, from)
		if err != nil {
			return "", "", err
		}
		tb = new(big.Int).Sub(tb, outUSDC)
		if new(big.Int).Add(p.Amount, h.usdc).Cmp(tb) > 0 {
			return "", "", userErr{"a swap started since you reviewed this, or the balance changed. Review the send again"}
		}
	}
	to := p.ethTo
	tx, err := c.BuildTxFixedGas(ctx, v.Keys.EthKey, &to, p.value, p.data, p.gas)
	if err != nil {
		return "", "", err
	}
	// Keep the fees the user reviewed so the total never exceeds what they saw.
	tx.TipCap, tx.FeeCap = p.tip, p.feeCap
	raw, _ := tx.Sign(v.Keys.EthKey)
	hash, err := c.SendRaw(ctx, raw)
	if err != nil {
		c.ForgetNonce(from, tx.Nonce)
		return "", "", err
	}
	o := sendOutflow{from: from, nonce: tx.Nonce, eth: new(big.Int).Add(p.Fee, p.value), usdc: new(big.Int), at: time.Now()}
	if p.Coin == "USDC" {
		o.usdc.Set(p.Amount)
	}
	s.send.out = append(s.send.out, o)
	return hash.Hex(), s.explorer() + "/tx/" + hash.Hex(), nil
}
