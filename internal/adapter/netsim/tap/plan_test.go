package tap

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

func loadEstate(t *testing.T, name string) *schema.Estate {
	t.Helper()
	b, err := os.ReadFile("../../../../test/linuxnet/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.NewReplacer("KEA_CONTAINER", "dctwin-kea-test", "PROBE_NETNS", "dctwin-probe-test").Replace(string(b)))
	var est schema.Estate
	if err := schema.Decode(b, &est); err != nil {
		t.Fatal(err)
	}
	return &est
}

func compileModel(t *testing.T, est *schema.Estate) *model.Model {
	t.Helper()
	return compileModelFor(t, est, compile.HostArch())
}

// compileModelFor compiles for a host of the architecture given: an estate whose vm hosts are
// x86_64 compiles for an x86_64 host (KVM) wherever the test runs.
func compileModelFor(t *testing.T, est *schema.Estate, hostArch string) *model.Model {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: hostArch})
	if err != nil {
		t.Fatal(err)
	}
	return res.Model
}

// On model switches every BMC gets an endpoint in its rack's bridge, and nothing is a NOS.
func TestPlanModelSwitches(t *testing.T) {
	est := loadEstate(t, "tap-model.yaml")
	p, err := BuildPlan(compileModel(t, est), est, nil, "aarch64")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Switches) != 0 || len(p.Links) != 0 {
		t.Fatalf("model switches planned %d NOS switches and %d fabric cables", len(p.Switches), len(p.Links))
	}
	if len(p.Bridges) != 2 {
		t.Fatalf("%d bridges, want one per rack segment (2)", len(p.Bridges))
	}
	macs := map[string]bool{}
	for _, ep := range p.Endpoints {
		if ep.Bridge == nil || ep.Switch != nil {
			t.Errorf("%s: not on a bridge", ep.Ref)
		}
		if !strings.HasSuffix(ep.Bridge.Rack, "/"+strings.Split(strings.TrimPrefix(ep.Device, "site-1/hall-a/"), "/")[0]) {
			t.Errorf("%s: on the bridge of %s", ep.Ref, ep.Bridge.Rack)
		}
		if macs[ep.MAC.String()] {
			t.Errorf("%s: MAC %s twice", ep.Ref, ep.MAC)
		}
		macs[ep.MAC.String()] = true
		if len(ep.TapIf) > 15 || ep.SwitchIf != ep.TapIf {
			t.Errorf("%s: interface names %q %q", ep.Ref, ep.TapIf, ep.SwitchIf)
		}
	}
	t.Logf("%d BMC endpoints on %d bridges; skipped %v", len(p.Endpoints), len(p.Bridges), p.Skipped)
	if len(p.Endpoints) < 6 {
		t.Fatalf("%d endpoints, want at least the six servers' BMCs", len(p.Endpoints))
	}
}

