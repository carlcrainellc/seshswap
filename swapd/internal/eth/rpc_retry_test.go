package eth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func rpcOK(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func rpcErrWrite(w http.ResponseWriter, status, code int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"error":   map[string]any{"code": code, "message": msg},
	})
}

func TestCallRetriesJSONRPCRateLimit(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := n.Add(1)
		if i <= 2 {
			rpcErrWrite(w, 200, -32005, "rate limit exceeded")
			return
		}
		rpcOK(w, "0x2a")
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.Retries = 4
	c.Backoff = time.Millisecond
	var s string
	if err := c.Call(context.Background(), &s, "eth_blockNumber"); err != nil {
		t.Fatal(err)
	}
	if s != "0x2a" {
		t.Fatalf("got %q", s)
	}
	if n.Load() != 3 {
		t.Fatalf("attempts=%d want 3", n.Load())
	}
}

func TestCallRetriesHTTP429(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte("Too Many Requests"))
			return
		}
		rpcOK(w, "0x1")
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.Retries = 3
	c.Backoff = time.Millisecond
	var s string
	if err := c.Call(context.Background(), &s, "eth_chainId"); err != nil || s != "0x1" {
		t.Fatalf("got %q %v", s, err)
	}
	if n.Load() != 2 {
		t.Fatalf("attempts=%d want 2", n.Load())
	}
}

func TestCallDoesNotRetryPermanentRPCError(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		rpcErrWrite(w, 200, -32000, "insufficient funds")
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.Retries = 5
	c.Backoff = time.Millisecond
	err := c.Call(context.Background(), nil, "eth_sendRawTransaction", "0x00")
	if err == nil {
		t.Fatal("expected error")
	}
	var re *rpcErr
	if !errors.As(err, &re) || re.Code != -32000 {
		t.Fatalf("want wrapped -32000, got %v", err)
	}
	if n.Load() != 1 {
		t.Fatalf("retried a permanent error: attempts=%d", n.Load())
	}
}

func TestCallFallbackOnRateLimit(t *testing.T) {
	var primary, secondary atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primary.Add(1)
		rpcErrWrite(w, 200, -32005, "rate limit exceeded")
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondary.Add(1)
		rpcOK(w, "0xaa")
	}))
	defer good.Close()
	c := NewClients(bad.URL, good.URL)
	c.Retries = 3
	c.Backoff = time.Millisecond
	var s string
	if err := c.Call(context.Background(), &s, "eth_blockNumber"); err != nil || s != "0xaa" {
		t.Fatalf("got %q %v", s, err)
	}
	if primary.Load() < 1 || secondary.Load() != 1 {
		t.Fatalf("primary=%d secondary=%d", primary.Load(), secondary.Load())
	}
}

func TestCallRateLimitExhausted(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		rpcErrWrite(w, 200, -32005, "rate limit exceeded")
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.Retries = 2
	c.Backoff = time.Millisecond
	err := c.Call(context.Background(), nil, "eth_sendRawTransaction", "0x00")
	if err == nil || !IsRateLimited(err, 0) {
		t.Fatalf("want rate-limit error, got %v", err)
	}
	if n.Load() != 3 { // first + 2 retries
		t.Fatalf("attempts=%d want 3", n.Load())
	}
}

func TestCallCancelsDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpcErrWrite(w, 200, -32005, "rate limit exceeded")
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.Retries = 8
	c.Backoff = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	t0 := time.Now()
	err := c.Call(ctx, nil, "eth_blockNumber")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatal("did not return after cancel")
	}
}

func TestNewClientsSplitsCommaAndDedups(t *testing.T) {
	c := NewClients("http://a, http://b", "http://b", "http://c")
	if c.URL != "http://a" {
		t.Fatalf("primary %q", c.URL)
	}
	if len(c.URLs) != 3 || c.URLs[2] != "http://c" {
		t.Fatalf("urls %v", c.URLs)
	}
}

func TestIsRateLimited(t *testing.T) {
	if !IsRateLimited(nil, 429) {
		t.Fatal("http 429")
	}
	if IsRateLimited(nil, 500) {
		t.Fatal("http 500 is not a rate limit")
	}
	if !IsRateLimited(&rpcErr{Code: -32005, Message: "rate limit exceeded"}, 200) {
		t.Fatal("-32005")
	}
	if !IsRateLimited(&rpcErr{Code: -32000, Message: "Too Many Requests"}, 200) {
		t.Fatal("message")
	}
	if IsRateLimited(&rpcErr{Code: -32000, Message: "nonce too low"}, 200) {
		t.Fatal("nonce too low is not a rate limit")
	}
}
