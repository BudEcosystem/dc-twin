package nos

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// a rack's management switch as the generic pack has it: 48x1G, 4x10G uplinks
func mgmtSwitch() Switch {
	sw := Switch{Path: "site-1/hall-a/r01/mgmt-switch", Hostname: "r01-mgmt-switch", Profile: "generic/mgmt-switch-1g",
		MAC: "02:DC:00:00:00:01", ASN: 65101, RouterID: netip.MustParseAddr("10.255.0.0")}
	for i := 1; i <= 48; i++ {
		sw.Panel = append(sw.Panel, PanelPort{Name: fmt.Sprintf("eth%d", i), Speed: "1G"})
	}
	for i := 1; i <= 4; i++ {
		sw.Panel = append(sw.Panel, PanelPort{Name: fmt.Sprintf("up%d", i), Speed: "10G"})
	}
	sw.Vlans = []Vlan{{Name: "Vlan1000", Network: "oob", Rack: "site-1/hall-a/r01", Gateway: netip.MustParsePrefix("10.30.0.1/24"),
		Ports: []string{"eth1", "eth4"}, Relay: []netip.Addr{netip.MustParseAddr("10.90.0.10")}}}
	sw.Routed = []Routed{{Port: "up1", Addr: netip.MustParsePrefix("10.255.0.0/31"), Peer: netip.MustParseAddr("10.255.0.1"), PeerASN: 65000, PeerName: "oob-agg-1:eth1"}}
	return sw
}

// a 128x400G aggregation switch with Kea attached at eth128
func aggSwitch() Switch {
	sw := Switch{Path: "site-1/hall-a/oob/oob-agg-1", Hostname: "oob-agg-1", Profile: "generic/spine-switch-400g", ASN: 65000,
		RouterID: netip.MustParseAddr("10.255.0.1")}
	for i := 1; i <= 128; i++ {
		sw.Panel = append(sw.Panel, PanelPort{Name: fmt.Sprintf("eth%d", i), Speed: "400G"})
	}
	sw.Routed = []Routed{{Port: "eth1", Addr: netip.MustParsePrefix("10.255.0.1/31"), Peer: netip.MustParseAddr("10.255.0.0"), PeerASN: 65101, PeerName: "r01-mgmt-switch:up1"}}
	sw.Service = []Service{{Port: "eth128", Gateway: netip.MustParsePrefix("10.90.0.1/24")}}
	return sw
}

func TestRegistrySonicVS(t *testing.T) {
	e, ok := Lookup(SonicVS)
	if !ok || !e.Runnable || !e.Real || e.Image != SonicVSImage || !strings.Contains(e.Image, "@sha256:") {
		t.Fatalf("sonic-vs: %+v", e)
	}
	if !e.RunsOn("x86_64") || e.RunsEmulated("x86_64") || e.RunsOn("aarch64") || !e.RunsEmulated("aarch64") || e.Platform != "linux/amd64" {
		t.Errorf("sonic-vs runs natively on %v, emulated on %v, as %s", e.Arch, e.Emulated, e.Platform)
	}
	if e.Relay.Circuit != "hostname:port" || e.RelayImage == "" || !e.SoftwareChecksum || !e.Privileged || len(e.Shared) != 2 {
		t.Errorf("sonic-vs relay %+v image %q, software checksum %v, privileged %v, shared %v", e.Relay, e.RelayImage, e.SoftwareChecksum, e.Privileged, e.Shared)
	}
	f, _ := Lookup(FRRDHCRelay)
	if !f.RunsOn("aarch64") || !f.RunsOn("x86_64") || f.RunsEmulated("aarch64") || f.SoftwareChecksum || f.RelayImage != "" {
		t.Errorf("generic/frr-dhcrelay: %+v", f)
	}
	if c, _ := Lookup("cumulus-vx"); c.Runnable || c.Why == "" {
		t.Errorf("cumulus-vx: %+v", c)
	}
}