// With a real NOS on the OOB fabric, BMC ports are the switches' ports, each management switch
// carries its rack's segment as a VLAN with the gateway and relays to the stack's server, the
// aggregation switch peers with both over /31s, and the attachments become service ports.
func TestPlanRealNOS(t *testing.T) {
	est := loadEstate(t, "tap-nos.yaml")
	p, err := BuildPlan(compileModel(t, est), est, nil, "aarch64")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Bridges) != 0 {
		t.Errorf("%d bridges: a real NOS's segments need none", len(p.Bridges))
	}
	if len(p.Switches) != 3 {
		t.Fatalf("%d real switches, want two management switches and the aggregation switch", len(p.Switches))
	}
	var agg *SwitchPlan
	for _, sp := range p.Switches {
		if sp.NOS.Name != nos.FRRDHCRelay || sp.Image == "" || !strings.HasPrefix(sp.Container, "dctwin-linuxnet-nos-") {
			t.Errorf("%s: nos %s image %q container %q", sp.Device.Path, sp.NOS.Name, sp.Image, sp.Container)
		}
		if len(sp.Config.Vlans) == 0 {
			agg = sp
			continue
		}
		v := sp.Config.Vlans[0]
		if v.Gateway.Addr() != netip.MustParseAddr("10.30.0.1") && v.Gateway.Addr() != netip.MustParseAddr("10.30.1.1") || v.Gateway.Bits() != 24 {
			t.Errorf("%s: VLAN gateway %s", sp.Device.Path, v.Gateway)
		}
		if len(v.Relay) != 1 || v.Relay[0] != netip.MustParseAddr("10.90.0.10") {
			t.Errorf("%s: relays to %v", sp.Device.Path, v.Relay)
		}
		if len(sp.Config.Routed) != 1 || sp.Config.Routed[0].Addr.Bits() != 31 || sp.Config.ASN < 65101 {
			t.Errorf("%s: routed %+v ASN %d", sp.Device.Path, sp.Config.Routed, sp.Config.ASN)
		}
		script, err := sp.NOS.StartScript(sp.Config)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("DCTWIN_DUMP_NOS") != "" {
			t.Logf("%s start script:\n%s", sp.Container, script)
		}
		for _, want := range []string{"ip link add " + v.Name + " type bridge", "dhcrelay -4 -d -a -m discard -id " + v.Name + " -iu up1 10.90.0.10", "router bgp", "redistribute connected"} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: script lacks %q:\n%s", sp.Device.Path, want, script)
			}
		}
	}
	if agg == nil || agg.Config.ASN != 65000 || len(agg.Config.Routed) != 2 || len(agg.Config.Service) != 2 {
		t.Fatalf("aggregation switch: %+v", agg)
	}
	if len(p.Links) != 2 || len(p.Attach) != 2 {
		t.Fatalf("%d fabric cables and %d attachments, want 2 and 2", len(p.Links), len(p.Attach))
	}
	for _, ep := range p.Endpoints {
		if ep.Switch == nil || !strings.HasPrefix(ep.SwitchIf, "eth") {
			t.Errorf("%s: switch side %q", ep.Ref, ep.SwitchIf)
		}
	}
	t.Logf("%d BMC endpoints into %d real switches, %d cables, %d attachments", len(p.Endpoints), len(p.Switches), len(p.Links), len(p.Attach))

	// a NOS that is registered but cannot run is refused with its reason
	est.Spec.Fabrics[0].NOS = "cumulus-vx"
	if _, err := BuildPlan(compileModel(t, est), est, nil, "x86_64"); err == nil || !strings.Contains(err.Error(), "not freely redistributable") {
		t.Errorf("cumulus-vx: %v", err)
	}
	// sonic-vs, refused here until it was runnable (D44), now plans: natively on x86_64 and
	// emulated on aarch64, where its linux/amd64 image runs under the Docker host's translation
	est.Spec.Fabrics[0].NOS = nos.SonicVS
	for arch, emulated := range map[string]bool{"x86_64": false, "aarch64": true} {
		sp, err := BuildPlan(compileModel(t, est), est, nil, arch)
		if err != nil {
			t.Fatalf("sonic-vs on %s: %v", arch, err)
		}
		for _, sw := range sp.Switches {
			if sw.Emulated != emulated {
				t.Errorf("sonic-vs on %s: %s emulated = %v", arch, sw.Device.Path, sw.Emulated)
			}
		}
	}
	// and a dhcp: twin network cannot be relayed by one
	est.Spec.Fabrics[0].NOS = nos.FRRDHCRelay
	n := est.Spec.Networks["oob"]
	n.DHCP = "twin"
	est.Spec.Networks["oob"] = n
	if _, err := BuildPlan(compileModel(t, est), est, nil, "aarch64"); err == nil || !strings.Contains(err.Error(), "dhcp: twin") {
		t.Errorf("dhcp: twin behind a real NOS: %v", err)
	}
}

