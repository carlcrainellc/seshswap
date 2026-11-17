package xmr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A fake monerod: 61 blocks, 280 s apart on average, one alternative chain of 18.
func TestAvgBlockSecsAndAltChains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var res any
		switch req.Method {
		case "get_info":
			res = map[string]any{"height": 1000}
		case "get_block_headers_range":
			if req.Params["start_height"].(float64) != 939 || req.Params["end_height"].(float64) != 999 {
				t.Errorf("range %v", req.Params)
			}
			var hs []map[string]any
			for i := 0; i < 61; i++ {
				hs = append(hs, map[string]any{"timestamp": 1_000_000 + 280*i})
			}
			res = map[string]any{"headers": hs}
		case "get_alternate_chains":
			res = map[string]any{"chains": []map[string]any{{"length": 3}, {"length": 18}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": res})
	}))
	defer srv.Close()
	d := NewDaemon(srv.URL)
	s, err := d.AvgBlockSecs(context.Background(), 60)
	if err != nil || s != 280 {
		t.Fatal(s, err)
	}
	a, err := d.MaxAltChainDepth(context.Background())
	if err != nil || a != 18 {
		t.Fatal(a, err)
	}
}
