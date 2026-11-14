package tap

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/day0"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

func loadDay0Estate(t *testing.T, fabricNOS, install string) *schema.Estate {
	t.Helper()
	b, err := os.ReadFile("../../../../test/linuxnet/testdata/tap-day0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	images := "[]"
	if install == "onie" {
		images = `[{ match: { name: "onie-installer.*" }, nos: ` + fabricNOS + ` }]`
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "dctwin-day0"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.NewReplacer("ESTATE_NAME", "linuxnet-day0", "STACKS", "[]", "CUMULUS_VERSION", "", "PLAN_MODE", "given", "FABRIC_NOS", fabricNOS, "INSTALL_MODE", install, "ZTP_MODE", "enabled", "BOOT_TIME", "60s",
		"DAY0_BINARIES", bin, "ONIE_IMAGES", images, "KEA_CONTAINER", "dctwin-kea-test", "PROBE_NETNS", "dctwin-probe-test").Replace(string(b)))
	var est schema.Estate
	if err := schema.Decode(b, &est); err != nil {
		t.Fatal(err)
	}
	return &est
}

// A factory fabric (D72): its switches are marked factory with their identity, each one's
// management port is cabled onto the OOB network (D73) as an access port of its rack segment's
// VLAN on the real OOB switch, the design (D77) keeps what the twin would have configured, and the
// twin-configured OOB is not factory.
func TestPlanFactory(t *testing.T) {
	for _, n := range []string{nos.NVUEFRR, nos.SonicVS} {
		t.Run(n, func(t *testing.T) {
			est := loadDay0Estate(t, n, "onie")
			m := compileModel(t, est)
			p, err := BuildPlan(m, est, nil, "x86_64")
			if err != nil {
				t.Fatal(err)
			}
			factory, twin := 0, 0
			for _, sp := range p.Switches {
				f := sp.Config.Factory
				if f == nil {
					twin++
					if sp.NOS.Name != nos.NVUEFRR {
						t.Errorf("%s: an OOB switch runs %s", sp.Device.Path, sp.NOS.Name)
					}
					continue
				}
				factory++
				if sp.NOS.Name != n {
					t.Errorf("%s: runs %s, its fabric's NOS is %s", sp.Device.Path, sp.NOS.Name, n)
				}
				if !sp.Config.Mgmt || f.Install != "onie" || !f.ZTP || f.Identity.Serial == "" || f.Identity.MgmtMAC == "" || len(f.Images) != 1 {
					t.Errorf("%s: factory %+v, mgmt %v", sp.Device.Path, f, sp.Config.Mgmt)
				}
				if f.BootTime.Seconds() != 3 { // 60s × timeScale 0.05
					t.Errorf("%s: boot time %s", sp.Device.Path, f.BootTime)
				}
				if len(sp.Binds) != 0 {
					t.Errorf("%s: binds %v (the substrate adds the day-0 ones at up)", sp.Device.Path, sp.Binds)
				}
				if _, err := sp.NOS.StartScript(sp.Config); err != nil {
					t.Errorf("%s: %v", sp.Device.Path, err)
				} else if s, _ := sp.NOS.StartScript(sp.Config); !strings.Contains(s, "onie-discover install") {
					t.Errorf("%s: its start script runs no ONIE discovery", sp.Device.Path)
				}
			}
			if factory != 4 || twin == 0 {
				t.Fatalf("%d factory switches (want 2 leaves, 2 spines), %d twin-configured", factory, twin)
			}
			if len(p.Mgmt) != factory {
				t.Fatalf("%d management ports planned for %d factory switches", len(p.Mgmt), factory)
			}
			for _, mp := range p.Mgmt {
				if mp.OOB == nil || mp.PeerIf == "" || mp.Bridge != nil {
					t.Errorf("%s: not on the real OOB switch: %+v", mp.Ref, mp)
					continue
				}
				found := false
				for _, v := range mp.OOB.Config.Vlans {
					for _, port := range v.Ports {
						found = found || port == mp.Peer.Name
					}
				}
				if !found {
					t.Errorf("%s: %s's port %s is in no VLAN", mp.Ref, mp.OOB.Config.Hostname, mp.Peer.Name)
				}
			}
			d := p.Day0Design(m)
			if d.Plan != "given" {
				t.Errorf("plan %q", d.Plan)
			}
			var leaves, spines int
			for _, s := range d.Switches {
				if !s.Factory {
					continue
				}
				switch s.Role {
				case "leaf":
					leaves++
				case "spine":
					spines++
				}
				if s.Loopback == "" || s.ASN == 0 || len(s.Interfaces) == 0 || s.Platform == "" {
					t.Errorf("%s: design %+v", s.Hostname, s)
				}
				for _, it := range s.Interfaces {
					if it.Kind == "fabric" && (it.PeerAddr == "" || !strings.Contains(it.Peer, ":")) {
						t.Errorf("%s %s: %+v", s.Hostname, it.Name, it)
					}
				}
			}
			if leaves != 2 || spines != 2 {
				t.Errorf("%d leaves and %d spines in the design", leaves, spines)
			}
			if len(d.Mgmt) == 0 || len(d.Cables) == 0 {
				t.Errorf("design: %d management networks, %d cables", len(d.Mgmt), len(d.Cables))
			}
			// the inputs a site's tooling reads, and the reference tooling's generator over them
			dir := t.TempDir()
			if err := day0.WriteInputs(filepath.Join(dir, "day0"), d); err != nil {
				t.Fatal(err)
			}
			if py, err := exec.LookPath("python3"); err == nil {
				out, err := exec.Command(py, "../../../../stacks/reference-day0/render.py", filepath.Join(dir, "day0"), filepath.Join(dir, "out"),
					"--server", "10.90.1.10:8080", "--password", "x").CombinedOutput()
				if err != nil {
					t.Fatalf("render.py: %v\n%s", err, out)
				}
				var kea map[string]any
				b, _ := os.ReadFile(filepath.Join(dir, "out", "kea-dhcp4.json"))
				if err := json.Unmarshal(b, &kea); err != nil {
					t.Fatalf("kea-dhcp4.json: %v", err)
				}
				files, _ := filepath.Glob(filepath.Join(dir, "out", "www", "ztp", "*"))
				if len(files) != factory {
					t.Errorf("render.py wrote %d ZTP files for %d factory switches: %s", len(files), factory, out)
				}
				if n == nos.NVUEFRR && len(files) > 0 {
					b, _ := os.ReadFile(files[0])
					if !strings.Contains(string(b), "CUMULUS-AUTOPROVISIONING") || !strings.Contains(string(b), "nv set router bgp autonomous-system") {
						t.Errorf("a ZTP script:\n%s", b)
					}
				}
			}
		})
	}
}

// A cable swap (generic/cabling) injected as the day-0 e2e injects it, by the design's port refs,
// is applied: each leaf's uplink reaches the other's spine port as built.
func TestCableSwapAsBuilt(t *testing.T) {
	est := loadDay0Estate(t, nos.NVUEFRR, "preinstalled")
	m := compileModel(t, est)
	p, err := BuildPlan(m, est, nil, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	d := p.Day0Design(m)
	var a, b string
	for _, c := range d.Cables {
		if strings.HasSuffix(c.ADevice, "spine-1") || strings.HasSuffix(c.BDevice, "spine-1") {
			leaf := c.ADevice + ":" + c.APort
			if strings.HasSuffix(c.ADevice, "spine-1") {
				leaf = c.BDevice + ":" + c.BPort
			}
			switch {
			case strings.Contains(leaf, "leaf-r01"):
				a = leaf
			case strings.Contains(leaf, "leaf-r02"):
				b = leaf
			}
		}
	}
	ref := func(hostPort string) string {
		host, port, _ := strings.Cut(hostPort, ":")
		for _, s := range d.Switches {
			if s.Hostname == host {
				return s.Path + ":" + port
			}
		}
		return hostPort
	}
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(m, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	portOf := func(r string) *model.Port {
		dev, port, _ := strings.Cut(r, ":")
		if d := m.DeviceBy[dev]; d != nil {
			return d.PortBy[port]
		}
		return nil
	}
	pa, pb := portOf(ref(a)), portOf(ref(b))
	if pa == nil || pb == nil {
		t.Fatalf("ports %s %s not found", ref(a), ref(b))
	}
	before := eng.AsBuiltPeer(pa)
	views, err := eng.Inject(ref(a), "generic/cabling/cable-swap", map[string]any{"with": ref(b)}, "swap", schema.EventSource{Class: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	after := eng.AsBuiltPeer(pa)
	t.Logf("%s reached %s, now %s; fault %+v", pa.Ref, before.Ref, after.Ref, views)
	if after == before || after != pb.Link.Peer(pb) {
		t.Fatalf("the swap is not applied: %s reaches %s", pa.Ref, after.Ref)
	}
}

// A DSX day 0 (18 — Z7d): the factory OOB leaves' management ports lease through the aggregation
// switch the twin configures, and NVCM's blueprint has each leaf's BMC segment as VLAN 201 in
// VRF OOB, the aggregation switch as the device NVCM does not provision, and an EVPN peering
// between their loopbacks.
func TestPlanDSXDay0(t *testing.T) {
	b, err := os.ReadFile("../../../../examples/estates/dsx-day0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "dctwin-day0"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.ReplaceAll(string(b), "day0Binaries: ./bin", "day0Binaries: "+bin))
	var est schema.Estate
	if err := schema.Decode(b, &est); err != nil {
		t.Fatal(err)
	}
	m := compileModel(t, &est)
	p, err := BuildPlan(m, &est, nil, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	var factory []string
	for _, sp := range p.Switches {
		if sp.Config.Factory != nil {
			factory = append(factory, sp.Device.Path)
			if !sp.Config.Mgmt {
				t.Errorf("%s: its management port is not planned", sp.Device.Path)
			}
		}
	}
	sort.Strings(factory)
	if strings.Join(factory, " ") != "site-1/hall-a/r01/mgmt-switch site-1/hall-a/r02/mgmt-switch" {
		t.Errorf("factory: %v", factory)
	}
	for _, mp := range p.Mgmt {
		if mp.OOB == nil || mp.OOB.Device.Name != "oob-agg-1" {
			t.Errorf("%s's management port is not on the aggregation switch", mp.Ref)
		}
	}
	// the aggregation switch, cabled to factory leaves, runs LLDP; a twin switch no factory
	// switch's front panel reaches does not
	for _, sp := range p.Switches {
		if want := sp.Device.Name == "oob-agg-1"; sp.Config.Factory == nil && sp.Config.LLDP != want {
			t.Errorf("%s: LLDP %v", sp.Device.Path, sp.Config.LLDP)
		}
	}
	dir := t.TempDir()
	if out := os.Getenv("DCTWIN_DESIGN_OUT"); out != "" {
		dir = out // to look at, or to load into NVCM by hand
	}
	if err := day0.WriteInputs(dir, p.Day0Design(m)); err != nil {
		t.Fatal(err)
	}
	bp := filepath.Join(dir, "nvcm", "context", day0.NVCMBlueprint)
	for _, f := range []string{"devices/r01-mgmt-switch.json", "devices/r02-mgmt-switch.json", "devices/n01-oob-agg-1.json", "helpers.json"} {
		if _, err := os.Stat(filepath.Join(bp, f)); err != nil {
			t.Errorf("blueprint: %v", err)
		}
	}
	leaf, _ := os.ReadFile(filepath.Join(bp, "devices/r01-mgmt-switch.json"))
	for _, want := range []string{`"name": "vlan201"`, `"rd": "*:2000"`, `"name": "lo-oob"`, `"l3_vlan_id": 4002`, `"vid": 201`} {
		if !strings.Contains(string(leaf), want) {
			t.Errorf("r01-mgmt-switch lacks %s", want)
		}
	}
	peer, _ := os.ReadFile(filepath.Join(bp, "bgp_routing_instances.yaml"))
	if !strings.Contains(string(peer), "source_interface: lo") {
		t.Errorf("no EVPN peering:\n%s", peer)
	}
	if t.Failed() {
		t.Logf("leaf:\n%s", leaf)
	}
}
