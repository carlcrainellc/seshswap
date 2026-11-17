package srouter_test

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/sesh"
	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/srouter/srtest"
)

func TestMain(m *testing.M) {
	srtest.MaybeRun()
	os.Exit(m.Run())
}

func TestRenderIsClientOnlyAndLeavesSystemAlone(t *testing.T) {
	c, err := srouter.Pick()
	if err != nil {
		t.Fatal(err)
	}
	c.DataDir, c.LogFile, c.LogLevel = "/tmp/x/data", "/tmp/x/router.log", "info"
	ini := c.Render()
	for _, want := range []string{
		"reachable=false", "auto-routing=false", "blackhole-routes=false",
		"[exit]\nenable=false", "no-resolvconf=1", "[api]\nenabled=false",
		"listen=" + c.DNS, "ifname=" + c.IfName, "data-dir=/tmp/x/data",
	} {
		if !strings.Contains(ini, want) {
			t.Fatalf("ini missing %q:\n%s", want, ini)
		}
	}
	for _, bad := range []string{"keyfile", "reserved-range", "exit-node", "upstream", "add-node"} {
		if strings.Contains(ini, bad) {
			t.Fatalf("ini has %q:\n%s", bad, ini)
		}
	}
	host, port, _ := strings.Cut(c.DNS, ":")
	if !strings.HasPrefix(host, "127.") || len(port) < 5 {
		t.Fatalf("DNS should be 127.x on a high port, got %s", c.DNS)
	}
	if c.BindPort < 10000 {
		t.Fatalf("bind port %d", c.BindPort)
	}
	if len(c.IfName) > 15 || !strings.HasPrefix(c.IfName, "swr") {
		t.Fatalf("ifname %q", c.IfName)
	}
	c2, _ := srouter.Pick()
	if c2.IfName == c.IfName && c2.DNS == c.DNS {
		t.Fatalf("two picks were identical: %+v", c2)
	}
}

func TestParseFileCaps(t *testing.T) {
	// setcap cap_net_admin+ep (v2) and v3 with rootid.
	v2 := []byte{0x01, 0, 0, 0x02, 0x00, 0x10, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	v3 := append(append([]byte{0x01, 0, 0, 0x03}, v2[4:]...), 0, 0, 0, 0)
	noEff := append([]byte{0x00, 0, 0, 0x02}, v2[4:]...)
	otherCap := []byte{0x01, 0, 0, 0x02, 0x00, 0x04, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0} // cap_net_bind_service (10)
	for name, tc := range map[string]struct {
		b    []byte
		want bool
	}{"v2": {v2, true}, "v3": {v3, true}, "no +e": {noEff, false}, "other cap": {otherCap, false}} {
		got, err := srouter.ParseFileCaps(tc.b, 12)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %v %v want %v", name, got, err, tc.want)
		}
	}
	if _, err := srouter.ParseFileCaps([]byte{1, 2}, 12); err == nil {
		t.Fatal("short xattr accepted")
	}
}

