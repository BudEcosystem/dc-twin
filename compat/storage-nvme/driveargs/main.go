// driveargs prints the QEMU arguments the twin gives a vm host's NVMe drives
// (internal/substrate/qemu.DriveArgs), one per line, for compat/storage-nvme/run.sh.
//
//	driveargs <qemu-system binary> <backing>:<serial>:<model>:<firmware>:<eui64> ...
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "driveargs <qemu-system binary> <backing>:<serial>:<model>:<firmware>:<eui64> ...")
		os.Exit(2)
	}
	in := &qemu.Installation{Binary: os.Args[1]}
	var drives []qemu.Drive
	for _, spec := range os.Args[2:] {
		f := strings.SplitN(spec, ":", 5)
		if len(f) != 5 {
			fmt.Fprintf(os.Stderr, "drive %q: <backing>:<serial>:<model>:<firmware>:<eui64>\n", spec)
			os.Exit(2)
		}
		drives = append(drives, qemu.Drive{Path: f[0], Serial: f[1], Model: f[2], Firmware: f[3], EUI64: f[4]})
	}
	for _, a := range qemu.DriveArgs(drives, in.NVMeProps()) {
		fmt.Println(a)
	}
}
