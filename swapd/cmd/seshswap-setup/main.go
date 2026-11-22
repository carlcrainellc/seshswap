//go:build seshswap_setup

// Command seshswap-setup is SeshSwap-setup.exe: one download that installs
// Rusty (Session Router as a Windows service, from its own pinned
// rusty-setup.exe), or updates it, then the SeshSwap wallet into
// Program Files with a Start menu entry. An installed Rusty is updated in
// place and keeps its key, so the .sesh address exit time was bought for
// stays the same.
//
// It embeds payload/, which scripts/package-windows-setup.sh fills:
//
//	payload/rusty-setup.exe   Rusty's installer (SHA-256 pinned below)
//	payload/wallet/...        the Windows zip: seshswap.exe, monero/, docs
//
// Build: GOOS=windows go build -tags seshswap_setup ./cmd/seshswap-setup
// Testnet only. Not audited.
package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

//go:embed payload
var payload embed.FS

// version is set at build time (-ldflags "-X main.version=...").
var version = "dev"

// The Rusty build this setup installs, and its installer's SHA-256 (set by
// scripts/package-windows-setup.sh with -X, checked again before running it).
var (
	rustyVersion = "v0.1.0-test"
	rustySHA256  = "2bbc19b439c045d72b05edc313d3b45f2a813a6f20975472e593c3fd641f49fe"
)

const serviceName = "RustySessionRouter"

func main() {
	quiet, err := run()
	if err != nil {
		fmt.Println("\nSetup failed:", err)
	}
	if !quiet {
		fmt.Print("\nPress Enter to close.")
		_, _ = fmt.Fscanln(os.Stdin)
	}
	if err != nil {
		os.Exit(1)
	}
}

func run() (quiet bool, err error) { // quiet: don't wait for Enter
	dir := flag.String("dir", filepath.Join(os.Getenv("ProgramFiles"), "SeshSwap"), "where to install the wallet")
	rustyDir := flag.String("rusty-dir", `C:\Rusty`, "where to install Rusty if it isn't installed yet")
	noStart := flag.Bool("no-start", false, "don't start Rusty's service at the end")
	q := flag.Bool("quiet", false, "no questions, no 'Press Enter'")
	uninstall := flag.Bool("uninstall", false, "remove the SeshSwap program and its Start menu entry (keeps Rusty and your wallet data)")
	flag.Parse()
	fmt.Printf("SeshSwap setup %s (testnet only; not audited)\n\n", version)
	if t, err := windows.UTF16PtrFromString("SeshSwap setup " + version); err == nil {
		_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleTitleW").Call(uintptr(unsafe.Pointer(t)))
	}
	if !elevated() {
		fmt.Println("Setup needs administrator rights; asking Windows…")
		if err := relaunchElevated(); err != nil {
			return *q, fmt.Errorf("couldn't get administrator rights: %v", err)
		}
		return true, nil // the elevated copy carries on in its own window
	}
	if *uninstall {
		return *q, doUninstall(*dir)
	}
	if err := stopRunningWallet(*dir); err != nil {
		return *q, err
	}

	// Rusty first: the wallet needs its service, and an installed one keeps
	// its key (exit time is bought for that .sesh address).
	m, err := mgr.Connect()
	if err != nil {
		return *q, fmt.Errorf("can't talk to the service manager: %v", err)
	}
	defer m.Disconnect()
	rusty := *rustyDir
	if at := installedService(m); at != "" {
		// Update it in place: rusty-setup.exe keeps client.key and the
		// service, and the wallet needs this build's VPN check and exit guard.
		fmt.Printf("Rusty is already installed (%s). Updating it to %s; its key stays.\n", at, rustyVersion)
		rusty = at
		if err := installRusty(rusty); err != nil {
			return *q, err
		}
	} else if err := installRusty(rusty); err != nil {
		return *q, err
	}

	fmt.Printf("\nInstalling SeshSwap into %s…\n", *dir)
	n, err := unpackWallet(*dir)
	if err != nil {
		// Rusty was stopped for its update; don't leave it that way.
		if !*noStart {
			_ = startService(m)
		}
		if strings.Contains(err.Error(), "virus or potentially unwanted") {
			err = fmt.Errorf("%v\nWindows Security blocked that file (it often flags monero-wallet-rpc.exe). Allow it under Windows Security > Protection history, then run setup again", err)
		}
		return *q, err
	}
	fmt.Printf("Unpacked %d files.\n", n)
	exe := filepath.Join(*dir, "seshswap.exe")
	lnk := filepath.Join(startMenuDir(), "SeshSwap.lnk")
	if err := makeShortcut(lnk, exe, *dir); err != nil {
		fmt.Println("Couldn't add the Start menu entry:", err)
	} else {
		fmt.Println("Added SeshSwap to the Start menu.")
	}

	// Start the service now so the wallet finds it; one that was already
	// running (an earlier install) is fine.
	if !*noStart {
		if err := startService(m); err != nil {
			fmt.Println("Couldn't start Rusty's service:", err)
			fmt.Println("Start it later in an admin PowerShell: sc.exe start RustySessionRouter")
		} else {
			fmt.Println("\nRusty's Session Router service is running.")
		}
	}
	if a := rustyAddress(rusty); a != "" {
		fmt.Println("\nThis PC's .sesh address (exit time is bought for it; give it to an exit operator to be allowed in):")
		fmt.Println("  " + a)
	}
	fmt.Println("\nDone. Open SeshSwap from the Start menu. Your browser shows the wallet;")
	fmt.Println("keep the SeshSwap window open while you use it.")
	return *q, nil
}

