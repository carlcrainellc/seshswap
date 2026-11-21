package wallet

// System DNS while an exit is connected (default route on the tunnel): the wallet points this
// computer's DNS at the exit's own resolver (from its listing), so lookups go
// through the tunnel too, and puts it back on disconnect. The change is made
// by the wallet binary's DNS helper (internal/sysdns) under sudo -n. A marker
// file in the data dir says a change may be in place, so a wallet that was
// killed puts DNS back at its next start.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/sysdns"
)

type dnsChanger interface {
	Set(link, server string) (string, error)
	Restore() (bool, error)
}

func (s *Server) dnsClient() dnsChanger {
	if s.ex.dns != nil {
		return s.ex.dns
	}
	return &sysdns.Client{}
}

// exitDNSOn says whether the wallet switches system DNS itself while an exit
// is connected. "off" leaves DNS alone. "router" means the router does it:
// Rusty's Session Router service on Windows points this computer's DNS at
// 127.0.0.1:53 and answers .sesh names and exit lookups itself, so the wallet
// must not touch the adapters.
func (s *Server) exitDNSOn() bool {
	return !strings.EqualFold(s.Cfg.ExitDNS, "off") && !s.exitDNSByRouter()
}

func (s *Server) exitDNSByRouter() bool { return strings.EqualFold(s.Cfg.ExitDNS, "router") } // the page says so

func (s *Server) dnsMarker() string { return filepath.Join(s.Cfg.DataDir, "exit-dns.json") }

// tunnelLink is the interface the kernel uses for ip, if that is a direct
// (tunnel) route rather than one through a gateway on the local network.
func tunnelLink(ip string) (string, error) {
	out, err := exec.Command("ip", "-o", "route", "get", ip).Output()
	if err != nil {
		return "", fmt.Errorf("ip route get %s: %v", ip, err)
	}
	f := strings.Fields(string(out))
	dev := ""
	for i := 0; i+1 < len(f); i++ {
		switch f[i] {
		case "via":
			return "", fmt.Errorf("%s goes through gateway %s, not the tunnel", ip, f[i+1])
		case "dev":
			dev = f[i+1]
		}
	}
	if dev == "" || dev == "lo" {
		return "", fmt.Errorf("no tunnel route to %s", ip)
	}
	return dev, nil
}

func (s *Server) setDNSState(c *exitConn, state string) {
	s.ex.mu.Lock()
	if s.ex.Conn == c {
		c.DNSState = state
	}
	s.ex.mu.Unlock()
}

// switchDNS waits for the tunnel's routes (the router installs them a few
// seconds after the exit is mapped), then points system DNS at the exit's
// resolver. Runs in the background after connect.
func (s *Server) switchDNS(c *exitConn) {
	if c.DNS == "" || !s.exitDNSOn() || net.ParseIP(c.DNS) == nil {
		return
	}
	s.setDNSState(c, "switching")
	linkFor := tunnelLink
	if s.ex.linkFor != nil {
		linkFor = s.ex.linkFor
	}
	var link string
	var err error
	deadline := time.Now().Add(60 * time.Second)
	for {
		if link, err = linkFor(c.DNS); err == nil {
			break
		}
		s.ex.mu.Lock()
		gone := s.ex.Conn != c
		s.ex.mu.Unlock()
		if gone || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	s.ex.mu.Lock()
	gone := s.ex.Conn != c
	s.ex.mu.Unlock()
	if gone {
		return
	}
	if err != nil {
		s.setDNSState(c, "failed: "+err.Error())
		s.logf("exit DNS not switched: %v", err)
		return
	}
	// The marker goes first: if the wallet dies after the change, the next
	// start puts DNS back.
	b, _ := json.Marshal(map[string]any{"server": c.DNS, "link": link, "at": time.Now().Unix()})
	_ = os.WriteFile(s.dnsMarker(), b, 0o600)
	method, err := s.dnsClient().Set(link, c.DNS)
	if err != nil {
		_ = os.Remove(s.dnsMarker())
		s.setDNSState(c, "failed: "+err.Error())
		s.logf("exit DNS not switched: %v", err)
		return
	}
	s.ex.mu.Lock()
	still := s.ex.Conn == c
	if still {
		c.DNSState, c.dnsSet = "on", true
	}
	s.ex.mu.Unlock()
	s.logf("system DNS now %s on %s (%s) while the exit is connected", c.DNS, link, method)
	if !still {
		s.restoreDNS("the exit was disconnected meanwhile")
	}
}

// restoreDNS puts system DNS back if the wallet changed it.
func (s *Server) restoreDNS(why string) {
	if _, err := os.Stat(s.dnsMarker()); err != nil {
		return
	}
	done, err := s.dnsClient().Restore()
	if err != nil {
		s.logf("couldn't put system DNS back (%s): %v", why, err)
		return
	}
	_ = os.Remove(s.dnsMarker())
	if done {
		s.logf("system DNS put back (%s)", why)
	}
}

// recoverDNS runs at startup: a marker left behind means the wallet stopped
// without putting DNS back.
// cleanStaleRouter stops a bundled router left running by a wallet that was
// killed together with its guard. Until it stops it keeps the exit
// routes, and nothing (DNS included) gets out.
func (s *Server) cleanStaleRouter() {
	base := os.TempDir()
	if rc := s.Cfg.SessionRouter; rc != nil && rc.TempDir != "" {
		base = rc.TempDir
	}
	srouter.CleanStale(base)
}

func (s *Server) recoverDNS() {
	if _, err := os.Stat(s.dnsMarker()); errors.Is(err, os.ErrNotExist) {
		return
	}
	s.restoreDNS("left over from a wallet that didn't shut down cleanly")
}
