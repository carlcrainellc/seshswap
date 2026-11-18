package wallet

// Testnet faucet: "Get test coins" sends this wallet's own Arbitrum Sepolia
// and stagenet addresses to the SeshSwap faucet. The faucet hands out a
// signed proof-of-work challenge; the wallet solves it here, in the backend,
// and claims. All limits are enforced by the faucet server.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

// DefaultFaucetURL is used when the config has no faucetUrl.
const DefaultFaucetURL = "https://faucet.seshswap.app"

// faucetTestnets are the EVM chains the faucet serves.
var faucetTestnets = map[int64]bool{421614: true}

// faucetMaxBits caps the work a faucet can ask for (2^28 hashes is about
// half a minute); anything higher is refused rather than burning CPU.
const faucetMaxBits = 28

// FaucetClient is sent as X-SeshSwap-Client, e.g. "seshswap-wallet/57ced1e".
func FaucetClient() string {
	v := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				v = s.Value[:7]
			}
		}
	}
	return "seshswap-wallet/" + v
}

// faucetURL returns the faucet base URL, or "" when the faucet is off or this
// is not a testnet wallet.
func (s *Server) faucetURL() string {
	u := strings.TrimSpace(s.Cfg.FaucetURL)
	if u == "off" || s.Cfg.Demo || s.Cfg.AllowMainnet {
		return ""
	}
	if !faucetTestnets[s.Cfg.ChainID] || s.Cfg.XmrNet == keys.Mainnet {
		return ""
	}
	if u == "" {
		u = DefaultFaucetURL
	}
	return strings.TrimRight(u, "/")
}

// FaucetPayout is one coin's result, as shown in the wallet.
type FaucetPayout struct {
	Coin   string `json:"coin"` // "ETH", "USDC", "XMR"
	Amount string `json:"amount,omitempty"`
	Tx     string `json:"tx,omitempty"`
	TxURL  string `json:"txUrl,omitempty"`
	Note   string `json:"note,omitempty"`
}

// FaucetResult is what /v1/faucet/claim returns.
type FaucetResult struct {
	OK      bool           `json:"ok"`
	Message string         `json:"message"`
	Payouts []FaucetPayout `json:"payouts"`
	Seconds float64        `json:"seconds"`
}

func faucetLeadingZeros(h [32]byte) int {
	n := 0
	for _, b := range h {
		if b == 0 {
			n += 8
			continue
		}
		for m := byte(0x80); m != 0 && b&m == 0; m >>= 1 {
			n++
		}
		break
	}
	return n
}

// solveFaucetPow finds a nonce with SHA-256(challenge|subject|nonce) having
// bits leading zero bits.
func solveFaucetPow(ctx context.Context, challenge, subject string, bits int) (string, error) {
	prefix := []byte(challenge + "|" + subject + "|")
	buf := make([]byte, 0, len(prefix)+20)
	for n := uint64(0); ; n++ {
		if n&0xffff == 0 && ctx.Err() != nil {
			return "", ctx.Err()
		}
		buf = strconv.AppendUint(append(buf[:0], prefix...), n, 10)
		if faucetLeadingZeros(sha256.Sum256(buf)) >= bits {
			return strconv.FormatUint(n, 10), nil
		}
	}
}

