package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

const labFS = "site-1/storage/hall-a-fs"

func storageLab(t *testing.T) *Engine {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../examples/estates/storage-lab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	est.Spec.Clock = "virtual"
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(res.Model, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func alerted(v StorageSystemView, id string) bool {
	for _, a := range v.Alerts {
		if a.ID == id {
			return true
		}
	}
	return false
}

// What a system holds is made through the engine and read back the same everywhere; capacity is
// reserved against the pool; the objects spread over the nodes.
func TestStorageSystemObjects(t *testing.T) {
	e := storageLab(t)
	r := Request{Path: labFS, Canonical: "storage.create", Source: stack, Protocol: "http"}
	v, err := e.StorageSystem("hall-a-fs")
	if err != nil || v.State != "Online" || v.Health != model.OK || len(v.Nodes) != 8 {
		t.Fatalf("a new system: %+v %v", v, err)
	}
	fs, err := e.CreateFileSystem(r, FileSystemSpec{Name: "models", QuotaBytes: 10 << 40, Exports: []model.Export{{Clients: "10.50.0.0/16", Access: "rw"}}})
	if err != nil {
		t.Fatal(err)
	}
	vol, err := e.CreateStorageVolume(r, VolumeCreate{Name: "db-1", CapacityBytes: 1 << 40})
	if err != nil {
		t.Fatal(err)
	}
	if vol.NQN != "nqn.2026-11.io.dctwin:hall-a-fs:db-1" || len(vol.NGUID) != 32 || vol.Home == fs.Home {
		t.Fatalf("volume %+v (file system on %s)", vol, fs.Home)
	}
	if _, err := e.MapVolume(r, "db-1", "nqn.2014-08.org.nvmexpress:uuid:host-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBucket(r, "datasets", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateSnapshot(r, "models", "nightly"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateFileSystem(r, FileSystemSpec{Name: "models"}); !errors.Is(err, ErrExists) {
		t.Fatalf("a second file system of the name: %v", err)
	}
	// what reaches a vm node's shell and exports is plain: no shell or exports(5) syntax
	if _, err := e.CreateFileSystem(r, FileSystemSpec{Name: "x", Path: "/k8s/x; rm -rf /pool"}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("a path with shell syntax: %v", err)
	}
	if _, err := e.CreateFileSystem(r, FileSystemSpec{Name: "y", Exports: []model.Export{{Clients: "*\nDCTWIN-EXPORTS", Access: "rw"}}}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("export clients with a newline: %v", err)
	}
	if _, err := e.MapVolume(r, "db-1", "nqn.x; reboot -f", false); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("a host NQN with shell syntax: %v", err)
	}
	if _, err := e.CreateStorageVolume(r, VolumeCreate{Name: "huge", CapacityBytes: v.UsableBytes}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("a volume beyond the pool: %v", err)
	}
	v, _ = e.StorageSystem(labFS)
	if len(v.FileSystems) != 1 || len(v.Volumes) != 1 || len(v.Buckets) != 1 || len(v.Snapshots) != 1 ||
		v.ProvisionedBytes != 10<<40+1<<40 || len(v.Volumes[0].Hosts) != 1 || !v.Volumes[0].Online {
		t.Fatalf("view: %+v", v)
	}
	// deleting the file system takes its snapshot with it
	if err := e.DeleteStorageObject(r, "filesystem", "models"); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.StorageSystem(labFS); len(v.FileSystems) != 0 || len(v.Snapshots) != 0 {
		t.Fatalf("after delete: %+v", v)
	}
}

// Use fills the pool and quotas; the alerts say so.
func TestStorageSystemFillAndAlerts(t *testing.T) {
	e := storageLab(t)
	r := Request{Path: labFS, Canonical: "storage.create", Source: stack}
	if _, err := e.CreateFileSystem(r, FileSystemSpec{Name: "scratch", QuotaBytes: 1 << 40}); err != nil {
		t.Fatal(err)
	}
	pct := 120.0
	if err := e.StorageAct(labFS, schema.StorageAct{Fill: "scratch", UsedPercent: &pct}, user); err != nil {
		t.Fatal(err)
	}
	v, _ := e.StorageSystem(labFS)
	if !alerted(v, "QuotaExceeded") || v.Health != model.Warning || v.State != "Degraded" {
		t.Fatalf("over quota: %+v", v.Alerts)
	}
	if _, err := e.Inject(labFS, "generic/storage-system/pool-near-full", map[string]any{"percent": 93}, "full", user); err != nil {
		t.Fatal(err)
	}
	v, _ = e.StorageSystem(labFS)
	if !alerted(v, "PoolNearlyFull") || v.UsedPercent < 93 {
		t.Fatalf("pool near full: %.1f%% %+v", v.UsedPercent, v.Alerts)
	}
	if fs := e.Faults(labFS); len(fs) != 1 || len(e.Faults(v.Nodes[0].Path)) != 0 {
		t.Fatalf("the system's fault listed as its anchor node's: %+v", e.Faults(v.Nodes[0].Path))
	}
	if _, err := e.Clear("full", "", user); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.StorageSystem(labFS); alerted(v, "PoolNearlyFull") {
		t.Fatalf("after clear: %+v", v.Alerts)
	}
	// a node's faults are not the system's, and the system's are refused on a node
	if _, err := e.Inject("site-1/hall-a/s01/node-01", "generic/storage-system/pool-near-full", nil, "", user); err == nil {
		t.Fatal("a storage-system fault on a device")
	}
}

// A node that powers off: its objects are unavailable, then fail over to the next node, then
// come back when it does.
func TestStorageSystemFailover(t *testing.T) {
	e := storageLab(t)
	r := Request{Path: labFS, Canonical: "storage.create", Source: stack}
	fs, err := e.CreateFileSystem(r, FileSystemSpec{Name: "home"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Power(Request{Path: fs.Home, Canonical: "power.off.force", Source: user}); err != nil {
		t.Fatal(err)
	}
	e.Advance(2 * time.Second)
	v, _ := e.StorageSystem(labFS)
	if v.FileSystems[0].Status != "FailingOver" || v.State != "Degraded" || !alerted(v, "NodeDown") {
		t.Fatalf("while failing over: %+v %+v", v.FileSystems[0], v.Alerts)
	}
	e.Advance(15 * time.Second)
	v, _ = e.StorageSystem(labFS)
	if f := v.FileSystems[0]; !f.Online || f.Node == fs.Home || f.Node == "" {
		t.Fatalf("after failover: %+v", f)
	}
	if err := e.Power(Request{Path: fs.Home, Canonical: "power.on", Source: user}); err != nil {
		t.Fatal(err)
	}
	e.Advance(5 * time.Minute)
	v, _ = e.StorageSystem(labFS)
	if f := v.FileSystems[0]; f.Node != fs.Home || v.State != "Online" {
		t.Fatalf("after the node is back: %+v %s %+v", f, v.State, v.Alerts)
	}
	// an enclosure without power takes its drives out of every stripe: the pool degrades
	jbof := v.Enclosures[0].Path
	if err := e.Power(Request{Path: jbof, Canonical: "power.off.force", Source: user}); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.StorageSystem(labFS); v.DevicesDown != 1 || !alerted(v, "PoolDegraded") || v.State != "Degraded" {
		t.Fatalf("its enclosure off: %d %s %+v", v.DevicesDown, v.State, v.Alerts)
	}
	if err := e.Power(Request{Path: jbof, Canonical: "power.on", Source: user}); err != nil {
		t.Fatal(err)
	}
	// a node's storage service fails with its power on; racks reuse slot names, so the node is
	// named with its rack
	if _, err := e.Inject(labFS, "generic/storage-system/node-service-failure", map[string]any{"node": "s01/node-02"}, "svc", user); err != nil {
		t.Fatal(err)
	}
	v, _ = e.StorageSystem(labFS)
	for _, n := range v.Nodes {
		if n.Name == "node-02" && n.Up == strings.Contains(n.Path, "/s01/") {
			t.Fatalf("s01/node-02 with its service failed: %+v", n)
		}
	}
	// more failed pool drives than parity takes the pool offline
	var failed int
	for _, d := range []*model.Device{e.M.DeviceBy["site-1/hall-a/s01/node-03"]} {
		for _, c := range d.Drives() {
			if c.Drive.Role == "data" && failed < 3 {
				if err := e.PullDrive(c.Path, user); err != nil {
					t.Fatal(err)
				}
				failed++
			}
		}
	}
	if v, _ = e.StorageSystem(labFS); v.State != "Offline" || v.Health != model.Critical || !alerted(v, "PoolFailed") {
		t.Fatalf("three of 8+2's drives gone: %s %s %+v", v.State, v.Health, v.Alerts)
	}
}