// SONiC-VS on the OOB fabric: each switch's cabled ports become its container's eth<n> (n the
// port's place on the front panel, so a management switch's up1 is eth49 and SONiC's Ethernet48),
// the switch's MAC is its management port's, and the start-up script carries the HWSKU, the
// config_db.json and the bgpd configuration rendered from the model.
func TestPlanSonicVS(t *testing.T) {
	est := loadEstate(t, "tap-sonic.yaml")
	p, err := BuildPlan(compileModel(t, est), est, nil, "aarch64")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Switches) != 2 || len(p.Links) != 1 || len(p.Attach) != 2 {
		t.Fatalf("%d switches, %d cables, %d attachments: want 2, 1, 2", len(p.Switches), len(p.Links), len(p.Attach))
	}
	// every switch's Loopback0 is its router id, from the top of the underlay down, clear of the /31s
	los := map[netip.Addr]bool{}
	for i, sp := range p.Switches {
		want := netip.AddrFrom4([4]byte{10, 255, 255, byte(254 - i)})
		if sp.Config.Loopback != want || sp.Config.RouterID != want {
			t.Errorf("%s: loopback %s router id %s, want %s", sp.Device.Path, sp.Config.Loopback, sp.Config.RouterID, want)
		}
		los[sp.Config.Loopback] = true
	}
	if len(los) != len(p.Switches) {
		t.Errorf("loopbacks are not unique: %v", los)
	}
	for _, sp := range p.Switches {
		if sp.NOS.Name != nos.SonicVS || !sp.Emulated || sp.Image != nos.SonicVSImage || sp.Config.MAC == "" || len(sp.Config.Panel) == 0 {
			t.Errorf("%s: nos %s emulated %v image %s MAC %q panel %d", sp.Device.Path, sp.NOS.Name, sp.Emulated, sp.Image, sp.Config.MAC, len(sp.Config.Panel))
		}
		script, err := sp.NOS.StartScript(sp.Config)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("DCTWIN_DUMP_NOS") != "" {
			t.Logf("%s start script:\n%s", sp.Container, script)
		}
		db, err := nos.ConfigDB(sp.Config)
		if err != nil {
			t.Fatal(err)
		}
		if len(sp.Config.Vlans) > 0 { // the rack's management switch
			v := sp.Config.Vlans[0]
			vlan := db["VLAN"][v.Name].(map[string]any)
			if v.Name != "Vlan1000" || v.Gateway.String() != "10.30.0.1/24" || len(vlan["dhcp_servers"].([]string)) != 1 || vlan["dhcp_servers"].([]string)[0] != "10.90.0.10" {
				t.Errorf("%s: VLAN %s %s %v", sp.Device.Path, v.Name, v.Gateway, vlan)
			}
			if _, ok := db["VLAN_INTERFACE"]["Vlan1000|10.30.0.1/24"]; !ok {
				t.Errorf("%s: no gateway on Vlan1000: %v", sp.Device.Path, db["VLAN_INTERFACE"])
			}
			if len(db["VLAN_MEMBER"]) != len(v.Ports) {
				t.Errorf("%s: %d VLAN members for %d ports", sp.Device.Path, len(db["VLAN_MEMBER"]), len(v.Ports))
			}
			up := sp.Config.Routed[0]
			if up.Port != "up1" || sp.NOS.Interface(sp.Config, "up1") != "eth49" || nos.SonicName(sp.Config, "up1") != "Ethernet48" {
				t.Errorf("%s: uplink %s is %s, %s", sp.Device.Path, up.Port, sp.NOS.Interface(sp.Config, up.Port), nos.SonicName(sp.Config, up.Port))
			}
			if _, ok := db["INTERFACE"]["Ethernet48|"+up.Addr.String()]; !ok {
				t.Errorf("%s: no %s on Ethernet48: %v", sp.Device.Path, up.Addr, db["INTERFACE"])
			}
			for _, want := range []string{"eth49:49\n", "Ethernet48 49 up1 10000 49\n", "redis-cli -n 6 HGET 'INTERFACE_TABLE|Vlan1000|10.30.0.1/24' state", "exec env HWSKU=DCTwin-generic-mgmt-switch-1g /usr/local/bin/supervisord"} {
				if !strings.Contains(script, want) {
					t.Errorf("%s: script lacks %q", sp.Device.Path, want)
				}
			}
			if n, _ := db["BGP_NEIGHBOR"]["default|"+up.Peer.String()].(map[string]any); n["asn"] != "65000" {
				t.Errorf("%s: BGP_NEIGHBOR %v", sp.Device.Path, db["BGP_NEIGHBOR"])
			}
			continue
		}
		// the aggregation switch: a 128x400G panel, three cabled ports
		if len(db["PORT"]) != 3 || sp.NOS.Interface(sp.Config, "eth128") != "eth128" || nos.SonicName(sp.Config, "eth128") != "Ethernet1016" {
			t.Errorf("%s: ports %v; eth128 is %s", sp.Device.Path, db["PORT"], nos.SonicName(sp.Config, "eth128"))
		}
		if _, ok := db["INTERFACE"]["Ethernet1016|10.90.0.1/24"]; !ok {
			t.Errorf("%s: Kea's port has no gateway: %v", sp.Device.Path, db["INTERFACE"])
		}
		if _, ok := db["VLAN"]; ok {
			t.Errorf("%s: the aggregation switch has VLANs", sp.Device.Path)
		}
	}
	for _, ep := range p.Endpoints {
		if want := ep.Switch.NOS.Interface(ep.Switch.Config, ep.Peer.Name); ep.SwitchIf != want || !strings.HasPrefix(want, "eth") {
			t.Errorf("%s: switch side %q, want %q", ep.Ref, ep.SwitchIf, want)
		}
	}
	for _, at := range p.Attach {
		if at.SwitchIf != at.Port { // eth127 and eth128 are the 127th and 128th ports
			t.Errorf("attach at %s: switch side %s", at.Port, at.SwitchIf)
		}
	}
	t.Logf("%d BMC endpoints into %d SONiC-VS switches, %d cables, %d attachments", len(p.Endpoints), len(p.Switches), len(p.Links), len(p.Attach))
}

