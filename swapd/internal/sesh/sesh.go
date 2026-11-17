// Package sesh is the Session Router transport for swapd: an HTTP client that
// dials http://<name>.sesh/... only through Session Router DNS (never the
// system resolver), so maker/taker/relayer comms hide both IPs.
//
// Session Router itself is not started by this package. The operator runs a
// router instance and points Config.DNS at its DNS (default 127.0.0.1:53).
// For tests, Loop maps .sesh names to localhost listeners.
package sesh

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

const DefaultDNS = "127.0.0.1:53"

// Config is how a Transport finds Session Router.
type Config struct {
	// DNS is the Session Router DNS address (host:port). Empty means DefaultDNS.
	// Used only for .sesh hosts, and only when Lookup is nil.
	DNS string
	// Lookup, if set, turns a .sesh host and URL port into a dial address
	// (host:port). Tests use this (Loop). Production leaves it nil.
	Lookup func(ctx context.Context, host, port string) (string, error)
	// Dialer is used to open the TCP connection after resolution. Nil means a
	// default net.Dialer with a 30s timeout.
	Dialer *net.Dialer
}

// Transport builds HTTP clients that route .sesh hosts through Session Router.
type Transport struct {
	cfg Config
}

// New returns a Transport. An empty Config uses DefaultDNS for .sesh names.
func New(cfg Config) *Transport {
	if cfg.DNS == "" {
		cfg.DNS = DefaultDNS
	}
	return &Transport{cfg: cfg}
}

// IsSesh reports whether host is a Session Router name (a single label plus
// ".sesh", trailing-dot allowed, case-insensitive).
func IsSesh(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if !strings.HasSuffix(h, ".sesh") {
		return false
	}
	name := strings.TrimSuffix(h, ".sesh")
	return name != "" && !strings.Contains(name, ".")
}

// Client returns an HTTP client that dials .sesh hosts through this Transport
// and every other host with a normal dial. The request Host stays the .sesh
// name. timeout is the overall client timeout (0 leaves it unset).
func (t *Transport) Client(timeout time.Duration) *http.Client {
	d := t.cfg.Dialer
	if d == nil {
		d = &net.Dialer{Timeout: 30 * time.Second}
	}
	tr := &http.Transport{
		Proxy:                 nil, // never a clearnet proxy for .sesh
		DialContext:           t.dialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     false,
	}
	return &http.Client{Transport: tr, Timeout: timeout, Jar: nil}
}

func (t *Transport) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if IsSesh(host) {
		dial, err := t.resolveSesh(ctx, host, port)
		if err != nil {
			return nil, err
		}
		addr = dial
	}
	d := t.cfg.Dialer
	if d == nil {
		d = &net.Dialer{Timeout: 30 * time.Second}
	}
	return d.DialContext(ctx, network, addr)
}

// resolveSesh returns a host:port to dial. It never uses net.DefaultResolver.
func (t *Transport) resolveSesh(ctx context.Context, host, port string) (string, error) {
	if t.cfg.Lookup != nil {
		return t.cfg.Lookup(ctx, host, port)
	}
	return ResolveVia(ctx, t.cfg.DNS, host, port)
}

// ResolveVia resolves a .sesh host through the Session Router DNS at dns
// (host:port) and returns host:port to dial. It never uses the system
// resolver.
func ResolveVia(ctx context.Context, dns, host, port string) (string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			// Always the configured Session Router DNS, never the address Go
			// would have used (system resolv.conf).
			n := network
			if n != "udp" && n != "udp4" && n != "udp6" {
				n = "udp"
			}
			return d.DialContext(ctx, n, dns)
		},
	}
	ips, err := r.LookupHost(ctx, host)
	if err != nil {
		return "", fmt.Errorf("sesh: resolve %s via %s: %w", host, dns, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("sesh: resolve %s via %s: no addresses", host, dns)
	}
	return net.JoinHostPort(ips[0], port), nil
}

// Default is the process-wide transport (Session Router DNS at DefaultDNS).
var Default = New(Config{})

// DefaultClient returns a 60s client on Default. Used by api and relay when
// the caller does not pass an HTTP client.
func DefaultClient() *http.Client {
	return Default.Client(60 * time.Second)
}
