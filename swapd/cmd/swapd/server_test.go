package main

import (
	"net"
	"net/http"
	"testing"
	"time"
)

// A client that never finishes its request headers is dropped.
func TestServerDropsSlowClient(t *testing.T) {
	readHeaderTimeout = 200 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer("", http.NotFoundHandler())
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("POST /v1/claim HTTP/1.1\r\nHost: x\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	for {
		if _, err := c.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("slow client still connected after 3s")
			}
			return // closed by the server
		}
	}
}
