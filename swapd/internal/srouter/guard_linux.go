//go:build linux

package srouter

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// GuardArg is the argv[1] that turns the wallet executable into the guard.
const GuardArg = "__session-router-guard"

// RunGuard is the body of the guard process: `<wallet> __session-router-guard
// <router> <args…>`. The wallet starts the guard with a parent-death signal.
// The router itself cannot carry one: Linux clears it when a program with
// file capabilities (setcap) is executed. So the guard (no capabilities)
// holds it, and stops the router when the wallet goes away for any reason,
// including a crash or kill -9. It never returns.
func RunGuard(args []string) {
	if len(args) < 1 {
		os.Exit(2)
	}
	parent := os.Getppid()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Stderr.WriteString("guard: " + err.Error() + "\n")
		os.Exit(1)
	}
	_ = os.WriteFile("router.pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	tick := time.NewTicker(time.Second)
	stop := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		os.Exit(0)
	}
	for {
		select {
		case err := <-done:
			if ee, ok := err.(*exec.ExitError); ok {
				os.Exit(ee.ExitCode())
			}
			os.Exit(0)
		case <-sig:
			stop()
		case <-tick.C:
			if os.Getppid() != parent { // belt and braces
				stop()
			}
		}
	}
}

// killOrphan stops a router left behind in dir (owner wallet gone, guard
// killed too). It only signals a pid whose command line names this dir.
func killOrphan(dir string) {
	b, err := os.ReadFile(filepath.Join(dir, "router.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return
	}
	cl, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || !strings.Contains(string(cl), dir) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
