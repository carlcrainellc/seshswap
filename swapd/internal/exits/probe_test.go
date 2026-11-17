package exits

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeResolver answers A queries for "example.com" with ip, and drops the
// first query when dropFirst is set.
func fakeResolver(t *testing.T, ip net.IP, dropFirst bool) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		first := true
		for {
			n, a, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if first && dropFirst {
				first = false
				continue
			}
			m := append([]byte{}, buf[:n]...)
			m[2] |= 0x80
			if !strings.Contains(string(m[12:]), "example") {
				m[3] = 3 // NXDOMAIN
				_, _ = pc.WriteTo(m, a)
				continue
			}
			binary.BigEndian.PutUint16(m[6:], 2)
			// a CNAME-ish record to skip, then the A record (names compressed to the question)
			m = append(m, 0xc0, 12, 0, 5, 0, 1, 0, 0, 0, 60, 0, 2, 0xc0, 12)
			m = append(m, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
			m = append(m, ip.To4()...)
			_, _ = pc.WriteTo(m, a)
		}
	}()
	return pc.LocalAddr().String()
}

func TestLookupA(t *testing.T) {
	srv := fakeResolver(t, net.IPv4(203, 0, 113, 7), true)
	ip, err := LookupA(context.Background(), "example.com", srv, 5*time.Second, nil)
	if err != nil || ip != "203.0.113.7" {
		t.Fatalf("%q %v (the first query is dropped; the resend must get it)", ip, err)
	}
	if _, err := LookupA(context.Background(), "nothing.test", srv, 3*time.Second, nil); err == nil || !strings.Contains(err.Error(), "rcode 3") {
		t.Fatalf("want rcode error, got %v", err)
	}
	dead, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer dead.Close()
	if _, err := LookupA(context.Background(), "example.com", dead.LocalAddr().String(), 300*time.Millisecond, nil); err == nil {
		t.Fatal("want timeout")
	}
}

func TestWebProbe(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ts.Close()
	tlsAddr := ts.Listener.Addr().String()
	srv := fakeResolver(t, net.IPv4(203, 0, 113, 7), false)
	var maps [][]string
	p := &WebProbe{
		DNS: "10.195.0.53", Host: "example.com", Timeout: 3 * time.Second,
		Roots: ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
		Dial: func(network, addr string, to time.Duration) (net.Conn, error) {
			switch {
			case network == "udp" && addr == "10.195.0.53:53":
				return net.Dial("udp", srv)
			case network == "tcp" && addr == "203.0.113.7:443":
				return net.Dial("tcp", tlsAddr)
			}
			t.Errorf("unexpected dial %s %s", network, addr)
			return nil, net.ErrClosed
		},
		Map: func(_ context.Context, r []string) error { maps = append(maps, r); return nil },
	}
	if err := p.Remap(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := p.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(maps) != 2 || strings.Join(maps[1], ",") != "10.195.0.53/32,203.0.113.7/32" {
		t.Fatalf("maps %v (the site is mapped once, with the resolver)", maps)
	}
	ts.Close() // the site stops answering: the probe fails
	if err := p.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "through the exit") {
		t.Fatalf("want a failure, got %v", err)
	}
}
