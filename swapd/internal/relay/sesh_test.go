package relay

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/eth"
	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
)

func TestPostOverSeshLoop(t *testing.T) {
	loop := sesh.NewLoop()
	defer loop.Close()
	var path, host string
	_, err := loop.Serve("relayer.sesh", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, host = r.URL.Path, r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{TxHash: "0x" + strings.Repeat("ab", 32)})
	}))
	if err != nil {
		t.Fatal(err)
	}
	old := HTTP
	HTTP = loop.Client()
	t.Cleanup(func() { HTTP = old })

	h, err := Post(t.Context(), "http://relayer.sesh", &eth.RelayedClaim{})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/relay" {
		t.Fatalf("path %s", path)
	}
	if host != "relayer.sesh" && host != "relayer.sesh:80" {
		t.Fatalf("host %s", host)
	}
	want, _ := eth.ParseHash("0x" + strings.Repeat("ab", 32))
	if h != want {
		t.Fatalf("hash %s", h)
	}

	path = ""
	_, err = PostIntent(t.Context(), "http://relayer.sesh", &Intent{Kind: "claimIntent"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/intent" {
		t.Fatalf("intent path %s", path)
	}
}
