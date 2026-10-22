//go:build !windows

package stats

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockStatsFileExclusive acquires an exclusive cross-process advisory lock on
// the shared stats file. Each process has one dedicated writer that holds the
// lock across read-modify-write, so server and launch deltas cannot overwrite
// each other; only those writer goroutines can wait on flock.
func lockStatsFileExclusive(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }

// unlockStatsFile releases a previously held advisory lock.
func unlockStatsFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
