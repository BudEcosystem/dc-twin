//go:build linux

package ztp

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// bindToDevice returns a dialer Control that binds each socket to dev with SO_BINDTODEVICE, as
// `ip vrf exec mgmt` effectively does for a program it starts: a socket bound to a VRF device
// routes by that VRF's table, so the fetch leaves through eth0 in VRF mgmt (D73).
func bindToDevice(dev string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.BindToDevice(int(fd), dev)
		}); err != nil {
			return err
		}
		return serr
	}
}
