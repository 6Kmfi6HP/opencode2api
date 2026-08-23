//go:build !windows

package app

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockStatsFileExclusive acquires an exclusive cross-process advisory lock on
// the open file description behind f. Use it to serialize writers of the
// shared token-stats file across independently launched binary instances
// (long-running server plus short-lived `opencode2api launch claude|codex`
// proxies) so a stale per-process snapshot can never overwrite another
// process's accumulated usage.
func lockStatsFileExclusive(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX) }

// lockStatsFileShared acquires a shared cross-process advisory lock so readers
// can take a stable snapshot without colliding with concurrent writers.
func lockStatsFileShared(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_SH) }

// unlockStatsFile releases a previously held advisory lock.
func unlockStatsFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
