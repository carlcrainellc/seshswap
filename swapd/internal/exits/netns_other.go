//go:build !linux

package exits

import (
	"errors"
	"net"
	"time"
)

// DialInNetNS is Linux only (the seller kit runs on Linux).
func DialInNetNS(ns, network, addr string, timeout time.Duration) (net.Conn, error) {
	return nil, errors.New("network namespaces are Linux only")
}
