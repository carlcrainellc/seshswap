// Package srouter starts a bundled Session Router as a child process only
// while the wallet needs to reach a .sesh maker, the way Tor Browser starts
// Tor. It is not a background service: each launch gets a fresh temporary
// data dir, its own UDP port, its own DNS on 127.x.y.z:high-port and its own
// network adapter, with auto-routing off and no exit. It never changes the
// system default route or resolv.conf.
package srouter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// State is the router's lifecycle state as shown to the user.
type State string

const (
	Off        State = "off"
	Connecting State = "connecting"
	Connected  State = "connected"
	Failed     State = "error"
)

// DirPrefix names every per-launch temp dir so stale ones can be cleaned.
const DirPrefix = "swap-wallet-sr-"

// Options configures a Router.
type Options struct {
	Binary       string        // path to the session-router binary
	BaseDir      string        // where per-launch temp dirs go; "" = os.TempDir()
	Bootstrap    string        // optional add-node file; "" = the router's built-in seeds
	ReadyTimeout time.Duration // how long to wait for the DNS listener; 0 = 60s
	LogLevel     string        // router log level; "" = info
	// Check is run before every launch. Nil means CheckPermissions.
	Check func(binary string) error
	// Guard, if set, is the command that wraps the router so it stops when
	// the wallet dies, e.g. {"/path/to/wallet", GuardArg}. See RunGuard.
	Guard []string
	// KeyFile, if set, is a persistent client key ([network] keyfile=) so the
	// router keeps the same .sesh address across launches. Paid exit time is
	// bought for that address. "" = a new random address each launch.
	KeyFile string
	// Exits turns on what MapExit needs: the router's RPC on a private ipc
	// socket in the launch dir, and auto-routing (the patched router only
	// installs routes while an exit is mapped). Off = the wallet-sr defaults.
	Exits bool
	// ArgsPrefix and Env are for tests (a fake router); production leaves them empty.
	ArgsPrefix []string
	Env        []string
	Log        func(string, ...any)
}

