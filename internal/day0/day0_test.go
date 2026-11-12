package day0

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fabric is a two-leaf, one-spine design: leaf1 numbered to the spine, leaf2 unnumbered, both
// with EVPN and leaf1 with a segment.
func fabric(plan string) Design {
	return Design{Estate: "e1", Site: "site-1", Plan: plan,
		Switches: []Switch{
			{Path: "site-1/a10/spine-1", Hostname: "a10-spine-1", Role: "spine", Fabric: "frontend", Rack: "a10", U: 40, Profile: "generic/spine-switch-400g",
				Manufacturer: "Generic", Model: "Spine 400G", Serial: "SP1", BaseMAC: "02:00:00:00:01:00", MgmtMAC: "02:00:00:00:01:01", NOS: "generic/nvue-frr", Factory: true,
				ASN: 65100, RouterID: "10.255.0.1", Loopback: "10.255.0.1/32", EVPN: true,
				Interfaces: []Interface{
					{Name: "swp1", Model: "eth1", Speed: "400G", Kind: "fabric", Address: "10.254.0.0/31", Peer: "a10-leaf-1:swp63", PeerAddr: "10.254.0.1", PeerASN: 65101},
					{Name: "swp2", Model: "eth2", Speed: "400G", Kind: "unnumbered", Peer: "a10-leaf-2:swp63"},
				}},
			{Path: "site-1/a10/leaf-1", Hostname: "a10-leaf-1", Role: "leaf", Fabric: "frontend", Rack: "a10", U: 38, Profile: "generic/leaf-switch-400g",
				Serial: "LF1", BaseMAC: "02:00:00:00:02:00", MgmtMAC: "02:00:00:00:02:01", NOS: "generic/nvue-frr", Factory: true,
				ASN: 65101, RouterID: "10.255.0.2", Loopback: "10.255.0.2/32", EVPN: true,
				Interfaces: []Interface{
					{Name: "swp63", Model: "eth63", Speed: "400G", Kind: "fabric", Address: "10.254.0.1/31", Peer: "a10-spine-1:swp1", PeerAddr: "10.254.0.0", PeerASN: 65100},
					{Name: "swp1", Model: "eth1", Speed: "100G", Kind: "access", Vlan: "Vlan1000"},
				},
				Vlans: []Vlan{{Name: "Vlan1000", ID: 1000, Network: "10.30.0.0/24", Gateway: "10.30.0.1/24", Relay: []string{"10.0.0.5"}, Ports: []string{"swp1"}}}},
			{Path: "site-1/a10/leaf-2", Hostname: "a10-leaf-2", Role: "leaf", Fabric: "frontend", Rack: "a10", U: 37, Profile: "generic/leaf-switch-400g",
				Serial: "LF2", BaseMAC: "02:00:00:00:03:00", NOS: "generic/nvue-frr", Factory: true,
				ASN: 65102, RouterID: "10.255.0.3", Loopback: "10.255.0.3/32", EVPN: true,
				Interfaces: []Interface{{Name: "swp63", Model: "eth63", Speed: "400G", Kind: "unnumbered", Peer: "a10-spine-1:swp2"}}},
		},
		Cables: []Cable{
			{ID: "c2", Fabric: "frontend", ADevice: "a10-spine-1", APort: "eth2", AIf: "swp2", BDevice: "a10-leaf-2", BPort: "eth63", BIf: "swp63", Speed: "400G"},
			{ID: "c1", Fabric: "frontend", ADevice: "a10-spine-1", APort: "eth1", AIf: "swp1", BDevice: "a10-leaf-1", BPort: "eth63", BIf: "swp63", Speed: "400G"},
			{ID: "c3", Fabric: "frontend", ADevice: "a10-leaf-1", APort: "eth1", AIf: "swp1", BDevice: "site-1/a10/server-01", BPort: "eth0", Speed: "100G"},
		},
		Mgmt: []MgmtNet{{Network: "oob-a10", Rack: "a10", Prefix: "10.20.0.0/24", Gateway: "10.20.0.1"}},
	}
}

