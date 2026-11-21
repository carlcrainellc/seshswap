package wallet

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v6OnlyDNS answers like Rusty's Session Router: a plain .sesh name gets only
// an AAAA (here ::1), ipv4.<name> gets an A (127.0.0.2).
func v6OnlyDNS(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			i := 12
			var labels []string
			for i < n && q[i] != 0 {
				l := int(q[i])
				labels = append(labels, string(q[i+1:i+1+l]))
				i += 1 + l
			}
			i++
			qtype := int(q[i])<<8 | int(q[i+1])
			name := strings.ToLower(strings.Join(labels, "."))
			resp := append([]byte{}, q[:i+4]...)
			resp[2], resp[3] = 0x81, 0x80
			resp[6], resp[7], resp[8], resp[9], resp[10], resp[11] = 0, 0, 0, 0, 0, 0
			v4 := strings.HasPrefix(name, "ipv4.")
			switch {
			case qtype == 28 && !v4:
				resp[7] = 1
				resp = append(resp, 0xc0, 0x0c, 0, 28, 0, 1, 0, 0, 0, 30, 0, 16)
				resp = append(resp, net.IPv6loopback...)
			case qtype == 1 && v4:
				resp[7] = 1
				resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 30, 0, 4, 127, 0, 0, 2)
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestExistingFallsBackToIPv4WhenTunHasNoIPv6(t *testing.T) {
	dns := v6OnlyDNS(t)
	var dialed []string
	e := &existingRouter{configured: dns, resolvConf: filepath.Join(t.TempDir(), "none"), extra: []string{},
		dial: func(_ context.Context, a string) error {
			dialed = append(dialed, a)
			if strings.HasPrefix(a, "[::1]") {
				return errors.New("connect: no route to host") // Rusty's TUN lost fd2e::1
			}
			return nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, err := e.lookup(ctx, testSesh, "80", true)
	if err != nil || addr != "127.0.0.2:80" {
		t.Fatalf("lookup = %q, %v (dialed %v)", addr, err, dialed)
	}
	// The cached path keeps using the IPv4 mapping.
	if addr, err = e.lookup(ctx, testSesh, "80", false); err != nil || addr != "127.0.0.2:80" {
		t.Fatalf("cached lookup = %q, %v", addr, err)
	}
}

func TestExistingNoRouteErrorAndReset(t *testing.T) {
	dns := v6OnlyDNS(t)
	e := &existingRouter{configured: dns, resolvConf: filepath.Join(t.TempDir(), "none"), extra: []string{},
		dial: func(context.Context, string) error { return errors.New("no route") }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := e.lookup(ctx, testSesh, "80", true)
	if err == nil || !strings.Contains(err.Error(), errNoRoute.Error()) {
		t.Fatalf("want no-route error, got %v", err)
	}
	if _, err := e.lookup(ctx, testSesh, "80", false); err == nil || !strings.Contains(err.Error(), "recently") {
		t.Fatalf("background lookup should trust the recent failure, got %v", err)
	}
	e.reset() // routes changed (exit unmapped)
	e.dial = func(context.Context, string) error { return nil }
	if _, err := e.lookup(ctx, testSesh, "80", false); err != nil {
		t.Fatalf("after reset a background lookup should probe again: %v", err)
	}
}

func TestV4Name(t *testing.T) {
	for in, want := range map[string]string{"abc.sesh": "ipv4.abc.sesh", "ipv4.abc.sesh": "", "example.com": "", "x.snode": "ipv4.x.snode"} {
		if got := v4Name(in); got != want {
			t.Errorf("v4Name(%q) = %q, want %q", in, got, want)
		}
	}
}
