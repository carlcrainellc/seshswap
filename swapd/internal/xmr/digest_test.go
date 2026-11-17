package xmr

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A stand-in for monero-wallet-rpc --rpc-login: Digest MD5, qop=auth.
func TestDigestLogin(t *testing.T) {
	const user, pass, realm, nonce = "seshswap", "s3cret", "monero-rpc", "abc123"
	h := func(s string) string { b := md5.Sum([]byte(s)); return hex.EncodeToString(b[:]) }
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		a := r.Header.Get("Authorization")
		f := map[string]string{}
		for _, p := range splitParams(strings.TrimPrefix(a, "Digest ")) {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			f[k] = strings.Trim(v, `"`)
		}
		ha1 := h(user + ":" + realm + ":" + pass)
		ha2 := h(r.Method + ":" + f["uri"])
		want := h(ha1 + ":" + nonce + ":" + f["nc"] + ":" + f["cnonce"] + ":auth:" + ha2)
		if a == "" || f["username"] != user || f["response"] != want {
			w.Header().Set("WWW-Authenticate", `Digest qop="auth",algorithm=MD5,realm="`+realm+`",nonce="`+nonce+`",stale=false`)
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"result":{"version":196613}}`))
	}))
	defer srv.Close()
	u := strings.Replace(srv.URL, "http://", "http://"+user+":"+pass+"@", 1)
	w := NewWallet(u, "")
	for i := 0; i < 2; i++ {
		v, err := w.Version(context.Background())
		if err != nil || v != 196613 {
			t.Fatalf("call %d: %v %v", i, v, err)
		}
	}
	if calls != 3 { // one challenge, then two answered requests
		t.Fatalf("server saw %d requests, want 3", calls)
	}
	bad := NewWallet(strings.Replace(u, pass, "wrong", 1), "")
	if _, err := bad.Version(context.Background()); err == nil || !strings.Contains(err.Error(), "login refused") {
		t.Fatalf("wrong password: %v", err)
	}
}
