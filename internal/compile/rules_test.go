package compile

import (
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// mixedHall loads mixed-hall and lets a test change it before compiling.
func compileMixed(t *testing.T, change func(e *schema.Estate)) (*Result, error) {
	t.Helper()
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := LoadEstate("../../examples/estates/mixed-hall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	change(est)
	return Compile(est, reg, Options{HostArch: "aarch64"})
}

func wantProblem(t *testing.T, err error, part string) {
	t.Helper()
	ce, ok := Fail(err)
	if !ok {
		t.Fatalf("want a refusal mentioning %q, got %v", part, err)
	}
	for _, p := range ce.Problems {
		if strings.Contains(p, part) {
			return
		}
	}
	t.Fatalf("no problem mentions %q: %v", part, ce.Problems)
}

func fabric(e *schema.Estate, name string) *schema.Fabric {
	for i := range e.Spec.Fabrics {
		if e.Spec.Fabrics[i].Name == name {
			return &e.Spec.Fabrics[i]
		}
	}
	return nil
}

func TestFabricParamsAreChecked(t *testing.T) {
	_, err := compileMixed(t, func(e *schema.Estate) { fabric(e, "frontend").Params["spines"] = 99 })
	wantProblem(t, err, "fabric frontend: parameter spines=99 out of range")
	_, err = compileMixed(t, func(e *schema.Estate) { fabric(e, "backend-ib").Params = map[string]any{"ibLeaf": "generic/server-2u"} })
	wantProblem(t, err, "is a server, the template wants a switch")
	// an undeclared template parameter is kept, with a warning
	res, err := compileMixed(t, func(e *schema.Estate) { fabric(e, "frontend").Params["underlay"] = "bgp-unnumbered" })
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(res, `parameter "underlay" is not declared by generic/clos-2tier`) {
		t.Fatalf("want a warning for the undeclared parameter: %v", res.Warnings)
	}
}

func TestSlotAcceptsUnitsAndDefaultProfiles(t *testing.T) {
	// a 2U server where the hgx format's server slot accepts rack-mounted servers: fine
	if _, err := compileMixed(t, func(e *schema.Estate) {
		e.Spec.Sites[0].Halls[0].Rows[1].Racks[0].Params = map[string]any{"server": "oem-dell/r760", "servers": 2}
	}); err != nil {
		t.Fatalf("an accepted $param profile should compile: %v", err)
	}
	// a switch as the hgx format's server: the $param profile is now checked against accepts
	_, err := compileMixed(t, func(e *schema.Estate) {
		e.Spec.Sites[0].Halls[0].Rows[1].Racks[0].Params = map[string]any{"server": "generic/mgmt-switch-1g", "servers": 2}
	})
	wantProblem(t, err, "parameter server: generic/mgmt-switch-1g is a switch")
}

func TestNamesAndSettings(t *testing.T) {
	_, err := compileMixed(t, func(e *schema.Estate) {
		rows := e.Spec.Sites[0].Halls[0].Rows
		e.Spec.Sites[0].Halls[0].Rows = append(rows, schema.Row{Name: rows[0].Name, Positions: 4})
	})
	wantProblem(t, err, "two rows named a")
	_, err = compileMixed(t, func(e *schema.Estate) {
		e.Spec.Sites[0].Halls = append(e.Spec.Sites[0].Halls, e.Spec.Sites[0].Halls[0])
	})
	wantProblem(t, err, "two halls named hall-b")
	_, err = compileMixed(t, func(e *schema.Estate) { e.Spec.Fidelity.Default = "virtual-machine" })
	wantProblem(t, err, `spec.fidelity.default "virtual-machine"`)
	_, err = compileMixed(t, func(e *schema.Estate) { e.Spec.InitialState = "standby" })
	wantProblem(t, err, `spec.initialState "standby"`)
	_, err = compileMixed(t, func(e *schema.Estate) { e.Spec.TimeScale = -1 })
	wantProblem(t, err, "spec.timeScale")
	_, err = compileMixed(t, func(e *schema.Estate) { e.Spec.Sites[0].Halls[0].Rows[0].Positions = 20 })
	wantProblem(t, err, "a row has at most 16")
}

func TestUnconnectedPortsWarn(t *testing.T) {
	res := compileFile(t, "../../examples/estates/mixed-hall.yaml")
	// b01's Dell XE9680 servers have InfiniBand scale-out NICs; backend-ib selects a0* and
	// backend-roce (Ethernet) cables none of them
	if !hasWarning(res, "rack b01: 32 infiniband scale-out ports are on no fabric") {
		t.Fatalf("want the b01 warning: %v", res.Warnings)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "rack a01:") && strings.Contains(w, "on no fabric") {
			t.Fatalf("a01 is on backend-ib; no warning expected: %s", w)
		}
	}
}

func hasWarning(res *Result, part string) bool {
	for _, w := range res.Warnings {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

func TestPowerChainIsChecked(t *testing.T) {
	top := func(e *schema.Estate) []schema.PowerNode { return e.Spec.Facility.Power.Topology }
	res, err := compileMixed(t, func(e *schema.Estate) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Model.Power) != 6 || res.Model.Power[0].Name != "ups-a" || res.Model.DeviceBy["site-1/hall-b/facility/power/rpp-a1"] == nil {
		t.Fatalf("the chain compiles to facility devices, parents first: %d nodes", len(res.Model.Power))
	}
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[2].From = "nope" })
	wantProblem(t, err, `rpp-a1: from "nope" is neither a feed nor a node`)
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[0].From = "rpp-a1" })
	wantProblem(t, err, "loops back on itself")
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[3].Serves = []string{"a01"} })
	wantProblem(t, err, "rack a01: feed A reaches it through both rpp-a1 and rpp-a2")
	if ce, _ := Fail(err); ce != nil {
		n := 0
		for _, p := range ce.Problems {
			if strings.Contains(p, "rack a01: feed A reaches it through both") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("one problem per rack, feed and pair of nodes, not per power source: %d", n)
		}
	}
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[2].RuntimeS = 60 })
	wantProblem(t, err, "runtimeS is a UPS's")
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[0].Kind = "generator" })
	wantProblem(t, err, `kind "generator"`)
	res, err = compileMixed(t, func(e *schema.Estate) { top(e)[2].CapacityW = 100000 })
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "rpp-a1: its racks draw up to")
	}
	if !found {
		t.Fatalf("a panel below its racks' max draw is warned about: %v", res.Warnings)
	}
}