// elevated: this process runs as administrator (setup writes to Program
// Files and talks to the service manager).
func elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// relaunchElevated starts this program again through UAC ("runas") with the
// same arguments.
func relaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	args, _ := windows.UTF16PtrFromString(quoteArgs(os.Args[1:]))
	return windows.ShellExecute(0, verb, file, args, nil, windows.SW_SHOWNORMAL)
}

// quoteArgs joins arguments for a Windows command line, quoting as the
// C runtime expects.
func quoteArgs(a []string) string {
	q := make([]string, len(a))
	for i, s := range a {
		q[i] = syscall.EscapeArg(s)
	}
	return strings.Join(q, " ")
}

func installedService(m *mgr.Mgr) string { // the folder of an installed Rusty service, or ""
	s, err := m.OpenService(serviceName)
	if err != nil {
		return ""
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return ""
	}
	// BinaryPathName may be quoted or unquoted (rusty-windows currently
	// writes the unquoted form when the path has no spaces).
	return serviceInstallDir(c.BinaryPathName)
}

// installRusty checks the bundled rusty-setup.exe against its pinned SHA-256
// and runs it unattended into dir. It doesn't start the service; run does.
func installRusty(dir string) error {
	b, err := payload.ReadFile("payload/rusty-setup.exe")
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != rustySHA256 {
		return fmt.Errorf("the bundled rusty-setup.exe doesn't match its pinned SHA-256 (%s)", rustySHA256)
	}
	// Run it from a temp folder (setup itself may be in Downloads).
	tmp, err := os.MkdirTemp("", "seshswap-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	exe := filepath.Join(tmp, "rusty-setup.exe")
	if err := os.WriteFile(exe, b, 0o755); err != nil {
		return err
	}
	fmt.Printf("Installing Rusty %s into %s…\n", rustyVersion, dir)
	cmd := exec.Command(exe, "--dir", dir, "--quiet", "--no-start")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Rusty's setup failed: %v", err)
	}
	return nil
}

// stopRunningWallet: a running seshswap.exe can't be replaced; `seshswap stop` (unmaps an exit).
func stopRunningWallet(dir string) error {
	exe := filepath.Join(dir, "seshswap.exe")
	for i := 0; i < 20; i++ {
		f, err := os.OpenFile(exe, os.O_WRONLY, 0) // fails while it runs
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			if f != nil {
				f.Close()
			}
			return nil
		}
		if i == 0 {
			fmt.Println("SeshSwap is running; asking it to quit (this unmaps an exit)…")
			_ = exec.Command(exe, "stop").Run()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("SeshSwap is still running. Close its window and run setup again")
}

// unpackWallet writes payload/wallet into dir (replacing the files of an
// older version; the wallet's data is in %LOCALAPPDATA%, not here) and
// returns how many files it wrote.
func unpackWallet(dir string) (int, error) {
	n := 0
	err := fs.WalkDir(payload, "payload/wallet", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "payload/wallet/")))
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		// WalkDir found it, so reading the embedded file can't fail; the
		// writes can (a full disk, a folder that isn't writable).
		b, _ := payload.ReadFile(p)
		f, err := os.Create(dst)
		if err != nil {
			return fmt.Errorf("can't write %s: %v", dst, err)
		}
		defer f.Close()
		if _, err := f.Write(b); err != nil {
			return fmt.Errorf("can't write %s: %v", dst, err)
		}
		n++
		return nil
	})
	return n, err
}

// startMenuDir is the all-users Start menu (C:\ProgramData\Microsoft\Windows\
// Start Menu\Programs), so the entry is there for every account on the PC;
// the wallet itself keeps each account's data apart in that account's
// %LOCALAPPDATA%\SeshSwap.
// Uninstall removes the entry again.
func startMenuDir() string {
	return filepath.Join(os.Getenv("ProgramData"), `Microsoft\Windows\Start Menu\Programs`)
}

// makeShortcut makes a .lnk with PowerShell's WScript.Shell (no COM in Go).
func makeShortcut(lnk, target, workDir string) error {
	// The paths (Program Files, ProgramData) have no single quotes to escape.
	ps := fmt.Sprintf(`$s=(New-Object -ComObject WScript.Shell).CreateShortcut('%s'); $s.TargetPath='%s'; $s.WorkingDirectory='%s'; $s.Description='SeshSwap wallet (testnet)'; $s.Save()`,
		lnk, target, workDir)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startService starts Rusty's service and waits up to 30 s for it to run.
// Its first start also writes my-client-address.txt.
func startService(m *mgr.Mgr) error {
	s, err := m.OpenService(serviceName)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return err
	}
	// The first start takes a few seconds: Rusty makes its key and its
	// network adapter.
	for i := 0; i < 60; i++ {
		if st, err := s.Query(); err == nil && st.State == svc.Running {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("it didn't report running within 30 s")
}

// rustyAddress is this PC's client .sesh address, as Rusty wrote it (a few
// seconds after its service first starts).
func rustyAddress(dir string) string {
	for i := 0; i < 10; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "my-client-address.txt")); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(500 * time.Millisecond)
	}
	return ""
}

// doUninstall removes the program folder and the Start menu entry. Rusty and
// the wallet data in %LOCALAPPDATA%\SeshSwap stay.
func doUninstall(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "seshswap.exe")); err != nil {
		return fmt.Errorf("SeshSwap isn't installed in %s", dir)
	}
	if err := stopRunningWallet(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(startMenuDir(), "SeshSwap.lnk"))
	fmt.Println("Removed SeshSwap from", dir, "and the Start menu.")
	fmt.Println("Kept: Rusty (remove it with rusty-service.exe uninstall in its folder) and your wallet in %LOCALAPPDATA%\\SeshSwap.")
	return nil
}
