package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sto "github.com/BudEcosystem/dc-twin/internal/adapter/storage"
	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/inhost"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// DPU storage (docs/reviews/2026-11-08-storage-deep-dive.md P5): a volume of a vm storage system
// attached to a host's BlueField-3 appears in the host's guest as an NVMe drive with the emulated
// function's serial; what the guest writes to it is in the volume on its storage node (through the
// twin's NVMe/TCP connection to the node's nvmet); detaching takes the drive off the guest's bus.
func TestVMDPUEmulatedVolume(t *testing.T) {
	if testing.Short() {
		t.Skip("boots three virtual machines")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("needs KVM: %v", err)
	}
	qdir := os.Getenv("DCTWIN_QEMU_DIR")
	if _, err := qemu.Find("x86_64", qdir, false); err != nil {
		t.Skipf("needs QEMU for x86_64: %v", err)
	}
	g := vm.Guests["x86_64"]
	gdir := vm.GuestDir(twin.AssetsDir(), "", g)
	if g.Present(gdir) != nil {
		t.Skipf("the twin's x86_64 guest image is not in %s (dctwin assets fetch)", twin.AssetsDir())
	}
	for _, f := range vm.StoragePackages["x86_64"] {
		if _, err := os.Stat(filepath.Join(gdir, f.Name)); err != nil {
			t.Skipf("the storage node packages are not fetched (dctwin assets fetch): %v", err)
		}
	}
	tw := up(t, "../../examples/estates/storage-dpu-vm.yaml", []string{"vm", "inhost", "storage"}, func(e *schema.Estate) { e.Spec.Adapters.VM.QEMU = qdir })
	ia := tw.Adapter("inhost").(*inhost.Adapter)
	sa := tw.Adapter("storage").(*sto.Adapter)
	const sys, host = "site-1/storage/lab-fs", "site-1/hall-a/c01/server-01"
	run := func(path, script string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, rc, err := ia.Exec(ctx, path, script)
		if err == nil && rc != 0 {
			err = &execErr{rc, out}
		}
		return out, err
	}
	waitFor := func(what string, within time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(2 * time.Second)
		}
		for _, l := range sa.Report().Lines {
			t.Log(l)
		}
		t.Fatalf("%s: not within %s", what, within)
	}
	var dpu *model.Device
	for _, ch := range tw.Engine.M.DeviceBy[host].Children {
		if ch.BaseClass == "dpu" {
			dpu = ch
		}
	}
	for _, n := range []string{"site-1/hall-a/s01/node-01", "site-1/hall-a/s01/node-02"} {
		n := n
		waitFor(n+" serving", 6*time.Minute, func() bool {
			out, _ := run(n, "dctwin-storage report")
			return strings.Contains(out, "up nvme-tcp")
		})
	}
	src := schema.EventSource{Class: "user", Principal: "test"}
	vol, err := tw.Engine.CreateStorageVolume(engine.Request{Path: sys, Source: src}, engine.VolumeCreate{Name: "vol1", CapacityBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	waitFor("the volume on its node", 2*time.Minute, func() bool {
		out, _ := run(vol.Home, "dctwin-storage report")
		return strings.Contains(out, "vol vol1 ") && strings.Contains(out, "up nvme-tcp")
	})
	waitFor("the host's guest up", 3*time.Minute, func() bool { _, err := run(host, "true"); return err == nil })
	r := engine.Request{Path: dpu.Path, Canonical: "dpu.emulation", Source: src}
	if err := tw.Engine.SetNVMeEmulation(r, true); err != nil {
		t.Fatal(err)
	}
	fn, err := tw.Engine.AttachEmulatedVolume(r, sys, "vol1")
	if err != nil {
		t.Fatal(err)
	}
	// the host's guest sees an NVMe drive with the function's serial
	find := `for b in /sys/block/nvme*n1; do [ "$(tr -d ' ' < $b/device/serial)" = "` + fn.Serial + `" ] && basename $b; done; true`
	var dev string
	waitFor("the emulated drive in the host's guest", 2*time.Minute, func() bool {
		out, _ := run(host, find)
		dev = strings.TrimSpace(out)
		return dev != ""
	})
	size, err := run(host, "cat /sys/block/"+dev+"/size")
	if err != nil || strings.TrimSpace(size) != "2097152" { // 512-byte sectors of 1 GiB
		t.Fatalf("the emulated drive's size: %q %v", size, err)
	}
	// what the guest writes lands in the volume on its storage node
	sum, err := run(host, "dd if=/dev/urandom of=/tmp/p bs=4096 count=8 2>/dev/null && dd if=/tmp/p of=/dev/"+dev+" bs=4096 count=8 seek=16 conv=fsync 2>/dev/null && md5sum < /tmp/p")
	if err != nil {
		t.Fatalf("write in the host: %v", err)
	}
	there, err := run(vol.Home, "sync; dd if=/pool/vol/vol1.img bs=4096 skip=16 count=8 2>/dev/null | md5sum")
	if err != nil || strings.Fields(there)[0] != strings.Fields(sum)[0] {
		t.Fatalf("the volume on %s holds %q, the host wrote %q (%v)", vol.Home, there, sum, err)
	}
	// and what is in the volume reads back in the host
	back, err := run(host, "echo 3 > /proc/sys/vm/drop_caches; dd if=/dev/"+dev+" bs=4096 skip=16 count=8 2>/dev/null | md5sum")
	if err != nil || strings.Fields(back)[0] != strings.Fields(sum)[0] {
		t.Fatalf("read back in the host: %q %v", back, err)
	}
	// detached, the drive leaves the guest's bus
	if err := tw.Engine.DetachEmulatedVolume(r, fn.ID); err != nil {
		t.Fatal(err)
	}
	waitFor("the emulated drive gone from the host's guest", 2*time.Minute, func() bool {
		out, err := run(host, find)
		return err == nil && strings.TrimSpace(out) == ""
	})
	for _, l := range sa.Report().Lines {
		t.Log(l)
	}
}

type execErr struct {
	rc  int
	out string
}

func (e *execErr) Error() string { return fmt.Sprintf("exit %d: %s", e.rc, e.out) }
