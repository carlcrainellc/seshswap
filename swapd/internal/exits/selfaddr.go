package exits

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"
)

// OwnSesh asks a Session Router's DNS listener (host:port) for its own .sesh
// address: localhost.sesh is a CNAME to it. Works with stock and patched builds.
func OwnSesh(ctx context.Context, dns string) (string, error) {
	var id [2]byte
	_, _ = rand.Read(id[:])
	q := append([]byte{id[0], id[1], 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}, encName("localhost.sesh")...)
	q = append(q, 0, 5, 0, 1) // CNAME, IN
	d := net.Dialer{}
	c, err := d.DialContext(ctx, "udp", dns)
	if err != nil {
		return "", err
	}
	defer c.Close()
	dl := time.Now().Add(3 * time.Second)
	if t, ok := ctx.Deadline(); ok && t.Before(dl) {
		dl = t
	}
	_ = c.SetDeadline(dl)
	if _, err := c.Write(q); err != nil {
		return "", err
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return "", err
	}
	m := buf[:n]
	if n < 12 || m[0] != id[0] || m[1] != id[1] || binary.BigEndian.Uint16(m[6:8]) == 0 {
		return "", errors.New("the router did not say its own address")
	}
	off := 12
	if _, off, err = readName(m, off); err != nil { // question
		return "", err
	}
	off += 4
	_, off, err = readName(m, off) // answer owner
	if err != nil || off+10 > n {
		return "", errors.New("odd DNS answer")
	}
	typ := binary.BigEndian.Uint16(m[off : off+2])
	off += 10
	if typ != 5 {
		return "", errors.New("odd DNS answer")
	}
	name, _, err := readName(m, off)
	if err != nil {
		return "", err
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if !ValidClientSesh(name) {
		return "", errors.New("the router's own address is not a client .sesh: " + name)
	}
	return name, nil
}

func encName(s string) []byte {
	var b []byte
	for _, l := range strings.Split(strings.TrimSuffix(s, "."), ".") {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

func readName(m []byte, off int) (string, int, error) {
	var labels []string
	end, jumps := -1, 0
	for {
		if off >= len(m) {
			return "", 0, errors.New("short DNS name")
		}
		l := int(m[off])
		switch {
		case l == 0:
			off++
			if end < 0 {
				end = off
			}
			return strings.Join(labels, "."), end, nil
		case l&0xC0 == 0xC0:
			if off+1 >= len(m) || jumps > 10 {
				return "", 0, errors.New("bad DNS pointer")
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(m[off:off+2]) & 0x3FFF)
			jumps++
		default:
			if off+1+l > len(m) {
				return "", 0, errors.New("short DNS label")
			}
			labels = append(labels, string(m[off+1:off+1+l]))
			off += 1 + l
		}
	}
}
