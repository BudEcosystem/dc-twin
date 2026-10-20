package bluefield

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"hash/crc32"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
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

// TestArmRelay answers the Arm's DHCP on its cable: an address below the twin site DHCP's
// allocations in the BMC's segment, the segment's gateway, and nothing for any other MAC.
func TestArmRelay(t *testing.T) {
	mac, _ := net.ParseMAC("b8:3f:d2:00:00:01")
	r, err := newArmRelay(mac, "10.30.0.12", "10.30.0.1", netip.MustParsePrefix("10.30.0.0/20"), net.HardwareAddr{2, 0, 0, 0, 0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.addr != netip.MustParseAddr("10.30.0.9") || r.gw.String() != "10.30.0.1/24" {
		t.Fatalf("address %s gateway %s", r.addr, r.gw)
	}
	ask := func(m *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
		t.Helper()
		if err := r.Send(m.ToBytes()); err != nil {
			t.Fatal(err)
		}
		select {
		case b := <-r.replies:
			reply, err := dhcpv4.FromBytes(b)
			if err != nil {
				t.Fatal(err)
			}
			return reply
		default:
			return nil
		}
	}
	disc, _ := dhcpv4.NewDiscovery(mac)
	offer := ask(disc)
	if offer == nil || offer.MessageType() != dhcpv4.MessageTypeOffer || !offer.YourIPAddr.Equal(net.ParseIP("10.30.0.9")) ||
		!offer.Router()[0].Equal(net.ParseIP("10.30.0.1")) || net.IP(offer.SubnetMask()).String() != "255.255.255.0" {
		t.Fatalf("offer: %v", offer)
	}
	req, _ := dhcpv4.NewRequestFromOffer(offer)
	if ack := ask(req); ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatalf("ack: %v", ack)
	}
	other, _ := net.ParseMAC("b8:3f:d2:00:00:02")
	disc2, _ := dhcpv4.NewDiscovery(other)
	if reply := ask(disc2); reply != nil {
		t.Fatalf("another MAC was answered: %v", reply)
	}
	wrong, _ := dhcpv4.NewRequestFromOffer(offer, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(net.ParseIP("10.30.0.77"))))
	if nak := ask(wrong); nak == nil || nak.MessageType() != dhcpv4.MessageTypeNak {
		t.Fatalf("a request for another address: %v", nak)
	}
	if _, err := newArmRelay(mac, "", "", netip.Prefix{}, nil); err == nil {
		t.Fatal("a relay without the BMC's lease was made")
	}
	if small, err := newArmRelay(mac, "192.168.7.20", "192.168.7.17", netip.MustParsePrefix("192.168.7.16/28"), nil); err != nil || small.addr != netip.MustParseAddr("192.168.7.25") {
		t.Fatalf("a network smaller than a /24 is the segment: %v %v", small, err)
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
