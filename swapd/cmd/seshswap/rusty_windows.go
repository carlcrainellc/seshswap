package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// rustyDir is where Rusty is installed: the folder of the RustySessionRouter
// service program, or C:\Rusty.
func rustyDir() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\RustySessionRouter`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if p, _, err := k.GetStringValue("ImagePath"); err == nil {
			if d := serviceDir(p); d != "" {
				return d
			}
		}
	}
	return `C:\Rusty`
}

// serviceDir is the folder of the program in a service ImagePath, quoted or not.
func serviceDir(imagePath string) string {
	s := strings.TrimSpace(imagePath)
	var exe string
	switch {
	case strings.HasPrefix(s, `"`):
		exe = strings.SplitN(s[1:], `"`, 2)[0]
	case strings.Contains(strings.ToLower(s), ".exe"):
		exe = s[:strings.Index(strings.ToLower(s), ".exe")+4]
	default:
		exe = strings.Fields(s + " ")[0]
	}
	if exe == "" {
		return ""
	}
	return filepath.Dir(exe)
}

// rustyPreflight asks Rusty whether another VPN holds the route before a map.
func rustyPreflight(ctx context.Context) (string, []string, error) {
	ctl := filepath.Join(rustyDir(), "rusty-ctl.exe")
	if _, err := os.Stat(ctl); err != nil {
		return "", nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, ctl, "preflight", "--json")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 3) { // 3: not ready, JSON says why
		return "", nil, err
	}
	return parsePreflight(out)
}

// rustyLastRollback reads what Rusty's exit guard last did.
func rustyLastRollback() (string, time.Time) {
	b, err := os.ReadFile(filepath.Join(rustyDir(), "exit-guard.json"))
	if err != nil {
		return "", time.Time{}
	}
	return parseGuardFile(b)
}
