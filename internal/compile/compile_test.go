package compile

import (
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
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
