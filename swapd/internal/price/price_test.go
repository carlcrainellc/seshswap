package price

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAggregateMedianAndOutlier(t *testing.T) {
	qs := []Quote{{"a", 300, 3000}, {"b", 301, 3010}, {"c", 299, 2990}, {"bad", 600, 3000}}
	x, e, used, err := Aggregate(qs, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if x != 300 || e != 3000 || len(used) != 3 {
		t.Fatalf("got %v %v %v", x, e, used)
	}
}

func TestAggregateNeedsAgreement(t *testing.T) {
	if _, _, _, err := Aggregate([]Quote{{"a", 300, 3000}}, Config{}); err == nil {
		t.Fatal("one source accepted")
	}
	if _, _, _, err := Aggregate([]Quote{{"a", 300, 3000}, {"b", 400, 3000}}, Config{}); err == nil {
		t.Fatal("two disagreeing sources accepted")
	}
}

func TestBreakerStaleAndSanity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	o := New(Config{MinXmrPerEth: 5, MaxXmrPerEth: 20}, Static{"a", 300, 3000}, Static{"b", 300, 3000})
	o.Now = func() time.Time { return now }
	if _, err := o.Current(); err == nil {
		t.Fatal("price before first refresh")
	}
	if err := o.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := o.Current()
	if err != nil || s.XmrPerEth() != 10 {
		t.Fatal(s, err)
	}
	now = now.Add(3 * time.Minute)
	if _, err := o.Current(); err == nil {
		t.Fatal("stale price accepted")
	}
	// a 50% jump trips the breaker
	o.Sources = []Source{Static{"a", 200, 3000}, Static{"b", 200, 3000}}
	if err := o.Refresh(context.Background()); err == nil {
		t.Fatal("jump accepted")
	}
	if _, err := o.Current(); err == nil {
		t.Fatal("breaker not open")
	}
	// sanity range
	o2 := New(Config{MinXmrPerEth: 5, MaxXmrPerEth: 20}, Static{"a", 100, 3000}, Static{"b", 100, 3000})
	if err := o2.Refresh(context.Background()); err == nil {
		t.Fatal("30 XMR/ETH accepted")
	}
}

func TestSourcesParse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/0/public/Ticker", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":[],"result":{"XXMRZUSD":{"c":["300.5","1"]},"XETHZUSD":{"c":["3000.1","1"]}}}`)
	})
	mux.HandleFunc("/api/v3/simple/price", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"monero":{"usd":301},"ethereum":{"usd":3001}}`)
	})
	mux.HandleFunc("/v2/tickers", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[["tXMRUSD",1,2,3,4,5,6,299.5,8,9,10],["tETHUSD",1,2,3,4,5,6,2999,8,9,10]]`)
	})
	mux.HandleFunc("/api/v1/market/orderbook/level1", func(w http.ResponseWriter, r *http.Request) {
		p := "300"
		if r.URL.Query().Get("symbol") == "ETH-USDT" {
			p = "3000"
		}
		fmt.Fprintf(w, `{"code":"200000","data":{"price":"%s"}}`, p)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	o := New(Config{MinSources: 4}, Kraken{srv.URL}, CoinGecko{srv.URL}, Bitfinex{srv.URL}, KuCoin{srv.URL})
	if err := o.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ := o.Current()
	if len(s.Sources) != 4 || s.XMRUSD < 299 || s.XMRUSD > 301 {
		t.Fatal(s)
	}
}

// TestLiveSources hits the real public APIs; opt-in (PRICE_LIVE=1).
func TestLiveSources(t *testing.T) {
	if testing.Short() || getenv("PRICE_LIVE") == "" {
		t.Skip("set PRICE_LIVE=1")
	}
	o := New(Config{})
	if err := o.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ := o.Current()
	t.Logf("XMR %.2f USD, ETH %.2f USD (%.4f XMR/ETH) from %v", s.XMRUSD, s.ETHUSD, s.XmrPerEth(), s.Sources)
}
