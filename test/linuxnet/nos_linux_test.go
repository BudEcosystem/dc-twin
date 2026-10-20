//go:build linux

package linuxnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
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

const keaImage = "dctwin-kea-e2e:2.6"

// (b) and (c): a real NOS on the OOB fabric. Each rack's management switch and the OOB
// aggregation switch run generic/frr-dhcrelay (FRR and ISC dhcrelay) in containers the twin
// starts and wires: BMC ports are TAPs moved into the switches' namespaces, fabric links are
// cables the twin holds. The stack's Kea sits in its own container, attached at the aggregation
// switch. Every BMC is leased by Kea through the management switch's dhcrelay, by giaddr (the
// segment gateway on the switch), with the relay's option 82; the routes Kea answers over are
// learned by eBGP between the FRR instances; Redfish answers on each BMC's own address across
// the routed fabric; the NOS observer reports port and session state back as events, and pulled
// cables (a BMC's, a fabric link's) show up there.
func TestRealNOS(t *testing.T) {
	need(t)
	d, err := nos.NewDocker()
	if err != nil {
		t.Skipf("no Docker Engine for the NOS runtime: %v", err)
	}
	ctx := context.Background()
	for _, img := range []string{"dctwin-nos-frr-dhcrelay:bookworm", keaImage} {
		if !d.HasImage(ctx, img) {
			t.Skipf("image %s is not present (scripts/linux-substrate-e2e.sh builds it)", img)
		}
	}
	suffix := strconv.Itoa(os.Getpid())
	keaName, probeName := "dctwin-kea-"+suffix, "dctwin-probe-nos"
	p := newProbe(t, probeName)
	kea := startKeaContainer(t, d, keaName)
	est := loadEstate(t, "tap-nos.yaml", map[string]string{"KEA_CONTAINER": keaName, "PROBE_NETNS": probeName})
	tw, sub, a := up(t, est)
	plan := sub.Plan()
	for path, c := range sub.Switches() {
		t.Logf("real switch %s: container %s", path, c)
	}
	logBGP(t, sub, "after up")

	took := waitLeased(t, a, len(plan.Endpoints), 120*time.Second)
	t.Logf("%d BMCs leased by Kea through ISC dhcrelay on the management switches in %s", len(plan.Endpoints), took.Round(time.Millisecond))

	// Kea's own record of every lease: the subnet it chose (by giaddr) and the relay agent
	// information the NOS's dhcrelay inserted
	leases := keaLeases(t, d, kea)
	t.Logf("%-42s %-17s %-11s %-6s %-12s %s", "BMC port", "MAC", "lease", "subnet", "circuit id", "switch:port")
	for _, ep := range plan.Endpoints {
		l := leaseOf(tw, ep.Device)
		k, ok := leases[ep.MAC.String()]
		if !ok {
			t.Errorf("%s (%s): no lease in Kea's store", ep.Ref, ep.MAC)
			continue
		}
		var vlan string
		for _, v := range ep.Switch.Config.Vlans {
			if v.Rack == ep.Rack {
				vlan = v.Name
			}
		}
		t.Logf("%-42s %-17s %-11s %-6d %-12s %s:%s", ep.Ref, ep.MAC, k.addr, k.subnet, k.circuit, ep.Switch.Config.Hostname, ep.SwitchIf)
		if k.addr != l.IP {
			t.Errorf("%s: Kea leased %s, the BMC holds %s", ep.Ref, k.addr, l.IP)
		}
		want := 1
		if strings.HasSuffix(ep.Rack, "/r02") {
			want = 2
		}
		if k.subnet != want {
			t.Errorf("%s: Kea chose subnet %d, want %d (the rack's, by giaddr)", ep.Ref, k.subnet, want)
		}
		// unpatched ISC dhcrelay: the circuit id is the interface the request came in on
		if k.circuit != vlan {
			t.Errorf("%s: circuit id %q, want %q (ISC dhcrelay's interface)", ep.Ref, k.circuit, vlan)
		}
	}
	out, _, _ := d.Exec(ctx, kea.ID, "cat", "/var/lib/kea/kea-leases4.csv")
	t.Logf("Kea's lease file:\n%s", indent(out))
	out, _, _ = d.Exec(ctx, kea.ID, "sh", "-c", "sed -n '/DHCP4_QUERY_DATA/,/DHCP4_SUBNET_SELECTED/p' /var/log/kea-packets.log | head -n 14 | cut -c1-220")
	t.Logf("Kea's packet log, the first relayed DISCOVER as Kea saw it (from the leaf's routed address, option 82 sub-option 1 = \"Vlan1000\") and the subnet it chose:\n%s", indent(out))
	out, _, _ = d.Exec(ctx, kea.ID, "sh", "-c", "grep -o 'the subnet with ID [0-9]* was selected' /var/log/kea-packets.log | sort | uniq -c")
	t.Logf("Kea's subnet selections:\n%s", indent(out))
	for path := range sub.Switches() {
		if strings.Contains(path, "mgmt-switch") {
			out, _ := sub.Exec(ctx, path, "sh", "-c", "grep -v -e '^Listening on' -e '^Sending on' -e Copyright -e 'All rights' -e 'visit https' -e 'Discarding packet' -e '^$' /var/log/dhcrelay.log | head -n 12")
			t.Logf("dhcrelay on %s (ISC dhcrelay -d; it also opens every bridge member and discards what arrives there, having no address on them):\n%s", path, indent(out))
			gw := "10.30.0.1"
			if strings.Contains(path, "r02/") {
				gw = "10.30.1.1"
			}
			all, _ := sub.Exec(ctx, path, "cat", "/var/log/dhcrelay.log")
			if !strings.Contains(all, "BOOTREPLY giaddr: "+gw) {
				t.Errorf("dhcrelay on %s relayed no reply for giaddr %s", path, gw)
			}
		}
	}

	// the wire: a BMC reset ends in an INIT-REBOOT the leaf relays; capture it at Kea's switch port
	agg := ""
	for path := range sub.Switches() {
		if strings.Contains(path, "oob-agg") {
			agg = path
		}
	}
	if err := sub.ExecDetached(ctx, agg, "sh", "-c", "timeout 60 tcpdump -l -n -vv -i eth128 -c 2 'udp port 67' > /tmp/relayed.txt 2>&1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
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
	waitFor(t, 60*time.Second, "the reset BMC's new lease", func() bool { return leaseOf(tw, victim.Device).At.After(before) })
	waitFor(t, 20*time.Second, "tcpdump to capture the relayed exchange", func() bool {
		out, _ := sub.Exec(ctx, agg, "cat", "/tmp/relayed.txt")
		return strings.Contains(out, "2 packets captured")
	})
	out, _ = sub.Exec(ctx, agg, "cat", "/tmp/relayed.txt")
	t.Logf("tcpdump at %s:eth128 (Kea's port) after resetting %s:\n%s", agg, victim.Device, indent(out))
	if !strings.Contains(out, "Gateway-IP 10.30.0.1") || !strings.Contains(out, "Circuit-ID") {
		t.Errorf("the relayed request on the wire lacks giaddr 10.30.0.1 or option 82")
	}

	// Redfish on each BMC's own address, from a host on the service side, across the fabric
	for _, ep := range plan.Endpoints {
		ip := leaseOf(tw, ep.Device).IP
		code, cn, err := p.redfishRoot(ip, 10*time.Second)
		if err != nil || code != 200 || cn != redfish.Hostname(ep.Dev) {
			t.Errorf("%s: https://%s/redfish/v1/ from 10.90.1.10 = %d %q %v", ep.Ref, ip, code, cn, err)
		}
	}
	t.Logf("every BMC answered https://<its lease>/redfish/v1/ from the probe at 10.90.1.10, routed agg -> leaf by BGP")
	out, _, _ = d.Exec(ctx, kea.ID, "ip", "route")
	t.Logf("Kea's routes:\n%s", indent(out))

	// ARP and MAC learning on the real switch: the BMCs' model MACs on their own ports
	for path := range sub.Switches() {
		if !strings.Contains(path, "r01/mgmt-switch") {
			continue
		}
		neigh, _ := sub.Exec(ctx, path, "ip", "neigh", "show", "dev", "Vlan1000")
		fdb, _ := sub.Exec(ctx, path, "bridge", "fdb", "show", "br", "Vlan1000")
		t.Logf("%s ARP table (ip neigh show dev Vlan1000):\n%s", path, indent(neigh))
		t.Logf("%s MAC table (bridge fdb, learned):\n%s", path, indent(grepNot(fdb, "permanent")))
		for _, ep := range plan.Endpoints {
			if ep.Switch.Device.Path != path {
				continue
			}
			mac := ep.MAC.String()
			if !strings.Contains(neigh, leaseOf(tw, ep.Device).IP+" lladdr "+mac) {
				t.Errorf("%s: the switch's ARP table has no %s at %s", ep.Ref, leaseOf(tw, ep.Device).IP, mac)
			}
			if !strings.Contains(fdb, mac+" dev "+ep.SwitchIf+" ") {
				t.Errorf("%s: the switch did not learn %s on %s", ep.Ref, mac, ep.SwitchIf)
			}
		}
	}

	// (c) BGP between the real switches, as FRR reports it and as the observer committed it
	logBGP(t, sub, "converged")
	for k, v := range sub.Observed() {
		if strings.Contains(k, " bgp ") && v != "Established" {
			t.Errorf("observer: %s is %s", k, v)
		}
	}

	// the NOS observer: a pulled BMC cable is the switch's port going down, read from the NOS
	portKey := victim.Switch.Device.Path + ":" + victim.SwitchIf
	if err := tw.Engine.SetLink(victim.Ref, false, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "the observer to see "+portKey+" go down", func() bool { return sub.Observed()[portKey] != "up" })
	out, _ = sub.Exec(ctx, victim.Switch.Device.Path, "ip", "link", "show", victim.SwitchIf)
	t.Logf("cable pulled at %s; the switch says:\n%s", victim.Ref, indent(out))
	t.Logf("observed %s = %s", portKey, sub.Observed()[portKey])
	if err := tw.Engine.SetLink(victim.Ref, true, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "the observer to see "+portKey+" up again", func() bool { return sub.Observed()[portKey] == "up" })

	// a pulled fabric cable drops the leaf's session; replaced, it comes back
	var fabric *tap.LinkPlan
	for _, lp := range plan.Links {
		if strings.Contains(lp.A.Device.Path, "r01/") || strings.Contains(lp.B.Device.Path, "r01/") {
			fabric = lp
		}
	}
	leaf, peer := fabric.A, fabric.B
	if !strings.Contains(leaf.Device.Path, "r01/") {
		leaf, peer = fabric.B, fabric.A
	}
	var peerAddr string
	for _, r := range leaf.Config.Routed {
		peerAddr = r.Peer.String()
	}
	sessKey := leaf.Device.Path + " bgp " + peerAddr
	if err := tw.Engine.SetLink(fabric.Link.ID, false, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the observer to see "+sessKey+" leave Established", func() bool { return sub.Observed()[sessKey] != "Established" })
	t.Logf("fabric cable %s pulled: observed %s = %s", fabric.Link.ID, sessKey, sub.Observed()[sessKey])
	if err := tw.Engine.SetLink(fabric.Link.ID, true, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "the session to re-establish", func() bool { return sub.Observed()[sessKey] == "Established" })
	t.Logf("fabric cable replaced: %s (%s) Established again", sessKey, peer.Config.Hostname)

	n := 0
	for _, ev := range tw.Engine.Events(0, func(ev schema.Event) bool { return ev.Source.Class == "observer" }) {
		if n < 12 || strings.Contains(ev.State.Field, "bgp") {
			b, _ := json.Marshal(map[string]any{"seq": ev.Seq, "source": ev.Source.Principal, "path": ev.Path, "field": ev.State.Field, "from": ev.State.From, "to": ev.State.To})
			t.Logf("observer event %s", b)
		}
		n++
	}
	t.Logf("%d observer events committed", n)
	for _, l := range tw.Net.SubstrateIsNot() {
		t.Logf("report substrate: %s", l)
	}
	for _, l := range a.Report().Lines {
		t.Logf("report dhcp: %s", l)
	}
}

func logBGP(t *testing.T, sub *tap.Substrate, when string) {
	t.Helper()
	ctx := context.Background()
	for _, path := range sortedKeys(sub.Switches()) {
		out, err := sub.Exec(ctx, path, "vtysh", "-c", "show bgp summary")
		if err != nil {
			t.Logf("%s: vtysh: %v", path, err)
			continue
		}
		t.Logf("%s: %s `vtysh -c 'show bgp summary'`:\n%s", when, path, indent(out))
		if strings.Contains(path, "oob-agg") {
			rt, _ := sub.Exec(ctx, path, "vtysh", "-c", "show ip route bgp")
			t.Logf("%s: %s `show ip route bgp`:\n%s", when, path, indent(rt))
		}
	}
}

// startKeaContainer runs the stack's Kea in a container with no network; the twin attaches it
// at the aggregation switch (spec.adapters.network.attach). One subnet per rack segment, chosen
// by giaddr, the relay's address in it. Kea starts once its port has a link: it opens no socket
// on an interface that is not running and does not try again, and a SONiC-VS switch brings its
// port up only when it has booted, minutes after the twin cabled it (generic/frr-dhcrelay's ports
// are up at once).
func startKeaContainer(t *testing.T, d *nos.Docker, name string) *nos.Container {
	t.Helper()
	var subnets []any
	for i, rack := range []string{"r01", "r02"} {
		subnets = append(subnets, map[string]any{"id": i + 1, "subnet": fmt.Sprintf("10.30.%d.0/24", i),
			"pools":        []any{map[string]any{"pool": fmt.Sprintf("10.30.%d.10 - 10.30.%d.250", i, i)}},
			"option-data":  []any{map[string]any{"name": "routers", "data": fmt.Sprintf("10.30.%d.1", i)}},
			"user-context": map[string]any{"rack": rack}})
	}
	cfg := map[string]any{"Dhcp4": map[string]any{
		"interfaces-config": map[string]any{"interfaces": []any{"svc0"}, "dhcp-socket-type": "udp"},
		"lease-database":    map[string]any{"type": "memfile", "persist": true, "name": "/var/lib/kea/kea-leases4.csv", "lfc-interval": 0},
		"valid-lifetime":    3600, "store-extended-info": true, "subnet4": subnets,
		"loggers": []any{
			map[string]any{"name": "kea-dhcp4", "severity": "INFO", "output-options": []any{map[string]any{"output": "/var/log/kea-dhcp4.log"}}},
			map[string]any{"name": "kea-dhcp4.packets", "severity": "DEBUG", "debuglevel": 55, "output-options": []any{map[string]any{"output": "/var/log/kea-packets.log"}}},
		},
	}}
	b, _ := json.Marshal(cfg)
	script := `mkdir -p /var/lib/kea /run/kea /var/log
printf '%s' "$KEA_CONFIG" > /etc/kea/kea-dhcp4.json
until ip -4 addr show svc0 2>/dev/null | grep -q inet; do sleep 0.2; done
until [ "$(cat /sys/class/net/svc0/carrier 2>/dev/null)" = 1 ]; do sleep 0.2; done
ip -4 addr show svc0
exec kea-dhcp4 -c /etc/kea/kea-dhcp4.json`
	ctx := context.Background()
	c, err := d.Run(ctx, nos.Spec{Name: name, Image: keaImage, Hostname: "kea", Cmd: []string{"/bin/sh", "-c", script},
		Env:    []string{"KEA_CONFIG=" + string(b), "KEA_PIDFILE_DIR=/run/kea", "KEA_LOCKFILE_DIR=/run/kea"},
		Labels: map[string]string{"dc-twin.test": "linuxnet"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := d.Logs(context.Background(), c.ID)
			t.Logf("Kea container output:\n%s", indent(logs))
		}
		_ = d.Remove(context.Background(), c.ID)
	})
	t.Logf("Kea container %s started (no network until the twin attaches it)", name)
	return c
}

type keaLease struct {
	addr    string
	subnet  int
	circuit string
	remote  string
}

// keaLeases reads Kea's memfile from its container: the last row per hardware address, with the
// circuit id and remote id decoded from the relay agent information Kea stored.
func keaLeases(t *testing.T, d *nos.Docker, c *nos.Container) map[string]keaLease {
	t.Helper()
	out, _, err := d.Exec(context.Background(), c.ID, "cat", "/var/lib/kea/kea-leases4.csv")
	if err != nil {
		t.Fatal(err)
	}
	res := map[string]keaLease{}
	var cols map[string]int
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.ReplaceAll(fields[i], "&#x2c", ",")
		}
		if cols == nil {
			cols = map[string]int{}
			for i, c := range fields {
				cols[strings.TrimSpace(c)] = i
			}
			continue
		}
		get := func(c string) string {
			if i, ok := cols[c]; ok && i < len(fields) {
				return fields[i]
			}
			return ""
		}
		l := keaLease{addr: get("address")}
		l.subnet, _ = strconv.Atoi(get("subnet_id"))
		var uc struct {
			ISC struct {
				RAI struct {
					Sub string `json:"sub-options"`
				} `json:"relay-agent-info"`
			} `json:"ISC"`
		}
		if json.Unmarshal([]byte(get("user_context")), &uc) == nil {
			if raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(uc.ISC.RAI.Sub), "0x")); err == nil {
				for len(raw) >= 2 && 2+int(raw[1]) <= len(raw) {
					switch raw[0] {
					case 1:
						l.circuit = string(raw[2 : 2+raw[1]])
					case 2:
						l.remote = string(raw[2 : 2+raw[1]])
					}
					raw = raw[2+raw[1]:]
				}
			}
		}
		if hw, err := net.ParseMAC(get("hwaddr")); err == nil {
			res[hw.String()] = l
		}
	}
	return res
}