// SONiC numbers ports by their first lane, lanes by speed; the container interface is the
// port's place on the panel.
func TestSonicPorts(t *testing.T) {
	ports, err := SonicPorts(mgmtSwitch())
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 52 {
		t.Fatalf("%d ports", len(ports))
	}
	for _, c := range []struct {
		i                int
		model, ifc, name string
		lanes            string
		speed            int
	}{{0, "eth1", "eth1", "Ethernet0", "1", 1000}, {47, "eth48", "eth48", "Ethernet47", "48", 1000}, {48, "up1", "eth49", "Ethernet48", "49", 10000}, {51, "up4", "eth52", "Ethernet51", "52", 10000}} {
		p := ports[c.i]
		if p.Model != c.model || p.Interface != c.ifc || p.Name != c.name || joinInts(p.Lanes) != c.lanes || p.Speed != c.speed || p.Index != c.i+1 {
			t.Errorf("port %d: %+v", c.i, p)
		}
	}
	agg, err := SonicPorts(aggSwitch())
	if err != nil {
		t.Fatal(err)
	}
	if p := agg[127]; p.Name != "Ethernet1016" || p.Interface != "eth128" || len(p.Lanes) != 8 || p.Lanes[0] != 1017 || p.Speed != 400000 {
		t.Errorf("agg eth128: %+v", p)
	}
	e, _ := Lookup(SonicVS)
	if e.Interface(mgmtSwitch(), "up1") != "eth49" || e.Interface(mgmtSwitch(), "mgmt0") != "" {
		t.Errorf("interfaces: up1 %q mgmt0 %q", e.Interface(mgmtSwitch(), "up1"), e.Interface(mgmtSwitch(), "mgmt0"))
	}
	if f, _ := Lookup(FRRDHCRelay); f.Interface(mgmtSwitch(), "up1") != "up1" {
		t.Errorf("generic/frr-dhcrelay renames its ports")
	}
	for s, want := range map[string]int{"1G": 1000, "10G": 10000, "100M": 100, "400g": 400000, "": 1000} {
		if got, err := speedMbps(s); err != nil || got != want {
			t.Errorf("speed %q: %d %v", s, got, err)
		}
	}
	if _, err := speedMbps("fast"); err == nil {
		t.Errorf("speed fast parsed")
	}
}

func TestConfigDB(t *testing.T) {
	db, err := ConfigDB(mgmtSwitch())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(db, "", "  ")
	t.Logf("config_db.json:\n%s", b)
	meta := db["DEVICE_METADATA"]["localhost"].(map[string]any)
	if meta["hostname"] != "r01-mgmt-switch" || meta["mac"] != "02:dc:00:00:00:01" || meta["bgp_asn"] != "65101" || meta["type"] != "ToRRouter" ||
		meta["hwsku"] != "DCTwin-generic-mgmt-switch-1g" || meta["platform"] != "x86_64-kvm_x86_64-r0" {
		t.Errorf("DEVICE_METADATA: %v", meta)
	}
	// only the cabled ports, aliased by the model's names
	if len(db["PORT"]) != 3 {
		t.Errorf("PORT: %v", db["PORT"])
	}
	for name, alias := range map[string]string{"Ethernet0": "eth1", "Ethernet3": "eth4", "Ethernet48": "up1"} {
		p, ok := db["PORT"][name].(map[string]any)
		if !ok || p["alias"] != alias || p["admin_status"] != "up" || p["mtu"] != "1500" {
			t.Errorf("PORT %s: %v", name, p)
		}
	}
	vlan := db["VLAN"]["Vlan1000"].(map[string]any)
	if vlan["vlanid"] != "1000" || fmt.Sprint(vlan["dhcp_servers"]) != "[10.90.0.10]" {
		t.Errorf("VLAN: %v", vlan)
	}
	for _, k := range []string{"Vlan1000|Ethernet0", "Vlan1000|Ethernet3"} {
		if m, ok := db["VLAN_MEMBER"][k].(map[string]any); !ok || m["tagging_mode"] != "untagged" {
			t.Errorf("VLAN_MEMBER %s: %v", k, db["VLAN_MEMBER"])
		}
	}
	for _, k := range []string{"Vlan1000", "Vlan1000|10.30.0.1/24"} {
		if _, ok := db["VLAN_INTERFACE"][k]; !ok {
			t.Errorf("VLAN_INTERFACE lacks %s", k)
		}
	}
	for _, k := range []string{"Ethernet48", "Ethernet48|10.255.0.0/31"} {
		if _, ok := db["INTERFACE"][k]; !ok {
			t.Errorf("INTERFACE lacks %s", k)
		}
	}
	n := db["BGP_NEIGHBOR"]["10.255.0.1"].(map[string]any)
	if n["asn"] != "65000" || n["local_addr"] != "10.255.0.0" || n["name"] != "oob-agg-1:eth1" {
		t.Errorf("BGP_NEIGHBOR: %v", n)
	}
	if d := db["DEVICE_NEIGHBOR"]["Ethernet48"].(map[string]any); d["name"] != "oob-agg-1" || d["port"] != "eth1" {
		t.Errorf("DEVICE_NEIGHBOR: %v", d)
	}
	agg, err := ConfigDB(aggSwitch())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agg["INTERFACE"]["Ethernet1016|10.90.0.1/24"]; !ok || agg["VLAN"] != nil || agg["DEVICE_METADATA"]["localhost"].(map[string]any)["type"] != "LeafRouter" {
		t.Errorf("agg: %v", agg)
	}
	if _, ok := agg["DEVICE_METADATA"]["localhost"].(map[string]any)["mac"]; ok {
		t.Errorf("agg without a management MAC has one in DEVICE_METADATA")
	}
	// a cabled port off the panel is refused
	bad := mgmtSwitch()
	bad.Vlans[0].Ports = append(bad.Vlans[0].Ports, "mgmt0")
	if _, err := ConfigDB(bad); err == nil || !strings.Contains(err.Error(), "front panel") {
		t.Errorf("mgmt0 in a VLAN: %v", err)
	}
}

