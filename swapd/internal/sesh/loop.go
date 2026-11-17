package sesh

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Loop is an in-process stand-in for Session Router. It maps a .sesh name to
// a localhost listener and returns a Client that dials through that map. It
// does not hide IPs; it is only for tests.
type Loop struct {
	mu      sync.Mutex
	names   map[string]string // "foo.sesh" -> "127.0.0.1:port"
	servers []*http.Server
	lns     []net.Listener
}

// NewLoop starts an empty name map.
func NewLoop() *Loop {
	return &Loop{names: map[string]string{}}
}

// Publish registers name (e.g. "maker.sesh") as a mapping to ln's address.
func (l *Loop) Publish(name string, ln net.Listener) error {
	if !IsSesh(name) {
		return fmt.Errorf("sesh: not a .sesh name: %q", name)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names[name] = ln.Addr().String()
	l.lns = append(l.lns, ln)
	return nil
}

// Serve publishes name and serves h on a new 127.0.0.1 listener. The returned
// URL is http://<name> (port 80 implied; the client remaps the port).
func (l *Loop) Serve(name string, h http.Handler) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	if err := l.Publish(name, ln); err != nil {
		_ = ln.Close()
		return "", err
	}
	s := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	l.mu.Lock()
	l.servers = append(l.servers, s)
	l.mu.Unlock()
	go func() { _ = s.Serve(ln) }()
	return "http://" + name, nil
}

// Client returns an HTTP client that dials this Loop's names and refuses any
// other .sesh name (no system DNS, no DefaultDNS).
func (l *Loop) Client() *http.Client {
	t := New(Config{Lookup: l.lookup})
	return t.Client(15 * time.Second)
}

func (l *Loop) lookup(_ context.Context, host, _ string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	addr, ok := l.names[host]
	if !ok {
		return "", fmt.Errorf("sesh: loop has no mapping for %s", host)
	}
	return addr, nil
}

// Close shuts down every server and listener.
func (l *Loop) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.servers {
		_ = s.Close()
	}
	for _, ln := range l.lns {
		_ = ln.Close()
	}
	l.servers, l.lns, l.names = nil, nil, map[string]string{}
}

// Transport is a Transport whose Lookup is this Loop (for callers that want
// the Config form).
func (l *Loop) Transport() *Transport {
	return New(Config{Lookup: l.lookup})
}
