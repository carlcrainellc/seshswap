package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/carlcrainellc/seshswap/swapd/internal/xmr"
)

// pickDaemon asks every node for get_info at once and returns the first one
// in list order that is a synced node on the wanted network.
func pickDaemon(ctx context.Context, nodes []string, nettype string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ok := make([]bool, len(nodes))
	why := make([]string, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			body := []byte(`{"jsonrpc":"2.0","id":"0","method":"get_info"}`)
			req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(n, "/")+"/json_rpc", bytes.NewReader(body))
			if err != nil {
				why[i] = err.Error()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				why[i] = err.Error()
				return
			}
			defer resp.Body.Close()
			var r struct {
				Result struct {
					Nettype      string `json:"nettype"`
					Synchronized bool   `json:"synchronized"`
					Height       uint64 `json:"height"`
				} `json:"result"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
				why[i] = fmt.Sprintf("HTTP %d", resp.StatusCode)
				return
			}
			switch {
			case r.Result.Nettype != nettype:
				why[i] = "network " + r.Result.Nettype
			case !r.Result.Synchronized:
				why[i] = "not synced"
			default:
				ok[i] = true
			}
		}(i, n)
	}
	wg.Wait()
	for i, n := range nodes {
		if ok[i] {
			return n, nil
		}
	}
	var parts []string
	for i, n := range nodes {
		parts = append(parts, fmt.Sprintf("%s: %s", n, why[i]))
	}
	return "", fmt.Errorf("%w (%s)", errNoDaemon, strings.Join(parts, "; "))
}

type moneroRPC struct {
	cmd  *exec.Cmd
	port int
	user string
	pass string
	done chan struct{}
}

func (m *moneroRPC) URL() string {
	return fmt.Sprintf("http://%s:%s@127.0.0.1:%d", m.user, m.pass, m.port)
}

func freePort(from int) (int, error) {
	for p := from; p < from+200; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port from %d", from)
}

// startMonero runs monero-wallet-rpc on 127.0.0.1 with a random login for
// this launch. The options (login included) go in a 0600 config file, not on
// the command line, so other users can't read them from ps.
func startMonero(ctx context.Context, bin, dataDir, daemon, nettype string, logLevel int) (*moneroRPC, error) {
	if !fileExists(bin) {
		return nil, fmt.Errorf("%s is missing", bin)
	}
	port, err := freePort(38083)
	if err != nil {
		return nil, err
	}
	var rb [16]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return nil, err
	}
	m := &moneroRPC{port: port, user: "seshswap", pass: hex.EncodeToString(rb[:]), done: make(chan struct{})}
	wdir := filepath.Join(dataDir, "xmr-wallets")
	runDir := filepath.Join(dataDir, "run")
	for _, d := range []string{wdir, runDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	u, err := url.Parse(daemon)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad Monero node address %q", daemon)
	}
	daemonAddr := u.Host
	conf := []string{
		"daemon-address=" + daemonAddr,
		"untrusted-daemon=1",
		"rpc-bind-ip=127.0.0.1",
		fmt.Sprintf("rpc-bind-port=%d", port),
		"rpc-login=" + m.user + ":" + m.pass,
		"rpc-ssl=disabled",
		"wallet-dir=" + wdir,
		"log-file=" + filepath.Join(dataDir, "logs", "monero-wallet-rpc.log"),
		fmt.Sprintf("log-level=%d", max(0, min(logLevel, 4))),
		"max-log-files=2",
		"non-interactive=1",
	}
	if u.Scheme == "https" {
		conf = append(conf, "daemon-ssl=enabled")
	}
	switch nettype {
	case "stagenet":
		conf = append(conf, "stagenet=1")
	case "testnet":
		conf = append(conf, "testnet=1")
	}
	confPath := filepath.Join(runDir, "monero-wallet-rpc.conf")
	if err := os.WriteFile(confPath, []byte(strings.Join(conf, "\n")+"\n"), 0o600); err != nil {
		return nil, err
	}
	m.cmd = exec.Command(bin, "--config-file", confPath)
	m.cmd.Dir = runDir
	// Own process group (Ctrl-C reaches us first and we stop it cleanly), and
	// it dies with us if we crash.
	m.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	if err := m.cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = m.cmd.Wait(); close(m.done) }()

	// Ready when it answers get_version with our login.
	w := xmr.NewWallet(m.URL(), "")
	end := time.Now().Add(45 * time.Second)
	for {
		select {
		case <-m.done:
			_ = os.Remove(confPath)
			return nil, fmt.Errorf("monero-wallet-rpc exited at start (see %s)", filepath.Join(dataDir, "logs", "monero-wallet-rpc.log"))
		case <-ctx.Done():
			m.Stop()
			return nil, ctx.Err()
		default:
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := w.Version(cctx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(end) {
			m.Stop()
			return nil, fmt.Errorf("monero-wallet-rpc didn't answer: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// It has read its config; the login only lives in memory from here.
	_ = os.Remove(confPath)
	log.Printf("Monero wallet service on 127.0.0.1:%d (pid %d)", port, m.cmd.Process.Pid)
	return m, nil
}

// Stop asks monero-wallet-rpc to save and exit, then makes sure it is gone.
func (m *moneroRPC) Stop() {
	if m == nil || m.cmd == nil || m.cmd.Process == nil {
		return
	}
	select {
	case <-m.done:
		return
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := xmr.NewWallet(m.URL(), "").StopWallet(ctx)
	cancel()
	wait := 15 * time.Second // saving a big wallet can take a while
	if err != nil {
		wait = 0 // no wallet open (locked): nothing to save
	}
	select {
	case <-m.done:
	case <-time.After(wait):
		_ = m.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-m.done:
		case <-time.After(5 * time.Second):
			_ = m.cmd.Process.Kill()
			<-m.done
		}
	}
	log.Print("Monero wallet service stopped")
}
