//go:build !linux

package app

// dropContainerPrivileges is a no-op outside Linux. The container image is
// Linux-only; this keeps local builds (darwin/windows/freebsd) compiling.
func dropContainerPrivileges() error { return nil }
