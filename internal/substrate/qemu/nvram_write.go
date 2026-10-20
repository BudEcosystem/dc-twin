package qemu

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// WriteBootOrder rewrites the BootOrder variable of a machine's UEFI variable store in place,
// to an order of the same load options: what a BMC does when a client changes a host's boot
// order through Redfish and the host next starts. Only a permutation of the order the store
// holds is accepted (the same Boot#### options, so the variable keeps its size and the store's
// layout is untouched). The machine must not be running: QEMU holds the store while it runs.
func WriteBootOrder(path string, order []string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 64)
	if _, err := io.ReadFull(f, head); err != nil {
		return err
	}
	if string(head[40:44]) != "_FVH" {
		return fmt.Errorf("%s: not a firmware volume", path)
	}
	fvLen := binary.LittleEndian.Uint64(head[32:40])
	hdrLen := int(binary.LittleEndian.Uint16(head[48:50]))
	if fvLen == 0 || fvLen > 64<<20 {
		return fmt.Errorf("%s: firmware volume length %d", path, fvLen)
	}
	buf := make([]byte, fvLen)
	if _, err := f.ReadAt(buf, 0); err != nil && err != io.EOF {
		return err
	}
	if hdrLen+28 > len(buf) {
		return fmt.Errorf("%s: short variable store", path)
	}
	vs := buf[hdrLen:]
	var sig [16]byte
	copy(sig[:], vs[:16])
	auth := sig == authVarGUID
	if !auth && sig != plainVarGUD {
		return fmt.Errorf("%s: unknown variable store format", path)
	}
	size := int(binary.LittleEndian.Uint32(vs[16:20]))
	if size > len(vs) {
		size = len(vs)
	}
	hsize := 32
	if auth {
		hsize = 60
	}
	// the last BootOrder in the added state is the one the firmware reads (as ReadNVRAM takes it)
	dataAt, dataLen := -1, 0
	for off := 28; off+hsize <= size; {
		h := vs[off:]
		if binary.LittleEndian.Uint16(h[0:2]) != 0x55AA {
			break
		}
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
		if st := h[2]; (st == 0x3f || st == 0x3e) && vendor == efiGlobal && ucs2(vs[off+hsize:off+hsize+nameSize]) == "BootOrder" {
			dataAt, dataLen = hdrLen+off+hsize+nameSize, dataSize
		}
		off = (end + 3) &^ 3
	}
	if dataAt < 0 {
		return fmt.Errorf("%s has no BootOrder variable", path)
	}
	cur := map[uint16]bool{}
	for i := 0; i+1 < dataLen; i += 2 {
		cur[binary.LittleEndian.Uint16(buf[dataAt+i:])] = true
	}
	if len(order)*2 != dataLen {
		return fmt.Errorf("BootOrder %v is not an order of the %d load options the firmware holds", order, dataLen/2)
	}
	data := make([]byte, 0, dataLen)
	seen := map[uint16]bool{}
	for _, id := range order {
		n, err := strconv.ParseUint(strings.TrimPrefix(id, "Boot"), 16, 16)
		if err != nil || !strings.HasPrefix(id, "Boot") || !cur[uint16(n)] || seen[uint16(n)] {
			return fmt.Errorf("BootOrder %v is not an order of the load options the firmware holds", order)
		}
		seen[uint16(n)] = true
		data = binary.LittleEndian.AppendUint16(data, uint16(n))
	}
	_, err = f.WriteAt(data, int64(dataAt))
	return err
}
