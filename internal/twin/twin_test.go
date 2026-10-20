package twin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

func TestLabEstateComesUp(t *testing.T) {
	tw, err := Up("../../examples/estates/lab-model.yaml", Options{StateDir: t.TempDir(), Adapters: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	st := tw.Engine.Status()
	t.Logf("devices=%v power=%v health=%v domains=%v", st["devices"], st["power"], st["health"], st["domains"])
	if st["devices"].(int) < 200 {
		t.Fatalf("too few devices: %v", st["devices"])
	}
}

// An estate with a stack gets stack-inputs/expected-machines.json (D38): every BMC, and on each
// NVLink switch tray the MAC of its NVOS management port (mgmt0), which is not the BMC's (D46).
func TestStackInputsCarryNVOSMACs(t *testing.T) {
	dir := t.TempDir()
	tw, err := Up("../../examples/estates/nvl72-nico.yaml", Options{StateDir: dir, Adapters: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	b, err := os.ReadFile(filepath.Join(dir, "stack-inputs", "expected-machines.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list []ExpectedMachine
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	devs := map[string]*model.Device{}
	tw.Engine.Read(func(m *model.Model) { devs = m.DeviceBy })
	trays, others := 0, 0
	for _, e := range list {
		d := devs[e.Path]
		if d == nil {
			t.Fatalf("%s is not in the model", e.Path)
		}
		if e.BMCMAC == "" || e.BMCMAC != d.Identity.BMCMAC || e.Serial != d.Identity.Serial || e.User == "" {
			t.Errorf("%s: %+v", e.Path, e)
		}
		if d.BaseClass != "switch-tray" {
			if len(e.NVOSMACs) != 0 {
				t.Errorf("%s (%s) lists NVOS MACs %v", e.Path, d.BaseClass, e.NVOSMACs)
			}
			others++
			continue
		}
		trays++
		mgmt := d.PortBy["mgmt0"]
		if len(e.NVOSMACs) != 1 || e.NVOSMACs[0] != mgmt.MAC {
			t.Errorf("%s: nvosMACs %v, want [%s] (mgmt0)", e.Path, e.NVOSMACs, mgmt.MAC)
		}
		if len(e.NVOSMACs) > 0 && e.NVOSMACs[0] == e.BMCMAC {
			t.Errorf("%s: the NVOS MAC is the BMC's", e.Path)
		}
	}
	if trays != 9 {
		t.Errorf("%d switch trays in expected-machines.json, want the NVL72 rack's 9", trays)
	}
	t.Logf("%d expected machines: %d switch trays with an NVOS MAC, %d other BMCs", len(list), trays, others)
	// the raw file names the field as documented
	var raw []map[string]any
	_ = json.Unmarshal(b, &raw)
	for _, r := range raw {
		if r["path"] == "site-1/hall-a/a03/switch-tray-01" {
			if _, ok := r["nvosMACs"]; !ok {
				t.Errorf("a switch tray's entry has no nvosMACs field: %v", r)
			}
			t.Logf("e.g. %v", r)
		}
	}
}