func (s *Server) faucetPost(ctx context.Context, url string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SeshSwap-Client", FaucetClient())
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(b, out); err != nil {
		return resp.StatusCode, fmt.Errorf("faucet answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

var errFaucetOffline = errors.New("The faucet can't be reached right now. Try again in a few minutes.")

// ClaimFaucet asks the faucet for test coins for ethAddr and xmrAddr.
func (s *Server) ClaimFaucet(ctx context.Context, base, ethAddr, xmrAddr, explorer string) (*FaucetResult, error) {
	t0 := time.Now()
	var ch struct {
		Challenge string `json:"challenge"`
		Bits      int    `json:"bits"`
		Error     string `json:"error"`
	}
	code, err := s.faucetPost(ctx, base+"/api/challenge", nil, &ch)
	if err != nil {
		s.logf("faucet challenge: %v", err)
		return nil, errFaucetOffline
	}
	if code != 200 || ch.Challenge == "" {
		if ch.Error != "" {
			return nil, errors.New(ch.Error)
		}
		return nil, errFaucetOffline
	}
	if ch.Bits < 0 || ch.Bits > faucetMaxBits {
		return nil, fmt.Errorf("the faucet asked for too much work (%d bits)", ch.Bits)
	}
	subject := strings.ToLower(ethAddr) + "|" + xmrAddr
	nonce, err := solveFaucetPow(ctx, ch.Challenge, subject, ch.Bits)
	if err != nil {
		return nil, err
	}
	var cr struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Payouts map[string]struct {
			Amount string `json:"amount"`
			Tx     string `json:"tx"`
			Note   string `json:"note"`
		} `json:"payouts"`
	}
	code, err = s.faucetPost(ctx, base+"/api/claim", map[string]string{
		"ethAddress": ethAddr, "xmrAddress": xmrAddr, "challenge": ch.Challenge, "nonce": nonce,
	}, &cr)
	if err != nil {
		s.logf("faucet claim: %v", err)
		return nil, errFaucetOffline
	}
	res := &FaucetResult{OK: cr.OK && code == 200, Seconds: time.Since(t0).Seconds()}
	explorer = strings.TrimRight(explorer, "/")
	for _, k := range []struct{ key, coin string }{{"eth", "ETH"}, {"usdc", "USDC"}, {"xmr", "XMR"}} {
		p, ok := cr.Payouts[k.key]
		if !ok {
			continue
		}
		fp := FaucetPayout{Coin: k.coin, Amount: p.Amount, Tx: p.Tx, Note: p.Note}
		if p.Tx != "" {
			if k.key == "xmr" {
				fp.TxURL = "https://stagenet.xmrchain.net/tx/" + p.Tx
			} else if explorer != "" {
				fp.TxURL = explorer + "/tx/" + p.Tx
			}
		}
		res.Payouts = append(res.Payouts, fp)
	}
	switch {
	case res.OK:
		res.Message = "Sent. The coins show up in a minute or two."
		for _, p := range res.Payouts {
			if p.Coin == "XMR" && p.Tx != "" {
				res.Message = "Sent. ETH and USDC show up in a minute or two; XMR needs about 20 minutes to unlock."
			}
		}
	case cr.Error != "":
		res.Message = cr.Error
	default:
		res.Message = "The faucet couldn't send anything right now."
	}
	return res, nil
}

// faucetClaim is POST /v1/faucet/claim.
func (s *Server) faucetClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST only")
		return
	}
	s.mu.Lock()
	v := s.vault
	base := s.faucetURL()
	explorer := s.explorer()
	s.mu.Unlock()
	if v == nil {
		writeErr(w, 401, "unlock the wallet first")
		return
	}
	if base == "" {
		writeErr(w, 400, "The faucet is only available in testnet builds.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	res, err := s.ClaimFaucet(ctx, base, v.Keys.EthAddr.Hex(), v.Keys.XmrAddr, explorer)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// faucetStatus passes on the faucet's public /api/status: which coins it can
// send right now, so the wallet can say so before anyone presses the button.
func (s *Server) faucetStatus(w http.ResponseWriter, r *http.Request) {
	base := s.faucetURL()
	if base == "" {
		writeErr(w, 404, "no faucet for this network")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/status", nil)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	req.Header.Set("X-SeshSwap-Client", FaucetClient())
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		writeErr(w, 502, "the faucet did not answer")
		return
	}
	defer resp.Body.Close()
	var st map[string]any
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&st) != nil {
		writeErr(w, 502, "the faucet did not answer")
		return
	}
	out := map[string]any{}
	for _, c := range []string{"eth", "usdc", "xmr"} {
		if m, ok := st[c].(map[string]any); ok {
			out[c] = map[string]any{"amount": m["amount"], "available": m["available"] == true, "note": m["note"]}
		}
	}
	writeJSON(w, 200, out)
}
