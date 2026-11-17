package wallet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
)

// existingRouter finds and uses a Session Router that is already running on
// this computer: a system service (for example session-router-client) or
// one the user started. The wallet only talks to its DNS and dials the
// address it hands out. It never starts, stops, restarts or reconfigures it.
//
// Where it looks, in order:
//  1. seshDns from the config, if set (the user told us where theirs is);
//  2. the system resolver, but only the nameservers in resolv.conf that are
//     on this computer (127.x / ::1, e.g. systemd-resolved's 127.0.0.53 with
//     a .sesh routing domain). A nameserver on another machine (an ISP or
//     LAN resolver) is skipped, so a .sesh name is never sent off this
//     computer in the clear;
//  3. 127.0.0.1:53, Session Router's documented default DNS.
//
// A candidate counts as working only if it resolves the maker's .sesh name
// AND a TCP connection to the address it returns opens (that connection runs
// through the router to the maker).
type existingRouter struct {
	configured string                                       // seshDns, "" = none
	resolvConf string                                       // path; "" = /etc/resolv.conf
	extra      []string                                     // tests: candidates to try instead of 127.0.0.1:53
	dial       func(ctx context.Context, addr string) error // tests may replace
	retryAfter time.Duration                                // how long a failed probe is trusted; 0 = 60s

	mu        sync.Mutex
	dns       string // the working candidate's DNS address
	kind      string // "configured", "system" or "default"
	okAt      time.Time
	failAt    time.Time
	lastErr   string
	connected bool
}

type candidate struct{ dns, kind string }

// ExistingStatus is a snapshot for the UI.
type ExistingStatus struct {
	DNS       string `json:"dns,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Connected bool   `json:"connected"`
	Err       string `json:"error,omitempty"`
}

func newExistingRouter(configured string) *existingRouter {
	return &existingRouter{configured: strings.TrimSpace(configured)}
}

func (e *existingRouter) status() ExistingStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return ExistingStatus{DNS: e.dns, Kind: e.kind, Connected: e.connected, Err: e.lastErr}
}

// markResult records how a request through the existing router went.
func (e *existingRouter) markResult(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.connected = true
		e.lastErr = ""
		return
	}
	// A failure through it: re-check from scratch next time (auto mode may
	// then fall back to the bundled router).
	e.connected = false
	e.dns, e.kind = "", ""
	e.failAt = time.Now()
	e.lastErr = err.Error()
}

func (e *existingRouter) candidates() []candidate {
	var out []candidate
	seen := map[string]bool{}
	add := func(dns, kind string) {
		if dns == "" || seen[dns] {
			return
		}
		seen[dns] = true
		out = append(out, candidate{dns, kind})
	}
	if e.configured != "" {
		add(e.configured, "configured")
	}
	for _, ns := range loopbackNameservers(e.resolvConf) {
		add(ns, "system")
	}
	if e.extra != nil {
		for _, d := range e.extra {
			add(d, "default")
		}
	} else {
		add(sesh.DefaultDNS, "default")
	}
	return out
}

// lookup returns a dial address for host:port through an already-running
// router. force=false honours a recent failed probe (auto mode should not
// wait on a missing router before every request); force=true always probes.
func (e *existingRouter) lookup(ctx context.Context, host, port string, force bool) (string, error) {
	e.mu.Lock()
	dns := e.dns
	retry := e.retryAfter
	if retry == 0 {
		retry = 60 * time.Second
	}
	recentFail := !e.failAt.IsZero() && time.Since(e.failAt) < retry
	e.mu.Unlock()

	if dns != "" {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addr, err := sesh.ResolveVia(lctx, dns, host, port)
		cancel()
		if err == nil {
			return addr, nil
		}
		e.markResult(err)
	} else if recentFail && !force {
		return "", errors.New("no working Session Router found recently")
	}

	var errs []string
	for _, c := range e.candidates() {
		addr, err := e.probe(ctx, c.dns, host, port)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s (%s): %v", c.dns, c.kind, err))
			continue
		}
		e.mu.Lock()
		e.dns, e.kind, e.okAt, e.lastErr = c.dns, c.kind, time.Now(), ""
		e.mu.Unlock()
		return addr, nil
	}
	err := errors.New("no running Session Router reached the maker: " + strings.Join(errs, "; "))
	e.mu.Lock()
	e.failAt = time.Now()
	e.lastErr = err.Error()
	e.connected = false
	e.mu.Unlock()
	return "", err
}

// probe resolves host via dns and opens (then closes) a TCP connection to
// the result, so "working" means the router actually reaches the maker.
func (e *existingRouter) probe(ctx context.Context, dns, host, port string) (string, error) {
	lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	addr, err := sesh.ResolveVia(lctx, dns, host, port)
	cancel()
	if err != nil {
		return "", errors.New("doesn't resolve .sesh names")
	}
	dctx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	dial := e.dial
	if dial == nil {
		dial = func(ctx context.Context, a string) error {
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", a)
			if err == nil {
				c.Close()
			}
			return err
		}
	}
	if err := dial(dctx, addr); err != nil {
		return "", errors.New("resolves the name but can't connect to the maker")
	}
	return addr, nil
}

// loopbackNameservers returns host:53 for every resolv.conf nameserver that
// is on this computer. Others are left out on purpose (see existingRouter).
func loopbackNameservers(path string) []string {
	if path == "" {
		path = "/etc/resolv.conf"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(strings.SplitN(fields[1], "%", 2)[0])
		if ip == nil || !ip.IsLoopback() {
			continue
		}
		out = append(out, net.JoinHostPort(ip.String(), "53"))
	}
	return out
}
