//go:build !linux

package dhcpc

import (
	"fmt"
	"os"
)

// Main is `dctwin-day0 dhcp`: a switch's management port is a Linux interface, so the client
// runs on Linux only.
func Main(args []string) int {
	fmt.Fprintln(os.Stderr, "dhcp: runs in a switch's Linux container only")
	return 1
}
