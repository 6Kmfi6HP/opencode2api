//go:build linux

package app

import "syscall"

// dropContainerPrivileges switches every OS thread to the image's nonroot
// UID/GID. Go's syscall.Setuid/Setgid use AllThreadsSyscall, so no thread keeps
// root credentials. Supplementary groups are cleared first to avoid retaining
// group 0 access after the switch.
func dropContainerPrivileges() error {
	if err := syscall.Setgroups([]int{containerGID}); err != nil {
		return err
	}
	if err := syscall.Setgid(containerGID); err != nil {
		return err
	}
	return syscall.Setuid(containerUID)
}
