package compile

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

func compileFile(t *testing.T, path string) *Result {
	t.Helper()
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := LoadEstate(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Compile(est, reg, Options{HostArch: "aarch64"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestExamplesCompile(t *testing.T) {
	for _, f := range []string{"../../examples/estates/gb300-nvl72-rack.yaml", "../../examples/estates/mixed-hall.yaml"} {
		res := compileFile(t, f)
		m := res.Model
		t.Logf("%s: %d racks, %d devices, %d ports, %d links, %d domains, warnings %v", f, len(m.Racks), len(m.Devices), len(m.Ports), len(m.Links), len(m.Domains), res.Warnings)
	}
}

func TestNVL72Shape(t *testing.T) {
	m := compileFile(t, "../../examples/estates/gb300-nvl72-rack.yaml").Model
	count := map[string]int{}
	for _, d := range m.Devices {
		if d.RackOf() != nil && d.RackOf().Name == "a03" {
			count[d.BaseClass]++
		}
	}
	if count["compute-tray"] != 18 || count["switch-tray"] != 9 || count["power-shelf"] != 8 || count["dpu"] != 18 {
		t.Fatalf("NVL72 composition: %v", count)
	}
	nvl := 0
	for _, l := range m.Links {
		if l.Fabric == "nvlink" {
			nvl++
		}
	}
	if nvl != 72*18 {
		t.Fatalf("NVLink spine: %d links, want %d", nvl, 72*18)
	}
	tray := m.DeviceBy["site-1/hall-a/a03/compute-tray-07"]
	if tray == nil || tray.PortBy["bmc0"].Link == nil {
		t.Fatal("tray-07's BMC port is not cabled")
	}
	if tray.Fidelity != "model" || m.DeviceBy["site-1/hall-a/a03/compute-tray-01"].Fidelity != "vm" {
		t.Fatal("fidelity overrides")
	}
	var dom *model.Domain
	for _, d := range m.Domains {
		if d.Kind == "scale-up" {
			dom = d
		}
	}
	if dom == nil || len(dom.Members) != 72 {
		t.Fatalf("scale-up domain: %+v", dom)
	}
}

func TestIdentityStableAndAllocationIsolated(t *testing.T) {
	a := compileFile(t, "../../examples/estates/mixed-hall.yaml").Model
	b := compileFile(t, "../../examples/estates/mixed-hall.yaml").Model
	for _, d := range a.Devices {
		if b.DeviceBy[d.Path].Identity.Serial != d.Identity.Serial || b.DeviceBy[d.Path].Identity.BMCMAC != d.Identity.BMCMAC {
			t.Fatalf("identity of %s not stable", d.Path)
		}
	}
}

// A factory OOB fabric (18 — Z7d): its selected management switches are factory, and each one's
// own management port is cabled to the aggregation switch, which stays the twin's.
func TestDSXDay0OOB(t *testing.T) {
	m := compileFile(t, "../../examples/estates/dsx-day0.yaml").Model
	f := m.Fabrics["oob"]
	if f == nil || f.Factory == nil {
		t.Fatal("no factory OOB fabric")
	}
	var factory []string
	for d := range f.Factory.Devices {
		factory = append(factory, d.Path)
	}
	sort.Strings(factory)
	if strings.Join(factory, " ") != "site-1/hall-a/r01/mgmt-switch site-1/hall-a/r02/mgmt-switch" {
		t.Errorf("factory switches: %v", factory)
	}
	for _, d := range m.Devices {
		if d.Name != "mgmt-switch" {
			continue
		}
		p := d.PortBy["mgmt0"]
		if p == nil || p.Link == nil || p.Link.Peer(p).Device.Name != "oob-agg-1" {
			t.Errorf("%s's management port is not cabled to the aggregation switch", d.Path)
		}
	}
}

// A factory OOB fabric of three rows of racks: each aggregation switch takes 63 management
// switches, uplinks counting up from eth1 and management ports down from eth126, so the two never
// take one port (a fourth of the racks' switches go to a second aggregation switch).
func TestDSXDay0OOBRows(t *testing.T) {
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := LoadEstate("../../examples/estates/dsx-day0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	hall := &est.Spec.Sites[0].Halls[0]
	cpu := hall.Rows[0].Racks[0]
	hall.Rows = nil
	for i, row := range []string{"a", "b", "c"} {
		r := schema.Row{Name: row, Positions: 16}
		for p := 1; p <= 12; p++ {
			x := cpu
			x.Name, x.Position = fmt.Sprintf("r%02d", i*12+p), p
			r.Racks = append(r.Racks, x)
		}
		if row == "a" {
			r.Racks = append(r.Racks, schema.Rack{Name: "n01", Position: 16, Format: "generic/network-rack", Feeds: []string{"A", "B"}})
		}
		hall.Rows = append(hall.Rows, r)
	}
	res, err := Compile(est, reg, Options{HostArch: "aarch64"})
	if err != nil {
		t.Fatal(err)
	}
	aggs := map[string]bool{}
	for _, d := range res.Model.Devices {
		if d.Name != "mgmt-switch" {
			continue
		}
		p := d.PortBy["mgmt0"]
		if p == nil || p.Link == nil || !strings.HasPrefix(p.Link.Peer(p).Device.Name, "oob-agg-") {
			t.Errorf("%s's management port is not cabled to an aggregation switch", d.Path)
			continue
		}
		aggs[p.Link.Peer(p).Device.Name] = true
	}
	if len(aggs) != 2 {
		t.Errorf("aggregation switches with management ports: %v, want 2", aggs)
	}
}