// spec.adapters.network.nos.memory bounds every NOS container; a bad value is refused at plan.
func TestPlanNOSMemory(t *testing.T) {
	est := loadEstate(t, "tap-nos.yaml")
	m := compileModel(t, est)
	p, err := BuildPlan(m, est, nil, "aarch64")
	if err != nil || p.NOSMemory != 0 {
		t.Fatalf("no memory set: %d, %v", p.NOSMemory, err)
	}
	est.Spec.Adapters.Network.NOS.Memory = "768m"
	if p, err = BuildPlan(m, est, nil, "aarch64"); err != nil || p.NOSMemory != 768<<20 {
		t.Fatalf("768m: %d, %v", p.NOSMemory, err)
	}
	est.Spec.Adapters.Network.NOS.Memory = "lots"
	if _, err = BuildPlan(m, est, nil, "aarch64"); err == nil || !strings.Contains(err.Error(), "spec.adapters.network.nos.memory") {
		t.Fatalf("lots: %v", err)
	}
}

// spec.adapters.network.nos.cumulusVersion is every generic/nvue-frr switch's build, which its
// NVUE's grammar and ZTP's version follow; unset, the build is NVUE's default.
func TestPlanCumulusVersion(t *testing.T) {
	est := loadEstate(t, "tap-nvue.yaml")
	m := compileModel(t, est)
	p, err := BuildPlan(m, est, nil, "aarch64")
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range p.Switches {
		if sp.Config.Build != "" || sp.NOS.SwitchVersion(sp.Config) != sp.NOS.FactoryVersion() {
			t.Errorf("%s: build %q with no version set", sp.Device.Path, sp.Config.Build)
		}
	}
	est.Spec.Adapters.Network.NOS.CumulusVersion = "5.18.3"
	if p, err = BuildPlan(m, est, nil, "aarch64"); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, sp := range p.Switches {
		if sp.NOS.Name != nos.NVUEFRR {
			continue
		}
		n++
		if sp.Config.Build != "Cumulus Linux 5.18.3" || sp.NOS.SwitchVersion(sp.Config) != "5.18.3" {
			t.Errorf("%s: build %q", sp.Device.Path, sp.Config.Build)
		}
	}
	if n == 0 {
		t.Fatalf("no generic/nvue-frr switch in tap-nvue.yaml")
	}
	est.Spec.Adapters.Network.NOS.CumulusVersion = "4.4"
	if _, err = BuildPlan(m, est, nil, "aarch64"); err == nil || !strings.Contains(err.Error(), "cumulusVersion") {
		t.Fatalf("4.4: %v", err)
	}
}

