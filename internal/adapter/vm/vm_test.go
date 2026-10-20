package vm

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
)

const tray = "site-1/hall-a/a01/compute-tray-01"

// The machine is the model's: SMBIOS from the identity the tray's Redfish tree serves, NICs
// with the MACs of the ports the host sees (the BlueField's, not either BMC's), the profile's
// size unless the estate scales it.
func TestMachineIsDerivedFromTheModel(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../examples/estates/gb300-vm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "aarch64"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(res.Model, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	d := res.Model.DeviceBy[tray]
	if d.Fidelity != "vm" || d.OS != "twin" || d.Children[0].Fidelity != "model" {
		t.Fatalf("estate: %s %s, bf3 %s", d.Fidelity, d.OS, d.Children[0].Fidelity)
	}
	a := New()
	n, err := netsim.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	a.rt = &adapter.Runtime{Engine: eng, Net: n, StateDir: t.TempDir(), Assets: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.opts = est.Spec.Adapters.VM
	p, err := planOf(d)
	if err != nil || p.arch != "aarch64" || p.ramGB != 12 || p.vcpu != 4 || !p.tpm {
		t.Fatalf("plan %+v %v", p, err)
	}
	run, err := os.MkdirTemp("/tmp", "dctwin-vm-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(run)
	m := &machine{a: a, path: tray, plan: p, cpus: a.opts.CPUs, mem: a.opts.MemoryMB, dir: "/state/vm/x", run: run,
		inst: &qemu.Installation{Arch: "aarch64", Binary: "qemu-system-aarch64", Code: "/fw/code.fd", VarsTemplate: "/fw/vars.fd", Accel: "hvf"}}
	if err := m.cableNICs(d, nil); err != nil {
		t.Fatal(err)
	}
	defer m.closeNICs()
	c := m.config()
	if c.UUID != d.Identity.UUID || c.CPUs != 2 || c.MemoryMB != 1024 {
		t.Fatalf("config %+v", c)
	}
	args := strings.Join(c.Args(), " ")
	for _, want := range []string{
		"type=1,manufacturer=NVIDIA,product=NVIDIA GB300,serial=" + d.Identity.Serial + ",uuid=" + d.Identity.UUID + ",sku=699-25859-0310-TS3",
		"type=2,manufacturer=NVIDIA,product=NVIDIA GB300,serial=" + d.Identity.Serial,
		"type=3,manufacturer=NVIDIA,serial=" + d.Identity.Serial,
		"type=0,vendor=NVIDIA,version=" + d.State.Firmware["uefi"].Version,
		"mac=" + d.Children[0].PortBy["p0"].MAC, "mac=" + d.Children[0].PortBy["p1"].MAC,
		// each NIC is cabled to the twin's switch port for it (D48)
		"-netdev stream,server=off,addr.type=unix,addr.path=" + filepath.Join(run, "nic0.sock") + ",id=net0",
		"-netdev stream,server=off,addr.type=unix,addr.path=" + filepath.Join(run, "nic1.sock") + ",id=net1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q", want)
		}
	}
	for _, bmc := range []string{d.Identity.BMCMAC, d.Children[0].Identity.BMCMAC} {
		if strings.Contains(args, bmc) {
			t.Errorf("a BMC's MAC %s is on a host NIC", bmc)
		}
	}
	if len(c.NICs) != 2 {
		t.Errorf("NICs %+v", c.NICs)
	}
}

// The twin's /init is appended as a second, aligned newc archive after Alpine's initramfs.
func TestBuildInitrd(t *testing.T) {
	dir := t.TempDir()
	base := []byte("\x1f\x8b fake gzip of odd length")
	os.WriteFile(filepath.Join(dir, "initramfs-virt"), base, 0o644)
	os.WriteFile(filepath.Join(dir, "modloop-virt"), []byte("hsqs fake squashfs"), 0o644)
	out, err := BuildInitrd(Guests["aarch64"], dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !bytes.HasPrefix(b, base) {
		t.Fatal("Alpine's archive must come first")
	}
	off := len(base)
	for off%4 != 0 {
		if b[off] != 0 {
			t.Fatal("padding must be NUL")
		}
		off++
	}
	var names []string
	for off < len(b) {
		h := string(b[off : off+110])
		if h[:6] != "070701" {
			t.Fatalf("no newc header at %d: %q", off, h[:6])
		}
		field := func(i int) int { v, _ := strconv.ParseUint(h[6+8*i:14+8*i], 16, 32); return int(v) }
		mode, size, nlen := field(1), field(6), field(11)
		name := string(b[off+110 : off+110+nlen-1])
		names = append(names, name)
		off = (off + 110 + nlen + 3) &^ 3
		if name == "init" {
			if mode != 0o100755 || string(b[off:off+size]) != guestInit {
				t.Fatalf("init: mode %o", mode)
			}
		}
		off = (off + size + 3) &^ 3
		if name == "TRAILER!!!" {
			break
		}
	}
	if strings.Join(names, ",") != "init,modloop.sqfs,TRAILER!!!" {
		t.Fatalf("entries %v", names)
	}
	// rebuilt only when its inputs change
	st1, _ := os.Stat(out)
	if _, err := BuildInitrd(Guests["aarch64"], dir); err != nil {
		t.Fatal(err)
	}
	if st2, _ := os.Stat(out); !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("an unchanged initrd was rebuilt")
	}
}

func TestConsoleLinesAreCleaned(t *testing.T) {
	in := "\x1b[2J\x1b[01;01H\x1b[=3h\x1b[2J[    0.000000] Booting Linux on physical CPU 0x0\r"
	if got := printable(in); got != "[    0.000000] Booting Linux on physical CPU 0x0" {
		t.Fatalf("%q", got)
	}
}

func TestRunDirIsShortEnoughForSockets(t *testing.T) {
	long := filepath.Join("/very", strings.Repeat("long-directory-name/", 8))
	if p := runDir(long, "k"); len(filepath.Join(p, "console.sock")) > 100 {
		t.Fatalf("%s is too long for a unix socket", p)
	}
	if p := runDir("/s/vm/x", "k"); p != "/s/vm/x" {
		t.Fatalf("a short state directory holds its own sockets, got %s", p)
	}
}
