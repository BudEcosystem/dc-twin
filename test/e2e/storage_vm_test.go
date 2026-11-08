package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	rfa "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/inhost"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const storageHost = "site-1/hall-a/c01/server-01"

// A vm host's drives are the model's (docs/reviews/2026-11-08-storage-deep-dive.md 5.1): the
// guest sees an NVMe controller per drive with the drive's serial and model; a SMART fault reaches
// the drive's SMART log; a failed drive leaves the guest's PCIe bus and a replacement comes back
// empty; Redfish SecureErase with the host off empties the drive. DCTWIN_QEMU_DIR names a QEMU
// 11.1 (scripts/qemu/build.sh) whose drives carry their model strings.
func TestVMHostDrives(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a virtual machine")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("needs KVM: %v", err)
	}
	qdir := os.Getenv("DCTWIN_QEMU_DIR")
	inst, err := qemu.Find("x86_64", qdir, false)
	if err != nil {
		t.Skipf("needs QEMU for x86_64: %v", err)
	}
	g := vm.Guests["x86_64"]
	if g.Present(vm.GuestDir(twin.AssetsDir(), "", g)) != nil {
		t.Skipf("the twin's x86_64 guest image is not in %s (dctwin assets fetch)", twin.AssetsDir())
	}
	tw := up(t, "../../examples/estates/storage-vm.yaml", []string{"redfish", "vm", "inhost"}, func(e *schema.Estate) { e.Spec.Adapters.VM.QEMU = qdir })
	va := tw.Adapter("vm").(*vm.Adapter)
	ia := tw.Adapter("inhost").(*inhost.Adapter)
	d := tw.Engine.M.DeviceBy[storageHost]
	drives := d.Drives()
	exec := func(script string) string {
		t.Helper()
		var out string
		var rc int
		var err error
		for i := 0; i < 120; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, rc, err = ia.Exec(ctx, storageHost, script)
			cancel()
			if err == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if err != nil || rc != 0 {
			t.Fatalf("guest exec %q: rc %d, %v\n%s", script, rc, err, out)
		}
		return out
	}
	waitGuest := func(what, script, want string) string {
		t.Helper()
		var out string
		for i := 0; i < 90; i++ {
			out = exec(script)
			if strings.Contains(out, want) {
				return out
			}
			time.Sleep(time.Second)
		}
		for _, e := range tw.Engine.Events(0, func(e schema.Event) bool {
			return e.Path == storageHost && e.State != nil && strings.HasPrefix(e.State.Field, "drive.")
		}) {
			t.Logf("event: %s %v %s", e.State.Field, e.State.To, e.Message)
		}
		dbg, _, _ := ia.Exec(context.Background(), storageHost, "echo BEFORE: $(ls /sys/block | grep nvme); echo 1 > /sys/class/nvme/nvme1/reset_controller; sleep 4; echo AFTER-RESET: $(ls /sys/block | grep nvme); dmesg | tail -5")
		t.Logf("guest debug:\n%s", dbg)
		for _, cmd := range []string{"qom-get /machine/peripheral/nvme1-ns1 parent_bus", "qom-get /machine/peripheral/nvme1 subsys", "qom-get /machine/peripheral/nvme0 subsys", "qom-get /machine/peripheral/nvme1 serial", "qom-get /machine/peripheral/nvme1-ns1 detached", "qom-get /machine/peripheral/nvme1-ns1 shared"} {
			o, err := va.HMP(storageHost, cmd)
			t.Logf("HMP %s: %v\n%s", cmd, err, o)
		}
		if qt, err := va.HMP(storageHost, "info qtree"); err == nil {
			var keep []string
			lines := strings.Split(qt, "\n")
			for i, l := range lines {
				if strings.Contains(l, "nvme-ns") || strings.Contains(l, "nvme-subsys") {
					for j := i; j < len(lines) && j < i+8; j++ {
						keep = append(keep, lines[j])
					}
				}
			}
			t.Logf("QEMU qtree (nvme):\n%s", strings.Join(keep, "\n"))
		}
		t.Fatalf("%s: the guest never showed %q:\n%s", what, want, out)
		return ""
	}
	// nsOf is the block device of the drive with this serial, as the guest names it (it numbers its
	// controllers as they probe, not by bay; multipath names a namespace by its subsystem)
	nsOf := func(serial string) string {
		t.Helper()
		out := exec(`for b in /sys/block/nvme*; do case $(basename $b) in *c*) continue;; esac; echo "$(basename $b) $(cat $b/device/serial 2>/dev/null)"; done`)
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[1] == serial {
				return f[0]
			}
		}
		return ""
	}
	// the guest's view of its drives
	out := waitGuest("the drives", `for c in /sys/class/nvme/nvme*; do echo "$(basename $c) $(cat $c/serial) | $(cat $c/model) | $(cat $c/firmware_rev)"; done; for b in /sys/block/nvme*n1; do echo "$(basename $b) $(cat $b/size)"; done`, fmt.Sprintf("nvme%dn1 ", len(drives)-1))
	t.Logf("the guest's drives:\n%s", out)
	withModel := inst.NVMeProps()["model"]
	for _, c := range drives {
		if !strings.Contains(out, " "+c.Drive.Serial) {
			t.Fatalf("the guest does not see %s (serial %s):\n%s", c.Name, c.Drive.Serial, out)
		}
		if withModel && !strings.Contains(out, c.Model) {
			t.Fatalf("the guest does not see %s's model %q:\n%s", c.Name, c.Model, out)
		}
		if dev := nsOf(c.Drive.Serial); dev == "" || !strings.Contains(out, fmt.Sprintf("%s %d", dev, c.Drive.CapacityBytes/512)) {
			t.Fatalf("%s's namespace (%q) and size in the guest:\n%s", c.Name, dev, out)
		}
	}
	src := schema.EventSource{Class: "user", Principal: "test"}
	// a SMART fault: the drive's SMART log carries the bits
	if _, err := tw.Engine.Inject(drives[1].Path, "generic/storage/spare-low", nil, "", src); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "SMART bits on nvme-1", 30*time.Second, func() bool { return va.Drives(storageHost)[1].Warning == engine.SMARTSpare })
	// a marker, then the drive fails: off the guest's bus
	dev1 := nsOf(drives[1].Drive.Serial)
	exec(`echo DCTWIN-MARKER | dd of=/dev/` + dev1 + ` bs=512 count=1 conv=sync 2>/dev/null; sync; dd if=/dev/` + dev1 + ` bs=13 count=1 2>/dev/null`)
	if _, err := tw.Engine.Inject(drives[1].Path, "generic/storage/drive-failure", nil, "", src); err != nil {
		t.Fatal(err)
	}
	waitGuest("drive failure", `cat /sys/class/nvme/*/serial | grep -c `+drives[1].Drive.Serial+` || true`, "0")
	waitFor(t, "the vm adapter sees nvme-1 removed", 30*time.Second, func() bool { return !va.Drives(storageHost)[1].Attached })
	// replaced: a new, empty drive in the bay, with its own serial
	if err := tw.Engine.ReplaceComponent(drives[1].Path, src); err != nil {
		t.Fatal(err)
	}
	waitGuest("replacement", `for c in /sys/class/nvme/nvme*; do echo "$(basename $c) $(cat $c/serial)"; done`, drives[1].Drive.Serial)
	var ns string
	waitFor(t, "the replacement's namespace", 60*time.Second, func() bool { ns = nsOf(drives[1].Drive.Serial); return ns != "" })
	out = exec(`od -An -c -N 13 /dev/` + ns)
	if strings.Contains(out, "D   C   T   W") || !strings.Contains(out, "\\0") {
		t.Fatalf("the replacement drive %s reads:\n%s", ns, out)
	}
	// SecureErase out of band, with the host off: nvme-0 comes back empty
	dev0 := nsOf(drives[0].Drive.Serial)
	exec(`echo DCTWIN-MARKER | dd of=/dev/` + dev0 + ` bs=512 count=1 conv=sync 2>/dev/null; sync`)
	if err := tw.Engine.Power(engine.Request{Path: storageHost, Canonical: "power.off.force", Source: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "power off", 60*time.Second, func() bool {
		return tw.Engine.DeviceView(d)["power"] == "Off"
	})
	rf := tw.Adapter("redfish").(*rfa.Adapter)
	addr, _, _ := rf.BMC(storageHost)
	req, _ := http.NewRequest("POST", "https://"+addr+"/redfish/v1/Systems/1/Storage/1/Drives/NVME_0/Actions/Drive.SecureErase", strings.NewReader(`{"SanitizationType":"CryptographicErase"}`))
	req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("SecureErase with the host off: %d %s", resp.StatusCode, body)
	}
	waitFor(t, "the erase", 60*time.Second, func() bool { return tw.Engine.DriveStatus(drives[0]).Contents == engine.ContentsErased })
	if err := tw.Engine.Power(engine.Request{Path: storageHost, Canonical: "power.on", Source: src}); err != nil {
		t.Fatal(err)
	}
	var dev string
	deadline := time.Now().Add(90 * time.Second)
	for dev == "" && time.Now().Before(deadline) {
		dev = nsOf(drives[0].Drive.Serial)
		time.Sleep(time.Second)
	}
	if dev == "" {
		dbg := exec(`ls /sys/block; for c in /sys/class/nvme/*; do echo "$c $(cat $c/serial) $(cat $c/state)"; done; for b in /sys/block/nvme*; do echo "$b -> $(readlink $b/device) $(cat $b/device/serial 2>&1)"; done; dmesg | grep -i nvme | tail -8`)
		t.Fatalf("the erased drive (serial %s) after the power-on:\n%s", drives[0].Drive.Serial, dbg)
	}
	out = exec(`od -An -c -N 13 /dev/` + dev)
	if strings.Contains(out, "D   C   T   W") || !strings.Contains(out, "\\0") {
		t.Fatalf("after SecureErase %s reads:\n%s", dev, out)
	}
	r := va.Report()
	t.Logf("vm report: %v\nisNot: %v", r.Lines, r.IsNot)
}

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
