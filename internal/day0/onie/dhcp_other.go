//go:build !linux

package onie

import "fmt"

// System is Linux-only: ONIE's stand-in runs in a Linux switch container.
func System(iface string) (DHCPClient, NetConfig, error) {
	return nil, nil, fmt.Errorf("ONIE's stand-in runs only on Linux (%s)", iface)
}
