// Package sysdns points this computer's DNS at an exit's resolver while a
// full-tunnel exit is connected, and puts it back afterwards, including after
// a crash (the change is recorded before it is made).
//
// Two ways, picked per computer:
//
//   - systemd-resolved (the resolv.conf stub at 127.0.0.53): the resolver is
//     set on the tunnel link with routing domain "~.", so every lookup goes
//     there. resolvectl revert undoes it, and resolved drops the setting by
//     itself when the tunnel goes away.
//   - otherwise /etc/resolv.conf is rewritten, after saving the original (or
//     the symlink it was). Restore puts the original back, unless something
//     else has rewritten the file since.
//
// Changing system DNS needs root. The wallet runs this through its own
// binary under `sudo -n` (see Helper); the state lives in Dir, root-only.
package sysdns

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// State is one change, saved before it is made.
type State struct {
	Method  string    `json:"method"` // "resolved" or "file"
	Link    string    `json:"link,omitempty"`
	Server  string    `json:"server"`
	Path    string    `json:"path,omitempty"`    // file: the resolv.conf that was changed
	WasLink string    `json:"wasLink,omitempty"` // file: it was a symlink to this
	Backup  string    `json:"backup,omitempty"`  // file: saved original content
	Missing bool      `json:"missing,omitempty"` // file: there was no resolv.conf
	Written string    `json:"written,omitempty"` // file: sha256 of what we wrote
	At      time.Time `json:"at"`
}

// Switcher does the work. The zero value uses the real system.
type Switcher struct {
	Dir        string                                  // state (default /var/lib/swap-wallet-dns)
	ResolvConf string                                  // default /etc/resolv.conf
	Run        func(name string, args ...string) error // default: exec
	Resolved   func(resolvConf string) bool            // default: stub resolv.conf, same netns as PID 1, resolvectl present
}

var ifnameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

func (w *Switcher) dir() string {
	if w.Dir != "" {
		return w.Dir
	}
	return "/var/lib/swap-wallet-dns"
}

func (w *Switcher) resolvConf() string {
	if w.ResolvConf != "" {
		return w.ResolvConf
	}
	return "/etc/resolv.conf"
}

func (w *Switcher) run(name string, args ...string) error {
	if w.Run != nil {
		return w.Run(name, args...)
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (w *Switcher) statePath(uid int) string {
	return filepath.Join(w.dir(), fmt.Sprintf("state-%d.json", uid))
}

// UsesResolved reports whether DNS here goes through systemd-resolved that
// this process can address: the stub resolver in resolv.conf, resolvectl
// installed, and the same network namespace as PID 1 (resolved only knows the
// host's links).
func UsesResolved(resolvConf string) bool {
	b, err := os.ReadFile(resolvConf)
	if err != nil || !strings.Contains(string(b), "127.0.0.53") {
		return false
	}
	if _, err := exec.LookPath("resolvectl"); err != nil {
		return false
	}
	self, err1 := os.Readlink("/proc/self/ns/net")
	init, err2 := os.Readlink("/proc/1/ns/net")
	return err1 == nil && err2 == nil && self == init
}

// Status returns the saved change for uid, or nil.
func (w *Switcher) Status(uid int) (*State, error) {
	b, err := os.ReadFile(w.statePath(uid))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (w *Switcher) save(uid int, st *State) error {
	if err := os.MkdirAll(w.dir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := w.statePath(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.statePath(uid))
}

// Set points DNS at server for uid's exit on link. Anything left from an
// earlier Set is restored first.
func (w *Switcher) Set(uid int, link, server string) (*State, error) {
	ip := net.ParseIP(server)
	if ip == nil {
		return nil, fmt.Errorf("resolver %q is not an IP address", server)
	}
	server = ip.String()
	if !ifnameRE.MatchString(link) {
		return nil, fmt.Errorf("bad interface name %q", link)
	}
	if _, err := w.Restore(uid); err != nil {
		return nil, fmt.Errorf("undoing the last change first: %w", err)
	}
	resolved := UsesResolved
	if w.Resolved != nil {
		resolved = w.Resolved
	}
	st := &State{Server: server, Link: link, At: time.Now().UTC()}
	if resolved(w.resolvConf()) {
		st.Method = "resolved"
		if err := w.save(uid, st); err != nil {
			return nil, err
		}
		for _, args := range [][]string{{"dns", link, server}, {"domain", link, "~."}, {"default-route", link, "yes"}} {
			if err := w.run("resolvectl", args...); err != nil {
				_ = w.run("resolvectl", "revert", link)
				_ = os.Remove(w.statePath(uid))
				return nil, err
			}
		}
		_ = w.run("resolvectl", "flush-caches")
		return st, nil
	}
	return st, w.setFile(uid, st)
}

func (w *Switcher) setFile(uid int, st *State) error {
	path := w.resolvConf()
	st.Method, st.Path = "file", path
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.Missing = true
	case err != nil:
		return err
	case fi.Mode()&os.ModeSymlink != 0:
		if st.WasLink, err = os.Readlink(path); err != nil {
			return err
		}
	default:
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		st.Backup = string(b)
	}
	body := "# Set by the swap wallet while an exit is connected; put back on disconnect.\n" +
		"nameserver " + st.Server + "\noptions edns0\n"
	sum := sha256.Sum256([]byte(body))
	st.Written = hex.EncodeToString(sum[:])
	// Saved first: a crash between here and the write still gets restored.
	if err := w.save(uid, st); err != nil {
		return err
	}
	if err := writeResolv(path, body, st.WasLink != ""); err != nil {
		_ = os.Remove(w.statePath(uid))
		return err
	}
	return nil
}

// writeResolv replaces a symlink with a file, but writes a regular file in
// place: /etc/resolv.conf may be a bind mount (network namespaces, containers)
// that a rename can't replace.
func writeResolv(path, body string, replaceLink bool) error {
	if replaceLink {
		tmp := path + ".swapwallet.tmp"
		if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// Restore undoes uid's change, if there is one. It reports whether anything
// was put back.
func (w *Switcher) Restore(uid int) (bool, error) {
	st, err := w.Status(uid)
	if err != nil || st == nil {
		return false, err
	}
	switch st.Method {
	case "resolved":
		// The link may be gone already (router stopped); resolved forgets
		// its settings then, so a failed revert is fine.
		if _, err := net.InterfaceByName(st.Link); err == nil {
			if err := w.run("resolvectl", "revert", st.Link); err != nil {
				return false, err
			}
			_ = w.run("resolvectl", "flush-caches")
		}
	case "file":
		cur, err := os.ReadFile(st.Path)
		sum := sha256.Sum256(cur)
		if err == nil && hex.EncodeToString(sum[:]) != st.Written {
			// Rewritten by someone else (DHCP, NetworkManager): theirs wins.
			_ = os.Remove(w.statePath(uid))
			return false, nil
		}
		if st.WasLink != "" {
			tmp := st.Path + ".swapwallet.tmp"
			_ = os.Remove(tmp)
			if err := os.Symlink(st.WasLink, tmp); err != nil {
				return false, err
			}
			if err := os.Rename(tmp, st.Path); err != nil {
				return false, err
			}
		} else if st.Missing {
			if err := os.Remove(st.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		} else if err := writeResolv(st.Path, st.Backup, false); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("unknown method %q in saved DNS state", st.Method)
	}
	return true, os.Remove(w.statePath(uid))
}