func TestStartResolveStop(t *testing.T) {
	base := t.TempDir()
	o := srtest.Options("ok")
	o.BaseDir = base
	r := srouter.New(o)
	if r.Status().State != srouter.Off || r.Running() {
		t.Fatal("not off at start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dns, err := r.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := r.Status()
	if st.State != srouter.Connecting || st.PID == 0 || st.DNS != dns {
		t.Fatalf("status %+v", st)
	}
	addr, err := sesh.ResolveVia(ctx, dns, "abc.sesh", "8080")
	if err != nil || addr != "127.0.0.1:8080" {
		t.Fatalf("resolve %q %v", addr, err)
	}
	// Second Ensure reuses the same child.
	dns2, err := r.Ensure(ctx)
	if err != nil || dns2 != dns || r.Status().PID != st.PID {
		t.Fatalf("relaunched: %s %v", dns2, err)
	}
	r.MarkConnected()
	if s := r.Status(); s.State != srouter.Connected || s.StartupMs < 0 {
		t.Fatalf("after MarkConnected %+v", s)
	}
	dirs, _ := filepath.Glob(filepath.Join(base, srouter.DirPrefix+"*"))
	if len(dirs) != 1 {
		t.Fatalf("temp dirs %v", dirs)
	}
	r.Stop()
	if r.Running() || r.Status().State != srouter.Off {
		t.Fatal("still running")
	}
	if srtest.Alive(st.PID) {
		t.Fatalf("child %d survived Stop", st.PID)
	}
	dirs, _ = filepath.Glob(filepath.Join(base, srouter.DirPrefix+"*"))
	if len(dirs) != 0 {
		t.Fatalf("temp dir left behind: %v", dirs)
	}
	// Each launch gets a fresh data dir and fresh ports.
	dns3, err := r.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if r.Status().Launches != 2 {
		t.Fatalf("launches %d", r.Status().Launches)
	}
	_ = dns3
}

func TestPlainErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	r := srouter.New(srouter.Options{Binary: filepath.Join(t.TempDir(), "nope"), BaseDir: t.TempDir()})
	_, err := r.Ensure(ctx)
	if err == nil || !strings.Contains(err.Error(), "isn't installed") || !strings.Contains(err.Error(), "Direct connection") {
		t.Fatalf("missing binary: %v", err)
	}
	if r.Status().State != srouter.Failed {
		t.Fatalf("state %v", r.Status().State)
	}

	o := srtest.Options("eperm")
	o.BaseDir = t.TempDir()
	_, err = srouter.New(o).Ensure(ctx)
	if err == nil || !strings.Contains(err.Error(), "setcap cap_net_admin+ep") {
		t.Fatalf("eperm: %v", err)
	}

	o = srtest.Options("crash")
	o.BaseDir = t.TempDir()
	_, err = srouter.New(o).Ensure(ctx)
	if err == nil || !strings.Contains(err.Error(), "stopped right after starting") {
		t.Fatalf("crash: %v", err)
	}
	if dirs, _ := filepath.Glob(filepath.Join(o.BaseDir, srouter.DirPrefix+"*")); len(dirs) != 0 {
		t.Fatalf("temp dir left after crash: %v", dirs)
	}
}

func TestCleanStale(t *testing.T) {
	base := t.TempDir()
	dead := filepath.Join(base, srouter.DirPrefix+"dead")
	mine := filepath.Join(base, srouter.DirPrefix+"mine")
	_ = os.MkdirAll(dead, 0o700)
	_ = os.MkdirAll(mine, 0o700)
	_ = os.WriteFile(filepath.Join(dead, "owner.pid"), []byte("999999999"), 0o600)
	_ = os.WriteFile(filepath.Join(mine, "owner.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	srouter.CleanStale(base)
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("stale dir from a dead wallet was kept")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("removed a live wallet's dir")
	}
}

func TestNoRootAndCapsMessage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	f := filepath.Join(t.TempDir(), "session-router")
	_ = os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755)
	err := srouter.CheckPermissions(f)
	if srouter.CanCreateTUN() {
		if err != nil {
			t.Fatalf("this host can create a TUN, but check failed: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "sudo setcap cap_net_admin+ep "+f) || !strings.Contains(err.Error(), "doesn't need to run as root") {
		t.Fatalf("message: %v", err)
	}
}

// A wallet killed with SIGKILL must not leave its router running.
func TestRouterDiesWithWallet(t *testing.T) {
	base := t.TempDir()
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), "SROUTER_FAKEWALLET="+base)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "UP" {
		_ = cmd.Process.Kill()
		t.Fatalf("fake wallet: %q", line)
	}
	dirs, _ := filepath.Glob(filepath.Join(base, srouter.DirPrefix+"*"))
	if len(dirs) != 1 {
		t.Fatalf("dirs %v", dirs)
	}
	var pid int
	for i := 0; i < 50 && pid == 0; i++ {
		b, _ := os.ReadFile(filepath.Join(dirs[0], "router.pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(20 * time.Millisecond)
	}
	if !srtest.Alive(pid) {
		t.Fatalf("router pid %d not running", pid)
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()
	deadline := time.Now().Add(8 * time.Second)
	for srtest.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if srtest.Alive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("router %d survived kill -9 of its wallet", pid)
	}
	// The next start cleans up the dead wallet's temp dir.
	srouter.CleanStale(base)
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Fatal("stale dir kept")
	}
}
