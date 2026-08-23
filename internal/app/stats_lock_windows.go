//go:build windows

package app

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	statsLockExclusiveFlag = 0x00000002 // LOCKFILE_EXCLUSIVE_LOCK
	statsLockLengthLow     = 0xffffffff
	statsLockLengthHigh     = 0xffffffff
)

// lockStatsFileExclusive acquires an exclusive cross-process advisory lock on
// the open file handle behind f (see stats_lock_unix.go for rationale).
func lockStatsFileExclusive(f *os.File) error {
	h := windows.Handle(syscall.Handle(f.Fd()))
	var overlapped windows.Overlapped
	return windows.LockFileEx(h, statsLockExclusiveFlag, 0, statsLockLengthLow, statsLockLengthHigh, &overlapped)
}

// lockStatsFileShared acquires a shared cross-process advisory lock for readers.
func lockStatsFileShared(f *os.File) error {
	h := windows.Handle(syscall.Handle(f.Fd()))
	var overlapped windows.Overlapped
	return windows.LockFileEx(h, 0, 0, statsLockLengthLow, statsLockLengthHigh, &overlapped)
}

// unlockStatsFile releases a previously held advisory lock.
func unlockStatsFile(f *os.File) error {
	h := windows.Handle(syscall.Handle(f.Fd()))
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(h, 0, statsLockLengthLow, statsLockLengthHigh, &overlapped)
}
