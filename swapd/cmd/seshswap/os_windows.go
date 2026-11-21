package main

// Windows: no bundled Session Router. SeshSwap uses Rusty's Session Router
// service (RustySessionRouter, installed into C:\Rusty by SeshSwap-setup.exe
// or rusty-setup.exe): its RPC on 127.0.0.1:1190 for exits and its DNS on
// 127.0.0.1:53 for .sesh names. Rusty points this computer's DNS at itself
// and handles DNS for exits, so the wallet doesn't switch DNS here. Data goes
// in %LOCALAPPDATA%\SeshSwap; monero-wallet-rpc is tied to SeshSwap with a
// job object; `seshswap stop` asks the running copy to quit through a file.
// Build: GOOS=windows CGO_ENABLED=0 (scripts/package-windows.sh).

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
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/carlcrainellc/seshswap/swapd/internal/exits"
	"github.com/carlcrainellc/seshswap/swapd/internal/wallet"
)

const exeSuffix = ".exe"

const (
	rustyRPC   = "tcp://127.0.0.1:1190"
	rustyDNS   = "127.0.0.1:53"
	rustyLabel = "Rusty's Session Router service"
)

const usage = `  seshswap                 start the wallet and open it in your browser
  seshswap stop            stop a running SeshSwap
  seshswap check           check monero-wallet-rpc and Rusty's Session Router service
  seshswap version
`

func defaultDataDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "SeshSwap")
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "SeshSwap")
	}
	return "SeshSwap"
}

// platformRouter: Rusty's service, unless config.json says otherwise.
func platformRouter(cfg *config, _ string) {
	if cfg.Transport == "" {
		cfg.Transport = "existing"
	}
	if cfg.ExitRouterRPC == "" {
		cfg.ExitRouterRPC = rustyRPC
	}
	if cfg.SeshDNS == "" {
		cfg.SeshDNS = rustyDNS
	}
	if cfg.ExitDNS == "" {
		cfg.ExitDNS = "router" // Rusty does DNS while an exit is mapped
	}
	if cfg.ExitRouterRPC == rustyRPC {
		wallet.ExistingRouterLabel = rustyLabel
		// Rusty checks for another VPN before a map and says why it
		// unmapped an exit that didn't come up.
		wallet.MapPreflight = rustyPreflight
		wallet.LastRollback = rustyLastRollback
	}
	if cfg.Transport == "direct" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err := (&exits.RouterRPC{Endpoint: cfg.ExitRouterRPC}).Version(ctx)
	cancel()
	if err != nil {
		fmt.Println(rustyLabel + " isn't answering at " + cfg.ExitRouterRPC + ".")
		fmt.Println("Private swaps and exits need it. Install it with SeshSwap-setup.exe (or rusty-setup.exe),")
		fmt.Println("or start it from an admin PowerShell:  sc.exe start RustySessionRouter")
		fmt.Println("Balances and Get test coins work without it.")
	}
}

func moneroProcAttr(cmd *exec.Cmd) { // own process group: Ctrl-C reaches us first and we stop it
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// moneroStarted puts monero-wallet-rpc in a job object that is closed when
// SeshSwap exits, so Windows ends it even if SeshSwap crashes.
func moneroStarted(cmd *exec.Cmd) {
	if err := killWithUs(cmd.Process.Pid); err != nil {
		log.Printf("monero-wallet-rpc may outlive SeshSwap if it crashes: %v", err)
	}
}

// moneroJob stays open for SeshSwap's lifetime; Windows closes it on exit.
var moneroJob windows.Handle

func killWithUs(pid int) error {
	if moneroJob == 0 {
		j, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return err
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(j, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			windows.CloseHandle(j)
			return err
		}
		moneroJob = j
	}
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(p)
	return windows.AssignProcessToJobObject(moneroJob, p)
}

// stopChild: Windows has no SIGTERM for console programs; Stop already
// asked monero-wallet-rpc to save and exit over RPC.
func stopChild(p *os.Process) { _ = p.Kill() }

// openBrowser opens u in the default browser (rundll32 returns at once).
func openBrowser(u string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	if err := cmd.Start(); err == nil {
		// rundll32 hands the address to the browser and exits by itself.
		return
	}
	fmt.Println("Open this address in your browser: " + u)
}

// watchStop: `seshswap stop` writes a stop-request file in the data dir; the
// running copy quits as on Ctrl-C (unmaps an exit, stops monero-wallet-rpc).
func watchStop(ctx context.Context, file string, stop func()) {
	_ = os.Remove(file)
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if os.Remove(file) == nil {
				log.Print("stop requested")
				stop()
				return
			}
		}
	}()
}

