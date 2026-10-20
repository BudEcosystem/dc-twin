//go:build !linux

package ibsim

import "syscall"

// sysProcAttr: ibsim and its LD_PRELOAD library are Linux software; elsewhere the adapter only
// renders the topology unless an ibsim binary is configured, and a parent-death signal is a
// Linux facility.
func sysProcAttr() *syscall.SysProcAttr { return nil }