var allLoopbacks = []string{"10.255.0.1/32", "10.255.0.2/32", "10.255.0.3/32"}

// converged is what the observer reads from every switch of fabric("given") once it converged.
func converged() map[string]Observed {
	return map[string]Observed{
		"site-1/a10/spine-1": {Hostname: "a10-spine-1", Mgmt: "10.20.0.10", Routes: allLoopbacks,
			Sessions: map[string]Session{"10.254.0.1": {State: "Established", EVPN: true}, "swp2": {State: "Established", EVPN: true}}},
		"site-1/a10/leaf-1": {Hostname: "a10-leaf-1", Mgmt: "10.20.0.11", Routes: allLoopbacks, Vlans: map[string]string{"Vlan1000": "10.30.0.1/24"},
			Sessions: map[string]Session{"10.254.0.0": {State: "Established", EVPN: true}}},
		"site-1/a10/leaf-2": {Hostname: "a10-leaf-2", Mgmt: "10.20.0.12", Routes: allLoopbacks,
			Sessions: map[string]Session{"swp63": {State: "Established", EVPN: true}}},
	}
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name  string
		plan  string
		edit  func(o map[string]Observed)
		path  string
		phase string
		diffs []string
	}{
		{name: "converged", plan: "given", path: "site-1/a10/leaf-1", phase: PhaseConverged},
		{name: "factory", plan: "given", path: "site-1/a10/leaf-1", phase: PhaseFactory,
			edit: func(o map[string]Observed) { o["site-1/a10/leaf-1"] = Observed{} },
			diffs: []string{"hostname is (none), the design says a10-leaf-1", "swp63 → a10-spine-1:swp1: no BGP session",
				"10.255.0.1/32 (a10-spine-1's loopback) is not in the RIB", "10.255.0.2/32 (a10-leaf-1's loopback) is not in the RIB",
				"10.255.0.3/32 (a10-leaf-2's loopback) is not in the RIB", "Vlan1000 is missing, the design says 10.30.0.1/24", "eth0 holds no management lease"}},
		{name: "booted with a lease", plan: "given", path: "site-1/a10/leaf-2", phase: PhaseBooted,
			edit: func(o map[string]Observed) {
				o["site-1/a10/leaf-2"] = Observed{Hostname: "cumulus", Mgmt: "10.20.0.12"}
			},
			diffs: []string{"hostname is cumulus, the design says a10-leaf-2", "swp63 → a10-spine-1:swp2: no BGP session",
				"10.255.0.1/32 (a10-spine-1's loopback) is not in the RIB", "10.255.0.2/32 (a10-leaf-1's loopback) is not in the RIB",
				"10.255.0.3/32 (a10-leaf-2's loopback) is not in the RIB"}},
		{name: "reported phase wins", plan: "given", path: "site-1/a10/leaf-2", phase: PhaseZTPRunning,
			edit: func(o map[string]Observed) {
				o["site-1/a10/leaf-2"] = Observed{Hostname: "cumulus", Mgmt: "10.20.0.12", Phase: PhaseZTPRunning}
			},
			diffs: []string{"hostname is cumulus, the design says a10-leaf-2", "swp63 → a10-spine-1:swp2: no BGP session",
				"10.255.0.1/32 (a10-spine-1's loopback) is not in the RIB", "10.255.0.2/32 (a10-leaf-1's loopback) is not in the RIB",
				"10.255.0.3/32 (a10-leaf-2's loopback) is not in the RIB"}},
		{name: "a session not up", plan: "given", path: "site-1/a10/spine-1", phase: PhaseConfigured,
			edit: func(o map[string]Observed) {
				s := o["site-1/a10/spine-1"]
				s.Sessions = map[string]Session{"10.254.0.1": {State: "Active"}, "swp2": {State: "Established"}}
				s.Phase = PhaseConverged // the observer's word is not the oracle's
				o["site-1/a10/spine-1"] = s
			},
			diffs: []string{"swp1 → a10-leaf-1:swp63: Active, not Established", "swp2 → a10-leaf-2:swp63: Established, but L2VPN EVPN is not up"}},
		{name: "wrong gateway and no lease", plan: "given", path: "site-1/a10/leaf-1", phase: PhaseConfigured,
			edit: func(o map[string]Observed) {
				s := o["site-1/a10/leaf-1"]
				s.Vlans, s.Mgmt = map[string]string{"Vlan1000": "10.30.0.2/24"}, ""
				o["site-1/a10/leaf-1"] = s
			},
			diffs: []string{"Vlan1000 has 10.30.0.2/24, the design says 10.30.0.1/24", "eth0 holds no management lease"}},
		{name: "not observed", plan: "given", path: "site-1/a10/leaf-2", phase: PhaseFactory,
			edit: func(o map[string]Observed) { delete(o, "site-1/a10/leaf-2") }, diffs: []string{"not observed yet"}},

		{name: "tool: its own addresses", plan: "tool", path: "site-1/a10/spine-1", phase: PhaseConverged,
			edit: func(o map[string]Observed) {
				s := o["site-1/a10/spine-1"]
				s.Hostname, s.Routes = "sp1", nil
				s.Sessions = map[string]Session{"192.168.0.7": {State: "Established", EVPN: true}, "swp2": {State: "Established", EVPN: true}}
				o["site-1/a10/spine-1"] = s
			}},
		{name: "tool: a link without a session", plan: "tool", path: "site-1/a10/spine-1", phase: PhaseConfigured,
			edit: func(o map[string]Observed) {
				s := o["site-1/a10/spine-1"]
				s.Sessions = map[string]Session{"192.168.0.7": {State: "Idle"}, "swp2": {State: "Established", EVPN: true}}
				o["site-1/a10/spine-1"] = s
			},
			diffs: []string{"0 Established session(s) for 1 design link(s) without one of their own (swp1)"}},
		{name: "tool: a hostname twice", plan: "tool", path: "site-1/a10/leaf-2", phase: PhaseConfigured,
			edit: func(o map[string]Observed) {
				s := o["site-1/a10/leaf-2"]
				s.Hostname = "a10-leaf-1"
				o["site-1/a10/leaf-2"] = s
			},
			diffs: []string{"hostname a10-leaf-1 is also site-1/a10/leaf-1's"}},
		{name: "tool: factory hostname", plan: "tool", path: "site-1/a10/leaf-2", phase: PhaseConfigured,
			edit: func(o map[string]Observed) {
				s := o["site-1/a10/leaf-2"]
				s.Hostname = "sonic"
				o["site-1/a10/leaf-2"] = s
			},
			diffs: []string{"hostname is sonic, a factory one"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs := converged()
			if c.edit != nil {
				c.edit(obs)
			}
			vs := Evaluate(fabric(c.plan), obs)
			if len(vs) != 3 || vs[0].Path != "site-1/a10/leaf-1" {
				t.Fatalf("verdicts not one per switch in path order: %+v", vs)
			}
			var v Verdict
			for _, x := range vs {
				if x.Path == c.path {
					v = x
				}
			}
			if v.Phase != c.phase || v.Converged != (c.phase == PhaseConverged) {
				t.Errorf("phase %s converged %v, want %s", v.Phase, v.Converged, c.phase)
			}
			if !reflect.DeepEqual(v.Diffs, c.diffs) {
				t.Errorf("diffs:\n got %q\nwant %q", v.Diffs, c.diffs)
			}
		})
	}
}

