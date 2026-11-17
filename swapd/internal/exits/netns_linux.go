package exits

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// DialInNetNS opens a connection from inside the network namespace ns (a
// name under /run/netns, or a path). The socket keeps that namespace after
// the thread switches back. Needs root.
func DialInNetNS(ns, network, addr string, timeout time.Duration) (net.Conn, error) {
	path := ns
	if !strings.Contains(ns, "/") {
		path = "/run/netns/" + ns
	}
	target, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("network namespace %s: %w", ns, err)
	}
	defer target.Close()
	runtime.LockOSThread()
	self, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
	if err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	defer self.Close()
	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("enter network namespace %s: %w", ns, err)
	}
	c, derr := (&net.Dialer{Timeout: timeout}).Dial(network, addr)
	if err := unix.Setns(int(self.Fd()), unix.CLONE_NEWNET); err != nil {
		// this thread is stuck in the other namespace: leave it locked so
		// it exits with the goroutine
		if c != nil {
			c.Close()
		}
		return nil, fmt.Errorf("leave network namespace %s: %w", ns, err)
	}
	runtime.UnlockOSThread()
	return c, derr
}
