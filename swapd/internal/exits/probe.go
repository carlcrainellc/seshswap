package exits

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// WebProbe checks the path a buyer's web traffic takes through the exit. It
// looks Host up with the exit's resolver, has the probe router send that
// address (and the resolver) through the exit, then completes a TLS
// handshake with Host on port 443.
type WebProbe struct {
	DNS     string        // the exit's resolver, e.g. 10.195.0.53
	Host    string        // a site that answers on 443
	Timeout time.Duration // per step
	// Dial opens a connection from the probe's network namespace.
	Dial func(network, addr string, timeout time.Duration) (net.Conn, error)
	// Map sends these ranges (and only these) through the exit.
	Map func(ctx context.Context, ranges []string) error
	// Roots verifies the site's certificate; nil = the system's roots.
	Roots *x509.CertPool

	mapped string // the site address currently mapped, besides the resolver
}

func (p *WebProbe) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 10 * time.Second
}

// Remap maps the exit again on the probe router (resolver only; the site's
// address is added on the next check).
func (p *WebProbe) Remap(ctx context.Context) error {
	p.mapped = ""
	return p.Map(ctx, []string{p.DNS + "/32"})
}

// Check is one probe; nil means a buyer's web traffic gets through.
func (p *WebProbe) Check(ctx context.Context) error {
	t := p.timeout()
	server := net.JoinHostPort(p.DNS, "53")
	ip, err := LookupA(ctx, p.Host, server, t, func() (net.Conn, error) { return p.Dial("udp", server, t) })
	if err != nil {
		return fmt.Errorf("lookup of %s with the exit's resolver: %w", p.Host, err)
	}
	if ip != p.mapped {
		if err := p.Map(ctx, []string{p.DNS + "/32", ip + "/32"}); err != nil {
			return fmt.Errorf("mapping %s through the exit: %w", ip, err)
		}
		p.mapped = ip
		time.Sleep(time.Second) // the router installs the route right after
	}
	c, err := p.Dial("tcp", net.JoinHostPort(ip, "443"), t)
	if err != nil {
		return fmt.Errorf("connect to %s (%s) port 443 through the exit: %w", p.Host, ip, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(t))
	tc := tls.Client(c, &tls.Config{ServerName: p.Host, MinVersion: tls.VersionTLS12, RootCAs: p.Roots})
	if err := tc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS with %s (%s) through the exit: %w", p.Host, ip, err)
	}
	return nil
}

// LookupA asks server for host's IPv4 address. Like any DNS client it sends
// again every couple of seconds: the first packet after a quiet spell can be
// lost while the session wakes up. dial nil = a plain UDP socket.
func LookupA(ctx context.Context, host, server string, timeout time.Duration, dial func() (net.Conn, error)) (string, error) {
	if dial == nil {
		dial = func() (net.Conn, error) { return net.Dial("udp", server) }
	}
	c, err := dial()
	if err != nil {
		return "", err
	}
	defer c.Close()
	dl := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	q, id, err := dnsQuery(host)
	if err != nil {
		return "", err
	}
	buf := make([]byte, 1500)
	for sent := 1; ; sent++ {
		if _, err := c.Write(q); err != nil {
			return "", err
		}
		next := time.Now().Add(2 * time.Second)
		if next.After(dl) {
			next = dl
		}
		_ = c.SetReadDeadline(next)
		for {
			n, err := c.Read(buf)
			if err != nil {
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					return "", err
				}
				if !time.Now().Before(dl) {
					return "", fmt.Errorf("no answer from %s within %s (%d tries)", server, timeout, sent)
				}
				break // send again
			}
			if n < 12 || binary.BigEndian.Uint16(buf) != id || buf[2]&0x80 == 0 {
				continue
			}
			return firstA(buf[:n])
		}
	}
}

func dnsQuery(host string) ([]byte, uint16, error) {
	var r [2]byte
	_, _ = rand.Read(r[:])
	id := binary.BigEndian.Uint16(r[:])
	q := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(q, id)
	q[2] = 0x01 // recursion desired
	q[5] = 1    // one question
	for _, l := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if l == "" || len(l) > 63 {
			return nil, 0, fmt.Errorf("bad host name %q", host)
		}
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, 0, 1, 0, 1) // A, IN
	return q, id, nil
}

// firstA returns the first A record in a DNS response.
func firstA(m []byte) (string, error) {
	if rc := m[3] & 0x0f; rc != 0 {
		return "", fmt.Errorf("resolver answered with rcode %d", rc)
	}
	qd, an := int(binary.BigEndian.Uint16(m[4:])), int(binary.BigEndian.Uint16(m[6:]))
	i := 12
	skipName := func() bool {
		for i < len(m) {
			l := int(m[i])
			switch {
			case l == 0:
				i++
				return true
			case l&0xc0 == 0xc0:
				i += 2
				return true
			default:
				i += 1 + l
			}
		}
		return false
	}
	for ; qd > 0; qd-- {
		if !skipName() {
			return "", errors.New("short DNS response")
		}
		i += 4
	}
	for ; an > 0; an-- {
		if !skipName() || i+10 > len(m) {
			return "", errors.New("short DNS response")
		}
		typ, rdlen := binary.BigEndian.Uint16(m[i:]), int(binary.BigEndian.Uint16(m[i+8:]))
		i += 10
		if i+rdlen > len(m) {
			return "", errors.New("short DNS response")
		}
		if typ == 1 && rdlen == 4 {
			return net.IP(m[i : i+4]).String(), nil
		}
		i += rdlen
	}
	return "", errors.New("no IPv4 address in the answer")
}
