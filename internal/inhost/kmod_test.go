package inhost_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/inhost"
)

// cpioEntries reads the newc archive the vm adapter appends after Alpine's initramfs: each entry's
// name, mode and body.
func cpioEntries(t *testing.T, b []byte, off int) (names []string, mode map[string]int, body map[string][]byte) {
	t.Helper()
	for off%4 != 0 {
		off++
	}
	mode, body = map[string]int{}, map[string][]byte{}
	for off+110 <= len(b) {
		h := string(b[off : off+110])
		if h[:6] != "070701" {
			t.Fatalf("no newc header at %d", off)
		}
		field := func(i int) int { v, _ := strconv.ParseUint(h[6+8*i:14+8*i], 16, 32); return int(v) }
		m, size, nlen := field(1), field(6), field(11)
		name := string(b[off+110 : off+110+nlen-1])
		off = (off + 110 + nlen + 3) &^ 3
		names = append(names, name)
		mode[name], body[name] = m, b[off:off+size]
		off = (off + size + 3) &^ 3
		if name == "TRAILER!!!" {
			break
		}
	}
	return names, mode, body
}

// The kernel module built for a guest image (kmod/build.sh puts it beside the image) goes into the
// guest's initramfs, where the agent loads it; a changed module rebuilds the initrd, and an image
// without one gets none, so the agent falls back to /dev/kmsg.
func TestInitrdCarriesTheKernelModule(t *testing.T) {
	dir := t.TempDir()
	base := []byte("\x1f\x8b fake gzip")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "initramfs-virt"), base, 0o644))
	must(os.WriteFile(filepath.Join(dir, "modloop-virt"), []byte("hsqs fake squashfs"), 0o644))
	ko := []byte("\x7fELF fake module")
	must(os.WriteFile(filepath.Join(dir, inhost.KernelModule), ko, 0o644))
	g := vm.Guests["x86_64"]

	out, err := vm.BuildInitrd(g, dir)
	must(err)
	b, _ := os.ReadFile(out)
	names, mode, body := cpioEntries(t, b, len(base))
	if strings.Join(names, ",") != "init,modloop.sqfs,"+inhost.KernelModule+",TRAILER!!!" {
		t.Fatalf("entries %v", names)
	}
	if mode[inhost.KernelModule] != 0o100644 || !bytes.Equal(body[inhost.KernelModule], ko) {
		t.Fatalf("module entry: mode %o, %d bytes", mode[inhost.KernelModule], len(body[inhost.KernelModule]))
	}
	if !strings.Contains(string(body["init"]), "insmod /"+inhost.KernelModule) {
		t.Fatal("the guest's /init does not load the module")
	}

	must(os.WriteFile(filepath.Join(dir, inhost.KernelModule), append(ko, " rebuilt"...), 0o644))
	out, err = vm.BuildInitrd(g, dir)
	must(err)
	b, _ = os.ReadFile(out)
	if _, _, body = cpioEntries(t, b, len(base)); !bytes.HasSuffix(body[inhost.KernelModule], []byte(" rebuilt")) {
		t.Fatal("a changed module did not rebuild the initrd")
	}

	must(os.Remove(filepath.Join(dir, inhost.KernelModule)))
	out, err = vm.BuildInitrd(g, dir)
	must(err)
	b, _ = os.ReadFile(out)
	if names, _, _ = cpioEntries(t, b, len(base)); strings.Join(names, ",") != "init,modloop.sqfs,TRAILER!!!" {
		t.Fatalf("without a module: entries %v", names)
	}
}

// The names the module's pieces use for one another agree: the agent's constants, the module's
// source and Kbuild, the Debian guests' loader and the relay that writes through it.
func TestKernelModuleNamesAgree(t *testing.T) {
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	src := read("kmod/dctwin_kmsg.c")
	file := strings.TrimPrefix(inhost.KernelModuleFile, "/proc/")
	if !strings.Contains(src, `proc_create("`+file+`", 0200,`) {
		t.Fatalf("the module does not create %s, root-writable only", inhost.KernelModuleFile)
	}
	name := strings.TrimSuffix(inhost.KernelModule, ".ko")
	if !strings.Contains(read("kmod/Kbuild"), "obj-m := "+name+".o") {
		t.Fatalf("Kbuild does not build %s", inhost.KernelModule)
	}
	// it takes the agent's longest line: "<N>" and 900 printable bytes
	m := regexp.MustCompile(`#define DCTWIN_KMSG_MAX (\d+)`).FindStringSubmatch(src)
	line := strings.TrimSuffix(strings.TrimPrefix(inhost.KmsgWriteLine(4, strings.Repeat("x", 2000)), inhost.Marker+"kmsg-write "), "\n")
	if m == nil || len(line) > atoi(m[1]) {
		t.Fatalf("the module takes %v bytes, the agent writes %d", m, len(line))
	}
	for _, p := range []string{"kmod/guest-load.sh", "../../compat/fleet-intelligence/kmsg-relay.py"} {
		if !strings.Contains(read(p), inhost.KernelModuleFile) {
			t.Fatalf("%s does not use %s", p, inhost.KernelModuleFile)
		}
	}
	if !strings.Contains(read("kmod/guest-load.sh"), "modprobe "+name) {
		t.Fatal("guest-load.sh does not load the module by its name")
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
