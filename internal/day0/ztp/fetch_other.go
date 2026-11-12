//go:build !linux

package ztp

import "syscall"

// bindToDevice is Linux's SO_BINDTODEVICE (fetch_linux.go); elsewhere (a developer's machine,
// where there is no VRF mgmt) sockets are left unbound.
func bindToDevice(string) func(network, address string, c syscall.RawConn) error {
	return nil
}
