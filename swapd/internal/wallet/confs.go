package wallet

import (
	"encoding/json"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/carlcrainellc/seshswap/swapd/internal/api"
	"github.com/carlcrainellc/seshswap/swapd/internal/swap"
)

// Monero confirmation setting (Settings screen). Standard waits for what the
// swap size needs (10 for small swaps, up to 30 for large ones). Safer and
// Safest ask the maker for at least 20 or 30; they never lower the size rule.
const (
	ConfsStandard = "standard"
	ConfsSafer    = "safer"
	ConfsSafest   = "safest"
)

var confLevels = map[string]uint64{ConfsStandard: swap.XmrConfFloor, ConfsSafer: 20, ConfsSafest: 30}

// normConfs maps a setting to a level, "" if unknown. Empty means Standard.
func normConfs(v string) string {
	if v == "" {
		return ConfsStandard
	}
	if _, ok := confLevels[v]; ok {
		return v
	}
	return ""
}

// confLevel is the current setting. Callers must not hold s.mu.
func (s *Server) confLevel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confLevelLocked()
}

func (s *Server) confLevelLocked() string {
	if l := normConfs(s.Cfg.Confirmations); l != "" {
		return l
	}
	return ConfsStandard
}

// wantConfs is the least confirmations this wallet asks makers for.
func (s *Server) wantConfs() uint64 { return confLevels[s.confLevel()] }

// quoteParam is the extra query for a maker's /v1/quote ("" for Standard,
// so older makers see the same request as before).
func quoteParam(want uint64) string {
	if want <= swap.XmrConfFloor {
		return ""
	}
	return "&minConfs=" + strconv.FormatUint(want, 10)
}

func (s *Server) prefsFile() string { return filepath.Join(s.Cfg.DataDir, "prefs.json") }

type prefs struct {
	Confirmations string `json:"confirmations,omitempty"`
}

// loadPrefs applies settings saved from the Settings screen over the config file.
func (s *Server) loadPrefs() {
	b, err := os.ReadFile(s.prefsFile())
	if err != nil {
		return
	}
	var p prefs
	if json.Unmarshal(b, &p) == nil && p.Confirmations != "" && normConfs(p.Confirmations) != "" {
		s.Cfg.Confirmations = p.Confirmations
	}
}

func (s *Server) savePrefsLocked() error {
	b, _ := json.MarshalIndent(prefs{Confirmations: s.confLevelLocked()}, "", "  ")
	if err := os.MkdirAll(s.Cfg.DataDir, 0o700); err != nil {
		return err
	}
	tmp := s.prefsFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.prefsFile())
}

// confSettings is GET/PUT /v1/settings/confirmations.
func (s *Server) confSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut, http.MethodPost:
		var req struct {
			Confirmations string `json:"confirmations"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, 400, "bad json")
			return
		}
		l := normConfs(req.Confirmations)
		if l == "" || req.Confirmations == "" {
			writeErr(w, 400, "confirmations must be \"standard\", \"safer\" or \"safest\"")
			return
		}
		s.mu.Lock()
		s.Cfg.Confirmations = l
		err := s.savePrefsLocked()
		s.mu.Unlock()
		if err != nil {
			writeErr(w, 500, "couldn't save the setting: "+err.Error())
			return
		}
	default:
		writeErr(w, 405, "GET or PUT")
		return
	}
	l := s.confLevel()
	writeJSON(w, 200, map[string]any{"confirmations": l, "minConfs": confLevels[l]})
}

// confsOut adds the confirmation lines for the quote screen: what the swap
// waits for, what its size alone would need, and the extra wait from the
// setting (about one block time per extra confirmation).
func confsOut(out map[string]any, level string, confs, xmrPico, reorg, blockSecs uint64) {
	size := swap.XmrConfs(xmrPico, reorg)
	if blockSecs == 0 {
		blockSecs = swap.XmrBlockSecs
	}
	out["confLevel"], out["sizeConfs"] = level, size
	if confs > size {
		out["extraWaitSecs"] = (confs - size) * blockSecs
	}
}

// rateOut adds the price lines for the quote screen: the maker's market rate,
// its markup for this size, and the rate you get, each as XMR per 1 ETH or
// 1 USDC. Makers before 0.1.8 send no market rate; then only "rate" is set.
func rateOut(out map[string]any, terms *api.Terms, asset string, decimals int) {
	per := func(ppu *big.Int) string {
		if ppu == nil || ppu.Sign() <= 0 {
			return ""
		}
		x := new(big.Int).Mul(ppu, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
		x.Div(x, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
		return "1 " + asset + " = " + formatUnits(x, 12) + " XMR"
	}
	if r := per(terms.PicoPerUnit); r != "" {
		out["rate"] = r
	}
	if r := per(terms.MarketPerUnit); r != "" {
		out["marketRate"] = r
		out["markupPct"] = strconv.FormatFloat(float64(terms.SpreadBps)/100, 'f', 2, 64)
	}
}
