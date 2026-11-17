//go:build !linux

package srouter

import (
	"os"
	"os/exec"
)

func setSysProcAttr(cmd *exec.Cmd) {}

// CheckPermissions is a no-op off Linux. On Windows the router needs the
// wintun driver (wintun.dll next to session-router.exe) and an elevated
// first run to install the adapter; see docs/WALLET.md.
func CheckPermissions(string) error { return nil }

func terminate(p *os.Process) { _ = p.Kill() }

// pidAlive is conservative off Linux: assume alive so nothing is deleted
// out from under another wallet.
func pidAlive(pid int) bool { return pid > 0 }

// GuardArg is accepted everywhere so the wallet's flag parsing is the same.
const GuardArg = "__session-router-guard"

// RunGuard just runs the router off Linux (no parent-death signal there).
func RunGuard(args []string) {
	if len(args) < 1 {
		os.Exit(2)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func killOrphan(string) {}
