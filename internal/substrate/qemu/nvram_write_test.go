package qemu

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf16"
)

// varStore builds an authenticated edk2 variable store holding the given variables in order.
func varStore(t *testing.T, vars ...[]byte) string {
	t.Helper()
	var store []byte
	for _, v := range vars {
		store = append(store, v...)
	}
	fv := make([]byte, 72)
	binary.LittleEndian.PutUint64(fv[32:], 0x10000)
	copy(fv[40:], "_FVH")
	binary.LittleEndian.PutUint16(fv[48:], 72)
	vsh := make([]byte, 28)
	copy(vsh, authVarGUID[:])
	binary.LittleEndian.PutUint32(vsh[16:], uint32(28+len(store)+64))
	vsh[20], vsh[21] = 0x5a, 0xfe
	img := append(append(fv, vsh...), store...)
	img = append(img, make([]byte, 0x10000-len(img))...)
	for i := 72 + 28 + len(store); i < len(img); i++ {
		img[i] = 0xff
	}
	p := filepath.Join(t.TempDir(), "vars.fd")
	if err := os.WriteFile(p, img, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func authVar(name string, state byte, data []byte) []byte {
	var n []byte
	for _, c := range append(utf16.Encode([]rune(name)), 0) {
		n = binary.LittleEndian.AppendUint16(n, c)
	}
	h := make([]byte, 60)
	binary.LittleEndian.PutUint16(h[0:], 0x55AA)
	h[2] = state
	binary.LittleEndian.PutUint32(h[36:], uint32(len(n)))
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	copy(h[44:], efiGlobal[:])
	v := append(append(h, n...), data...)
	for len(v)%4 != 0 {
		v = append(v, 0xff)
	}
	return v
}

func loadOption(desc string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 1)
	b = binary.LittleEndian.AppendUint16(b, 4)
	for _, c := range append(utf16.Encode([]rune(desc)), 0) {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return append(b, 0x7f, 0xff, 0x04, 0x00)
}

// A boot order set through Redfish is written into the variable the firmware reads (the last
// BootOrder in the added state), as a permutation of the options it holds; anything else is
// refused and the store is left as it was.
func TestWriteBootOrder(t *testing.T) {
	order := func(ids ...uint16) []byte {
		var b []byte
		for _, id := range ids {
			b = binary.LittleEndian.AppendUint16(b, id)
		}
		return b
	}
	p := varStore(t,
		authVar("BootOrder", 0x3c, order(3, 2, 1)), // deleted: an earlier order
		authVar("Boot0001", 0x3f, loadOption("UEFI PXEv4 (MAC:B83FD28ECE7B)")),
		authVar("Boot0002", 0x3f, loadOption("UEFI HTTPv4 (MAC:B83FD28ECE7B)")),
		authVar("Boot0003", 0x3f, loadOption("EFI Internal Shell")),
		authVar("BootOrder", 0x3f, order(1, 2, 3)))
	if err := WriteBootOrder(p, []string{"Boot0002", "Boot0001", "Boot0003"}); err != nil {
		t.Fatal(err)
	}
	nv, err := ReadNVRAM(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nv.BootOrder, []string{"Boot0002", "Boot0001", "Boot0003"}) || nv.Entries[0].Description != "UEFI HTTPv4 (MAC:B83FD28ECE7B)" {
		t.Fatalf("after the write: %+v", nv)
	}
	for _, bad := range [][]string{{"Boot0002", "Boot0001"}, {"Boot0002", "Boot0001", "Boot0004"}, {"Boot0002", "Boot0002", "Boot0003"}, {"Boot0002", "Boot0001", "Shell"}} {
		if err := WriteBootOrder(p, bad); err == nil {
			t.Errorf("%v was written", bad)
		}
	}
	if nv2, _ := ReadNVRAM(p); !reflect.DeepEqual(nv2.BootOrder, nv.BootOrder) {
		t.Fatalf("a refused write changed the store: %v", nv2.BootOrder)
	}
}

// A load option's device path reads as EDK2 prints it, and says what it boots.
func TestDevicePathText(t *testing.T) {
	node := func(typ, sub byte, data []byte) []byte {
		return append([]byte{typ, sub, byte(4 + len(data)), byte((4 + len(data)) >> 8)}, data...)
	}
	mac := make([]byte, 33)
	copy(mac, []byte{0xb8, 0x3f, 0xd2, 0x8e, 0xce, 0x7b})
	mac[32] = 1
	ipv4 := make([]byte, 23)
	var p []byte
	p = append(p, node(2, 1, []byte{0xd0, 0x41, 0x03, 0x0a, 0, 0, 0, 0})...) // PciRoot(0x0)
	p = append(p, node(1, 1, []byte{0, 2})...)                               // Pci(0x2,0x0)
	p = append(p, node(3, 11, mac)...)
	p = append(p, node(3, 12, ipv4)...)
	http := append(append([]byte{}, p...), node(3, 24, nil)...)
	http = append(http, 0x7f, 0xff, 4, 0)
	want := "PciRoot(0x0)/Pci(0x2,0x0)/MAC(B83FD28ECE7B,0x1)/IPv4(0.0.0.0,0x0,DHCP,0.0.0.0,0.0.0.0,0.0.0.0)/Uri()"
	if got := DevicePathText(http); got != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
	if a := BootAlias(want); a != "UefiHttp" {
		t.Fatalf("alias %s", a)
	}
	if a := BootAlias(DevicePathText(append(p, 0x7f, 0xff, 4, 0))); a != "Pxe" {
		t.Fatalf("PXE alias %s", a)
	}
	shell := node(4, 6, []byte{0x83, 0xa5, 0x04, 0x7c, 0x3e, 0x9e, 0x1c, 0x4f, 0xad, 0x65, 0xe0, 0x52, 0x68, 0xd0, 0xb4, 0xd1})
	if got := DevicePathText(shell); got != "FvFile(7C04A583-9E3E-4F1C-AD65-E05268D0B4D1)" || BootAlias(got) != "UefiShell" {
		t.Fatalf("shell %s %s", got, BootAlias(got))
	}
}
