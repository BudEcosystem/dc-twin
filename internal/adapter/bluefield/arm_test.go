package bluefield

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// readCpio lists a newc archive's members (name -> data), checking each header's alignment.
func readCpio(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for off := 0; off < len(b); {
		if off%4 != 0 || off+110 > len(b) || string(b[off:off+6]) != "070701" {
			t.Fatalf("no newc header at %d", off)
		}
		field := func(i int) int {
			v, err := strconv.ParseUint(string(b[off+6+i*8:off+14+i*8]), 16, 32)
			if err != nil {
				t.Fatal(err)
			}
			return int(v)
		}
		size, nameLen := field(6), field(11)
		name := string(b[off+110 : off+110+nameLen-1])
		data := off + 110 + nameLen
		data += (4 - data%4) % 4
		if name == "TRAILER!!!" {
			return out
		}
		out[name] = b[data : data+size]
		off = data + size
		off += (4 - off%4) % 4
	}
	t.Fatal("no trailer")
	return nil
}

// TestSplitBFB splits a boot stream as the Arm boots it: the Linux image (decompressed), the
// initramfs with the bf.cfg after it as etc/bf.cfg on a 4-byte boundary, and the boot arguments.
func TestSplitBFB(t *testing.T) {
	dir := t.TempDir()
	kernel := make([]byte, 128)
	copy(kernel[56:], "ARM\x64")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(kernel)
	zw.Close()
	initramfs := []byte("initramfs bytes, 22 of") // 22 bytes: not a multiple of four
	cfg := []byte("UPDATE_DPU_OS=yes\nbfb_modify_os() { :; }\n")
	stream := append(append(append(append(image(50, []byte(`{"Members":[]}`), false), image(59, []byte("console=hvc0 console=ttyAMA0 initrd=initramfs\x00"), false)...),
		image(62, gz.Bytes(), false)...), image(63, initramfs, true)...), cfg...)
	path := filepath.Join(dir, "stream.bfb")
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := splitBFB(path, filepath.Join(dir, "installer"))
	if err != nil {
		t.Fatal(err)
	}
	if p.args != "console=hvc0 console=ttyAMA0 initrd=initramfs" || p.cfg != len(cfg) {
		t.Fatalf("args %q, cfg %d", p.args, p.cfg)
	}
	k, _ := os.ReadFile(p.kernel)
	if !bytes.Equal(k, kernel) {
		t.Fatalf("the kernel was not unpacked: %d bytes", len(k))
	}
	ird, _ := os.ReadFile(p.initrd)
	if !bytes.HasPrefix(ird, initramfs) {
		t.Fatal("the initramfs is not first in the installer's initrd")
	}
	rest := ird[len(initramfs):]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	if (len(ird)-len(rest))%4 != 0 {
		t.Fatalf("the bf.cfg archive starts at %d, not on a 4-byte boundary", len(ird)-len(rest))
	}
	members := readCpio(t, rest)
	if !bytes.Equal(members["etc/bf.cfg"], cfg) {
		t.Fatalf("etc/bf.cfg: %q", members["etc/bf.cfg"])
	}
	if _, ok := members["etc"]; !ok {
		t.Fatal("no etc directory in the archive")
	}

	// a stream with nothing after its last image hands the installer no bf.cfg
	if err := os.WriteFile(path, stream[:len(stream)-len(cfg)], 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err = splitBFB(path, filepath.Join(dir, "installer2")); err != nil || p.cfg != 0 {
		t.Fatalf("no bf.cfg: %+v %v", p, err)
	}
	// and one without a Linux image cannot boot
	if err := os.WriteFile(path, image(50, []byte("{}"), true), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := splitBFB(path, filepath.Join(dir, "installer3")); err == nil {
		t.Fatal("a stream without a Linux image was accepted")
	}
}

// TestGrubDefault reads the entry GRUB boots by default from an installed grub.cfg.
func TestGrubDefault(t *testing.T) {
	cfg := `set default="0"
function load_video {
  insmod all_video
}
menuentry 'Ubuntu' --class ubuntu $menuentry_id_option 'gnulinux-simple-x' --unrestricted {
	recordfail
	search --no-floppy --fs-uuid --set=root 7162
	linux	/boot/vmlinuz-6.8.0-1030-bluefield-64k root=UUID=7162 ro  console=hvc0 console=ttyAMA0 earlycon=pl011,0x01000000 fixrtc $vt_handoff
	initrd	/boot/initrd.img-6.8.0-1030-bluefield-64k
}
submenu 'Advanced options for Ubuntu' {
	menuentry 'Ubuntu, recovery' {
		linux /boot/vmlinuz-old root=UUID=7162 ro single
	}
}
`
	k, i, c, err := grubDefault(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if k != "/boot/vmlinuz-6.8.0-1030-bluefield-64k" || i != "/boot/initrd.img-6.8.0-1030-bluefield-64k" ||
		c != "root=UUID=7162 ro console=hvc0 console=ttyAMA0 earlycon=pl011,0x01000000 fixrtc" {
		t.Fatalf("%q %q %q", k, i, c)
	}
	if _, _, _, err := grubDefault("set timeout=2\n"); err == nil {
		t.Fatal("a grub.cfg without an entry was accepted")
	}
}

// TestArmPlugsIntoItsBMCsPort: the Arm's DHCP is the network's, not the adapter's. Its NIC is
// plugged into the twin relay of its BMC's switch port by its own MAC, a second client behind the
// port (the relay tells the two apart by chaddr, package dhcp's TestTwoClientsBehindOnePort). This
// inverts the first form of D52, where the adapter answered the Arm on its cable with a fixed .9.
func TestArmPlugsIntoItsBMCsPort(t *testing.T) {
	n, err := netsim.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	var gotRef string
	var gotMAC net.HardwareAddr
	n.SetRelayDirectory(func(ref string, client net.HardwareAddr) (netsim.PortRelay, error) {
		gotRef, gotMAC = ref, client
		return nil, errors.New("recorded")
	})
	t.Cleanup(func() { n.SetRelayDirectory(nil) })
	mac, _ := net.ParseMAC("b8:3f:d2:00:00:02")
	d := &model.Device{Path: "site-1/hall-a/a01/compute-tray-01/bf3-0",
		PortBy: map[string]*model.Port{"bmc0": {Ref: "site-1/hall-a/a01/compute-tray-01/bf3-0:bmc0", Name: "bmc0", MAC: "b8:3f:d2:00:00:01"}}}
	m := &arm{a: &Adapter{rt: &adapter.Runtime{Net: n}}, dev: d, mac: mac}
	if _, err := m.plug(); err == nil || err.Error() != "recorded" {
		t.Fatalf("plug: %v", err)
	}
	if gotRef != "site-1/hall-a/a01/compute-tray-01/bf3-0:bmc0" || gotMAC.String() != mac.String() {
		t.Fatalf("the Arm plugged into %q as %s, want its BMC's port as its own MAC %s", gotRef, gotMAC, mac)
	}
	if _, err := (&arm{a: m.a, dev: &model.Device{}, mac: mac}).plug(); err == nil {
		t.Fatal("a DPU without a management port was plugged in")
	}
}

// gptDisk writes a raw disk with a GPT naming one Linux file system partition at 1 MiB.
func gptDisk(t *testing.T, path string, size int64) int64 {
	t.Helper()
	b := make([]byte, 34*512)
	h := b[512:]
	copy(h, "EFI PART")
	binary.LittleEndian.PutUint32(h[8:], 0x00010000)
	binary.LittleEndian.PutUint32(h[12:], 92)
	binary.LittleEndian.PutUint64(h[72:], 2)
	binary.LittleEndian.PutUint32(h[80:], 128)
	binary.LittleEndian.PutUint32(h[84:], 128)
	e := b[1024:]
	copy(e, []byte{0xaf, 0x3d, 0xc6, 0x0f, 0x83, 0x84, 0x72, 0x47, 0x8e, 0x79, 0x3d, 0x69, 0xd8, 0x47, 0x7d, 0xe4})
	binary.LittleEndian.PutUint64(e[32:], 2048)
	binary.LittleEndian.PutUint64(e[40:], uint64(size/512-34))
	binary.LittleEndian.PutUint32(h[88:], crc32.ChecksumIEEE(b[1024:1024+128*128]))
	binary.LittleEndian.PutUint32(h[16:], crc32.ChecksumIEEE(h[:92]))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	return 2048 * 512
}

// TestInstalledBoot reads GRUB's default entry from a real ext4 root file system on a GPT disk,
// as the Arm does after the installer, and names the OOB port in it. It needs e2fsprogs (mke2fs
// and debugfs), which the Arm needs anyway.
func TestInstalledBoot(t *testing.T) {
	mke2fs, err := exec.LookPath("mke2fs")
	if err != nil {
		for _, p := range []string{"/usr/sbin/mke2fs", "/sbin/mke2fs"} {
			if _, e := os.Stat(p); e == nil {
				mke2fs, err = p, nil
			}
		}
	}
	if _, derr := debugfsPath(); err != nil || derr != nil {
		t.Skip("e2fsprogs (mke2fs, debugfs) is not installed")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	for _, d := range []string{"boot/grub", "etc/systemd/network"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	kernel := make([]byte, 4096)
	copy(kernel[56:], "ARM\x64")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(kernel)
	zw.Close()
	files := map[string][]byte{
		"boot/vmlinuz-6.8.0-test":    gz.Bytes(),
		"boot/initrd.img-6.8.0-test": []byte("initrd"),
		"boot/grub/grub.cfg":         []byte("menuentry 'Ubuntu' {\n\tlinux /boot/vmlinuz-6.8.0-test root=UUID=1 ro console=ttyAMA0 earlycon=pl011,0x13010000\n\tinitrd /boot/initrd.img-6.8.0-test\n}\n"),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	disk := filepath.Join(dir, "disk.raw")
	off := gptDisk(t, disk, 64<<20)
	out, err := exec.Command(mke2fs, "-q", "-F", "-t", "ext4", "-E", "offset="+strconv.FormatInt(off, 10), "-d", root, disk, "32M").CombinedOutput()
	if err != nil {
		t.Skipf("mke2fs cannot build a test file system here: %v: %s", err, out)
	}
	k, i, c, err := installedBoot(disk, filepath.Join(dir, "boot"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(k); !bytes.Equal(got, kernel) {
		t.Fatal("the installed kernel was not read and unpacked")
	}
	if got, _ := os.ReadFile(i); string(got) != "initrd" {
		t.Fatalf("initrd %q", got)
	}
	if c != "root=UUID=1 ro console=ttyAMA0 earlycon=pl011,0x13010000" {
		t.Fatalf("command line %q", c)
	}
	if err := writeOOBLink(disk, dir, "b8:3f:d2:00:00:01"); err != nil {
		t.Fatal(err)
	}
	link, err := debugfs(disk, off, false, "cat "+oobLinkName, "stat "+oobLinkName)
	if err != nil || !strings.Contains(link, "MACAddress=b8:3f:d2:00:00:01") || !strings.Contains(link, "Name=oob_net0") || !strings.Contains(link, "User:     0") {
		t.Fatalf("the link file: %v\n%s", err, link)
	}
}

// TestArmReachedInterleavedLogin is the DPU OS's login prompt with a kernel message printed
// between the hostname and `login:`, as a run's console had it: the prompt is still the milestone.
func TestArmReachedInterleavedLogin(t *testing.T) {
	console := "Ubuntu 24.04.4 LTS dpu-node-x ttyAMA0\r\n\r\n" +
		"dpu-node-x [  147.023186] bfvcheck[1914]: strings: '/sys/firmware/acpi/tables/SSDT*': No such file\r\n" +
		"login: [  147.081693] bfvcheck[1920]: strings: '/sys/firmware/acpi/tables/SSDT*': No such file\r\n"
	got := map[string]bool{}
	for _, ms := range armReached("os", console) {
		got[ms[1]] = true
	}
	if !got["os.login"] {
		t.Fatalf("the interleaved prompt was not seen: %v", got)
	}
	if len(armReached("os", "[    0.000000] Booting Linux on physical CPU 0x0\r\n")) != 0 {
		t.Fatal("a milestone was seen in a line that has none")
	}
}

// TestCapsuleVersions is the boot firmware a BFB carries for a BlueField-3, as the `info`
// inventory of the HBN guide's BFB names it (BF2_ entries are another generation's).
func TestCapsuleVersions(t *testing.T) {
	info := map[string]string{"BF2_ATF": "v2.2(release):4.16.0-8-gda930bca9", "BF3_ATF": "v2.2(release):4.16.0-8-gda930bca9",
		"BF3_UEFI": "4.16.0-48-g3e1cb60aa0", "BF3_BSP": "4.16.0.14075", "DOCA": "3.5.0089"}
	got := capsuleVersions(info, "BF3")
	want := map[string]string{"uefi": "4.16.0-48-g3e1cb60aa0", "atf": "v2.2(release):4.16.0-8-gda930bca9", "bsp": "4.16.0.14075"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q, want %q", k, got[k], v)
		}
	}
	if len(capsuleVersions(map[string]string{"DOCA": "3.5.0089"}, "BF3")) != 0 {
		t.Fatal("versions from a stream without boot firmware")
	}
}
