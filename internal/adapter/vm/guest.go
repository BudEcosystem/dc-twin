package vm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/inhost"
)

// GuestImage is the twin's own OS for `os: twin` hosts (D23): Alpine Linux's netboot kernel,
// initramfs and module image at a pinned release, verified by digest and never committed
// (D13). The twin appends its own /init as a second initramfs archive, so what boots is
// Alpine's kernel through the machine's UEFI firmware with a small init that reports what the
// firmware's tables say about the machine and honours the ACPI power button.
type GuestImage struct {
	Name    string // the directory under the assets directory
	Arch    string
	Kernel  string
	Console string // the serial console device the kernel writes to
	Files   []GuestFile
	Licence string
	Source  string
}

// GuestFile is one artifact of a guest image and its pinned SHA-256.
type GuestFile struct {
	Name, URL, SHA256 string
}

const (
	alpineBase    = "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/aarch64/netboot-3.24.2/"
	alpineBaseX86 = "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/x86_64/netboot-3.24.2/"
)

// Guests are the pinned guest images by architecture.
var Guests = map[string]GuestImage{
	"aarch64": {
		Name: "alpine-netboot@3.24.2-aarch64", Arch: "aarch64", Kernel: "vmlinuz-virt", Console: "ttyAMA0",
		Licence: "GPL-2.0 (Linux), various (Alpine packages)", Source: alpineBase,
		Files: []GuestFile{
			{"vmlinuz-virt", alpineBase + "vmlinuz-virt", "e45e1f6083d1ed45db6647b422e32b6ae6dc54de7b8190b7b97744fb293412e3"},
			{"initramfs-virt", alpineBase + "initramfs-virt", "ffe65ec5a0c0bf470042ad28f7ce7aa5f842ce8090e4230fb2703a7a34e1bebe"},
			{"modloop-virt", alpineBase + "modloop-virt", "32a189d5e4ae4417cf65bfcc91d2cc8ffdf5957fbbc1f055b037861c595b7328"},
		},
	},
	// x86_64 hosts (the XE9680 and SYS-821GE profiles) under KVM, for the in-host agent on a Linux host
	"x86_64": {
		Name: "alpine-netboot@3.24.2-x86_64", Arch: "x86_64", Kernel: "vmlinuz-virt", Console: "ttyS0",
		Licence: "GPL-2.0 (Linux), various (Alpine packages)", Source: alpineBaseX86,
		Files: []GuestFile{
			{"vmlinuz-virt", alpineBaseX86 + "vmlinuz-virt", "40f620bc8c93d952e57dd8dfc0f94fca1759d192a4fc4a260705d50ca378559c"},
			{"initramfs-virt", alpineBaseX86 + "initramfs-virt", "c990c63e4602aa84b92d7df54fd180cb0e56590d61b71221ba6d60e913d26357"},
			{"modloop-virt", alpineBaseX86 + "modloop-virt", "1e7a3eea707d2ecb3d0502f97fb4dc4dcbac1fe06075e24ae19bfdf4343bb1cf"},
		},
	},
}

// GuestDir is where a guest image lives: dir when the estate names one, else under assets.
func GuestDir(assets, dir string, g GuestImage) string {
	if dir != "" {
		return dir
	}
	return filepath.Join(assets, g.Name)
}

// Present checks every file of the image is in dir with its pinned digest.
func (g GuestImage) Present(dir string) error {
	for _, f := range g.Files {
		p := filepath.Join(dir, f.Name)
		sum, err := fileSHA256(p)
		if err != nil {
			return fmt.Errorf("the os: twin guest image %s is not present at %s; run `dctwin assets fetch` (from %s, licence %s)", g.Name, dir, g.Source, g.Licence)
		}
		if sum != f.SHA256 {
			return fmt.Errorf("%s has SHA-256 %s, not the pinned %s", p, sum, f.SHA256)
		}
	}
	return nil
}