func TestStartScripts(t *testing.T) {
	sonic, _ := Lookup(SonicVS)
	frr, _ := Lookup(FRRDHCRelay)
	script, err := sonic.StartScript(mgmtSwitch())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`want="eth1 eth4 eth49"`,
		"sku=/usr/share/sonic/device/x86_64-kvm_x86_64-r0/DCTwin-generic-mgmt-switch-1g",
		"SAI_VS_INTERFACE_LANE_MAP_FILE=/usr/share/sonic/hwsku/lanemap.ini",
		"eth1:1\neth4:4\neth49:49\n",
		"# name lanes alias speed index\nEthernet0 1 eth1 1000 1\nEthernet3 4 eth4 1000 4\nEthernet48 49 up1 10000 49\n",
		"cat > /etc/sonic/config_db.json <<'EOF'",
		"cat > /etc/frr/bgpd.conf <<'EOF'",
		"router bgp 65101\n bgp router-id 10.255.0.0\n",
		" neighbor 10.255.0.1 remote-as 65000\n neighbor 10.255.0.1 description oob-agg-1:eth1\n neighbor 10.255.0.1 timers connect 10\n",
		"  neighbor 10.255.0.1 activate\n  redistribute connected\n",
		"redis-cli -n 0 EXISTS PORT_TABLE:PortInitDone",
		"redis-cli -n 6 HGET 'INTERFACE_TABLE|Vlan1000|10.30.0.1/24' state",
		"redis-cli -n 6 HGET 'INTERFACE_TABLE|Ethernet48|10.255.0.0/31' state",
		"supervisorctl start bgpd",
		"  log started\n) &\nexec env HWSKU=DCTwin-generic-mgmt-switch-1g /usr/local/bin/supervisord\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("sonic-vs script lacks %q:\n%s", want, script)
		}
	}
	frrScript, err := frr.StartScript(mgmtSwitch())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frrScript, "dhcrelay -4 -d -a -m discard -id Vlan1000 -iu up1 10.90.0.10") || !strings.Contains(frrScript, "ip addr add 10.255.0.0/31 dev up1") {
		t.Errorf("generic/frr-dhcrelay script:\n%s", frrScript)
	}
	// both are shell the switch's /bin/sh parses
	for name, s := range map[string]string{SonicVS: script, FRRDHCRelay: frrScript} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s script does not parse: %v %s", name, err, out)
		}
	}
	// the JSON inside the SONiC script is the config_db
	start := strings.Index(script, "cat > /etc/sonic/config_db.json <<'EOF'\n") + len("cat > /etc/sonic/config_db.json <<'EOF'\n")
	end := strings.Index(script[start:], "\nEOF\n")
	var parsed map[string]map[string]any
	if err := json.Unmarshal([]byte(script[start:start+end]), &parsed); err != nil || parsed["VLAN"]["Vlan1000"] == nil {
		t.Errorf("config_db.json in the script: %v", err)
	}
	// a switch with no fabric links starts no bgpd and writes no bgpd.conf
	leaf := mgmtSwitch()
	leaf.Routed = nil
	leaf.Service = []Service{{Port: "up1", Gateway: netip.MustParsePrefix("10.90.0.1/24")}}
	s2, err := sonic.StartScript(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s2, "bgpd") {
		t.Errorf("a switch without BGP neighbours starts bgpd:\n%s", s2)
	}
	if _, err := (Entry{Name: "cumulus-vx"}).StartScript(leaf); err == nil {
		t.Errorf("cumulus-vx rendered a script")
	}
}

