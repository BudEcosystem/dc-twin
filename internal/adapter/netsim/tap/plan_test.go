package tap

import (
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
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
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch()})
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
			for _, want := range []string{"eth49:49\n", "Ethernet48 49 up1 10000 49\n", "redis-cli -n 6 HGET 'INTERFACE_TABLE|Vlan1000|10.30.0.1/24' state", "exec env HWSKU=DCTwin-generic-mgmt-switch-1g /usr/local/bin/supervisord", "neighbor " + up.Peer.String() + " remote-as 65000"} {
				if !strings.Contains(script, want) {
					t.Errorf("%s: script lacks %q", sp.Device.Path, want)
				}
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
