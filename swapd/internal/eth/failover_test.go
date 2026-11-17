package eth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeNode struct {
	srv   *httptest.Server
	hits  atomic.Int32
	mode  atomic.Value // "ok", "hang", "500", "revert", "ratelimit"
	block chan struct{}
}

func newNode(t *testing.T, mode string) *fakeNode {
	n := &fakeNode{block: make(chan struct{})}
	n.mode.Store(mode)
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.hits.Add(1)
		switch n.mode.Load().(string) {
		case "hang":
			select {
			case <-n.block:
			case <-r.Context().Done():
			}
			return
		case "500":
			http.Error(w, "bad gateway", 502)
			return
		case "revert":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted"}}`)
			return
		case "ratelimit":
			w.WriteHeader(429)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"rate limit"}}`)
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":"0x66eee"}`)
	}))
	t.Cleanup(func() { close(n.block); n.srv.Close() })
	return n
}

func failoverClient(urls ...string) (*Client, *[]string) {
	c := NewClients(urls...)
	c.Failover = true
	c.AttemptTimeout = 300 * time.Millisecond
	c.Backoff = time.Millisecond
	var ev []string
	c.OnFailover = func(from, to string, err error) { ev = append(ev, from+"->"+to) }
	return c, &ev
}

func TestFailoverOnTimeout(t *testing.T) {
	a, b := newNode(t, "hang"), newNode(t, "ok")
	c, ev := failoverClient(a.srv.URL, b.srv.URL)
	t0 := time.Now()
	id, err := c.ChainID(context.Background())
	if err != nil || id.Int64() != 421614 {
		t.Fatalf("%v %v", id, err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("took %s", d)
	}
	if len(*ev) != 1 || !strings.HasPrefix((*ev)[0], a.srv.URL) {
		t.Fatalf("events %v", *ev)
	}
	// sticky: the next call goes straight to the secondary
	before := a.hits.Load()
	if _, err := c.ChainID(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.hits.Load() != before {
		t.Fatal("primary was tried again right away")
	}
}

func TestFailoverOnHTTPError(t *testing.T) {
	a, b := newNode(t, "500"), newNode(t, "ok")
	c, _ := failoverClient(a.srv.URL, b.srv.URL)
	if _, err := c.ChainID(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.hits.Load() != 1 {
		t.Fatalf("secondary hits %d", b.hits.Load())
	}
}

func TestFailoverPrimaryRetried(t *testing.T) {
	a, b := newNode(t, "500"), newNode(t, "ok")
	c, _ := failoverClient(a.srv.URL, b.srv.URL)
	c.PrimaryRetry = 50 * time.Millisecond
	_, _ = c.ChainID(context.Background())
	a.mode.Store("ok")
	time.Sleep(80 * time.Millisecond)
	_, _ = c.ChainID(context.Background())
	if a.hits.Load() != 2 {
		t.Fatalf("primary should be retried after PrimaryRetry, hits %d", a.hits.Load())
	}
	_, _ = c.ChainID(context.Background())
	if a.hits.Load() != 3 || b.hits.Load() != 1 {
		t.Fatalf("back on the primary: a=%d b=%d", a.hits.Load(), b.hits.Load())
	}
}

func TestNoFailoverOnRevert(t *testing.T) {
	a, b := newNode(t, "revert"), newNode(t, "ok")
	c, _ := failoverClient(a.srv.URL, b.srv.URL)
	if _, err := c.ChainID(context.Background()); err == nil || !strings.Contains(err.Error(), "reverted") {
		t.Fatalf("want the revert, got %v", err)
	}
	if b.hits.Load() != 0 {
		t.Fatal("a revert must not be retried elsewhere")
	}
}

func TestAllNodesDown(t *testing.T) {
	a, b := newNode(t, "500"), newNode(t, "hang")
	c, _ := failoverClient(a.srv.URL, b.srv.URL)
	c.Retries = 3
	if _, err := c.ChainID(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if a.hits.Load()+b.hits.Load() != 4 {
		t.Fatalf("attempts %d", a.hits.Load()+b.hits.Load())
	}
}

func TestDefaultClientStillStopsOnNodeError(t *testing.T) {
	a, b := newNode(t, "500"), newNode(t, "ok")
	c := NewClients(a.srv.URL, b.srv.URL)
	if _, err := c.ChainID(context.Background()); err == nil {
		t.Fatal("without Failover a node error is returned as before")
	}
	if b.hits.Load() != 0 {
		t.Fatal("secondary used without Failover")
	}
}
