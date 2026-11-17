//go:build linux

package srouter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

const capNetAdmin = 12

func setSysProcAttr(cmd *exec.Cmd) {
	// Own process group (so a terminal Ctrl-C reaches the wallet first, which
	// then stops the router cleanly), and die with the wallet if it crashes.
	// Linux drops the parent-death signal when a setcap binary is executed,
	// which is why production launches go through the guard (RunGuard).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

// CheckPermissions makes sure the router can create its network adapter
// without running as root: either the binary has cap_net_admin+ep, or this
// process can already create a TUN (some sandboxes allow it).
func CheckPermissions(bin string) error {
	if os.Geteuid() == 0 {
		return &UserError{Msg: "The wallet is running as root. Please run it as your normal user; the private connection only needs a one-time setup step, not root."}
	}
	if ok, _ := FileHasNetAdmin(bin); ok {
		return nil
	}
	if CanCreateTUN() {
		return nil
	}
	return &UserError{Msg: "The private connection isn't set up yet: Session Router needs permission to create its own network adapter. " +
		strings.Replace(setcapHint, "<wallet>/session-router/bin/session-router", bin, 1)}
}

// FileHasNetAdmin reports whether bin carries CAP_NET_ADMIN in its permitted
// file capabilities with the effective flag set (setcap cap_net_admin+ep).
func FileHasNetAdmin(bin string) (bool, error) {
	buf := make([]byte, 64)
	n, err := syscall.Getxattr(bin, "security.capability", buf)
	if err != nil {
		return false, err
	}
	return ParseFileCaps(buf[:n], capNetAdmin)
}

// ParseFileCaps decodes a vfs_cap_data xattr and reports whether capability
// bit c is permitted and the effective flag is set.
func ParseFileCaps(b []byte, c uint) (bool, error) {
	if len(b) < 12 {
		return false, fmt.Errorf("capability xattr too short (%d bytes)", len(b))
	}
	magic := binary.LittleEndian.Uint32(b[0:4])
	rev := magic & 0xFF000000
	eff := magic&0x000001 != 0
	switch rev {
	case 0x01000000, 0x02000000, 0x03000000:
	default:
		return false, fmt.Errorf("unknown capability revision %#x", rev)
	}
	idx := c / 32
	off := 4 + int(idx)*8
	if off+4 > len(b) {
		return false, nil
	}
	perm := binary.LittleEndian.Uint32(b[off : off+4])
	return eff && perm&(1<<(c%32)) != 0, nil
}

// CanCreateTUN tries to create (and immediately drops) a non-persistent TUN.
func CanCreateTUN() bool {
	fd, err := syscall.Open("/dev/net/tun", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	var ifr [40]byte
	name := fmt.Sprintf("swprobe%04x", os.Getpid()&0xffff)
	copy(ifr[:15], name)
	binary.LittleEndian.PutUint16(ifr[16:18], 0x0001|0x1000) // IFF_TUN | IFF_NO_PI
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(0x400454ca), uintptr(unsafe.Pointer(&ifr[0])))
	return e == 0
}

func terminate(p *os.Process) { _ = p.Signal(syscall.SIGTERM) }

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
