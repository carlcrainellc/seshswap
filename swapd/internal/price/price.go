// Package price gives the maker a live XMR and ETH price in USD from several
// public sources. It takes the median, drops sources that disagree with it,
// refuses to quote unless at least MinSources agree, bounds the result by a
// configured sanity range and by the largest jump allowed between refreshes,
// and stops quoting (circuit breaker) when the data is stale.
package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Quote is one source's answer, in USD.
type Quote struct {
	Source string
	XMR    float64
	ETH    float64
}

// Source fetches XMR/USD and ETH/USD.
type Source interface {
	Name() string
	Fetch(ctx context.Context, c *http.Client) (Quote, error)
}

func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "swapd-maker/0.3")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(out)
}

func pos(name string, x float64) error {
	if !(x > 0) || math.IsInf(x, 0) {
		return fmt.Errorf("%s: bad price %v", name, x)
	}
	return nil
}

// Kraken: /0/public/Ticker; c[0] is the last trade price.
type Kraken struct{ Base string }

func (Kraken) Name() string { return "kraken" }
func (k Kraken) Fetch(ctx context.Context, c *http.Client) (Quote, error) {
	base := k.Base
	if base == "" {
		base = "https://api.kraken.com"
	}
	var o struct {
		Error  []string `json:"error"`
		Result map[string]struct {
			C []string `json:"c"`
		} `json:"result"`
	}
	if err := getJSON(ctx, c, base+"/0/public/Ticker?pair=XMRUSD,ETHUSD", &o); err != nil {
		return Quote{}, err
	}
	if len(o.Error) > 0 {
		return Quote{}, fmt.Errorf("kraken: %v", o.Error)
	}
	last := func(keys ...string) float64 {
		for _, k := range keys {
			if t, ok := o.Result[k]; ok && len(t.C) > 0 {
				v, _ := strconv.ParseFloat(t.C[0], 64)
				return v
			}
		}
		return 0
	}
	q := Quote{Source: "kraken", XMR: last("XXMRZUSD", "XMRUSD"), ETH: last("XETHZUSD", "ETHUSD")}
	return q, errors.Join(pos("kraken xmr", q.XMR), pos("kraken eth", q.ETH))
}

// CoinGecko: /api/v3/simple/price (aggregated index).
type CoinGecko struct{ Base string }

func (CoinGecko) Name() string { return "coingecko" }
func (g CoinGecko) Fetch(ctx context.Context, c *http.Client) (Quote, error) {
	base := g.Base
	if base == "" {
		base = "https://api.coingecko.com"
	}
	var o map[string]map[string]float64
	if err := getJSON(ctx, c, base+"/api/v3/simple/price?ids=monero,ethereum&vs_currencies=usd", &o); err != nil {
		return Quote{}, err
	}
	q := Quote{Source: "coingecko", XMR: o["monero"]["usd"], ETH: o["ethereum"]["usd"]}
	return q, errors.Join(pos("coingecko xmr", q.XMR), pos("coingecko eth", q.ETH))
}

// Bitfinex: /v2/tickers; index 7 is the last price.
type Bitfinex struct{ Base string }

func (Bitfinex) Name() string { return "bitfinex" }
func (b Bitfinex) Fetch(ctx context.Context, c *http.Client) (Quote, error) {
	base := b.Base
	if base == "" {
		base = "https://api-pub.bitfinex.com"
	}
	var o [][]any
	if err := getJSON(ctx, c, base+"/v2/tickers?symbols=tXMRUSD,tETHUSD", &o); err != nil {
		return Quote{}, err
	}
	q := Quote{Source: "bitfinex"}
	for _, row := range o {
		if len(row) < 8 {
			continue
		}
		sym, _ := row[0].(string)
		v, _ := row[7].(float64)
		switch sym {
		case "tXMRUSD":
			q.XMR = v
		case "tETHUSD":
			q.ETH = v
		}
	}
	return q, errors.Join(pos("bitfinex xmr", q.XMR), pos("bitfinex eth", q.ETH))
}

// KuCoin: level-1 order book against USDT (treated as USD).
type KuCoin struct{ Base string }

func (KuCoin) Name() string { return "kucoin" }
func (k KuCoin) Fetch(ctx context.Context, c *http.Client) (Quote, error) {
	base := k.Base
	if base == "" {
		base = "https://api.kucoin.com"
	}
	one := func(sym string) (float64, error) {
		var o struct {
			Code string `json:"code"`
			Data struct {
				Price string `json:"price"`
			} `json:"data"`
		}
		if err := getJSON(ctx, c, base+"/api/v1/market/orderbook/level1?symbol="+sym, &o); err != nil {
			return 0, err
		}
		return strconv.ParseFloat(o.Data.Price, 64)
	}
	x, err1 := one("XMR-USDT")
	e, err2 := one("ETH-USDT")
	q := Quote{Source: "kucoin", XMR: x, ETH: e}
	return q, errors.Join(err1, err2, pos("kucoin xmr", x), pos("kucoin eth", e))
}

// DefaultSources are the public endpoints checked live when this was written.
func DefaultSources() []Source {
	return []Source{Kraken{}, CoinGecko{}, Bitfinex{}, KuCoin{}}
}

// Config bounds what the oracle will accept.
type Config struct {
	MinSources   int           // sources that must agree (>= 2)
	MaxDeviation float64       // drop a source more than this fraction from the median (0.02)
	MaxAge       time.Duration // stop quoting if the last good price is older (2m)
	MaxJump      float64       // a refresh moving XMR/ETH by more than this fraction trips the breaker (0.10)
	MinXmrPerEth float64       // sanity range for the XMR-per-ETH ratio (0 = unchecked)
	MaxXmrPerEth float64
}

