//go:build windows

package exits

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on f (LockFileEx on the whole file),
// waiting for it, like flock(LOCK_EX) does on Linux.
func lockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol)
}

func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
