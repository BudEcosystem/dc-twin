package qemu

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"strings"
)

// DevicePathText renders a UEFI device path (a load option's FilePathList) in the text form the
// UEFI specification gives it and EDK2 prints, for the nodes a firmware's boot options carry:
// PciRoot and Pci, MAC, IPv4, IPv6 and Uri for network boot, and the firmware-volume, hard-drive
// and file-path media nodes. Any other node is Path(type,subtype,data). It stops at the first
// end node.
func DevicePathText(b []byte) string {
	var nodes []string
	for len(b) >= 4 {
		t, st, n := b[0], b[1], int(binary.LittleEndian.Uint16(b[2:4]))
		if n < 4 || n > len(b) {
			break
		}
		d := b[4:n]
		if t == 0x7f {
			break
		}
		nodes = append(nodes, pathNode(t, st, d))
		b = b[n:]
	}
	return strings.Join(nodes, "/")
}

func pathNode(t, st byte, d []byte) string {
	u16 := func(i int) uint16 { return binary.LittleEndian.Uint16(d[i:]) }
	u32 := func(i int) uint32 { return binary.LittleEndian.Uint32(d[i:]) }
	ip4 := func(i int) string { return netip.AddrFrom4([4]byte(d[i : i+4])).String() }
	ip6 := func(i int) string {
		var x []string
		for j := 0; j < 16; j += 2 {
			x = append(x, fmt.Sprintf("%04x", binary.BigEndian.Uint16(d[i+j:])))
		}
		return strings.Join(x, ":")
	}
	switch {
	case t == 1 && st == 1 && len(d) >= 2:
		return fmt.Sprintf("Pci(0x%X,0x%X)", d[1], d[0])
	case t == 2 && st == 1 && len(d) >= 8:
		switch u32(0) {
		case 0x0a0341d0:
			return fmt.Sprintf("PciRoot(0x%X)", u32(4))
		case 0x0a0841d0:
			return fmt.Sprintf("PcieRoot(0x%X)", u32(4))
		}
		return fmt.Sprintf("Acpi(0x%08X,0x%X)", u32(0), u32(4))
	case t == 3 && st == 11 && len(d) >= 33:
		size := 32
		if d[32] == 0 || d[32] == 1 {
			size = 6
		}
		return fmt.Sprintf("MAC(%X,0x%X)", d[:size], d[32])
	case t == 3 && st == 12 && len(d) >= 15:
		origin := "DHCP"
		if d[14] != 0 {
			origin = "Static"
		}
		gw, mask := "0.0.0.0", "0.0.0.0"
		if len(d) >= 23 {
			gw, mask = ip4(15), ip4(19)
		}
		return fmt.Sprintf("IPv4(%s,0x%X,%s,%s,%s,%s)", ip4(4), u16(12), origin, ip4(0), gw, mask)
	case t == 3 && st == 13 && len(d) >= 40:
		origin := map[byte]string{0: "Static", 1: "StatelessAutoConfigure", 2: "StatefulAutoConfigure"}[d[38]]
		gw := "0000:0000:0000:0000:0000:0000:0000:0000"
		if len(d) >= 56 {
			gw = ip6(40)
		}
		return fmt.Sprintf("IPv6(%s,0x%X,%s,%s,0x%X,%s)", ip6(16), u16(36), origin, ip6(0), d[39], gw)
	case t == 3 && st == 24:
		return "Uri(" + string(d) + ")"
	case t == 4 && st == 1 && len(d) >= 38:
		sig := d[22:38]
		if d[37] == 2 { // GPT
			return fmt.Sprintf("HD(%d,GPT,%s,0x%X,0x%X)", u32(0), guidText(sig), binary.LittleEndian.Uint64(d[4:]), binary.LittleEndian.Uint64(d[12:]))
		}
		return fmt.Sprintf("HD(%d,MBR,0x%X,0x%X,0x%X)", u32(0), u32(22), binary.LittleEndian.Uint64(d[4:]), binary.LittleEndian.Uint64(d[12:]))
	case t == 4 && st == 4:
		return ucs2(d)
	case t == 4 && st == 6 && len(d) >= 16:
		return "FvFile(" + guidText(d[:16]) + ")"
	case t == 4 && st == 7 && len(d) >= 16:
		return "Fv(" + guidText(d[:16]) + ")"
	}
	return fmt.Sprintf("Path(%d,%d,%X)", t, st, d)
}

// guidText is a GUID's text form from its in-memory (mixed-endian) layout.
func guidText(g []byte) string {
	return fmt.Sprintf("%08X-%04X-%04X-%X-%X", binary.LittleEndian.Uint32(g[0:]), binary.LittleEndian.Uint16(g[4:]),
		binary.LittleEndian.Uint16(g[6:]), g[8:10], g[10:16])
}

// BootAlias is the Redfish BootSourceOverrideTarget a load option boots, from its device path:
// UefiHttp for a URI (HTTP boot), Pxe for an IP node without one, UefiShell and BiosSetup for
// EDK2's shell and setup applications, Hdd for a disk partition; None otherwise.
func BootAlias(devicePath string) string {
	switch p := strings.ToUpper(devicePath); {
	case strings.Contains(p, "/URI("):
		return "UefiHttp"
	case strings.Contains(p, "/IPV4(") || strings.Contains(p, "/IPV6("):
		return "Pxe"
	case strings.Contains(p, "FVFILE(7C04A583-9E3E-4F1C-AD65-E05268D0B4D1)"):
		return "UefiShell"
	case strings.Contains(p, "FVFILE(462CAA21-7614-4503-836E-8AB6F4662331)"):
		return "BiosSetup"
	case strings.Contains(p, "HD("):
		return "Hdd"
	}
	return "None"
}