// Status is a snapshot for the UI.
type Status struct {
	State     State     `json:"state"`
	Message   string    `json:"message,omitempty"`
	PID       int       `json:"pid,omitempty"`
	DNS       string    `json:"dns,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	// StartupMs is launch → first successful reply from a .sesh host
	// (set by MarkConnected). 0 until then.
	StartupMs int64 `json:"startupMs,omitempty"`
	Launches  int   `json:"launches"`
}

// Router manages at most one child Session Router.
type Router struct {
	opt Options

	mu       sync.Mutex
	cmd      *exec.Cmd
	dir      string
	dns      string
	rpc      string
	keyFile  string // KeyFile the running child was launched with
	exited   chan struct{}
	exitErr  error
	starting chan struct{} // non-nil while a launch is in progress
	st       Status
}

// New returns a stopped Router.
func New(opt Options) *Router {
	if opt.ReadyTimeout == 0 {
		opt.ReadyTimeout = 60 * time.Second
	}
	if opt.LogLevel == "" {
		opt.LogLevel = "info"
	}
	if opt.Check == nil {
		opt.Check = CheckPermissions
	}
	if opt.Binary != "" {
		if abs, err := filepath.Abs(opt.Binary); err == nil {
			opt.Binary = abs // cmd.Dir is the temp dir, so never relative
		}
	}
	return &Router{opt: opt, st: Status{State: Off}}
}

func (r *Router) logf(f string, a ...any) {
	if r.opt.Log != nil {
		r.opt.Log(f, a...)
	}
}

// Status returns the current status.
func (r *Router) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}

// Running reports whether a child process is alive (or starting).
func (r *Router) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd != nil || r.starting != nil
}

// DNS returns the child's DNS address, or "" when not running.
func (r *Router) DNS() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dns
}

// RPC returns the child's RPC endpoint (ipc://…), or "" when not running or
// when Options.Exits is off.
func (r *Router) RPC() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rpc
}

// SetKeyFile changes Options.KeyFile for the next launch. A running child
// keeps its address until it is stopped.
func (r *Router) SetKeyFile(path string) {
	r.mu.Lock()
	r.opt.KeyFile = path
	r.mu.Unlock()
}

// LaunchedKeyFile is the key file the running child uses ("" = random or not running).
func (r *Router) LaunchedKeyFile() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keyFile
}

// MarkConnected records the first successful .sesh reply since launch.
func (r *Router) MarkConnected() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd == nil || r.st.State == Connected {
		return
	}
	r.st.State = Connected
	r.st.Message = ""
	if !r.st.StartedAt.IsZero() {
		r.st.StartupMs = time.Since(r.st.StartedAt).Milliseconds()
	}
	r.logf("private connection ready %d ms after launch", r.st.StartupMs)
}

// MarkFailed records a user-facing error without stopping the router.
func (r *Router) MarkFailed(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st.State != Connected {
		r.st.State = Failed
		r.st.Message = msg
	}
}

// Ensure starts the router if it is not running and waits until its DNS
// listener answers. It returns the DNS address (host:port).
func (r *Router) Ensure(ctx context.Context) (string, error) {
	for {
		r.mu.Lock()
		if r.cmd != nil && r.starting == nil {
			select {
			case <-r.exited:
				// died; fall through to relaunch below
				r.cleanupLocked()
			default:
				dns := r.dns
				r.mu.Unlock()
				return dns, nil
			}
		}
		if ch := r.starting; ch != nil {
			r.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		ch := make(chan struct{})
		r.starting = ch
		r.st = Status{State: Connecting, Launches: r.st.Launches + 1}
		r.mu.Unlock()

		dns, err := r.launch(ctx)

		r.mu.Lock()
		r.starting = nil
		close(ch)
		if err != nil {
			r.st.State = Failed
			r.st.Message = err.Error()
			r.mu.Unlock()
			return "", err
		}
		r.mu.Unlock()
		return dns, nil
	}
}

func (r *Router) launch(ctx context.Context) (string, error) {
	if r.opt.Binary == "" {
		return "", &UserError{Msg: "Session Router isn't set up for this wallet. Run scripts/fetch-session-router.sh, or switch to Direct connection in Settings."}
	}
	if _, err := os.Stat(r.opt.Binary); err != nil {
		return "", &UserError{Msg: "Session Router isn't installed next to the wallet (" + r.opt.Binary + " is missing). Run scripts/fetch-session-router.sh, or switch to Direct connection in Settings.", Err: err}
	}
	if err := r.opt.Check(r.opt.Binary); err != nil {
		return "", err
	}
	base := r.opt.BaseDir
	if base == "" {
		base = os.TempDir()
	}
	CleanStale(base)
	dir, err := os.MkdirTemp(base, DirPrefix)
	if err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(dir, "owner.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	ic, err := Pick()
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	ic.DataDir = filepath.Join(dir, "data")
	ic.LogFile = filepath.Join(dir, "router.log")
	ic.Bootstrap = r.opt.Bootstrap
	ic.LogLevel = r.opt.LogLevel
	r.mu.Lock()
	ic.KeyFile = r.opt.KeyFile
	r.mu.Unlock()
	if r.opt.Exits {
		ic.API = "ipc://" + filepath.Join(dir, "rpc.sock")
		ic.AutoRouting = true
	}
	ini := filepath.Join(dir, "session-router.ini")
	if err := os.WriteFile(ini, []byte(ic.Render()), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	args := append(append([]string{}, r.opt.ArgsPrefix...), ini)
	exe := r.opt.Binary
	if len(r.opt.Guard) > 0 {
		args = append(append(append([]string{}, r.opt.Guard[1:]...), exe), args...)
		exe = r.opt.Guard[0]
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), r.opt.Env...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &limitedBuf{b: &out, max: 16 << 10}
	cmd.Stderr = cmd.Stdout
	setSysProcAttr(cmd)
	started := time.Now()
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return "", &UserError{Msg: "Couldn't start Session Router: " + err.Error(), Err: err}
	}
	exited := make(chan struct{})
	r.mu.Lock()
	r.cmd, r.dir, r.dns, r.rpc, r.keyFile, r.exited = cmd, dir, ic.DNS, ic.API, ic.KeyFile, exited
	r.st.PID = cmd.Process.Pid
	r.st.DNS = ic.DNS
	r.st.StartedAt = started
	r.mu.Unlock()
	go func() {
		err := cmd.Wait()
		r.mu.Lock()
		r.exitErr = err
		r.mu.Unlock()
		close(exited)
	}()
	r.logf("started Session Router pid %d (adapter %s, DNS %s, temp data %s)", cmd.Process.Pid, ic.IfName, ic.DNS, dir)

	// Wait for the DNS listener: any reply (even NXDOMAIN) means it is up.
	deadline := time.Now().Add(r.opt.ReadyTimeout)
	for {
		if dnsAlive(ic.DNS, 500*time.Millisecond) {
			return ic.DNS, nil
		}
		select {
		case <-exited:
			msg := explainExit(tailFile(ic.LogFile, 4096) + "\n" + out.String())
			r.Stop()
			return "", &UserError{Msg: msg}
		case <-ctx.Done():
			r.Stop()
			return "", ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			r.Stop()
			return "", &UserError{Msg: "Session Router started but didn't come up in time. Check your internet connection, or switch to Direct connection in Settings."}
		}
	}
}

// Stop terminates the child (SIGTERM, then SIGKILL after 5s) and removes its
// temp dir. Safe to call when stopped.
func (r *Router) Stop() {
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	r.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		terminate(cmd.Process)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}
	r.mu.Lock()
	if r.cmd == cmd {
		r.cleanupLocked()
		r.st.State = Off
		r.st.Message = ""
		r.st.PID = 0
		r.st.DNS = ""
	}
	r.mu.Unlock()
	if cmd != nil {
		r.logf("stopped Session Router pid %d", cmd.Process.Pid)
	}
}

func (r *Router) cleanupLocked() {
	if r.dir != "" {
		_ = os.RemoveAll(r.dir)
	}
	r.cmd, r.dir, r.dns, r.rpc, r.keyFile, r.exited = nil, "", "", "", "", nil
}

// UserError carries a plain-English message for the wallet screen.
type UserError struct {
	Msg string
	Err error
}

func (e *UserError) Error() string { return e.Msg }
func (e *UserError) Unwrap() error { return e.Err }

// IniConfig is one launch's settings.
type IniConfig struct {
	DataDir   string
	LogFile   string
	LogLevel  string
	IfName    string
	IfAddr4   string // e.g. 10.201.0.1/16
	IfAddr6   string // e.g. fd2e:7365:7372:ab12::1/64
	DNS       string // 127.x.y.z:port
	BindPort  int    // UDP
	Bootstrap string
	KeyFile   string // persistent client key; "" = random address
	API       string // RPC bind (ipc://…); "" = RPC off
	// AutoRouting lets the router add routes for a mapped exit. The patched
	// 1.1.1 router does nothing with it until MapExit is called.
	AutoRouting bool
}

// Render returns the session-router.ini text. Client only: not reachable,
// auto-routing only when AutoRouting (exits), blackhole routes off, exit disabled, no resolv.conf.
func (c IniConfig) Render() string {
	var b strings.Builder
	b.WriteString("# Written by the swap wallet for one launch; deleted when the router stops.\n")
	b.WriteString("[router]\n")
	fmt.Fprintf(&b, "data-dir=%s\nnetid=session-router\n\n", c.DataDir)
	b.WriteString("[logging]\n")
	fmt.Fprintf(&b, "level=%s\ntype=file\nfile=%s\n\n", c.LogLevel, c.LogFile)
	b.WriteString("[network]\nreachable=false\n")
	fmt.Fprintf(&b, "ifname=%s\nifaddr=%s\nifaddr=%s\n", c.IfName, c.IfAddr4, c.IfAddr6)
	if c.KeyFile != "" {
		fmt.Fprintf(&b, "keyfile=%s\n", c.KeyFile)
	}
	fmt.Fprintf(&b, "auto-routing=%t\nblackhole-routes=false\n\n", c.AutoRouting)
	b.WriteString("[exit]\nenable=false\n\n")
	fmt.Fprintf(&b, "[dns]\nlisten=%s\nno-resolvconf=1\n\n", c.DNS)
	if c.API != "" {
		fmt.Fprintf(&b, "[api]\nenabled=true\nbind=%s\n\n", c.API)
	} else {
		b.WriteString("[api]\nenabled=false\n\n")
	}
	fmt.Fprintf(&b, "[bind]\nlisten=0.0.0.0:%d\n", c.BindPort)
	if c.Bootstrap != "" {
		fmt.Fprintf(&b, "\n[bootstrap]\nadd-node=%s\n", c.Bootstrap)
	}
	return b.String()
}

// Pick chooses a fresh adapter name, private address ranges that don't clash
// with existing interfaces, a free UDP port, and a free DNS port on a random
// 127.x.y.z.
func Pick() (IniConfig, error) {
	var c IniConfig
	rb := make([]byte, 8)
	if _, err := rand.Read(rb); err != nil {
		return c, err
	}
	c.IfName = fmt.Sprintf("swr%x", rb[:3]) // 9 chars, under IFNAMSIZ
	used := usedNets()
	var v4 *net.IPNet
	for i := 0; i < 64; i++ {
		second := 160 + int(rb[3]+byte(i))%64 // 10.160.0.0 – 10.223.0.0
		_, n, _ := net.ParseCIDR(fmt.Sprintf("10.%d.0.0/16", second))
		if !overlaps(n, used) {
			v4 = n
			break
		}
	}
	if v4 == nil {
		return c, errors.New("no free private address range for Session Router")
	}
	ip := v4.IP.To4()
	c.IfAddr4 = fmt.Sprintf("%d.%d.0.1/16", ip[0], ip[1])
	c.IfAddr6 = fmt.Sprintf("fd2e:7365:7372:%x::1/64", binary.BigEndian.Uint16(rb[4:6])|1)
	dnsIP := fmt.Sprintf("127.%d.%d.%d", 1+int(rb[6])%250, 1+int(rb[7])%250, 2+int(rb[5])%250)
	p, err := freeUDP(dnsIP)
	if err != nil {
		// Some sandboxes only allow 127.0.0.1.
		dnsIP = "127.0.0.1"
		if p, err = freeUDP(dnsIP); err != nil {
			return c, err
		}
	}
	c.DNS = net.JoinHostPort(dnsIP, strconv.Itoa(p))
	if c.BindPort, err = freeUDP("0.0.0.0"); err != nil {
		return c, err
	}
	return c, nil
}

func freeUDP(host string) (int, error) {
	for i := 0; i < 20; i++ {
		pc, err := net.ListenPacket("udp", net.JoinHostPort(host, "0"))
		if err != nil {
			return 0, err
		}
		p := pc.LocalAddr().(*net.UDPAddr).Port
		_ = pc.Close()
		if p >= 10000 {
			return p, nil
		}
	}
	return 0, errors.New("no free high UDP port")
}

func usedNets() []*net.IPNet {
	var out []*net.IPNet
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out = append(out, n)
		}
	}
	return out
}

func overlaps(n *net.IPNet, used []*net.IPNet) bool {
	for _, u := range used {
		if n.Contains(u.IP) || u.Contains(n.IP) {
			return true
		}
	}
	return false
}

// dnsAlive sends a tiny query for "localhost.sesh" and reports whether
// anything answered.
func dnsAlive(addr string, timeout time.Duration) bool {
	c, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return false
	}
	defer c.Close()
	q := []byte{0x51, 0x7e, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range []string{"localhost", "sesh"} {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write(q); err != nil {
		return false
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	return err == nil && n >= 12 && buf[0] == 0x51 && buf[1] == 0x7e
}

// CleanStale removes temp dirs left by a wallet that crashed (its owner pid
// is gone). Normally the guard already stopped that router; if the guard was
// killed too, the leftover router is stopped here.
func CleanStale(base string) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), DirPrefix) {
			continue
		}
		d := filepath.Join(base, e.Name())
		b, err := os.ReadFile(filepath.Join(d, "owner.pid"))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid == os.Getpid() || pidAlive(pid) {
			continue
		}
		killOrphan(d)
		_ = os.RemoveAll(d)
	}
}

func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - n
	if off < 0 {
		off = 0
	}
	b := make([]byte, st.Size()-off)
	_, _ = f.ReadAt(b, off)
	return string(b)
}

// explainExit turns a router log tail into a plain-English reason.
func explainExit(log string) string {
	l := strings.ToLower(log)
	switch {
	case strings.Contains(l, "operation not permitted") || strings.Contains(l, "permission denied") || strings.Contains(l, "eperm"):
		return "Session Router wasn't allowed to create its private network adapter. " + setcapHint
	case strings.Contains(l, "address already in use"):
		return "Session Router couldn't start because a port it picked was taken. Try again."
	case strings.Contains(l, "error while loading shared libraries"):
		return "Session Router is missing a library. Run scripts/fetch-session-router.sh again."
	}
	return "Session Router stopped right after starting. Try again, or switch to Direct connection in Settings."
}

const setcapHint = "Run this once: sudo setcap cap_net_admin+ep <wallet>/session-router/bin/session-router (or scripts/fetch-session-router.sh --setcap). The wallet itself doesn't need to run as root."

type limitedBuf struct {
	mu  sync.Mutex
	b   *bytes.Buffer
	max int
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.max - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