func readCSV(t *testing.T, p string) [][]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestWriteInputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteInputs(dir, fabric("tool")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "plan.json")); !os.IsNotExist(err) {
		t.Error("plan: tool left a plan.json")
	}
	if err := WriteInputs(dir, fabric("given")); err != nil {
		t.Fatal(err)
	}

	var exp []ExpectedSwitch
	data, _ := os.ReadFile(filepath.Join(dir, "expected-switches.json"))
	if err := json.Unmarshal(data, &exp); err != nil {
		t.Fatal(err)
	}
	if len(exp) != 3 || exp[0].Hostname != "a10-leaf-1" || exp[2].MgmtMAC != "02:00:00:00:01:01" || !exp[2].Factory || exp[2].NOS != "generic/nvue-frr" {
		t.Errorf("expected-switches.json: %+v", exp)
	}

	cab := readCSV(t, filepath.Join(dir, "cabling.csv"))
	if strings.Join(cab[0], ",") != "id,fabric,a_device,a_port,a_interface,b_device,b_port,b_interface,speed" || len(cab) != 4 || cab[1][0] != "c1" {
		t.Errorf("cabling.csv: %v", cab)
	}

	dot, _ := os.ReadFile(filepath.Join(dir, "topology.dot"))
	want := "graph G {\n  \"a10-spine-1\":\"swp1\" -- \"a10-leaf-1\":\"swp63\";\n  \"a10-spine-1\":\"swp2\" -- \"a10-leaf-2\":\"swp63\";\n}\n"
	if string(dot) != want {
		t.Errorf("topology.dot:\n%s\nwant\n%s", dot, want)
	}

	var plan Plan
	data, _ = os.ReadFile(filepath.Join(dir, "plan.json"))
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Switches) != 3 || plan.Switches[0].ASN != 65101 || plan.Switches[0].Interfaces[0].Name != "swp1" || len(plan.Mgmt) != 1 {
		t.Errorf("plan.json: %+v", plan)
	}

	nb := filepath.Join(dir, "netbox")
	for _, n := range netboxOrder {
		if _, err := os.Stat(filepath.Join(nb, n)); err != nil {
			t.Errorf("netbox/%s: %v", n, err)
		}
	}
	devs := readCSV(t, filepath.Join(nb, "devices.csv"))
	if strings.Join(devs[0], ",") != "name,role,manufacturer,device_type,platform,serial,status,site,rack,position,face,description,cf_bgp_asn" ||
		devs[1][0] != "a10-leaf-1" || devs[1][12] != "65101" || devs[1][2] != "Generic" || devs[1][3] != "generic/leaf-switch-400g" || devs[3][3] != "Spine 400G" {
		t.Errorf("devices.csv: %v", devs)
	}
	cables := readCSV(t, filepath.Join(nb, "cables.csv"))
	if len(cables) != 3 || cables[1][1] != "dcim.interface" {
		t.Errorf("cables.csv (switch-to-switch only): %v", cables)
	}
	ifs := readCSV(t, filepath.Join(nb, "interfaces.csv"))
	found := false
	for _, r := range ifs {
		if r[0] == "a10-spine-1" && r[1] == "swp1" {
			found = r[2] == "400gbase-x-qsfpdd" && r[3] == "400000000"
		}
	}
	if !found {
		t.Errorf("interfaces.csv: %v", ifs)
	}
	ips := readCSV(t, filepath.Join(nb, "ip_addresses.csv"))
	if ips[1][0] != "10.255.0.2/32" || ips[1][2] != "loopback" || ips[1][4] != "lo" {
		t.Errorf("ip_addresses.csv: %v", ips)
	}
	readme, _ := os.ReadFile(filepath.Join(nb, "README.md"))
	if !strings.Contains(string(readme), "NetBox "+NetBoxVersion) || !strings.Contains(string(readme), "1. `custom_fields.csv`") {
		t.Errorf("README.md: %s", readme)
	}

	// deterministic: the same design in another order writes the same bytes
	d2 := fabric("given")
	d2.Switches[0], d2.Switches[2] = d2.Switches[2], d2.Switches[0]
	dir2 := t.TempDir()
	if err := WriteInputs(dir2, d2); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"expected-switches.json", "cabling.csv", "topology.dot", "plan.json", "netbox/devices.csv", "netbox/interfaces.csv", "netbox/prefixes.csv"} {
		a, _ := os.ReadFile(filepath.Join(dir, n))
		b, _ := os.ReadFile(filepath.Join(dir2, n))
		if string(a) != string(b) {
			t.Errorf("%s differs with the switches in another order", n)
		}
	}
}