// A dedicated storage fabric cables the storage-role ports, the frontend the rest; a storage
// overlay rides the frontend's ports; storage racks hold storage nodes and a JBOF.
func TestStorageFabricOverlayAndRack(t *testing.T) {
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := LoadEstate("../../examples/estates/storage-lab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	est.Spec.Networks["tenant-storage"] = schema.Network{Role: "storage", CIDR: "10.60.0.0/16", Overlay: &schema.NetworkOverlay{On: "frontend", Encap: "vxlan", ID: 5001}}
	res, err := Compile(est, reg, Options{HostArch: "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Model
	node := m.DeviceBy["site-1/hall-a/s01/node-01"]
	if node == nil || len(node.Drives()) != 22 || len(node.Controllers()) != 1 {
		t.Fatalf("storage node: %v", node)
	}
	for _, p := range node.Ports {
		if p.Role == "storage" && (p.Link == nil || p.Link.Fabric != "storage" || p.Network != "storage") {
			t.Fatalf("%s: link %v network %q, want the storage fabric and network", p.Ref, p.Link, p.Network)
		}
		if p.Role == "provisioning" && (p.Link == nil || p.Link.Fabric != "frontend") {
			t.Fatalf("%s: link %v, want the frontend fabric", p.Ref, p.Link)
		}
	}
	if m.DeviceBy["site-1/hall-a/s01/jbof-01"] == nil || m.DeviceBy["site-1/hall-a/s02/jbof-01"] != nil {
		t.Fatal("s01 has one JBOF, s02 none")
	}
	if m.DeviceBy["site-1/hall-a/n01/storage-spine-1"] == nil || m.DeviceBy["site-1/hall-a/n01/spine-1"] == nil {
		t.Fatal("each fabric has its own spines")
	}
	ov := m.Networks["tenant-storage"]
	if ov == nil || ov.Overlay != "frontend" || ov.Encap != "vxlan" || ov.OverlayID != 5001 || len(ov.Ports) != len(m.Networks["frontend"].Ports) {
		t.Fatalf("overlay: %+v", ov)
	}
	// a bad overlay is refused
	est.Spec.Networks["bad"] = schema.Network{Role: "storage", Overlay: &schema.NetworkOverlay{On: "nowhere", Encap: "vlan", ID: 9}}
	_, err = Compile(est, reg, Options{HostArch: "x86_64"})
	wantProblem(t, err, "overlay on \"nowhere\"")
}

func TestStorageSystems(t *testing.T) {
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := LoadEstate("../../examples/estates/storage-lab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Compile(est, reg, Options{HostArch: "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	sys := res.Model.StorageBy["site-1/storage/hall-a-fs"]
	if sys == nil || len(sys.Nodes) != 8 || len(sys.Enclosures) != 1 {
		t.Fatalf("storage system: %+v", sys)
	}
	// eight nodes' 12 NVMe and the JBOF's 24; the boot M.2 and the SAS HDDs behind the RAID card are not the pool's
	if len(sys.Drives) != 8*12+24 {
		t.Fatalf("pool drives: %d", len(sys.Drives))
	}
	raw := int64(8*12)*15_360_000_000_000 + 24*30_720_000_000_000
	if sys.RawBytes != raw || sys.UsableBytes != raw/10*8 || sys.DataUnits != 8 || sys.ParityUnits != 2 {
		t.Fatalf("capacity: raw %d usable %d (%d+%d)", sys.RawBytes, sys.UsableBytes, sys.DataUnits, sys.ParityUnits)
	}
	if sys.Manager != "generic/swordfish" || sys.Fidelity != "model" || len(sys.Protocols) != 3 || sys.FullPercent != 90 {
		t.Fatalf("system: %+v", sys)
	}
	bad := func(mut func(*schema.StorageSystem), want string) {
		t.Helper()
		est, _, _ := LoadEstate("../../examples/estates/storage-lab.yaml")
		mut(&est.Spec.StorageSystems[0])
		_, err := Compile(est, reg, Options{HostArch: "x86_64"})
		wantProblem(t, err, want)
	}
	bad(func(s *schema.StorageSystem) { s.Params["protection"] = "200+2" }, "protection 200+2 needs 202 drives")
	bad(func(s *schema.StorageSystem) { s.Networks.Data = "oob" }, "network oob has role management, not storage")
	bad(func(s *schema.StorageSystem) { s.Select = "n01" }, "hold no server")
	bad(func(s *schema.StorageSystem) { s.Protocols = []string{"smb"} }, `protocol "smb"`)
	bad(func(s *schema.StorageSystem) { s.Fidelity = "vm" }, "its nodes must be vm hosts with os twin")
}
