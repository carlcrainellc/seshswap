package xmr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A wallet-rpc that forgot the open wallet (restart) must be reopened, not
// fail every call until swapd restarts.
func TestSessionReopensAfterNoWalletFile(t *testing.T) {
	open, opens := "", 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "open_wallet":
			open, opens = "x", opens+1
			_, _ = w.Write([]byte(`{"result":{}}`))
		case "close_wallet":
			open = ""
			_, _ = w.Write([]byte(`{"result":{}}`))
		default:
			if open == "" {
				_, _ = w.Write([]byte(`{"error":{"code":-13,"message":"No wallet file"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{}}`))
		}
	}))
	defer srv.Close()
	w := NewWallet(srv.URL, "pw")
	ctx := context.Background()
	refresh := func(s *Sess) error { return s.Refresh(ctx) }
	if err := w.Session(ctx, "x", refresh); err != nil {
		t.Fatal(err)
	}
	open = "" // wallet-rpc restarted behind our back
	if err := w.Session(ctx, "x", refresh); err != nil {
		t.Fatalf("not recovered: %v", err)
	}
	if opens != 2 {
		t.Fatalf("opens = %d, want 2", opens)
	}
}
