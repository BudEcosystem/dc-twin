// Command dctwin-day0 is the twin's day-0 programs for a factory switch (18), one static binary
// mounted read-only at /opt/dctwin/bin in the switch's container (D72): its NOS's ZTP client
// (D74), Cumulus's `nv` CLI over the twin's NVUE (D76), ONIE's discovery (D75), and a DHCP
// client for a management port whose NOS image has none (docker-sonic-vs, Q25). It runs as
// `dctwin-day0 <program> <args>`, or as the program itself through a link of that name (ztp,
// nv, onie-discover), as a switch has them on its PATH.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BudEcosystem/dc-twin/internal/day0/clsys"
	"github.com/BudEcosystem/dc-twin/internal/day0/dhcpc"
	"github.com/BudEcosystem/dc-twin/internal/day0/nvcli"
	"github.com/BudEcosystem/dc-twin/internal/day0/onie"
	"github.com/BudEcosystem/dc-twin/internal/day0/ztp"
)

var programs = map[string]func([]string) int{
	"ztp":           ztp.Main,
	"nv":            nvcli.Main,
	"onie-discover": onie.Main,
	"dhcp":          dhcpc.Main,
	// Cumulus's system beyond NVUE (clsys): ip (vrf exec), onie-install, reboot, the os-release
	// files, and the VRF binding the relay runs under
	"ip":               clsys.IP,
	"onie-install":     clsys.OnieInstall,
	"reboot":           clsys.RebootCmd,
	"os-release":       clsys.OSRelease,
	"dctwin-vrf-exec":  clsys.VRFExec,
	"decode-syseeprom": clsys.DecodeSyseeprom,
	"timedatectl":      clsys.Timedatectl,
	// SONiC's docker exec into its service containers, carried out by the twin
	"docker": clsys.Docker,
	// sudo where the image has none (sonic-vs), installed setuid root
	"sudo": clsys.Sudo,
	// SONiC's sshd ForceCommand: each SSH command run, then told to the twin
	"dctwin-ssh-command": clsys.SSHCommand,
	// copies this binary into a directory: the chart's tap substrate fills the directory its
	// engine mounts in switches from the twin's own image, which has no shell to copy with
	"install-to": installTo,
}

// installTo copies the running binary to <dir>/dctwin-day0, executable.
func installTo(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: dctwin-day0 install-to <dir>")
		return 2
	}
	if err := copyExecutable(filepath.Join(args[0], "dctwin-day0")); err != nil {
		fmt.Fprintln(os.Stderr, "dctwin-day0 install-to:", err)
		return 1
	}
	return 0
}

func copyExecutable(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func main() {
	name, args := filepath.Base(os.Args[0]), os.Args[1:]
	if _, ok := programs[name]; !ok {
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: dctwin-day0 ztp|nv|onie-discover|dhcp|ip|onie-install|reboot|os-release|dctwin-vrf-exec [args]")
			os.Exit(2)
		}
		name, args = args[0], args[1:]
	}
	run, ok := programs[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "dctwin-day0: no program %q (ztp, nv, onie-discover, dhcp, ip, onie-install, reboot, os-release, dctwin-vrf-exec)\n", name)
		os.Exit(2)
	}
	os.Exit(run(args))
}
