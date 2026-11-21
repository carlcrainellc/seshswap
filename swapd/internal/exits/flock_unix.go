//go:build !windows

package exits

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f (flock), waiting for it.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