// Every device the kernel probe creates has a name the kernel takes (IFNAMSIZ: 15 characters):
// a longer one fails with "Attribute failed policy validation", which read as a kernel without
// VRF devices on every host.
func TestKernelProbeNames(t *testing.T) {
	for _, p := range kernelProbes {
		f := strings.Fields(p.add)
		if len(f) < 4 || f[0] != "ip" || f[2] != "add" {
			t.Fatalf("%s: %q is not ip link add <dev>", p.feature, p.add)
		}
		if len(f[3]) > 15 {
			t.Errorf("%s: device %q is %d characters, more than IFNAMSIZ allows (15)", p.feature, f[3], len(f[3]))
		}
	}
}

// A vm host's data ports cabled to a real NOS are the switch's ports (D57): each is a TAP moved
// into its leaf, and the leaf carries each network's rack segment as a VLAN of its own, with the
// segment gateway, relaying the DHCP network to the stack's server and not the other. The BMCs
// stay on their model switches' bridges.
func TestPlanVMHostPorts(t *testing.T) {
	est := loadEstate(t, "tap-vm-nos.yaml")
	p, err := BuildPlan(compileModelFor(t, est, "x86_64"), est, nil, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.HostPorts) != 4 {
		t.Fatalf("%d host ports, want both servers' eth0 and eth1; skipped %v", len(p.HostPorts), p.Skipped)
	}
	names := map[string]bool{}
	for _, ep := range p.Endpoints {
		names[ep.TapIf] = true
		if ep.Bridge == nil {
			t.Errorf("BMC %s: not on its model switch's bridge", ep.Ref)
		}
	}
	for _, hp := range p.HostPorts {
		if hp.Switch == nil || !strings.Contains(hp.Switch.Device.Path, "leaf-") {
			t.Fatalf("%s: not on a leaf (%v)", hp.Ref, hp.Switch)
		}
		rack := hp.Rack[strings.LastIndex(hp.Rack, "/")+1:]
		if !strings.Contains(hp.Switch.Device.Path, "leaf-"+rack+"-") {
			t.Errorf("%s: on %s, not its rack's leaf", hp.Ref, hp.Switch.Device.Path)
		}
		if names[hp.TapIf] || len(hp.TapIf) > 15 {
			t.Errorf("%s: TAP name %q is taken or too long", hp.Ref, hp.TapIf)
		}
		names[hp.TapIf] = true
		if want := hp.Switch.NOS.Interface(hp.Switch.Config, hp.Peer.Name); hp.SwitchIf != want || !strings.HasPrefix(want, "swp") {
			t.Errorf("%s: switch interface %q, want %q", hp.Ref, hp.SwitchIf, want)
		}
		if hp.MAC.String() != hp.Port.MAC {
			t.Errorf("%s: MAC %s, the model's is %s", hp.Ref, hp.MAC, hp.Port.MAC)
		}
		if hp.Vlan == "" {
			t.Errorf("%s: no VLAN", hp.Ref)
		}
	}
	leaves := 0
	for _, sp := range p.Switches {
		if !strings.Contains(sp.Device.Path, "leaf-") {
			if len(sp.Config.Vlans) != 0 || len(sp.Config.Service) != 2 {
				t.Errorf("spine %s: %d VLANs, %d service ports", sp.Device.Path, len(sp.Config.Vlans), len(sp.Config.Service))
			}
			continue
		}
		leaves++
		r := 0
		if strings.Contains(sp.Device.Path, "leaf-r02-") {
			r = 1
		}
		if len(sp.Config.Vlans) != 2 {
			t.Fatalf("leaf %s: VLANs %+v, want provisioning and frontend", sp.Device.Path, sp.Config.Vlans)
		}
		seen := map[string]bool{}
		for _, v := range sp.Config.Vlans {
			if seen[v.Name] {
				t.Errorf("leaf %s: VLAN %s twice", sp.Device.Path, v.Name)
			}
			seen[v.Name] = true
			if len(v.Ports) != 1 {
				t.Errorf("leaf %s: %s ports %v, want the one server's", sp.Device.Path, v.Name, v.Ports)
			}
			switch v.Network {
			case "provisioning":
				if v.Gateway != netip.MustParsePrefix(fmt.Sprintf("10.40.%d.1/24", r)) || len(v.Relay) != 1 || v.Relay[0] != netip.MustParseAddr("10.90.0.10") {
					t.Errorf("leaf %s: provisioning VLAN %+v", sp.Device.Path, v)
				}
			case "frontend":
				if v.Gateway != netip.MustParsePrefix(fmt.Sprintf("10.50.%d.1/24", r)) || len(v.Relay) != 0 {
					t.Errorf("leaf %s: frontend VLAN %+v, want its gateway and no relay (no DHCP)", sp.Device.Path, v)
				}
			default:
				t.Errorf("leaf %s: VLAN on network %s", sp.Device.Path, v.Network)
			}
		}
		for _, hp := range p.HostPorts {
			if hp.Switch == sp && !seen[hp.Vlan] {
				t.Errorf("%s: VLAN %s is not on its leaf", hp.Ref, hp.Vlan)
			}
		}
	}
	if leaves != 2 {
		t.Errorf("%d leaves", leaves)
	}
}

