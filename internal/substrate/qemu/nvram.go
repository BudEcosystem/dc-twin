package qemu

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"unicode/utf16"
)

// BootEntry is one UEFI load option (Boot####) as the firmware holds it in NVRAM.
type BootEntry struct {
	ID          string `json:"id"` // Boot0001
	Description string `json:"description"`
	Active      bool   `json:"active"`
	// DevicePath is the option's device path in UEFI's text form (devicepath.go), e.g.
	// PciRoot(0x0)/Pci(0x2,0x0)/MAC(B83FD28ECE7B,0x1)/IPv4(0.0.0.0,0x0,DHCP,…)/Uri()
	DevicePath string `json:"devicePath,omitempty"`
}

// NVRAM is what the boot-order observer reads from a machine's UEFI variable store: the boot
// order and the load options it names, as the firmware (or an OS installer) last wrote them.
type NVRAM struct {
	BootOrder []string    `json:"bootOrder"`
	Entries   []BootEntry `json:"entries"`
	BootNext  string      `json:"bootNext,omitempty"`
	Variables int         `json:"variables"`
}

var (
	efiGlobal   = guid("8be4df61-93ca-11d2-aa0d-00e098032b8c")
	authVarGUID = guid("aaf32c78-947b-439a-a180-2e144ec37792")
	plainVarGUD = guid("ddcf3616-3275-4164-98b6-fe85707ffe7d")
)

// guid encodes a GUID's text form in its in-memory (mixed-endian) layout.
func guid(s string) [16]byte {
	var g [16]byte
	var a uint32
	var b, c uint16
	var d [8]byte
	fmt.Sscanf(s, "%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x", &a, &b, &c, &d[0], &d[1], &d[2], &d[3], &d[4], &d[5], &d[6], &d[7])
	binary.LittleEndian.PutUint32(g[0:], a)
	binary.LittleEndian.PutUint16(g[4:], b)
	binary.LittleEndian.PutUint16(g[6:], c)
	copy(g[8:], d[:])
	return g
}

// ReadNVRAM parses an edk2 variable store (OVMF/AAVMF vars file): the firmware volume header,
// the variable store header, then each variable that is in the added state.
func ReadNVRAM(path string) (*NVRAM, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 64)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, err
	}
	if string(head[40:44]) != "_FVH" {
		return nil, fmt.Errorf("%s: not a firmware volume", path)
	}
	fvLen := binary.LittleEndian.Uint64(head[32:40])
	hdrLen := int(binary.LittleEndian.Uint16(head[48:50]))
	if fvLen == 0 || fvLen > 64<<20 {
		return nil, fmt.Errorf("%s: firmware volume length %d", path, fvLen)
	}
	buf := make([]byte, fvLen)
	if _, err := f.ReadAt(buf, 0); err != nil && err != io.EOF {
		return nil, err
	}
	if hdrLen+28 > len(buf) {
		return nil, fmt.Errorf("%s: short variable store", path)
	}
	vs := buf[hdrLen:]
	var sig [16]byte
	copy(sig[:], vs[:16])
	auth := sig == authVarGUID
	if !auth && sig != plainVarGUD {
		return nil, fmt.Errorf("%s: unknown variable store format", path)
	}
	size := int(binary.LittleEndian.Uint32(vs[16:20]))
	if size > len(vs) {
		size = len(vs)
	}
	hsize := 32
	if auth {
		hsize = 60
	}
	vars := map[string][]byte{}
	n := 0
	for off := 28; off+hsize <= size; {
		h := vs[off:]
		if binary.LittleEndian.Uint16(h[0:2]) != 0x55AA {
			break
		}
		state := h[2]
		var nameSize, dataSize int
		var vendor [16]byte
		if auth {
			nameSize = int(binary.LittleEndian.Uint32(h[36:40]))
			dataSize = int(binary.LittleEndian.Uint32(h[40:44]))
			copy(vendor[:], h[44:60])
		} else {
			nameSize = int(binary.LittleEndian.Uint32(h[8:12]))
			dataSize = int(binary.LittleEndian.Uint32(h[12:16]))
			copy(vendor[:], h[16:32])
		}
		end := off + hsize + nameSize + dataSize
		if nameSize < 0 || dataSize < 0 || end > size {
			break
		}
		if state == 0x3f || state == 0x3e { // VAR_ADDED, or added and in deleted transition
			n++
			if vendor == efiGlobal {
				name := ucs2(vs[off+hsize : off+hsize+nameSize])
				vars[name] = append([]byte{}, vs[off+hsize+nameSize:end]...)
			}
		}
		off = (end + 3) &^ 3
	}
	out := &NVRAM{Variables: n}
	if bo, ok := vars["BootOrder"]; ok {
		for i := 0; i+1 < len(bo); i += 2 {
			out.BootOrder = append(out.BootOrder, fmt.Sprintf("Boot%04X", binary.LittleEndian.Uint16(bo[i:])))
		}
	}
	if bn, ok := vars["BootNext"]; ok && len(bn) >= 2 {
		out.BootNext = fmt.Sprintf("Boot%04X", binary.LittleEndian.Uint16(bn))
	}
	for _, id := range out.BootOrder {
		opt, ok := vars[id]
		if !ok || len(opt) < 6 {
			continue
		}
		attrs := binary.LittleEndian.Uint32(opt[0:4])
		e := BootEntry{ID: id, Description: ucs2(opt[6:]), Active: attrs&1 == 1}
		// the device path list follows the description's terminating NUL
		pathLen := int(binary.LittleEndian.Uint16(opt[4:6]))
		for i := 6; i+1 < len(opt); i += 2 {
			if opt[i] == 0 && opt[i+1] == 0 {
				if i+2+pathLen <= len(opt) {
					e.DevicePath = DevicePathText(opt[i+2 : i+2+pathLen])
				}
				break
			}
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// ucs2 decodes a NUL-terminated UTF-16LE string.
func ucs2(b []byte) string {
	var u []uint16
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(bytes.TrimSpace([]byte(string(utf16.Decode(u)))))
}
