//go:build linux

package linuxnet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap"
	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/inhost"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// (f) vm hosts on a real NOS's ports (D57). Two x86_64 servers at vm fidelity with the twin's own
// OS, under KVM, on a clos-2tier frontend fabric of generic/nvue-frr switches: each server's
// provisioning and frontend NICs are TAPs moved into its rack's leaf, the guest's NIC spliced onto
// them. In the guests (through the in-host agent's exec, over the serial console):
//
//   - provisioning: the guest's DHCP client is answered by the stack's Kea through the leaf's
//     relay, by giaddr, with the rack's subnet; the twin observes the lease on the port;
//   - frontend (no DHCP): static addresses on each leaf's VLAN; the guests reach each other across
//     leaf, spine and leaf, routed by eBGP, and a client at the spine reaches them; a connection
//     one guest opens to the other is observed as a flow the switch routed;
//   - a pulled frontend cable is the leaf's port going down and the guest unreachable; replaced,
//     it is reachable again;
//   - a tenant VRF with an L3 VNI on both leaves through NVUE (EVPN across the spine): the
//     frontend segments move into it; the guests still reach each other, now over VXLAN, and the
//     client in the default VRF no longer reaches them.
func TestVMHostsOnRealNOS(t *testing.T) {
	need(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("vm hosts need KVM: %v", err)
	}
	if _, err := qemu.Find("x86_64", "", false); err != nil {
		t.Skipf("vm hosts need QEMU for x86_64: %v", err)
	}
	if g := vm.Guests["x86_64"]; g.Present(vm.GuestDir(twin.AssetsDir(), "", g)) != nil {
		t.Skipf("the twin's x86_64 guest image %s is not in %s (dctwin assets fetch; DCTWIN_ASSETS)", g.Name, twin.AssetsDir())
	}
	d, err := nosRuntime(t)
	if err != nil {
		t.Skipf("no Docker Engine for the NOS runtime: %v", err)
	}
	ctx := context.Background()
	for _, img := range []string{nos.NVUEFRRImage, keaImage} {
		if !d.HasImage(ctx, img) {
			t.Skipf("image %s is not present (scripts/linux-substrate-e2e.sh builds it)", img)
		}
	}
	suffix := strconv.Itoa(os.Getpid())
	keaName, probeName := "dctwin-kea-"+suffix, "dctwin-probe-vm"
	p := newProbe(t, probeName)
	kea := startKeaContainerOn(t, d, keaName, "10.40")
	est := loadEstate(t, "tap-vm-nos.yaml", map[string]string{"KEA_CONTAINER": keaName, "PROBE_NETNS": probeName})
	tw, sub := upWith(t, est, "dhcp", "redfish", "vm", "inhost")
	plan := sub.Plan()
	if len(plan.HostPorts) != 4 {
		t.Fatalf("%d host ports spliced, want 4; skipped %v", len(plan.HostPorts), plan.HostSkipped)
	}
	for _, hp := range plan.HostPorts {
		t.Logf("host port %s (%s, %s) is %s:%s in %s", hp.Ref, hp.MAC, hp.Network, hp.Switch.Config.Hostname, hp.SwitchIf, hp.Vlan)
	}
	logBGP(t, sub, "after up")
	va := tw.Adapter("vm").(*vm.Adapter)
	ia := tw.Adapter("inhost").(*inhost.Adapter)
	hosts := []string{"site-1/hall-a/r01/server-01", "site-1/hall-a/r02/server-01"}
	port := func(host, name string) *tap.HostPortPlan {
		for _, hp := range plan.HostPorts {
			if hp.Device == host && hp.Port.Name == name {
				return hp
			}
		}
		t.Fatalf("%s:%s is not spliced", host, name)
		return nil
	}

	waitFor(t, 5*time.Minute, "both guests' in-host agents to attach", func() bool {
		n := 0
		for _, g := range ia.Guests() {
			if g.Attached {
				n++
			}
		}
		return n == len(hosts)
	})
	run := func(host, script string) (string, int) {
		t.Helper()
		ectx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		out, rc, err := ia.Exec(ectx, host, script)
		if err != nil {
			t.Fatalf("%s: exec: %v", host, err)
		}
		return out, rc
	}
	must := func(host, script string) string {
		t.Helper()
		out, rc := run(host, script)
		if rc != 0 {
			t.Fatalf("%s: exit %d from\n%s\n%s", host, rc, indent(script), indent(out))
		}
		return out
	}
	// the guest's interface for a port, by the model's MAC
	ifOf := func(hp *tap.HostPortPlan) string {
		return fmt.Sprintf("$(grep -il %s /sys/class/net/*/address | cut -d/ -f5)", hp.MAC)
	}
	t.Logf("%s: the guest's NICs:\n%s", hosts[0], indent(must(hosts[0], "ip link")))

	// provisioning: the guest asks, the leaf relays to Kea by giaddr
	const udhcpcScript = `cat > /tmp/udhcpc.sh <<'X'
#!/bin/sh
case $1 in bound|renew)
	ip addr flush dev $interface
	ip addr add $ip/$mask dev $interface
	[ -n "$router" ] && ip route replace default via ${router%% *} dev $interface ;;
esac
X
chmod +x /tmp/udhcpc.sh
`
	leases := map[string]string{}
	for i, h := range hosts {
		hp := port(h, "eth0")
		out := must(h, udhcpcScript+fmt.Sprintf("i=%s; ip link set $i up; udhcpc -i $i -n -q -t 30 -T 2 -s /tmp/udhcpc.sh 2>&1 | tail -n 3; ip -4 addr show $i", ifOf(hp)))
		t.Logf("%s: DHCP on %s:\n%s", h, hp.Port.Name, indent(out))
		want := fmt.Sprintf("10.40.%d.", i)
		var addr string
		for _, f := range strings.Fields(out) {
			if strings.HasPrefix(f, want) {
				addr, _, _ = strings.Cut(f, "/")
			}
		}
		if addr == "" {
			t.Fatalf("%s: no address in %s/24 (the rack's subnet, by the leaf's giaddr)", h, want)
		}
		leases[h] = addr
		waitFor(t, 15*time.Second, h+"'s lease observed on its port", func() bool {
			for _, x := range va.NICs(h) {
				if x.Port == hp.Ref && x.Lease == addr && x.Spliced {
					return true
				}
			}
			return false
		})
	}
	kl := keaLeases(t, d, kea)
	for i, h := range hosts {
		hp := port(h, "eth0")
		k, ok := kl[hp.MAC.String()]
		if !ok || k.addr != leases[h] || k.subnet != i+1 {
			t.Errorf("%s: Kea's lease %+v, the guest holds %s", h, k, leases[h])
		}
		if k.circuit != hp.Vlan && k.circuit != strings.ToLower(hp.Vlan) {
			t.Errorf("%s: circuit id %q, want the leaf's VLAN %s (ISC dhcrelay's interface)", h, k.circuit, hp.Vlan)
		}
		t.Logf("%s: leased %s by Kea (subnet %d, relayed by %s, circuit %q)", h, k.addr, k.subnet, hp.Switch.Config.Hostname, k.circuit)
	}

	// frontend: static addresses on each leaf's VLAN, routed across the fabric
	fe := map[string]string{}
	for i, h := range hosts {
		hp := port(h, "eth1")
		fe[h] = fmt.Sprintf("10.50.%d.10", i)
		must(h, fmt.Sprintf("i=%s; ip link set $i up; ip addr add %s/24 dev $i; ip route add 10.50.0.0/20 via 10.50.%d.1 dev $i; ping -c 3 -W 2 10.50.%d.1",
			ifOf(hp), fe[h], i, i))
	}
	ping := func(from, to string) (string, int) {
		return run(from, "ping -c 3 -W 2 "+to)
	}
	waitFor(t, 60*time.Second, "r01's guest to reach r02's across the fabric", func() bool {
		_, rc := ping(hosts[0], fe[hosts[1]])
		return rc == 0
	})
	out, _ := ping(hosts[0], fe[hosts[1]])
	t.Logf("%s → %s (frontend, leaf → spine → leaf):\n%s", hosts[0], fe[hosts[1]], indent(out))
	out = must(hosts[1], "ping -c 2 -W 2 "+leases[hosts[0]])
	t.Logf("%s → %s (provisioning):\n%s", hosts[1], leases[hosts[0]], indent(out))
	probePing := func(addr string) bool {
		return exec.Command("ip", "netns", "exec", p.name, "ping", "-c", "2", "-W", "2", addr).Run() == nil
	}
	for _, h := range hosts {
		if !probePing(fe[h]) || !probePing(leases[h]) {
			t.Errorf("the client at the spine does not reach %s at %s and %s", h, fe[h], leases[h])
		}
	}

	// a connection the guest opens is observed as the switch's
	must(hosts[1], "(nc -l -p 7000 > /tmp/got 2>&1 &) ; sleep 0.5")
	must(hosts[0], "echo hello-from-r01 | nc -w 3 "+fe[hosts[1]]+" 7000")
	waitFor(t, 10*time.Second, "r02's guest to receive the connection's data", func() bool {
		out, _ := run(hosts[1], "cat /tmp/got")
		return strings.Contains(out, "hello-from-r01")
	})
	want := "tcp " + fe[hosts[1]] + ":7000"
	waitFor(t, 10*time.Second, "the connection observed on r01's frontend port", func() bool {
		for _, x := range va.NICs(hosts[0]) {
			if x.Port == port(hosts[0], "eth1").Ref && x.Flows[want] > 0 {
				return true
			}
		}
		return false
	})
	for _, l := range va.Report().Lines {
		if strings.Contains(l, "network:") {
			t.Logf("report vm: %s", l)
		}
	}

	// a pulled frontend cable: the leaf's port goes down and the guest is unreachable
	victim := port(hosts[1], "eth1")
	portKey := victim.Switch.Device.Path + ":" + victim.Peer.Name // the observer names ports as the model does
	waitFor(t, 30*time.Second, "the observer to see "+portKey+" up", func() bool { return sub.Observed()[portKey] == "up" })
	if err := tw.Engine.SetLink(victim.Ref, false, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "the observer to see "+portKey+" go down", func() bool { return sub.Observed()[portKey] != "up" })
	if out, rc := ping(hosts[0], fe[hosts[1]]); rc == 0 {
		t.Errorf("r02's guest still answers with its cable pulled:\n%s", out)
	}
	carrier := func() string {
		out, _ := run(hosts[1], "cat /sys/class/net/"+ifOf(victim)+"/carrier 2>&1")
		return strings.TrimSpace(out)
	}
	waitFor(t, 10*time.Second, "r02's guest NIC to lose its carrier", func() bool { return carrier() == "0" })
	out, _ = run(hosts[1], "ip link show "+ifOf(victim))
	t.Logf("cable pulled at %s; the guest's NIC has no carrier:\n%s", victim.Ref, indent(out))
	if err := tw.Engine.SetLink(victim.Ref, true, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "the observer to see "+portKey+" up again", func() bool { return sub.Observed()[portKey] == "up" })
	waitFor(t, 10*time.Second, "r02's guest NIC to regain its carrier", func() bool { return carrier() == "1" })
	waitFor(t, 60*time.Second, "r02's guest reachable again", func() bool {
		_, rc := ping(hosts[0], fe[hosts[1]])
		return rc == 0
	})
	t.Logf("cable replaced at %s: reachable again", victim.Ref)

	// a tenant VRF with an L3 VNI on both leaves, EVPN across the spine
	if err := sub.Kernel()["vrf"]; err != nil {
		t.Logf("the Docker host's kernel has no VRF devices (%v): the tenant VRF is not built", err)
		return
	}
	// the switches' management interfaces answer in band at their loopbacks, routed by the fabric;
	// a revision that adds an address family to a BGP neighbour renegotiates the session, as FRR
	// does on Cumulus, so the fabric is waited on to converge again after each
	converged := func(why string) {
		t.Helper()
		for _, sp := range plan.Switches {
			lo := sp.Config.Loopback.String()
			deadline := time.Now().Add(90 * time.Second)
			for !probePing(lo) {
				if time.Now().After(deadline) {
					r, _ := exec.Command("ip", "netns", "exec", p.name, "ip", "route").CombinedOutput()
					t.Fatalf("%s: the client does not reach %s's loopback %s; its routes:\n%s", why, sp.Config.Hostname, lo, indent(string(r)))
				}
				time.Sleep(time.Second)
			}
		}
	}
	converged("before NVUE")
	var inputs []tap.SwitchInput
	b, err := os.ReadFile(filepath.Join(tw.Runtime.StateDir, "stack-inputs", tap.SwitchesFile))
	if err != nil || json.Unmarshal(b, &inputs) != nil {
		t.Fatalf("stack-inputs/%s: %v", tap.SwitchesFile, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(tw.Net.CAPEM)
	clients := map[string]*nvueClient{}
	for _, in := range inputs {
		if in.NVUE == "" || in.NVUEUser != nvue.User {
			t.Fatalf("%s: no NVUE in stack-inputs", in.Path)
		}
		clients[in.Path] = &nvueClient{t: t, in: in, pw: in.NVUEPassword, http: &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{DialContext: p.dial,
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: in.Hostname}}}}
	}
	evpnNeighbours := func(c *nvueClient) map[string]any {
		_, ns := c.do("GET", "vrf/default/router/bgp/neighbor", nil)
		out := map[string]any{}
		for n := range ns {
			out[n] = map[string]any{"address-family": map[string]any{"l2vpn-evpn": map[string]any{"enable": "on"}}}
		}
		return out
	}
	for path, c := range clients {
		if strings.Contains(path, "/spine-") {
			if out := c.apply(map[string]any{"evpn": map[string]any{"enable": "on"}, "vrf/default/router/bgp/neighbor": evpnNeighbours(c)}); out["state"] != "applied" {
				t.Fatalf("%s: EVPN: %v", c.in.Hostname, out)
			}
			converged("after EVPN on " + c.in.Hostname)
		}
	}
	for i, h := range hosts {
		hp := port(h, "eth1")
		c := clients[hp.Switch.Device.Path]
		svi := strings.ToLower(hp.Vlan)
		out := c.apply(map[string]any{
			"evpn":      map[string]any{"enable": "on"},
			"nve/vxlan": map[string]any{"enable": "on", "source": map[string]any{"address": c.in.Loopback}},
			"vrf/tenant-a": map[string]any{"evpn": map[string]any{"vni": map[string]any{"104001": map[string]any{}}},
				"router": map[string]any{"bgp": map[string]any{"enable": "on", "address-family": map[string]any{"ipv4-unicast": map[string]any{"redistribute": map[string]any{"connected": map[string]any{"enable": "on"}}}}}}},
			"interface/" + svi:                map[string]any{"ip": map[string]any{"address": map[string]any{fmt.Sprintf("10.50.%d.1/24", i): map[string]any{}}, "vrf": "tenant-a"}},
			"vrf/default/router/bgp/neighbor": evpnNeighbours(c),
		})
		if out["state"] != "applied" {
			conf, _ := sub.RoutingExec(ctx, c.in.Path, "sh", "-c", "echo '== frr-reload.log'; tail -n 60 /var/log/frr/frr-reload.log; echo '== running'; vtysh -c 'show running-config'")
			t.Fatalf("%s: tenant-a: %v\nFRR's configuration (the revision's, then the running one):\n%s", c.in.Hostname, out, indent(conf))
		}
		t.Logf("%s: %s into tenant-a with L3 VNI 104001: %v", c.in.Hostname, svi, out["transition"])
		converged("after tenant-a on " + c.in.Hostname)
	}
	waitFor(t, 120*time.Second, "r01's guest to reach r02's in tenant-a, over the L3 VNI", func() bool {
		_, rc := ping(hosts[0], fe[hosts[1]])
		return rc == 0
	})
	for i, h := range hosts {
		c := clients[port(h, "eth1").Switch.Device.Path]
		out, _ := sub.RoutingExec(ctx, c.in.Path, "vtysh", "-c", "show ip route vrf tenant-a", "-c", "show bgp l2vpn evpn route type prefix")
		t.Logf("%s: tenant-a's routes and EVPN type-5 routes:\n%s", c.in.Hostname, indent(out))
		if !strings.Contains(out, fmt.Sprintf("10.50.%d.0/24", 1-i)) {
			t.Errorf("%s: tenant-a has no route to the other rack's segment", c.in.Hostname)
		}
	}
	for _, h := range hosts {
		if probePing(fe[h]) {
			t.Errorf("the client in the default VRF still reaches %s at %s, in tenant-a", h, fe[h])
		}
		if !probePing(leases[h]) {
			t.Errorf("the client no longer reaches %s's provisioning address %s, which stayed in the default VRF", h, leases[h])
		}
	}
	t.Logf("tenant-a: the guests reach each other over VXLAN; the default VRF reaches only their provisioning addresses")
}
