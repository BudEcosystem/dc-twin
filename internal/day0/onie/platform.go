package onie

import (
	"fmt"
	"strings"
)

// Platform is an ONIE platform string taken apart: onie_platform is
// ${onie_arch}-${onie_machine}-r${onie_machine_rev} (rootconf/default/etc/init.d/gen-config.sh;
// build-config/Makefile's PLATFORM = $(ARCH)-$(MACHINE_PREFIX), facts D.1), and onie_machine is
// <vendor>_<model> (mlnx_msn2700, nvidia_sn5600), its vendor part being onie_switch_asic in the
// installer names (facts D.4: x86_64-mlnx).
type Platform struct {
	Arch    string // x86_64
	Machine string // mlnx_msn2700
	Rev     string // 0
	ASIC    string // mlnx
}

// ParsePlatform reads `<arch>-<vendor>_<machine>-r<rev>`. The architecture is everything before
// the first '-' (ONIE's architectures, x86_64, arm64, armv7a, ppc, have none) and the revision
// everything after the last "-r".
func ParsePlatform(s string) (Platform, error) {
	arch, rest, ok := strings.Cut(s, "-")
	i := strings.LastIndex(rest, "-r")
	if !ok || arch == "" || i <= 0 || i+2 == len(rest) {
		return Platform{}, fmt.Errorf("platform %q is not <arch>-<vendor>_<machine>-r<rev>", s)
	}
	p := Platform{Arch: arch, Machine: rest[:i], Rev: rest[i+2:]}
	p.ASIC, _, _ = strings.Cut(p.Machine, "_")
	return p, nil
}

func (p Platform) String() string { return p.Arch + "-" + p.Machine + "-r" + p.Rev }

// InstallerNames are ONIE's default installer file names for os-install, most precise first,
// each bare name followed by its .bin form: lib/onie/functions' onie_default_filenames with
// onie_image_suffixes=".bin", as get_default_filenames lists them (facts D.4).
func (p Platform) InstallerNames() []string {
	const prefix = "onie-installer"
	var out []string
	for _, n := range []string{
		prefix + "-" + p.String(),
		prefix + "-" + p.Arch + "-" + p.Machine,
		prefix + "-" + p.Machine,
		prefix + "-" + p.Arch + "-" + p.ASIC,
		prefix + "-" + p.Arch,
		prefix,
	} {
		out = append(out, n, n+".bin")
	}
	return out
}

// DefaultName is onie_default_filename, the one name the TFTP waterfall tries under the MAC and
// IP directories: "${filename_prefix}-${onie_platform}" (lib/onie/functions).
func (p Platform) DefaultName() string { return "onie-installer-" + p.String() }

// vendorIDs are IANA enterprise numbers for ONIE-VENDOR-ID by the machine's vendor prefix. Only
// Mellanox's is in the facts (machine/mellanox/mlnx_x86/machine.make VENDOR_ID = 33049, D.6);
// NVIDIA-era Spectrum machines (nvidia_*) are given the same, as their ONIE comes from the same
// Mellanox tree. Any other vendor reports OCP's 42623, a stand-in choice.
var vendorIDs = map[string]int{"mlnx": 33049, "nvidia": 33049}

// VendorID is the platform's ONIE-VENDOR-ID.
func (p Platform) VendorID() int {
	if id, ok := vendorIDs[p.ASIC]; ok {
		return id
	}
	return IANAEnterprise
}
