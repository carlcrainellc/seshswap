package exits

import (
	"bufio"
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
)

// fakeOxenMQ is a one-connection ROUTER stand-in: ZMTP 3.0 NULL handshake,
// then it answers one request. reply(method, payload) returns the frames.
func fakeOxenMQ(t *testing.T, reply func(method string, payload []byte, tag []byte) [][]byte) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "rpc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				if err := handshake(c, r, ""); err != nil {
					return
				}
				msg, err := readMsg(r)
				if err != nil || len(msg) < 2 {
					return
				}
				var payload []byte
				if len(msg) > 2 {
					payload = msg[2]
				}
				_ = writeMsg(c, reply(string(msg[0]), payload, msg[1]))
			}(c)
		}
	}()
	return "ipc://" + sock
}

func TestOMQCallAgainstFakeRouter(t *testing.T) {
	ep := fakeOxenMQ(t, func(m string, p, tag []byte) [][]byte {
		switch m {
		case "llarp.map_exit":
			return [][]byte{[]byte("REPLY"), tag, append([]byte(`{"result":"got `), append(p, []byte(`"}`)...)...)}
		case "llarp.version":
			return [][]byte{[]byte("REPLY"), tag, []byte(`{"result":{"version_full":"session-router-1.1.1-test"}}`)}
		}
		return [][]byte{[]byte("UNKNOWNCOMMAND"), []byte(m)}
	})
	ctx := context.Background()
	out, err := OMQCall(ctx, ep, "llarp.map_exit", []byte(`x`))
	if err != nil || string(out) != `{"result":"got x"}` {
		t.Fatalf("map_exit: %q %v", out, err)
	}
	v, err := (&RouterRPC{Endpoint: ep}).Version(ctx)
	if err != nil || v != "session-router-1.1.1-test" {
		t.Fatalf("version: %q %v", v, err)
	}
	var uc *UnknownCommandError
	if _, err := OMQCall(ctx, ep, "llarp.list_exits", nil); !errors.As(err, &uc) {
		t.Fatalf("unknown command: %v", err)
	}
}
