package exits

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// A minimal OxenMQ request client: one ZMTP 3.0 DEALER connection per call,
// NULL security, the way Session Router's own tools talk
// to its RPC ([api] bind=...). It only does what the wallet needs: send
// [method, tag, json] and read [REPLY, tag, body]. No dependency on libzmq.
//
// endpoint is "ipc:///path/to/rpc.sock" or "tcp://127.0.0.1:port". Anything
// that is not a local socket or loopback TCP address is refused: the router
// RPC is admin-level and must never be reached over the network.

// ErrNotLocal is returned for an RPC endpoint that isn't on this computer.
var ErrNotLocal = errors.New("Session Router RPC must be a local socket or 127.0.0.1")

func dialRPC(ctx context.Context, endpoint string) (net.Conn, error) {
	var d net.Dialer
	switch {
	case strings.HasPrefix(endpoint, "ipc://"):
		return d.DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "ipc://"))
	case strings.HasPrefix(endpoint, "tcp://"):
		hp := strings.TrimPrefix(endpoint, "tcp://")
		h, _, err := net.SplitHostPort(hp)
		if err != nil {
			return nil, err
		}
		ip := net.ParseIP(h)
		if h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, ErrNotLocal
		}
		return d.DialContext(ctx, "tcp", hp)
	}
	return nil, fmt.Errorf("unsupported RPC endpoint %q (want ipc:// or tcp://127.0.0.1:port)", endpoint)
}

// OMQCall sends one request and returns the first reply body.
func OMQCall(ctx context.Context, endpoint, method string, payload []byte) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	c, err := dialRPC(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
	defer stop()
	r := bufio.NewReader(c)
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	if err := handshake(c, r, "wallet-"+hex.EncodeToString(id)); err != nil {
		return nil, fmt.Errorf("RPC handshake: %w", err)
	}
	tag := []byte("t" + hex.EncodeToString(id[:4]))
	parts := [][]byte{[]byte(method), tag}
	if payload != nil {
		parts = append(parts, payload)
	}
	if err := writeMsg(c, parts); err != nil {
		return nil, err
	}
	for {
		msg, err := readMsg(r)
		if err != nil {
			return nil, err
		}
		if len(msg) >= 2 && string(msg[0]) == "REPLY" && string(msg[1]) == string(tag) {
			if len(msg) < 3 {
				return nil, nil
			}
			return msg[2], nil
		}
		if len(msg) >= 1 && strings.HasPrefix(string(msg[0]), "UNKNOWNCOMMAND") {
			return nil, &UnknownCommandError{Method: method}
		}
		if len(msg) >= 1 && (string(msg[0]) == "FORBIDDEN" || string(msg[0]) == "FORBIDDEN_SN") {
			return nil, fmt.Errorf("Session Router refused %s (not allowed)", method)
		}
	}
}

// UnknownCommandError means the router doesn't have that RPC (e.g. a stock
// build without MapExit).
type UnknownCommandError struct{ Method string }

func (e *UnknownCommandError) Error() string {
	return "this Session Router doesn't support " + e.Method
}

func handshake(w io.Writer, r *bufio.Reader, identity string) error {
	// Greeting: signature, version 3.0, mechanism NULL, as-server 0, filler.
	g := make([]byte, 64)
	g[0] = 0xFF
	g[9] = 0x7F
	g[10] = 3
	g[11] = 0
	copy(g[12:], "NULL")
	if _, err := w.Write(g); err != nil {
		return err
	}
	pg := make([]byte, 64)
	if _, err := io.ReadFull(r, pg); err != nil {
		return err
	}
	if pg[0] != 0xFF || pg[9] != 0x7F || pg[10] < 3 {
		return errors.New("not a ZMTP 3 peer")
	}
	if strings.TrimRight(string(pg[12:32]), "\x00") != "NULL" {
		return errors.New("peer wants a security mechanism other than NULL")
	}
	// READY command with Socket-Type and Identity.
	var body []byte
	body = append(body, 5)
	body = append(body, "READY"...)
	body = appendProp(body, "Socket-Type", "DEALER")
	body = appendProp(body, "Identity", identity)
	if err := writeFrame(w, 0x04, body); err != nil {
		return err
	}
	flags, cmd, err := readFrame(r)
	if err != nil {
		return err
	}
	if flags&0x04 == 0 || len(cmd) < 6 || string(cmd[1:6]) == "ERROR" {
		return errors.New("peer did not send READY")
	}
	return nil
}

func appendProp(b []byte, k, v string) []byte {
	b = append(b, byte(len(k)))
	b = append(b, k...)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(v)))
	b = append(b, l[:]...)
	return append(b, v...)
}

func writeFrame(w io.Writer, flags byte, body []byte) error {
	var hdr []byte
	if len(body) > 255 {
		hdr = make([]byte, 9)
		hdr[0] = flags | 0x02
		binary.BigEndian.PutUint64(hdr[1:], uint64(len(body)))
	} else {
		hdr = []byte{flags, byte(len(body))}
	}
	if _, err := w.Write(append(hdr, body...)); err != nil {
		return err
	}
	return nil
}

func writeMsg(w io.Writer, parts [][]byte) error {
	for i, p := range parts {
		var f byte
		if i < len(parts)-1 {
			f = 0x01
		}
		if err := writeFrame(w, f, p); err != nil {
			return err
		}
	}
	return nil
}

const maxFrame = 4 << 20

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	flags, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var n uint64
	if flags&0x02 != 0 {
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	} else {
		b, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n = uint64(b)
	}
	if n > maxFrame {
		return 0, nil, errors.New("RPC reply too large")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return flags, body, nil
}

func readMsg(r *bufio.Reader) ([][]byte, error) {
	var parts [][]byte
	for {
		flags, body, err := readFrame(r)
		if err != nil {
			return nil, err
		}
		if flags&0x04 != 0 { // a command (e.g. PING): skip
			continue
		}
		parts = append(parts, body)
		if flags&0x01 == 0 {
			return parts, nil
		}
		if len(parts) > 64 {
			return nil, errors.New("RPC reply has too many parts")
		}
	}
}
