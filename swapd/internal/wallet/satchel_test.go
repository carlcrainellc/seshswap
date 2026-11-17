package wallet

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/carlcrainellc/seshswap/swapd/internal/keys"
)

func TestSatchelProposeNeedsWalletApproval(t *testing.T) {
	_, h := newTestServer(t)
	m, _ := NewMnemonic()
	body, _ := json.Marshal(map[string]string{"mnemonic": m, "password": "correct-password-ok"})
	if code, r := do(t, h, "POST", "/v1/wallet/restore", string(body), ""); code != 200 {
		t.Fatalf("restore %d %v", code, r)
	}
	satchel := "http://127.0.0.1:7677"
	code, r := do(t, h, "POST", "/v1/satchel/propose", `{"eth":"0.002"}`, satchel)
	if code != 200 {
		t.Fatalf("propose %d %v", code, r)
	}
	if r["status"] == nil || !strings.Contains(r["status"].(string), "approval") {
		t.Fatalf("status %v", r)
	}
	id, _ := r["id"].(string)
	if id == "" {
		t.Fatal("no proposal id")
	}

	// A Satchel-style page cannot start the swap itself.
	code, r = do(t, h, "POST", "/v1/swaps", `{"eth":"0.002"}`, satchel)
	if code != 403 {
		t.Fatalf("direct swap from satchel: %d %v", code, r)
	}
	code, r = do(t, h, "POST", "/v1/satchel/approve", `{"id":"`+id+`"}`, satchel)
	if code != 403 {
		t.Fatalf("approve from satchel: %d %v", code, r)
	}

	// The wallet can reject it.
	code, r = do(t, h, "POST", "/v1/satchel/reject", `{"id":"`+id+`"}`, "")
	if code != 200 {
		t.Fatalf("reject %d %v", code, r)
	}
	code, r = do(t, h, "GET", "/v1/satchel/pending", "", "")
	if code != 200 || r["pending"] != nil {
		t.Fatalf("pending after reject %d %v", code, r)
	}
}

func TestSatchelTestListenMustBeLoopback(t *testing.T) {
	if err := satchelTestListenOK("0.0.0.0:7677"); err == nil {
		t.Fatal("allowed non-loopback")
	}
	if err := satchelTestListenOK("127.0.0.1:7677"); err != nil {
		t.Fatal(err)
	}
	_ = keys.Stagenet
}