// Only vm hosts' data ports are spliced: a model host has no NIC to splice, and a vm host's port
// on a model switch keeps the twin's gateway (D48).
func TestPlanHostPortsOnlyVMOnRealNOS(t *testing.T) {
	est := loadEstate(t, "tap-vm-nos.yaml")
	est.Spec.Fidelity.Overrides = nil
	p, err := BuildPlan(compileModelFor(t, est, "x86_64"), est, nil, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.HostPorts) != 0 {
		t.Errorf("model hosts: %d host ports", len(p.HostPorts))
	}
	for _, sp := range p.Switches {
		if len(sp.Config.Vlans) != 0 {
			t.Errorf("model hosts: %s carries VLANs %+v", sp.Device.Path, sp.Config.Vlans)
		}
	}
	est = loadEstate(t, "tap-vm-nos.yaml")
	est.Spec.Fabrics[1].NOS = "none"
	est.Spec.Adapters.Network.Attach = nil
	p, err = BuildPlan(compileModelFor(t, est, "x86_64"), est, nil, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.HostPorts) != 0 || p.HostSkipped["a vm host's port on a model switch (the twin's gateway serves it, D48)"] != 4 {
		t.Errorf("model switches: %d host ports; skipped %v", len(p.HostPorts), p.HostSkipped)
	}
}

// A real NOS's switches are vm by their NOS (D21), not hosts: their ports are never host ports,
// and an estate without vm hosts counts no host port skipped.
func TestPlanHostPortsAreHostsOnly(t *testing.T) {
	est := loadEstate(t, "tap-nvue.yaml")
	p, err := BuildPlan(compileModel(t, est), est, nil, "aarch64")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.HostPorts) != 0 || len(p.HostSkipped) != 0 {
		t.Errorf("no vm hosts: %d host ports, skipped %v", len(p.HostPorts), p.HostSkipped)
	}
	for why := range p.Skipped {
		if strings.Contains(why, "vm host") {
			t.Errorf("a BMC port skipped as %q", why)
		}
	}
}

