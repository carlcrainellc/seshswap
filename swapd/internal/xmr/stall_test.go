package xmr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stallingWalletRPC answers every method at once except "refresh", which
// hangs (like monero-wallet-rpc when its node link stalls) until release is
// closed or the client gives up.
func stallingWalletRPC(t *testing.T) (url string, entered <-chan struct{}, release chan struct{}) {
	ent := make(chan struct{}, 16)
	rel := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "refresh" {
			ent <- struct{}{}
			select {
			case <-rel:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"balance": 1, "unlocked_balance": 1}})
	}))
	t.Cleanup(func() {
		select {
		case <-rel:
		default:
			close(rel)
		}
		srv.Close()
	})
	return srv.URL, ent, rel
}

// v0.1.5 freeze: while one Session is stuck in a stalled RPC, a second
// caller waits for the wallet mutex with no regard for its own deadline.
// The wallet's UI calls (balances 12 s, send 60 s) then hang for as long as
// the stalled call does (up to the 300 s HTTP timeout, again and again).
func TestSessionWaitHonoursContext(t *testing.T) {
	url, entered, release := stallingWalletRPC(t)
	w := NewWallet(url, "pw")
	go func() {
		_ = w.Session(context.Background(), "main", func(s *Sess) error { return s.Refresh(context.Background()) })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- w.Session(ctx, "main", func(s *Sess) error { _, _, err := s.Balance(ctx); return err })
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("got a balance while another call held the wallet")
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("waited %v past a 200ms deadline", d)
		}
	case <-time.After(3 * time.Second):
		close(release)
		<-done
		t.Fatal("FREEZE: Session ignored its context while another call was stalled (waited 3 s past a 200 ms deadline)")
	}
}
