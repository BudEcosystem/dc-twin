//go:build linux

package linuxnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap"
	"github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// sonicManagement: SONiC's bgp and gnmi containers beside each switch (D55). FRR runs in the bgp
// container, configured from CONFIG_DB by frrcfgd. gNMI answers at each switch's Loopback0, which
// BGP carries across the fabric: from beside the aggregation switch, a client with the estate-CA
// certificate the twin hands the stack reads the leaf's CONFIG_DB and creates a VRF on it through
// SONiC's YANG model, which frrcfgd takes to FRR where the host's kernel can create the VRF; a
// client with any other certificate is refused. An EVPN VNI written to CONFIG_DB becomes the
// kernel's VXLAN device (vxlanmgrd) and FRR's advertise-all-vni (frrcfgd).
func sonicManagement(t *testing.T, stateDir string, sub *tap.Substrate, leaf, agg string) {
	t.Helper()
	ctx := context.Background()
	for _, path := range []string{leaf, agg} {
		out, err := sub.SidecarExec(ctx, path, "bgp", "supervisorctl", "status")
		t.Logf("%s: its bgp container's programs:\n%s", path, indent(out))
		if err != nil && !strings.Contains(out, "frrcfgd") {
			t.Errorf("%s: no bgp container: %v", path, err)
		}
		for _, d := range []string{"zebra", "bgpd", "fpmsyncd", "frrcfgd"} {
			if !regexp.MustCompile(`(?m)^` + d + `\s+RUNNING`).MatchString(out) {
				t.Errorf("%s: %s is not running in the bgp container", path, d)
			}
		}
		out, _ = sub.Exec(ctx, path, "sh", "-c", "supervisorctl status zebra bgpd fpmsyncd")
		if strings.Contains(out, "RUNNING") {
			t.Errorf("%s: the switch container runs routing daemons of its own:\n%s", path, out)
		}
	}
	var inputs []tap.SwitchInput
	b, err := os.ReadFile(filepath.Join(stateDir, "stack-inputs", tap.SwitchesFile))
	if err != nil || json.Unmarshal(b, &inputs) != nil {
		t.Fatalf("stack-inputs/%s: %v", tap.SwitchesFile, err)
	}
	t.Logf("stack-inputs/%s:\n%s", tap.SwitchesFile, indent(string(b)))
	files := map[string]string{}
	for _, f := range []string{"switch-client.crt", "switch-client.key", "switch-ca.crt"} {
		b, err := os.ReadFile(filepath.Join(stateDir, "stack-inputs", f))
		if err != nil {
			t.Fatal(err)
		}
		files[f] = string(b)
	}
	var target string
	for _, in := range inputs {
		if in.Path == leaf {
			target = in.GNMI
		}
	}
	if target == "" {
		t.Fatalf("stack-inputs names no gNMI server for %s", leaf)
	}
	// a client beside the aggregation switch (its gnmi container has SONiC's gnmi_get and
	// gnmi_set), reaching the leaf's loopback across the fabric
	host, _, _ := strings.Cut(target, ":")
	hostname := ""
	for _, in := range inputs {
		if in.Path == leaf {
			hostname = in.Hostname
		}
	}
	gnmi := func(tool string, args ...string) (string, error) {
		script := `mkdir -p /tmp/c && printf '%s' "$1" > /tmp/c/client.crt && printf '%s' "$2" > /tmp/c/client.key && printf '%s' "$3" > /tmp/c/ca.crt && printf '%s' "$4" > /tmp/c/body.json && shift 4 && cd /tmp/c && exec "$@"`
		cmd := append([]string{"sh", "-c", script, "sh", files["switch-client.crt"], files["switch-client.key"], files["switch-ca.crt"], `{"sonic-vrf:VRF_LIST":[{"name":"Vrf-tenant-a","vni":10001}]}`,
			tool, "-target_addr", target, "-target_name", hostname, "-ca", "ca.crt", "-cert", "client.crt", "-key", "client.key", "-logtostderr"}, args...)
		return sub.SidecarExec(ctx, agg, "gnmi", cmd...)
	}
	out, err := gnmi("gnmi_get", "-xpath_target", "CONFIG_DB", "-xpath", "BGP_NEIGHBOR")
	t.Logf("gNMI Get CONFIG_DB/BGP_NEIGHBOR from %s at %s (across the fabric):\n%s", hostname, target, indent(out))
	if err != nil || !strings.Contains(out, "default|") {
		t.Errorf("gNMI Get of the leaf's BGP_NEIGHBOR: %v", err)
	}
	out, err = gnmi("gnmi_set", "-update", "/sonic-vrf:sonic-vrf/VRF/VRF_LIST:@./body.json")
	t.Logf("gNMI Set /sonic-vrf:sonic-vrf/VRF/VRF_LIST (Vrf-tenant-a, VNI 10001):\n%s", indent(out))
	if err != nil {
		t.Errorf("gNMI Set of a VRF: %v", err)
	}
	var vrfRow string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		vrfRow, _ = sub.Exec(ctx, leaf, "redis-cli", "-n", "4", "--raw", "hgetall", "VRF|Vrf-tenant-a")
		if strings.Contains(vrfRow, "10001") {
			break
		}
		if time.Now().After(deadline) {
			for _, path := range []string{leaf, agg} {
				out, _ := sub.Exec(ctx, path, "sh", "-c", "redis-cli -n 4 --raw keys 'VRF*'; redis-cli -n 4 --raw hgetall 'VRF|Vrf-tenant-a'; grep -iE 'translib|cvl|vrf' /var/log/syslog | tail -5")
				t.Logf("%s: CONFIG_DB VRF keys, the row, the log:\n%s", path, indent(out))
				out, _ = sub.SidecarExec(ctx, path, "gnmi", "sh", "-c", "grep -iE 'set|vrf|error' /var/log/syslog 2>/dev/null | tail -8")
				t.Logf("%s: its gnmi container's log:\n%s", path, indent(out))
			}
			t.Fatalf("Vrf-tenant-a is not in %s's CONFIG_DB 30 s after the gNMI Set (the row read: %q)", leaf, vrfRow)
		}
	}
	t.Logf("%s's CONFIG_DB VRF|Vrf-tenant-a after the gNMI Set:\n%s", leaf, indent(vrfRow))
	// a client whose certificate the estate CA did not issue
	out, err = sub.SidecarExec(ctx, agg, "gnmi", "sh", "-c", `cd /tmp && openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=stranger -keyout k -out c 2>/dev/null && printf '%s' "$1" > ca.crt && exec gnmi_get -target_addr "$2" -target_name "$3" -ca ca.crt -cert c -key k -xpath_target CONFIG_DB -xpath VRF -logtostderr`,
		"sh", files["switch-ca.crt"], target, hostname)
	if err == nil {
		t.Errorf("a client with a certificate from another CA read CONFIG_DB:\n%s", out)
	}
	t.Logf("a client with a self-signed certificate is refused: %v", err)

	vrf := sub.Kernel()["vrf"]
	out, _ = sub.Exec(ctx, leaf, "ip", "-br", "link", "show", "type", "vrf")
	t.Logf("kernel VRF support: %v; %s's VRF devices:\n%s", vrf, leaf, indent(out))
	if vrf == nil {
		waitFor(t, 30*time.Second, "Vrf-tenant-a in the leaf's kernel and FRR", func() bool {
			k, _ := sub.Exec(ctx, leaf, "ip", "link", "show", "Vrf-tenant-a")
			f, _ := sub.RoutingExec(ctx, leaf, "vtysh", "-c", "show running-config")
			return strings.Contains(k, "vrf") && strings.Contains(f, "vrf Vrf-tenant-a\n vni 10001")
		})
	} else {
		t.Logf("the Docker host's kernel has no VRF devices (%v): vrfmgrd and FRR refuse Vrf-tenant-a, as the fidelity report says", vrf)
	}

	// EVPN: a VTEP and a VNI mapped to a VLAN become the kernel's VXLAN device; the l2vpn evpn
	// address family with advertise-all-vni reaches FRR
	if _, err := sub.Exec(ctx, leaf, "sh", "-c", fmt.Sprintf(`redis-cli -n 4 HSET "VXLAN_TUNNEL|vtep1" src_ip %s >/dev/null &&
redis-cli -n 4 HSET "VXLAN_EVPN_NVO|nvo1" source_vtep vtep1 >/dev/null &&
redis-cli -n 4 HSET "VLAN|Vlan2001" vlanid 2001 >/dev/null &&
redis-cli -n 4 HSET "VXLAN_TUNNEL_MAP|vtep1|map_10001_Vlan2001" vni 10001 vlan Vlan2001 >/dev/null &&
redis-cli -n 4 HSET "BGP_GLOBALS_AF|default|l2vpn_evpn" advertise-all-vni true >/dev/null`, host)); err != nil {
		t.Fatal(err)
	}
	if sub.Kernel()["vxlan"] == nil {
		waitFor(t, 60*time.Second, "the leaf's VXLAN device for VNI 10001", func() bool {
			out, _ := sub.Exec(ctx, leaf, "ip", "-d", "link", "show", "type", "vxlan")
			return strings.Contains(out, "vxlan id 10001 local "+host)
		})
	}
	waitFor(t, 60*time.Second, "advertise-all-vni in the leaf's FRR", func() bool {
		out, _ := sub.RoutingExec(ctx, leaf, "vtysh", "-c", "show running-config")
		return strings.Contains(out, "address-family l2vpn evpn") && strings.Contains(out, "advertise-all-vni")
	})
	out, _ = sub.RoutingExec(ctx, leaf, "vtysh", "-c", "show running-config")
	t.Logf("%s: FRR's running configuration, from CONFIG_DB:\n%s", leaf, indent(out))
	out, _ = sub.Exec(ctx, leaf, "ip", "-d", "link", "show", "type", "vxlan")
	t.Logf("%s: the kernel's VXLAN devices:\n%s", leaf, indent(out))
}