func (c *Config) defaults() {
	if c.MinSources < 2 {
		c.MinSources = 2
	}
	if c.MaxDeviation == 0 {
		c.MaxDeviation = 0.02
	}
	if c.MaxAge == 0 {
		c.MaxAge = 2 * time.Minute
	}
	if c.MaxJump == 0 {
		c.MaxJump = 0.10
	}
}

// Snapshot is the last accepted price.
type Snapshot struct {
	XMRUSD   float64   `json:"xmrUsd"`
	ETHUSD   float64   `json:"ethUsd"`
	Sources  []string  `json:"sources"`
	At       time.Time `json:"at"`
	Tripped  string    `json:"tripped,omitempty"` // non-empty: circuit breaker open, why
	Failures int       `json:"failures"`
}

// XmrPerEth is how many XMR one ETH buys at the mid price.
func (s Snapshot) XmrPerEth() float64 { return s.ETHUSD / s.XMRUSD }

type Oracle struct {
	Sources []Source
	Cfg     Config
	HTTP    *http.Client
	Now     func() time.Time

	mu   sync.Mutex
	last Snapshot
}

func New(cfg Config, src ...Source) *Oracle {
	cfg.defaults()
	if len(src) == 0 {
		src = DefaultSources()
	}
	return &Oracle{Sources: src, Cfg: cfg, HTTP: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Aggregate combines quotes: median, outlier removal, agreement count.
func Aggregate(qs []Quote, cfg Config) (xmr, eth float64, used []string, err error) {
	cfg.defaults()
	if len(qs) < cfg.MinSources {
		return 0, 0, nil, fmt.Errorf("only %d price sources answered; need %d", len(qs), cfg.MinSources)
	}
	var xs, es []float64
	for _, q := range qs {
		xs, es = append(xs, q.XMR), append(es, q.ETH)
	}
	mx, me := median(xs), median(es)
	xs, es = nil, nil
	for _, q := range qs {
		if math.Abs(q.XMR/mx-1) > cfg.MaxDeviation || math.Abs(q.ETH/me-1) > cfg.MaxDeviation {
			continue
		}
		xs, es, used = append(xs, q.XMR), append(es, q.ETH), append(used, q.Source)
	}
	if len(used) < cfg.MinSources {
		return 0, 0, nil, fmt.Errorf("only %d price sources agree within %.1f%%; need %d", len(used), cfg.MaxDeviation*100, cfg.MinSources)
	}
	return median(xs), median(es), used, nil
}

// Refresh queries every source in parallel and updates the snapshot.
func (o *Oracle) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res := make([]Quote, len(o.Sources))
	errs := make([]error, len(o.Sources))
	var wg sync.WaitGroup
	for i, s := range o.Sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i], errs[i] = s.Fetch(ctx, o.HTTP)
		}()
	}
	wg.Wait()
	var ok []Quote
	for i := range res {
		if errs[i] == nil {
			ok = append(ok, res[i])
		}
	}
	x, e, used, err := Aggregate(ok, o.Cfg)
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		o.last.Failures++
		return errors.Join(append([]error{err}, errs...)...)
	}
	ratio := e / x
	if (o.Cfg.MinXmrPerEth > 0 && ratio < o.Cfg.MinXmrPerEth) || (o.Cfg.MaxXmrPerEth > 0 && ratio > o.Cfg.MaxXmrPerEth) {
		o.last.Failures++
		o.last.Tripped = fmt.Sprintf("XMR per ETH %.4f outside the sanity range [%.4f, %.4f]", ratio, o.Cfg.MinXmrPerEth, o.Cfg.MaxXmrPerEth)
		return errors.New(o.last.Tripped)
	}
	if o.last.XMRUSD > 0 && o.Now().Sub(o.last.At) < 10*o.Cfg.MaxAge {
		if prev := o.last.XmrPerEth(); math.Abs(ratio/prev-1) > o.Cfg.MaxJump {
			o.last.Failures++
			o.last.Tripped = fmt.Sprintf("XMR per ETH moved %.1f%% in one refresh (limit %.1f%%)", (ratio/prev-1)*100, o.Cfg.MaxJump*100)
			// accept it as the new reference, but stay tripped until an operator resets or the next refresh agrees
			o.last.XMRUSD, o.last.ETHUSD, o.last.At = x, e, o.Now()
			return errors.New(o.last.Tripped)
		}
	}
	o.last = Snapshot{XMRUSD: x, ETHUSD: e, Sources: used, At: o.Now()}
	return nil
}

// Current returns a price that is safe to quote from, or an error (breaker open).
func (o *Oracle) Current() (Snapshot, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.last
	switch {
	case s.At.IsZero():
		return s, errors.New("no price yet")
	case s.Tripped != "":
		return s, errors.New("price circuit breaker open: " + s.Tripped)
	case o.Now().Sub(s.At) > o.Cfg.MaxAge:
		return s, fmt.Errorf("price is stale (%s old)", o.Now().Sub(s.At).Round(time.Second))
	}
	return s, nil
}

// Status is Current without the error, for health and metrics.
func (o *Oracle) Status() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last
}

// Run refreshes every `every` until ctx is done.
func (o *Oracle) Run(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	for {
		if err := o.Refresh(ctx); err != nil && logf != nil {
			logf("price: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Static is a fixed price source (tests and devnets).
type Static struct {
	ID       string
	XMR, ETH float64
}

func (s Static) Name() string { return s.ID }
func (s Static) Fetch(context.Context, *http.Client) (Quote, error) {
	return Quote{Source: s.ID, XMR: s.XMR, ETH: s.ETH}, nil
}
