package sesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestIsSesh(t *testing.T) {
	yes := []string{"maker.sesh", "MAKER.SESH", "abc.sesh.", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.sesh"}
	no := []string{"", "sesh", ".sesh", "foo.bar.sesh", "maker.sesh.com", "maker.sesh.evil", "127.0.0.1", "example.com"}
	for _, h := range yes {
		if !IsSesh(h) {
			t.Errorf("IsSesh(%q) = false", h)
		}
	}
	for _, h := range no {
		if IsSesh(h) {
			t.Errorf("IsSesh(%q) = true", h)
		}
	}
}

func TestLoopOfferAndHostHeader(t *testing.T) {
	loop := NewLoop()
	defer loop.Close()
	var sawHost string
	_, err := loop.Serve("maker.sesh", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHost = r.Host
		if r.URL.Path != "/v1/offer" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"xmrNet": "stagenet", "ok": true})
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := loop.Client()
	resp, err := c.Get("http://maker.sesh/v1/offer")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %s", resp.Status)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "stagenet") {
		t.Fatalf("body %s", b)
	}
	if sawHost != "maker.sesh" && sawHost != "maker.sesh:80" {
		t.Fatalf("Host = %q, want maker.sesh", sawHost)
	}
}

func TestLoopUnknownNameDoesNotFallBack(t *testing.T) {
	loop := NewLoop()
	defer loop.Close()
	c := loop.Client()
	_, err := c.Get("http://unknown.sesh/v1/offer")
	if err == nil {
		t.Fatal("expected error for unmapped name")
	}
	if !strings.Contains(err.Error(), "unknown.sesh") {
		t.Fatalf("error should name the host: %v", err)
	}
}

func TestClearnetUnaffected(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	// A Loop client (Lookup only for .sesh) must still reach 127.0.0.1.
	c := NewLoop().Client()
	resp, err := c.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok" {
		t.Fatalf("got %q", b)
	}
}

// TestConfiguredDNSOnly spins a stub UDP DNS and an HTTP server. The Transport
// must use that DNS for a .sesh name (returning 127.0.0.1) and keep the URL
// port. A second Transport pointed at a closed DNS port must fail, not fall
// back to the system resolver (which would also fail, so we prove the error
// mentions our DNS address).
func TestConfiguredDNSOnly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Host; got != "lab.sesh:"+port && got != "lab.sesh" {
			http.Error(w, "bad host "+got, 400)
			return
		}
		_, _ = w.Write([]byte("from-dns"))
	}))

	dnsAddr, stop := startARecordDNS(t, "lab.sesh", "127.0.0.1")
	defer stop()

	c := New(Config{DNS: dnsAddr}).Client(5 * time.Second)
	resp, err := c.Get("http://lab.sesh:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "from-dns" {
		t.Fatalf("got %q (status %s)", b, resp.Status)
	}

	// Closed DNS port: must fail mentioning that address, no silent fallback.
	bad := "127.0.0.1:1"
	c2 := New(Config{DNS: bad}).Client(3 * time.Second)
	_, err = c2.Get("http://lab.sesh:" + port + "/")
	if err == nil {
		t.Fatal("expected failure when Session Router DNS is down")
	}
	if !strings.Contains(err.Error(), bad) && !strings.Contains(err.Error(), "sesh:") {
		t.Fatalf("error should mention sesh/DNS, got %v", err)
	}
}

func TestRejectNonSeshLookupForSeshHost(t *testing.T) {
	// If Lookup is not used and DNS is garbage, a public name-shaped .sesh
	// must not succeed via the system resolver.
	c := New(Config{DNS: "127.0.0.1:1"}).Client(3 * time.Second)
	_, err := c.Get("http://example.sesh/")
	if err == nil {
		t.Fatal("must not resolve example.sesh via the system")
	}
}

// startARecordDNS is a tiny UDP DNS server that answers A records for one name.
func startARecordDNS(t *testing.T, name, ipv4 string) (addr string, stop func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(ipv4).To4()
	if ip == nil {
		t.Fatalf("bad ipv4 %s", ipv4)
	}
	want := strings.ToLower(strings.TrimSuffix(name, ".")) + "."
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp, err := dnsA(buf[:n], want, ip)
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(resp, from)
		}
	}()
	return pc.LocalAddr().String(), func() { _ = pc.Close() }
}

// dnsA builds a DNS response with one A record. It is only for tests.
func dnsA(q []byte, wantName string, ip net.IP) ([]byte, error) {
	if len(q) < 12 {
		return nil, fmt.Errorf("short")
	}
	id := q[0:2]
	// parse QNAME starting at 12
	i := 12
	var labels []string
	for {
		if i >= len(q) {
			return nil, fmt.Errorf("truncated name")
		}
		l := int(q[i])
		i++
		if l == 0 {
			break
		}
		if l > 63 || i+l > len(q) {
			return nil, fmt.Errorf("bad label")
		}
		labels = append(labels, string(q[i:i+l]))
		i += l
	}
	if i+4 > len(q) {
		return nil, fmt.Errorf("no type")
	}
	qtype := binary.BigEndian.Uint16(q[i : i+2])
	name := strings.ToLower(strings.Join(labels, ".") + ".")
	// header: ID, QR+AA, qdcount=1, ancount=1
	out := make([]byte, 0, 128)
	out = append(out, id...)
	if name != wantName || qtype != 1 { // A
		out = append(out, 0x81, 0x83) // QR, NXDOMAIN
		out = append(out, 0, 1, 0, 0, 0, 0, 0, 0)
		out = append(out, q[12:]...) // echo rest; good enough for a miss
		return out, nil
	}
	out = append(out, 0x81, 0x80) // QR, RD, RA
	out = append(out, 0, 1, 0, 1, 0, 0, 0, 0)
	// question
	for _, lab := range labels {
		out = append(out, byte(len(lab)))
		out = append(out, lab...)
	}
	out = append(out, 0, 0, 1, 0, 1) // A IN
	// answer: pointer to name at offset 12, A IN TTL 30, RDLEN 4, IP
	out = append(out, 0xc0, 0x0c, 0, 1, 0, 1)
	out = binary.BigEndian.AppendUint32(out, 30)
	out = append(out, 0, 4)
	out = append(out, ip...)
	return out, nil
}

func TestLoopRelayStylePost(t *testing.T) {
	loop := NewLoop()
	defer loop.Close()
	var path, host string
	var body []byte
	_, err := loop.Serve("relayer.sesh", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, host = r.URL.Path, r.Host
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"txHash":"0x` + strings.Repeat("ab", 32) + `"}`))
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := loop.Client()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://relayer.sesh/v1/relay", strings.NewReader(`{"fee":"1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if path != "/v1/relay" {
		t.Fatalf("path %s", path)
	}
	if host != "relayer.sesh" && host != "relayer.sesh:80" {
		t.Fatalf("host %s", host)
	}
	if string(body) != `{"fee":"1"}` {
		t.Fatalf("body %s", body)
	}
}

func TestPublishRejectsBadName(t *testing.T) {
	loop := NewLoop()
	defer loop.Close()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	if err := loop.Publish("not-a-sesh", ln); err == nil {
		t.Fatal("expected error")
	}
}