// stopRunning asks a running SeshSwap to quit and waits up to 30 s for it.
func stopRunning(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "seshswap.pid"))
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || perr != nil || pid <= 0 || !processIsSeshSwap(pid) {
		fmt.Println("SeshSwap isn't running.")
		return 0
	}
	if err := os.WriteFile(filepath.Join(dir, "stop-request"), []byte("stop\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "seshswap stop:", err)
		return 1
	}
	for i := 0; i < 300; i++ {
		if !processIsSeshSwap(pid) {
			fmt.Println("SeshSwap stopped.")
			return 0
		}
		// It unmaps an exit (up to 10 s if the router is slow) and stops
		// monero-wallet-rpc, which saves the wallet first (up to 15 s), so
		// 30 s covers a normal quit.
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("SeshSwap is still stopping.")
	return 0
}

// processIsSeshSwap: pid is running and its program is seshswap*.exe.
func processIsSeshSwap(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil || code != 259 { // STILL_ACTIVE
		return false
	}
	// PROCESS_QUERY_LIMITED_INFORMATION is enough for the image name too.
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) != nil {
		return false
	}
	name := strings.ToLower(filepath.Base(windows.UTF16ToString(buf[:n])))
	return strings.HasPrefix(name, "seshswap") && strings.HasSuffix(name, ".exe")
}

func setupRouter() int {
	fmt.Println("Nothing to set up here. On Windows SeshSwap uses Rusty's Session Router service;")
	fmt.Println("SeshSwap-setup.exe installs it. Run `seshswap check` to see if it is running.")
	return 0
}

// check runs monero-wallet-rpc once and asks Rusty's service for its version.
func check() int {
	bdir := bundleDir()
	bad := 0
	bin := filepath.Join(bdir, "monero", "monero-wallet-rpc.exe")
	out, err := exec.Command(bin, "--version").CombinedOutput()
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if err != nil {
		fmt.Printf("%-18s FAILED: %v %s\n", "monero-wallet-rpc", err, line)
		bad++
	} else {
		fmt.Printf("%-18s ok  %s\n", "monero-wallet-rpc", line)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rr := &exits.RouterRPC{Endpoint: rustyRPC}
	v, err := rr.Version(ctx)
	switch {
	case err != nil:
		fmt.Printf("%-18s not answering at %s: %v\n", "session router", rustyRPC, err)
		fmt.Println("                   install Rusty (SeshSwap-setup.exe) or: sc.exe start RustySessionRouter (admin)")
		bad++
	case rr.SupportsExits(ctx) != nil:
		fmt.Printf("%-18s %s, but it can't map exits: %v\n", "session router", v, rr.SupportsExits(ctx))
		bad++
	default:
		fmt.Printf("%-18s ok  %s (Rusty, exits supported)\n", "session router", v)
	}
	cancel()
	// 1 if anything is wrong, like the Linux check.
	return min(bad, 1)
}

// pauseIfOwnConsole keeps the window open when SeshSwap was started by a
// double-click (Windows made a console just for it), so its output can be read.
func pauseIfOwnConsole(when bool) {
	if !when {
		return
	}
	if h, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE); err != nil || h == 0 || h == windows.InvalidHandle {
		return
	}
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	if proc.Find() != nil {
		return
	}
	var pids [4]uint32
	n, _, _ := proc.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n != 1 { // other programs (a shell) share this console
		return
	}
	fmt.Print("\nPress Enter to close this window.")
	_, _ = fmt.Fscanln(os.Stdin)
}
