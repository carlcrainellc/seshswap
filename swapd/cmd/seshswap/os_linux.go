package main

// Linux: the bundled Session Router (setcap once with `seshswap setup-router`),
// SIGTERM for `seshswap stop`, and the data in ~/.seshswap.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
	"github.com/carlcrainellc/seshswap/swapd/internal/wallet"
)

const exeSuffix = ""

const usage = `  seshswap                 start the wallet and open it in your browser
  seshswap setup-router    one-time permission for private swaps (asks for sudo)
  seshswap stop            stop a running SeshSwap
  seshswap check           check that the bundled programs run on this computer
  seshswap version
`

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".seshswap"
	}
	return filepath.Join(home, ".seshswap")
}

// platformRouter points the wallet at the bundled Session Router, unless the
// config already names one.
func platformRouter(cfg *config, bdir string) {
	srDir := filepath.Join(bdir, "session-router")
	if cfg.SessionRouter == nil {
		cfg.SessionRouter = &wallet.RouterConfig{}
	}
	if cfg.SessionRouter.Binary == "" && fileExists(filepath.Join(srDir, "bin", "session-router")) {
		cfg.SessionRouter.Binary = filepath.Join(srDir, "bin", "session-router")
		if _, err := relocRouter(srDir); err != nil {
			log.Printf("Session Router: %v", err)
		}
		if ok, _ := srouter.FileHasNetAdmin(cfg.SessionRouter.Binary); !ok && !srouter.CanCreateTUN() {
			fmt.Println("Private swaps need a one-time permission for Session Router. Run:")
			fmt.Println("  " + selfCommand() + " setup-router")
		}
	}
}

// moneroProcAttr: own process group (Ctrl-C reaches us first and we stop it
// cleanly), and it dies with us if we crash.
func moneroProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

// moneroStarted: nothing more to do on Linux (Pdeathsig does it).
func moneroStarted(*exec.Cmd) {}

func stopChild(p *os.Process) { _ = p.Signal(syscall.SIGTERM) }

// watchStop: `seshswap stop` sends SIGTERM on Linux, which ctx already handles.
func watchStop(context.Context, string, func()) {}

// pauseIfOwnConsole: a Linux terminal stays open by itself.
func pauseIfOwnConsole(bool) {}

// openBrowser uses xdg-open (or the Debian/Ubuntu fallbacks). Without one
// that works, the address printed above is enough.
func openBrowser(u string) {
	for _, c := range []string{"xdg-open", "sensible-browser", "x-www-browser"} {
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		cmd := exec.Command(p, u)
		if err := cmd.Start(); err != nil {
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err == nil {
				return
			}
		case <-time.After(3 * time.Second):
			return // still running: a browser is opening
		}
	}
	fmt.Println("Open this address in your browser: " + u)
}

func stopRunning(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "seshswap.pid"))
	if err != nil {
		fmt.Println("SeshSwap isn't running.")
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		fmt.Println("SeshSwap isn't running.")
		return 0
	}
	// Only signal it if it really is seshswap.
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err != nil || !strings.HasPrefix(filepath.Base(exe), "seshswap") {
		fmt.Println("SeshSwap isn't running.")
		return 0
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintln(os.Stderr, "seshswap stop:", err)
		return 1
	}
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			fmt.Println("SeshSwap stopped.")
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("SeshSwap is still stopping.")
	return 0
}

// setupRouter gives the bundled Session Router cap_net_admin so it can make
// its own network adapter while running as you. Needs sudo once (and again
// if the folder moves).
func setupRouter() int {
	srDir := filepath.Join(bundleDir(), "session-router")
	bin := filepath.Join(srDir, "bin", "session-router")
	if !fileExists(bin) {
		fmt.Fprintln(os.Stderr, "No bundled Session Router in", srDir)
		return 1
	}
	if _, err := relocRouter(srDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if ok, _ := srouter.FileHasNetAdmin(bin); ok {
		fmt.Println("Already set up. Private swaps are ready.")
		return 0
	}
	fmt.Println("Session Router needs permission to create its own network adapter.")
	fmt.Println("This runs once with sudo:")
	var cmd *exec.Cmd
	if p, err := exec.LookPath("setcap"); err == nil {
		fmt.Println("  sudo setcap cap_net_admin+ep " + bin)
		cmd = exec.Command("sudo", p, "cap_net_admin+ep", bin)
	} else if p, err := exec.LookPath("python3"); err == nil {
		// No libcap tools: write the same xattr setcap would (v2, cap_net_admin, +ep).
		fmt.Println("  (setcap isn't installed; writing the same capability with python3)")
		cmd = exec.Command("sudo", p, "-c", `import os,struct,sys; os.setxattr(sys.argv[1],"security.capability",struct.pack("<IIIII",0x02000001,1<<12,0,0,0))`, bin)
	} else {
		fmt.Println("  sudo apt install libcap2-bin && sudo setcap cap_net_admin+ep " + bin)
		return 1
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "setup-router:", err)
		fmt.Fprintln(os.Stderr, "If your account can't use sudo, ask someone who can to run the line above, then run ./seshswap check.")
		return 1
	}
	if ok, _ := srouter.FileHasNetAdmin(bin); !ok {
		fmt.Fprintln(os.Stderr, "The permission didn't stick. The folder may be on a drive that doesn't support it; move it to your home folder and try again.")
		return 1
	}
	fmt.Println("Done. Session Router still runs as you, not root.")
	return 0
}

// check runs the bundled programs once to show they work on this computer.
func check() int {
	bdir := bundleDir()
	srDir := filepath.Join(bdir, "session-router")
	bad := 0
	if _, err := relocRouter(srDir); err != nil {
		fmt.Println("session-router: ", err)
		bad++
	}
	for _, c := range []struct{ name, bin, arg string }{
		{"session-router", filepath.Join(srDir, "bin", "session-router"), "--version"},
		{"monero-wallet-rpc", filepath.Join(bdir, "monero", "monero-wallet-rpc"), "--version"},
	} {
		out, err := exec.Command(c.bin, c.arg).CombinedOutput()
		line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if err != nil {
			fmt.Printf("%-18s FAILED: %v %s\n", c.name, err, line)
			bad++
			continue
		}
		fmt.Printf("%-18s ok  %s\n", c.name, line)
	}
	bin := filepath.Join(srDir, "bin", "session-router")
	if ok, _ := srouter.FileHasNetAdmin(bin); ok {
		fmt.Printf("%-18s ok  private swaps are set up\n", "permission")
	} else {
		fmt.Printf("%-18s not set up yet: run %s setup-router\n", "permission", selfCommand())
	}
	if bad > 0 {
		return 1
	}
	return 0
}

func consoleTitle(string) {}
