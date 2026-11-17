// Package srtest is a fake Session Router for tests: the test binary re-runs
// itself as the "router", serves DNS on the ini's [dns] listen address and
// answers every .sesh name with 127.0.0.1. It hides nothing; tests only.
package srtest

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/carlcrainellc/seshswap/swapd/internal/srouter"
)

const envKey = "SROUTER_FAKE"

// MaybeRun turns the current process into the fake router when the test
// binary was started by Options(). Call it first thing in TestMain.
func MaybeRun() {
	if len(os.Args) > 1 && os.Args[1] == srouter.GuardArg {
		srouter.RunGuard(os.Args[2:])
	}
	mode := os.Getenv(envKey)
	if mode == "" {
		if base := os.Getenv("SROUTER_FAKEWALLET"); base != "" {
			// Children must not become fake wallets themselves.
			_ = os.Unsetenv("SROUTER_FAKEWALLET")
			runFakeWallet(base)
		}
		return
	}
	ini := os.Args[len(os.Args)-1]
	dns, logf := parseIni(ini)
	if mode == "eperm" {
		_ = os.WriteFile(logf, []byte("[vpn.linux:critical] failed to create TUN: Operation not permitted\n"), 0o600)
		os.Exit(1)
	}
	if mode == "crash" {
		os.Exit(3)
	}
	pc, err := net.ListenPacket("udp", dns)
	if err != nil {
		os.Exit(2)
	}
	_ = os.WriteFile(logf, []byte("fake router up\n"), 0o600)
	go serve(pc)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
	os.Exit(0)
}

// runFakeWallet starts a guarded fake router, prints its temp dir, and
// waits to be killed (for the crash test).
func runFakeWallet(base string) {
	o := Options("ok")
	o.Guard = []string{o.Binary, srouter.GuardArg}
	o.BaseDir = base
	r := srouter.New(o)
	if _, err := r.Ensure(context.Background()); err != nil {
		os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("UP\n")
	select {}
}

// Options returns router options that start the fake. mode is "ok",
// "eperm" (adapter refused) or "crash".
func Options(mode string) srouter.Options {
	exe, _ := os.Executable()
	return srouter.Options{
		Binary:     exe,
		ArgsPrefix: []string{"-test.run=^$"},
		Env:        []string{envKey + "=" + mode},
		Check:      func(string) error { return nil },
	}
}

// Alive reports whether pid is still running (for leak checks).
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z") && len(strings.TrimSpace(string(out))) > 0
}

func parseIni(path string) (dns, logf string) {
	f, err := os.Open(path)
	if err != nil {
		os.Exit(2)
	}
	defer f.Close()
	sec := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "[") {
			sec = l
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		switch {
		case sec == "[dns]" && k == "listen":
			dns = v
		case sec == "[logging]" && k == "file":
			logf = v
		}
	}
	return
}

func serve(pc net.PacketConn) {
	buf := make([]byte, 512)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 12 {
			continue
		}
		q := buf[:n]
		// question name
		i := 12
		var labels []string
		for i < n && q[i] != 0 {
			l := int(q[i])
			if i+1+l > n {
				break
			}
			labels = append(labels, string(q[i+1:i+1+l]))
			i += 1 + l
		}
		i++ // zero
		if i+4 > n {
			continue
		}
		qtype := int(q[i])<<8 | int(q[i+1])
		qend := i + 4
		name := strings.ToLower(strings.Join(labels, "."))
		resp := append([]byte{}, q[:qend]...)
		resp[2] = 0x81
		resp[3] = 0x80
		resp[8], resp[9], resp[10], resp[11] = 0, 0, 0, 0
		if strings.HasSuffix(name, ".sesh") && name != "localhost.sesh" && qtype == 1 {
			resp[6], resp[7] = 0, 1
			resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 30, 0, 4, 127, 0, 0, 1)
		} else {
			resp[6], resp[7] = 0, 0
			if !strings.HasSuffix(name, ".sesh") || name == "localhost.sesh" {
				resp[3] = 0x83 // NXDOMAIN
			}
		}
		_, _ = pc.WriteTo(resp, addr)
	}
}

// StartDNS runs the fake .sesh DNS on 127.0.0.1 (a random UDP port) in this
// process, standing in for a Session Router the wallet did not start (a
// system service or a user-run instance). It answers every .sesh name with
// 127.0.0.1. stop closes it.
func StartDNS() (addr string, stop func(), err error) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	go serve(pc)
	return pc.LocalAddr().String(), func() { _ = pc.Close() }, nil
}