// With spec.adapters.network.hbn, each BlueField under a vm host is a NOS of its own (D71): its
// uplinks are its leaf's routed, unnumbered ports carrying EVPN, every switch carries EVPN, its
// host's only data NIC through it is its host PF on pf0hpf_if, and it has no AS, loopback or
// startup routing of the twin's (NICo's agent gives it those through NVUE).
func TestPlanHBN(t *testing.T) {
	est := loadEstate(t, "tap-dpu.yaml")
	m := compileModelFor(t, est, "x86_64")
	p, err := BuildPlan(m, est, nil, "x86_64")
	if err != nil {
		t.Fatal(err)
	}
	var dpus, leaves []*SwitchPlan
	for _, sp := range p.Switches {
		switch {
		case sp.NOS.Name == nos.HBN:
			dpus = append(dpus, sp)
		case len(sp.Config.Unnumbered) > 0:
			leaves = append(leaves, sp)
		}
		if sp.NOS.Name != nos.HBN && !sp.Config.EVPN {
			t.Errorf("%s does not carry EVPN", sp.Config.Hostname)
		}
	}
	if len(dpus) != 3 || len(leaves) != 2 {
		t.Fatalf("%d emulated BlueFields and %d leaves facing them, want 3 and 2", len(dpus), len(leaves))
	}
	for _, sp := range dpus {
		c := sp.Config
		if !c.Native || c.Build != "HBN 3.2.3-doca3.2.3" || c.ASN != 0 || c.Loopback.IsValid() || len(c.Routed) != 0 || sp.Image != nos.HBNImage {
			t.Errorf("%s: %+v", c.Hostname, c)
		}
		if sp.NOS.Interface(c, "p0") != "p0_if" || sp.NOS.Interface(c, "pf0hpf") != "pf0hpf_if" {
			t.Errorf("%s: p0 is %q", c.Hostname, sp.NOS.Interface(c, "p0"))
		}
		script, err := sp.NOS.StartScript(c)
		if err != nil {
			t.Error(err)
		}
		if !strings.Contains(script, `want="p0_if p1_if pf0hpf_if"`) || strings.Contains(script, "ip link add pf0hpf_if") || !strings.Contains(script, "ip link add pf0vf0_if type dummy") {
			t.Errorf("%s: the start script waits for the wrong ports or makes a cabled one a dummy:\n%s", c.Hostname, script)
		}
	}
	for _, sp := range leaves {
		if sp.Config.ASN < 65101 || len(sp.Config.Unnumbered) < 2 {
			t.Errorf("%s: AS %d, %d unnumbered ports", sp.Config.Hostname, sp.Config.ASN, len(sp.Config.Unnumbered))
		}
		for _, u := range sp.Config.Unnumbered {
			if !strings.Contains(u.PeerName, "-bf3-0:p") {
				t.Errorf("%s: unnumbered %+v", sp.Config.Hostname, u)
			}
		}
	}
	dpuLinks := 0
	for _, lp := range p.Links {
		if lp.A.NOS.Name == nos.HBN || lp.B.NOS.Name == nos.HBN {
			dpuLinks++
		}
	}
	if dpuLinks != 6 {
		t.Errorf("%d cables from BlueFields to leaves, want 6", dpuLinks)
	}
	pf := 0
	for _, hp := range p.HostPorts {
		if hp.Switch.NOS.Name == nos.HBN {
			pf++
			if hp.SwitchIf != "pf0hpf_if" || !strings.HasSuffix(hp.Ref, "/bf3-0:pf0hpf") || hp.Port.MAC != hp.Dev.Children[0].PortBy["pf0hpf"].MAC {
				t.Errorf("host PF %+v", hp)
			}
		}
		if strings.HasSuffix(hp.Ref, ":p0") || strings.HasSuffix(hp.Ref, ":p1") {
			t.Errorf("%s: a BlueField uplink is its host's NIC", hp.Ref)
		}
	}
	if pf != 3 {
		t.Errorf("%d host PFs spliced onto BlueFields, want 3", pf)
	}
	// the leaf's startup configuration: unnumbered sessions with EVPN, accepted from any AS
	tree, err := nvue.Startup(leaves[0].Config)
	if err != nil {
		t.Fatal(err)
	}
	f, err := nvue.Render(tree, leaves[0].Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{" interface remote-as external\n", "  advertise-all-vni\n", " default-originate\n"} {
		if !strings.Contains(f.FRR, want) {
			t.Errorf("leaf frr.conf lacks %q:\n%s", want, f.FRR)
		}
	}
}