// FetchGuest downloads the missing files of every pinned guest image into the assets
// directory, verifying each against its pinned digest.
func FetchGuest(assets string, w io.Writer) error {
	for _, arch := range []string{"aarch64", "x86_64"} {
		g := Guests[arch]
		dir := GuestDir(assets, "", g)
		if g.Present(dir) == nil {
			fmt.Fprintf(w, "%s: present at %s\n", g.Name, dir)
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, f := range g.Files {
			dst := filepath.Join(dir, f.Name)
			if sum, err := fileSHA256(dst); err == nil && sum == f.SHA256 {
				continue
			}
			fmt.Fprintf(w, "%s: fetching %s (licence %s)\n", g.Name, f.URL, g.Licence)
			if err := download(f.URL, dst, f.SHA256); err != nil {
				return fmt.Errorf("%s: %w", g.Name, err)
			}
		}
	}
	return nil
}

func download(url, dst, want string) error {
	cl := &http.Client{Timeout: 30 * time.Minute}
	resp, err := cl.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dst + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return fmt.Errorf("%s: SHA-256 %s, pinned %s", url, got, want)
	}
	return os.Rename(tmp, dst)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// guestInit is the twin's /init. It runs as PID 1 from the initramfs: it prints what the
// firmware's SMBIOS tables say about the machine (as Linux reads them), loads the drivers that
// deliver the ACPI power button from the module image appended beside it, prints the NICs'
// MACs, starts the in-host agent (internal/inhost), says it is up, and powers the machine off
// when the power button is pressed. Nothing else runs; a guest whose power button is never
// pressed runs until it is forced off.
const guestInitBase = `#!/usr/bin/busybox sh
# dc-twin guest init (os: twin, D23)
export PATH=/usr/bin:/usr/sbin:/bin:/sbin
/usr/bin/busybox --install -s 2>/dev/null
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
# through the kernel log, so that its lines reach the console whole and in order with the
# kernel's own (the cmdline sets printk.devkmsg=on)
say() { echo "dc-twin guest: $*" > /dev/kmsg; }
say "init $(uname -r) $(uname -m)"
for f in sys_vendor product_name product_version product_serial product_uuid product_sku product_family \
         board_vendor board_name board_serial chassis_vendor chassis_serial chassis_asset_tag \
         bios_vendor bios_version; do
	say "dmi $f=$(cat /sys/class/dmi/id/$f 2>/dev/null)"
done
modprobe loop 2>/dev/null; modprobe squashfs 2>/dev/null
mkdir -p /.modloop
if mount -o loop,ro -t squashfs /modloop.sqfs /.modloop 2>/dev/null; then
	rm -rf /usr/lib/modules && ln -s /.modloop/modules /usr/lib/modules
fi
for m in virtio_net gpio-pl061 button evdev; do modprobe $m 2>/dev/null; done
sleep 1
for n in /sys/class/net/*; do
	[ -e "$n/device" ] && say "nic $(basename $n) $(cat $n/address)"
done
pb=""
for e in /sys/class/input/event*; do
	grep -q "Power Button" "$e/device/name" 2>/dev/null && pb=/dev/input/$(basename $e)
done
say "power button ${pb:-absent}"
#@inhost-agent@
say "up"
if [ -n "$pb" ]; then
	head -c 24 "$pb" >/dev/null
	say "power button pressed: powering off"
	sync
	poweroff -f
fi
while :; do sleep 3600; done
`

// guestInit is the twin's /init: the in-host agent's guest half (internal/inhost) is started
// before the guest says it is up.
var guestInit = strings.Replace(guestInitBase, "#@inhost-agent@\n", inhost.GuestAgent(), 1)

// consoleSocket is where a machine's serial console is served while it runs (vm.go's plan and
// runDir): the in-host agent's channel into an `os: twin` guest, which it is told here.
func consoleSocket(stateDir, devicePath string) string {
	dir := filepath.Join(stateDir, "vm", strings.ReplaceAll(devicePath, "/", "_"))
	return filepath.Join(runDir(dir, stateDir+"|"+devicePath), "console.sock")
}

func init() { inhost.ConsoleSocket = consoleSocket }

// GuestCmdline is the kernel command line of the twin's guest, after console=.
const GuestCmdline = "printk.devkmsg=on"

// GuestReady is the console line the twin's guest prints when it is up.
const GuestReady = "dc-twin guest: up"

// BuildInitrd writes <dir>/initrd-twin: Alpine's initramfs, then (4-byte aligned) an
// uncompressed newc archive with the twin's /init, the module image and, when one was built for
// the image (<dir>/dctwin-kmsg.ko, internal/inhost/kmod), the in-host agent's kernel module. Linux
// unpacks the archives in order, so the twin's /init replaces Alpine's. It is rebuilt when its
// inputs change.
func BuildInitrd(g GuestImage, dir string) (string, error) {
	base, err := os.ReadFile(filepath.Join(dir, "initramfs-virt"))
	if err != nil {
		return "", err
	}
	modloop, err := os.ReadFile(filepath.Join(dir, "modloop-virt"))
	if err != nil {
		return "", err
	}
	// the in-host agent's kernel module, when one was built for this image (internal/inhost/kmod)
	kmod, _ := os.ReadFile(filepath.Join(dir, inhost.KernelModule))
	kmodSum := sha256.Sum256(kmod)
	stamp := sha256.Sum256([]byte(guestInit + g.Files[1].SHA256 + g.Files[2].SHA256 + hex.EncodeToString(kmodSum[:])))
	out := filepath.Join(dir, "initrd-twin")
	stampFile := out + ".sha256"
	if b, err := os.ReadFile(stampFile); err == nil && strings.TrimSpace(string(b)) == hex.EncodeToString(stamp[:]) {
		if _, err := os.Stat(out); err == nil {
			return out, nil
		}
	}
	var buf bytes.Buffer
	buf.Write(base)
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
	cw := &cpioWriter{w: &buf}
	cw.file("init", 0o100755, []byte(guestInit))
	cw.file("modloop.sqfs", 0o100644, modloop)
	if len(kmod) > 0 {
		cw.file(inhost.KernelModule, 0o100644, kmod)
	}
	cw.trailer()
	tmp := out + ".partial"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	_ = os.WriteFile(stampFile, []byte(hex.EncodeToString(stamp[:])+"\n"), 0o644)
	return out, nil
}

// cpioWriter writes the "newc" (SVR4, no CRC) format the kernel's initramfs unpacker reads.
type cpioWriter struct {
	w   *bytes.Buffer
	ino int
}

func (c *cpioWriter) header(name string, mode, size int) {
	c.ino++
	fmt.Fprintf(c.w, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		c.ino, mode, 0, 0, 1, 0, size, 0, 0, 0, 0, len(name)+1, 0)
	c.w.WriteString(name)
	c.w.WriteByte(0)
	c.pad()
}

func (c *cpioWriter) pad() {
	for c.w.Len()%4 != 0 {
		c.w.WriteByte(0)
	}
}

func (c *cpioWriter) file(name string, mode int, data []byte) {
	c.header(name, mode, len(data))
	c.w.Write(data)
	c.pad()
}

func (c *cpioWriter) trailer() { c.header("TRAILER!!!", 0, 0) }
