package xmr

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

// digest is HTTP Digest auth (RFC 2617, MD5, qop=auth), which is what
// monero-wallet-rpc --rpc-login and monerod --rpc-login expect.
type digest struct {
	user, pass string

	mu     sync.Mutex
	realm  string
	nonce  string
	opaque string
	qop    string
	nc     int
}

// splitLogin takes "http://user:pass@host:port" and returns the URL without
// the login plus a digest helper (nil when there is no login).
func splitLogin(raw string) (string, *digest) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw, nil
	}
	pass, _ := u.User.Password()
	d := &digest{user: u.User.Username(), pass: pass}
	u.User = nil
	return strings.TrimRight(u.String(), "/"), d
}

func (d *digest) challenge(h string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(strings.ToLower(h), "digest ") {
		return
	}
	d.realm, d.nonce, d.opaque, d.qop, d.nc = "", "", "", "", 0
	for _, part := range splitParams(h[len("digest "):]) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "realm":
			d.realm = v
		case "nonce":
			d.nonce = v
		case "opaque":
			d.opaque = v
		case "qop":
			for _, q := range strings.Split(v, ",") {
				if strings.TrimSpace(q) == "auth" {
					d.qop = "auth"
				}
			}
		}
	}
}

// splitParams splits on commas that are not inside quotes.
func splitParams(s string) []string {
	var out []string
	var b strings.Builder
	inQ := false
	for _, c := range s {
		switch {
		case c == '"':
			inQ = !inQ
			b.WriteRune(c)
		case c == ',' && !inQ:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(c)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// header returns the Authorization value for the next request, or "" if no
// challenge has been seen yet.
func (d *digest) header(method, uri string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nonce == "" {
		return ""
	}
	ha1 := md5hex(d.user + ":" + d.realm + ":" + d.pass)
	ha2 := md5hex(method + ":" + uri)
	var resp, extra string
	if d.qop == "auth" {
		d.nc++
		var cb [8]byte
		_, _ = rand.Read(cb[:])
		cnonce := hex.EncodeToString(cb[:])
		nc := fmt.Sprintf("%08x", d.nc)
		resp = md5hex(ha1 + ":" + d.nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
		extra = fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
	} else {
		resp = md5hex(ha1 + ":" + d.nonce + ":" + ha2)
	}
	h := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5, response="%s"%s`,
		d.user, d.realm, d.nonce, uri, resp, extra)
	if d.opaque != "" {
		h += fmt.Sprintf(`, opaque="%s"`, d.opaque)
	}
	return h
}
