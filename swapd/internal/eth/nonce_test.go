package eth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A lagging load-balanced RPC keeps answering the same pending nonce: two txs
// built back to back must still get consecutive nonces; after the hint expires
// the RPC is trusted again (no permanent gap from an unsent tx).
func TestNonceHintOverLaggingRPC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0x7"})
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	a := Address{1}
	ctx := context.Background()
	for i, want := range []uint64{7, 8, 9} {
		n, err := c.nextNonce(ctx, a)
		if err != nil || n != want {
			t.Fatalf("call %d: got %d, %v; want %d", i, n, err, want)
		}
	}
	if n, _ := c.nextNonce(ctx, Address{2}); n != 7 {
		t.Fatal("hint leaked to another address", n)
	}
	old := NonceHintTTL
	NonceHintTTL = time.Millisecond
	defer func() { NonceHintTTL = old }()
	time.Sleep(5 * time.Millisecond)
	if n, _ := c.nextNonce(ctx, a); n != 7 {
		t.Fatal("expired hint still used", n)
	}
}

// Two swaps share one key. A's lock is in flight at nonce 7; the RPC still
// reports 7. B is handed 8 for a reveal that is then dropped unpublished.
// Forgetting must give back only 8, so the next build is 8, not 7 (which would
// collide with A's in-flight lock). If a later nonce was already handed out,
// the hint is left alone and the gap is accepted.
func TestForgetNonceTwoSwapsAndGiveBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0x7"})
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	a := Address{1}
	ctx := context.Background()

	nA, err := c.nextNonce(ctx, a) // A's lock in flight
	if err != nil || nA != 7 {
		t.Fatalf("A: got %d, %v", nA, err)
	}
	nB, err := c.nextNonce(ctx, a) // B's unsent reveal
	if err != nil || nB != 8 {
		t.Fatalf("B: got %d, %v", nB, err)
	}
	c.ForgetNonce(a, nB) // B dropped; last handed out
	n, err := c.nextNonce(ctx, a)
	if err != nil || n != 8 {
		t.Fatalf("after give-back: got %d, %v; want 8 (not 7, A's in-flight nonce)", n, err)
	}

	// A later nonce was already handed out: forget the middle one, leave the hint.
	c2 := NewClient(srv.URL)
	for i, want := range []uint64{7, 8, 9} {
		n, err := c2.nextNonce(ctx, a)
		if err != nil || n != want {
			t.Fatalf("setup %d: got %d, %v", i, n, err)
		}
	}
	// hint.next is 10. Dropping 8 must not shrink it (10 != 8+1) and must not
	// wipe it (next would then take 7 from the lagging RPC).
	c2.ForgetNonce(a, 8)
	n, err = c2.nextNonce(ctx, a)
	if err != nil || n != 10 {
		t.Fatalf("after dropping a non-last nonce: got %d, %v; want 10 (accept the gap)", n, err)
	}

	// Give-back is a no-op with no hint (nothing to collide with).
	c3 := NewClient(srv.URL)
	c3.ForgetNonce(a, 7)
	if n, _ := c3.nextNonce(ctx, a); n != 7 {
		t.Fatalf("no hint: got %d", n)
	}
}