func TestStarted(t *testing.T) {
	for logs, want := range map[string]bool{
		"dctwin-nos 12:00:01: ports present: eth1\ndctwin-nos 12:00:09: started\n":                              true,
		"dctwin-nos 12:00:01: ports present: eth1\n2026-10-19 14:10:07,695 INFO supervisord started with pid 1": false,
		"dctwin-nos 12:00:01: interfaces ready\r\ndctwin-nos 12:00:02: started\r\n":                             true,
		"": false,
	} {
		if Started(logs) != want {
			t.Errorf("Started(%q) = %v", logs, !want)
		}
	}
}

func TestPortStatesAndRelays(t *testing.T) {
	e, _ := Lookup(SonicVS)
	if cmd := e.PortStateCommand(); len(cmd) == 0 || cmd[0] != "redis-cli" {
		t.Fatalf("port state command %v", cmd)
	}
	if f, _ := Lookup(FRRDHCRelay); f.PortStateCommand() != nil {
		t.Errorf("generic/frr-dhcrelay's ports are read from its namespace")
	}
	out := "PORT_TABLE:Ethernet0 up up\nPORT_TABLE:Ethernet3 up down\nPORT_TABLE:Ethernet48 down down\nPORT_TABLE:Ethernet999 up up\n(nil)\n"
	got := e.ParsePortStates(mgmtSwitch(), out)
	want := map[string]string{"eth1": "up", "eth4": "down", "up1": "admin-down"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("port states %v, want %v", got, want)
	}
	status := "dhcp-relay:dhcprelayd            RUNNING   pid 58, uptime 0:00:08\nisc-dhcpv4-relay-Vlan1000        STARTING\nstart                            EXITED    Oct 19 02:13 PM\n"
	if p := RelaysPending(mgmtSwitch(), status); len(p) != 1 || p[0] != "isc-dhcpv4-relay-Vlan1000" {
		t.Errorf("pending %v", p)
	}
	status = strings.Replace(status, "STARTING", "RUNNING   pid 59, uptime 0:00:08", 1)
	if p := RelaysPending(mgmtSwitch(), status); len(p) != 0 {
		t.Errorf("pending %v", p)
	}
	if !mgmtSwitch().Relaying() || aggSwitch().Relaying() {
		t.Errorf("relaying")
	}
}

// scripts/linux-substrate-e2e.sh fetches the SONiC images the registry pins; the two must agree.
func TestSonicPinsMatchTheE2EScript(t *testing.T) {
	b, err := os.ReadFile("../../scripts/linux-substrate-e2e.sh")
	if err != nil {
		t.Skipf("the e2e script is not beside the package: %v", err)
	}
	for _, pin := range []string{SonicVSImage, SonicRelayImage, SonicRelayArtifact, SonicRelaySHA256} {
		if !strings.Contains(string(b), pin) {
			t.Errorf("scripts/linux-substrate-e2e.sh does not pin %s", pin)
		}
	}
}
