package qemu

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestArgs(t *testing.T) {
	c := Config{Name: "site-1/a01/tray", Arch: "aarch64", Accel: "hvf", CPUs: 2, MemoryMB: 1024, Code: "/fw/code.fd", Vars: "/state/vars.fd",
		UUID: "6a8c3e2a-1b2c-4d5e-8f90-123456789abc", RunDir: "/run/x", Console: "/state/console.log",
		SMBIOS: []SMBIOS{{Type: 1, Fields: [][2]string{{"manufacturer", "ACME, Inc."}, {"product", "Box 9"}, {"version", ""}}}},
		NICs:   []NIC{{MAC: "3c:6d:66:00:00:01", Netdev: "user,restrict=on"}},
		Kernel: "/k", Initrd: "/i", Append: "console=ttyAMA0", TPM: "/run/x/swtpm.sock"}
	a := strings.Join(c.Args(), " ")
	for _, want := range []string{
		"-S", "-machine virt,gic-version=max", "-accel hvf", "-cpu host", "-smp 2", "-m 1024",
		"-drive if=pflash,format=raw,unit=0,readonly=on,file=/fw/code.fd",
		"-drive if=pflash,format=raw,unit=1,file=/state/vars.fd",
		"-smbios type=1,manufacturer=ACME,, Inc.,product=Box 9", // commas doubled, empty fields left to QEMU
		"-netdev user,restrict=on,id=net0 -device virtio-net-pci,romfile=,netdev=net0,mac=3c:6d:66:00:00:01",
		"-device tpm-tis-device,ppi=off,tpmdev=tpm0",
		"-qmp unix:/run/x/qmp.sock,server=on,wait=off",
		"-kernel /k -initrd /i -append console=ttyAMA0",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("args lack %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "Box 9,version") {
		t.Errorf("an empty SMBIOS field was passed: %s", a)
	}
	c.Accel = "tcg"
	if a := strings.Join(c.Args(), " "); !strings.Contains(a, "-cpu max") {
		t.Errorf("tcg needs -cpu max: %s", a)
	}
}

// a minimal edk2 authenticated variable store, as OVMF/AAVMF write it
func TestReadNVRAM(t *testing.T) {
	ucs := func(s string) []byte {
		var b []byte
		for _, c := range append(utf16.Encode([]rune(s)), 0) {
			b = binary.LittleEndian.AppendUint16(b, c)
		}
		return b
	}
	variable := func(name string, vendor [16]byte, state byte, data []byte) []byte {
		n := ucs(name)
		h := make([]byte, 60)
		binary.LittleEndian.PutUint16(h[0:], 0x55AA)
		h[2] = state
		binary.LittleEndian.PutUint32(h[4:], 7)
		binary.LittleEndian.PutUint32(h[36:], uint32(len(n)))
		binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
		copy(h[44:], vendor[:])
		v := append(append(h, n...), data...)
		for len(v)%4 != 0 {
			v = append(v, 0xff)
		}
		return v
	}
	option := func(desc string) []byte {
		b := binary.LittleEndian.AppendUint32(nil, 1) // LOAD_OPTION_ACTIVE
		b = binary.LittleEndian.AppendUint16(b, 4)
		b = append(b, ucs(desc)...)
		return append(b, 0x7f, 0xff, 0x04, 0x00) // end of device path
	}
	order := binary.LittleEndian.AppendUint16(binary.LittleEndian.AppendUint16(nil, 2), 1)
	var store []byte
	store = append(store, variable("BootOrder", efiGlobal, 0x3f, []byte{0, 0})...) // superseded
	store = append(store, variable("BootOrder", efiGlobal, 0x3c, order)...)        // deleted
	store = append(store, variable("Boot0001", efiGlobal, 0x3f, option("UEFI PXEv4 (MAC:3C6D66000001)"))...)
	store = append(store, variable("Boot0002", efiGlobal, 0x3f, option("UiApp"))...)
	store = append(store, variable("BootOrder", efiGlobal, 0x3f, order)...)
	store = append(store, variable("Lang", guid("4c19049f-4137-4dd3-9c10-8b97a83ffdfa"), 0x3f, []byte("en"))...)

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
	nv, err := ReadNVRAM(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nv.BootOrder, []string{"Boot0002", "Boot0001"}) || len(nv.Entries) != 2 ||
		nv.Entries[0].Description != "UiApp" || nv.Entries[1].Description != "UEFI PXEv4 (MAC:3C6D66000001)" || !nv.Entries[1].Active {
		t.Fatalf("read %+v", nv)
	}
	if nv.Variables != 5 {
		t.Fatalf("variables in the added state: %d, want 5", nv.Variables)
	}
}