// SONiC-VS on the OOB fabric (D4, D44): the rack's management switch and the aggregation switch
// run docker-sonic-vs, the linux/amd64 image SONiC publishes, under the Docker host's binary
// translation when the host is aarch64, each with SONiC's dhcp_relay container beside it. The
// twin renders each switch's HWSKU and config_db.json from the model (FRR is SONiC's bgp container's) and
// cables its TAPs and veths in as the container's eth<n>. Every BMC is leased by Kea through
// SONiC's own dhcrelay, which SONiC configured from CONFIG_DB, with SONiC's circuit id
// (`<hostname>:<port alias>`, the alias the model's port name); routes to Kea and back are learned
// by eBGP between the two SONiC switches; Redfish answers on every lease across the fabric; the
// NOS observer reads ports from SONiC's APPL_DB and sessions from its FRR, and a pulled BMC cable
// is that port oper down in `show interfaces status`, a pulled fabric cable the session down and
// back.
func TestSonicVS(t *testing.T) {
	need(t)
	d, err := nosRuntime(t)
	if err != nil {
		t.Skipf("no Docker Engine for the NOS runtime: %v", err)
	}
	ctx := context.Background()
	e, _ := nos.Lookup(nos.SonicVS)
	for _, img := range []string{e.Image, e.RelayImage, keaImage, nos.SonicBGPImage, nos.SonicGNMIImage} {
		if !d.HasImage(ctx, img) {
			t.Skipf("image %s is not present (RUN=TestSonicVS scripts/linux-substrate-e2e.sh fetches it)", img)
		}
	}
	suffix := strconv.Itoa(os.Getpid())
	keaName, probeName := "dctwin-kea-"+suffix, "dctwin-probe-sonic"
	p := newProbe(t, probeName)
	kea := startKeaContainer(t, d, keaName)
	est := loadEstate(t, "tap-sonic.yaml", map[string]string{"KEA_CONTAINER": keaName, "PROBE_NETNS": probeName})
	start := time.Now()
	tw, sub, a := up(t, est)
	plan := sub.Plan()
	t.Logf("up returned after %s: every SONiC switch started, its relay running and its sessions established", time.Since(start).Round(time.Second))
	var leaf, agg string
	for _, sp := range plan.Switches {
		t.Logf("real switch %s: nos %s, image %s, emulated %v, container %s, relay %s", sp.Device.Path, sp.NOS.Name, sp.Image, sp.Emulated, sp.Container, sub.Relays()[sp.Device.Path])
		if len(sp.Config.Vlans) > 0 {
			leaf = sp.Device.Path
		} else {
			agg = sp.Device.Path
		}
	}

	// SONiC's own view of itself
	for _, path := range []string{leaf, agg} {
		out, _ := sub.Exec(ctx, path, "sh", "-c", "show version 2>/dev/null | grep -E 'SONiC Software|Build commit|Platform|HwSKU'; uname -m")
		t.Logf("%s `show version`:\n%s", path, indent(out))
		out, _ = sub.Exec(ctx, path, "show", "interfaces", "status")
		t.Logf("%s `show interfaces status`:\n%s", path, indent(grepNot(out, "sudo: not found")))
	}
	out, _ := sub.Exec(ctx, leaf, "show", "vlan", "brief")
	t.Logf("%s `show vlan brief`:\n%s", leaf, indent(grepNot(out, "sudo: not found")))
	if !strings.Contains(out, "10.30.0.1/24") {
		t.Errorf("Vlan1000 has no gateway in SONiC's view")
	}
	out, err = sub.RelayExec(ctx, leaf, "sh", "-c", "grep '^command=/usr/sbin/dhcrelay' /etc/supervisor/conf.d/docker-dhcp-relay.supervisord.conf; ps -eo args | grep -E '^/usr/sbin/(dhcrelay|dhcpmon)'")
	t.Logf("SONiC's dhcp_relay container beside %s rendered and runs (from CONFIG_DB):\n%s", leaf, indent(out))
	if err != nil || !strings.Contains(out, "-a %h:%p") || !strings.Contains(out, "-id Vlan1000") || !strings.Contains(out, "10.90.0.10") {
		t.Errorf("SONiC's relay is not relaying Vlan1000 to Kea with -a %%h:%%p: %v", err)
	}

	took := waitLeased(t, a, len(plan.Endpoints), 180*time.Second)
	t.Logf("%d BMCs leased by Kea through SONiC's dhcp_relay in %s", len(plan.Endpoints), took.Round(time.Millisecond))
	leases := keaLeases(t, d, kea)
	t.Logf("%-38s %-17s %-11s %-6s %-22s %s", "BMC port", "MAC", "lease", "subnet", "circuit id", "remote id")
	circuits := map[string]bool{}
	for _, ep := range plan.Endpoints {
		l := leaseOf(tw, ep.Device)
		k, ok := leases[ep.MAC.String()]
		if !ok {
			t.Errorf("%s (%s): no lease in Kea's store", ep.Ref, ep.MAC)
			continue
		}
		t.Logf("%-38s %-17s %-11s %-6d %-22s %s", ep.Ref, ep.MAC, k.addr, k.subnet, k.circuit, k.remote)
		if k.addr != l.IP || k.subnet != 1 {
			t.Errorf("%s: Kea leased %s in subnet %d, the BMC holds %s", ep.Ref, k.addr, k.subnet, l.IP)
		}
		// SONiC's patched dhcrelay: hostname, then the port's alias, which is the model's name
		if want := ep.Switch.Config.Hostname + ":" + ep.Peer.Name; k.circuit != want {
			t.Errorf("%s: circuit id %q, want %q (SONiC's %%h:%%p)", ep.Ref, k.circuit, want)
		}
		if want := strings.ToLower(ep.Switch.Config.MAC); k.remote != want {
			t.Errorf("%s: remote id %q, want the switch's MAC %q", ep.Ref, k.remote, want)
		}
		circuits[k.circuit] = true
	}
	if len(circuits) != len(plan.Endpoints) {
		t.Errorf("%d distinct circuit ids for %d BMCs: SONiC's are per port", len(circuits), len(plan.Endpoints))
	}
	out, _, _ = d.Exec(ctx, kea.ID, "cat", "/var/lib/kea/kea-leases4.csv")
	t.Logf("Kea's lease file:\n%s", indent(out))

	// the wire: a BMC reset ends in an INIT-REBOOT; capture it at Kea's port on the aggregation switch
	keaPort := ""
	for _, at := range plan.Attach {
		if at.Container != "" {
			keaPort = nos.SonicName(at.Switch.Config, at.Port)
		}
	}
	if err := sub.ExecDetached(ctx, agg, "sh", "-c", "timeout 90 tcpdump -l -n -vv -i "+keaPort+" -c 2 'udp port 67' > /tmp/relayed.txt 2>&1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	victim := plan.Endpoints[0]
	for _, ep := range plan.Endpoints {
		if strings.Contains(ep.Device, "r01/server-02") {
			victim = ep
		}
	}
	before := leaseOf(tw, victim.Device).At
	if err := tw.Engine.ResetBMC(engine.Request{Path: victim.Device, Canonical: "bmc.reset", Protocol: "test", Source: testSrc}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 90*time.Second, "the reset BMC's new lease", func() bool { return leaseOf(tw, victim.Device).At.After(before) })
	waitFor(t, 30*time.Second, "tcpdump to capture the relayed exchange", func() bool {
		out, _ := sub.Exec(ctx, agg, "cat", "/tmp/relayed.txt")
		return strings.Contains(out, "2 packets captured")
	})
	out, _ = sub.Exec(ctx, agg, "cat", "/tmp/relayed.txt")
	t.Logf("tcpdump at %s:%s (Kea's port) after resetting %s:\n%s", agg, keaPort, victim.Device, indent(out))
	if want := victim.Switch.Config.Hostname + ":" + victim.Peer.Name; !strings.Contains(out, "Gateway-IP 10.30.0.1") || !strings.Contains(out, want) {
		t.Errorf("the relayed request on the wire lacks giaddr 10.30.0.1 or circuit id %s", want)
	}

	// Redfish on each BMC's own address, from a host on the service side, routed agg -> leaf
	for _, ep := range plan.Endpoints {
		ip := leaseOf(tw, ep.Device).IP
		code, cn, err := p.redfishRoot(ip, 20*time.Second)
		if err != nil || code != 200 || cn != redfish.Hostname(ep.Dev) {
			t.Errorf("%s: https://%s/redfish/v1/ from 10.90.1.10 = %d %q %v", ep.Ref, ip, code, cn, err)
		}
	}
	t.Logf("every BMC answered https://<its lease>/redfish/v1/ from the probe at 10.90.1.10, routed through both SONiC switches")

	// BGP between the SONiC switches, as their FRR reports it, in the kernel and in SONiC's APPL_DB
	logBGP(t, sub, "converged")
	out, _ = sub.Exec(ctx, agg, "sh", "-c", "ip route show proto bgp; redis-cli -n 0 --raw keys 'ROUTE_TABLE:10.30.*'")
	t.Logf("%s: kernel routes from BGP and SONiC's APPL_DB ROUTE_TABLE (fpmsyncd):\n%s", agg, indent(out))
	if !strings.Contains(out, "10.30.0.0/24") {
		t.Errorf("%s has no route to the BMC segment", agg)
	}
	for k, v := range sub.Observed() {
		if strings.Contains(k, " bgp ") && v != "Established" {
			t.Errorf("observer: %s is %s", k, v)
		}
	}

	sonicManagement(t, tw.Runtime.StateDir, sub, leaf, agg)

	// the NOS observer: a pulled BMC cable is the switch's port oper down, read from SONiC. The
	// reset above took the BMC's link down for a moment (its NIC goes with its controller), which
	// SONiC saw as well: the port is up again before the cable is pulled
	portKey := victim.Switch.Device.Path + ":" + victim.Peer.Name
	sonicPort := nos.SonicName(victim.Switch.Config, victim.Peer.Name)
	waitFor(t, 60*time.Second, "the observer to see "+portKey+" up after the BMC's reset", func() bool { return sub.Observed()[portKey] == "up" })
	if err := tw.Engine.SetLink(victim.Ref, false, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "the observer to see "+portKey+" go down", func() bool { return sub.Observed()[portKey] == "down" })
	out, _ = sub.Exec(ctx, victim.Switch.Device.Path, "sh", "-c", "show interfaces status "+sonicPort+" 2>/dev/null; echo; redis-cli -n 0 hgetall PORT_TABLE:"+sonicPort+" | paste - - | grep -E 'status'; ip link show "+victim.SwitchIf)
	t.Logf("cable pulled at %s; SONiC says (show interfaces status, APPL_DB PORT_TABLE, the container's %s):\n%s", victim.Ref, victim.SwitchIf, indent(out))
	if !strings.Contains(out, "oper_status\tdown") {
		t.Errorf("APPL_DB does not have %s oper down", sonicPort)
	}
	if err := tw.Engine.SetLink(victim.Ref, true, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "the observer to see "+portKey+" up again", func() bool { return sub.Observed()[portKey] == "up" })
	t.Logf("cable replaced: observed %s = up", portKey)

	// a pulled fabric cable takes the leaf's session down; replaced, it comes back
	fabric := plan.Links[0]
	lp, peerPlan := fabric.A, fabric.B
	if lp.Device.Path != leaf {
		lp, peerPlan = fabric.B, fabric.A
	}
	sessKey := leaf + " bgp " + lp.Config.Routed[0].Peer.String()
	if err := tw.Engine.SetLink(fabric.Link.ID, false, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "the observer to see "+sessKey+" leave Established", func() bool { return sub.Observed()[sessKey] != "Established" })
	t.Logf("fabric cable %s pulled: observed %s = %s", fabric.Link.ID, sessKey, sub.Observed()[sessKey])
	if err := tw.Engine.SetLink(fabric.Link.ID, true, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 120*time.Second, "the session to re-establish", func() bool { return sub.Observed()[sessKey] == "Established" })
	t.Logf("fabric cable replaced: %s (%s) Established again", sessKey, peerPlan.Config.Hostname)

	// dhcpmon counts what SONiC's relay relayed and writes it to COUNTERS_DB from a cache it
	// flushes now and then: read last, when it has had the time
	out, _ = sub.Exec(ctx, leaf, "sh", "-c", "for k in $(redis-cli -n 2 --raw keys 'DHCPV4_COUNTER_TABLE*' | sort); do for d in RX TX; do echo \"$k $d $(redis-cli -n 2 --raw hget $k $d)\"; done; done | sed \"s/'[A-Za-z]*':'0',\\{0,1\\}//g\"")
	t.Logf("dhcpmon's counters in %s's COUNTERS_DB (SONiC's own record of what it relayed):\n%s", leaf, indent(out))
	n := 0
	for _, ev := range tw.Engine.Events(0, func(ev schema.Event) bool { return ev.Source.Class == "observer" }) {
		if n < 12 || strings.Contains(ev.State.Field, "bgp") || ev.Path == portKey {
			b, _ := json.Marshal(map[string]any{"seq": ev.Seq, "source": ev.Source.Principal, "path": ev.Path, "field": ev.State.Field, "from": ev.State.From, "to": ev.State.To})
			t.Logf("observer event %s", b)
		}
		n++
	}
	t.Logf("%d observer events committed", n)
	for _, l := range sub.Report().Lines {
		t.Logf("report substrate: %s", l)
	}
	for _, l := range tw.Net.SubstrateIsNot() {
		t.Logf("report substrate is not: %s", l)
	}
	for _, l := range a.Report().Lines {
		t.Logf("report dhcp: %s", l)
	}
}
