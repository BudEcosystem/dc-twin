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
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap"
	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/inhost"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// dpuTenant is one host's place behind its BlueField: the DPU's AS and loopback (NICo's fnn-asn
// and lo-ip pools), the VPC's VRF and L3 VNI, the host's VLAN on pf0hpf_if and its gateway.
type dpuTenant struct {
	host, dpu string
	asn       uint32
	lo        string
	vpc       string
	vni       int
	vlan      int
	gw, addr  string // the SVI's address and the host's, each with its prefix
}

// datacenterASN is the site's datacenter_asn, the AS of every VPC's route target (NICo's FNN:
// <datacenter_asn>:<L3VNI>).
const datacenterASN = 65500

// fnnConfig is the NVUE configuration NICo's FNN template gives a DPU for one host interface in a
// VPC (crates/agent/templates/nvue_startup_fnn.conf, reduced to what isolation rests on): the
// loopback as VTEP and router id, unnumbered eBGP to the leaves on p0_if and p1_if in a peer
// group with IPv4 and EVPN, the host's PF an access port of a VLAN whose SVI is in the VPC's
// VRF, and the VRF's L3 VNI with route targets <datacenter_asn>:<VNI> imported and exported.
// acl, when not nil, is bound inbound on pf0hpf_if.
func fnnConfig(x dpuTenant, acl map[string]any) map[string]any {
	rt := fmt.Sprintf("%d:%d", datacenterASN, x.vni)
	svi := fmt.Sprintf("vlan%d", x.vlan)
	pf := map[string]any{"type": "swp", "bridge": map[string]any{"domain": map[string]any{"br_default": map[string]any{"access": x.vlan}}}}
	cfg := map[string]any{
		"system": map[string]any{"hostname": x.dpu[strings.LastIndex(x.dpu, "/")+1:], "api": map[string]any{"listening-address": map[string]any{"0.0.0.0": map[string]any{}}}},
		"interface": map[string]any{
			"lo":        map[string]any{"type": "loopback", "ip": map[string]any{"address": map[string]any{x.lo + "/32": map[string]any{}}}},
			"p0_if":     map[string]any{"type": "swp"},
			"p1_if":     map[string]any{"type": "swp"},
			"pf0hpf_if": pf,
			svi:         map[string]any{"type": "svi", "vlan": x.vlan, "ip": map[string]any{"address": map[string]any{x.gw: map[string]any{}}, "vrf": x.vpc}},
		},
		"bridge": map[string]any{"domain": map[string]any{"br_default": map[string]any{"vlan": map[string]any{fmt.Sprint(x.vlan): map[string]any{}}}}},
		"evpn":   map[string]any{"enable": "on"},
		"nve":    map[string]any{"vxlan": map[string]any{"enable": "on", "arp-nd-suppress": "on", "source": map[string]any{"address": x.lo}}},
		"router": map[string]any{"bgp": map[string]any{"enable": "on", "autonomous-system": x.asn, "router-id": x.lo}},
		"vrf": map[string]any{
			"default": map[string]any{"router": map[string]any{"bgp": map[string]any{
				"enable": "on",
				"address-family": map[string]any{
					"ipv4-unicast": map[string]any{"enable": "on", "redistribute": map[string]any{"connected": map[string]any{"enable": "on"}}, "multipaths": map[string]any{"ebgp": 128}},
					"l2vpn-evpn":   map[string]any{"enable": "on"},
				},
				"path-selection": map[string]any{"multipath": map[string]any{"aspath-ignore": "on"}},
				"peer-group": map[string]any{"underlay": map[string]any{"remote-as": "external",
					"address-family": map[string]any{"l2vpn-evpn": map[string]any{"enable": "on"}}}},
				"neighbor": map[string]any{
					"p0_if": map[string]any{"type": "unnumbered", "peer-group": "underlay"},
					"p1_if": map[string]any{"type": "unnumbered", "peer-group": "underlay"},
				},
			}}},
			x.vpc: map[string]any{
				"evpn": map[string]any{"enable": "on", "vni": map[string]any{fmt.Sprint(x.vni): map[string]any{}}},
				"router": map[string]any{"bgp": map[string]any{
					"enable": "on",
					"address-family": map[string]any{"ipv4-unicast": map[string]any{"enable": "on",
						"redistribute": map[string]any{"connected": map[string]any{"enable": "on"}},
						"route-export": map[string]any{"to-evpn": map[string]any{"enable": "on"}}}},
					"route-import": map[string]any{"from-evpn": map[string]any{"route-target": map[string]any{rt: map[string]any{}}}},
					"route-export": map[string]any{"to-evpn": map[string]any{"route-target": map[string]any{rt: map[string]any{}}}},
				}},
			},
		},
	}
	if acl != nil {
		cfg["acl"] = acl
		pf["acl"] = map[string]any{}
		for name := range acl {
			pf["acl"].(map[string]any)[name] = map[string]any{"inbound": map[string]any{}}
		}
	}
	return cfg
}

// push is NICo's agent's push_config over NVUE's REST API (crates/nvue-client): a revision,
// emptied, the whole configuration patched in, the diff from applied asked, applied when there is
// one, its state polled. It returns the final state, "" when there was no change.
func (c *nvueClient) push(cfg map[string]any) (string, map[string]any) {
	c.t.Helper()
	st, out := c.do("POST", "revision", nil)
	var rev string
	for id := range out {
		rev = id
	}
	if st != 200 || len(out) != 1 {
		c.t.Fatalf("%s: POST revision: %d %v", c.in.Hostname, st, out)
	}
	if st, out := c.do("DELETE", "?rev="+rev, map[string]any{}); st != 200 {
		c.t.Fatalf("%s: DELETE /: %d %v", c.in.Hostname, st, out)
	}
	body := map[string]any{}
	for _, k := range []string{"bridge", "evpn", "interface", "nve", "router", "system", "vrf", "acl"} {
		body[k] = cfg[k]
	}
	if st, out := c.do("PATCH", "?rev="+rev, body); st != 200 {
		c.t.Fatalf("%s: PATCH /: %d %v", c.in.Hostname, st, out)
	}
	if st, d := c.do("GET", "?diff=applied&rev="+rev+"&filled=false", nil); st != 200 {
		c.t.Fatalf("%s: diff: %d %v", c.in.Hostname, st, d)
	} else if len(d) == 0 {
		return "", nil
	}
	if st, out := c.do("PATCH", "revision/"+rev, map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes", "ignore_fail": "ignore_fail_no"}}); st != 200 {
		c.t.Fatalf("%s: apply: %d %v", c.in.Hostname, st, out)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, out = c.do("GET", "revision/"+rev, nil)
		s := fmt.Sprint(out["state"])
		if s != "applying" && s != "pending" || time.Now().After(deadline) {
			return s, out
		}
		time.Sleep(time.Second)
	}
}

// hold keeps a failed test's twin up for DCTWIN_HOLD (a duration) to look at by hand.
func hold(t *testing.T) {
	if d, err := time.ParseDuration(os.Getenv("DCTWIN_HOLD")); err == nil {
		t.Logf("holding for %s (DCTWIN_HOLD)", d)
		time.Sleep(d)
	}
}

// eventually is whether ok holds within a time.
func eventually(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
	return true
}

// (g) BlueFields whose HBN is emulated (D71, P2 and P5 of the DPU isolation design): three x86
// servers at vm behind a BlueField each, two in rack r01 (one leaf) and one in r02, on a
// generic/nvue-frr clos-2tier fabric. Each BlueField is given, through its NVUE as NICo's agent
// gives it, the FNN configuration of a VPC: A1 (r01) and B1 (r02) in VPC 1, A2 (r01, the same
// leaf as A1) in VPC 2. In the guests, over the host PF that crosses the DPU:
//
//   - the DPUs' unnumbered sessions to their leaf are established on both uplinks, read as NICo's
//     health check reads them (NVUE: vrf default router bgp neighbor, include /*/state);
//   - A1 and B1 reach each other, routed by their DPUs' VPC 1 VRF over the L3 VNI, each leaf
//     carrying the other DPU's EVPN type-5 route and holding no tenant VRF itself;
//   - A2 reaches neither, across a leaf it shares with A1; the fabric's default VRF (the probe at
//     the spine) reaches no tenant address;
//   - an ACL NICo would write (a network security group) on B1's DPU drops B1's TCP to A1's port
//     7000 and nothing else; removed, the port is reached;
//   - the DPU's MAC table (NVUE) shows B1's NIC on pf0hpf_if; the same configuration again is no
//     change; every revision is a committed request event with its client;
//   - a pulled uplink is that session down and the VPC still up over the other;
//   - A2 moved into VPC 1 reaches both; then an Ethernet Virtualizer segment (an L2 VNI across
//     DPUs with a VRR gateway on each) puts A1 and B1 on one subnet.
func TestDPUIsolation(t *testing.T) {
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
	for _, img := range []string{nos.NVUEFRRImage, nos.HBNImage} {
		if !d.HasImage(ctx, img) {
			t.Skipf("image %s is not present (scripts/linux-substrate-e2e.sh builds it)", img)
		}
	}
	probeName := "dctwin-probe-dpu"
	p := newProbe(t, probeName)
	est := loadEstate(t, "tap-dpu.yaml", map[string]string{"PROBE_NETNS": probeName})
	tw, sub := upWith(t, est, "dhcp", "redfish", "vm", "inhost")
	if err := sub.Kernel()["vrf"]; err != nil {
		t.Skipf("the Docker host's kernel has no VRF devices (%v)", err)
	}
	plan := sub.Plan()
	labelled := false
	for _, l := range sub.Report().Lines {
		labelled = labelled || strings.Contains(l, "3 BlueFields' HBN is EMULATED")
	}
	if !labelled {
		t.Errorf("the substrate's report does not label the emulated HBN: %v", sub.Report().Lines)
	}
	const r01, r02 = "site-1/hall-a/r01/", "site-1/hall-a/r02/"
	ten := map[string]*dpuTenant{
		"A1": {host: r01 + "server-01", asn: 4200000001, lo: "10.71.0.1", vpc: "vpc_1", vni: 5001, vlan: 101, gw: "10.70.1.1/24", addr: "10.70.1.10/24"},
		"A2": {host: r01 + "server-02", asn: 4200000002, lo: "10.71.0.2", vpc: "vpc_2", vni: 5002, vlan: 102, gw: "10.70.2.1/24", addr: "10.70.2.10/24"},
		"B1": {host: r02 + "server-01", asn: 4200000003, lo: "10.71.0.3", vpc: "vpc_1", vni: 5001, vlan: 103, gw: "10.70.3.1/24", addr: "10.70.3.10/24"},
	}
	names := []string{"A1", "A2", "B1"}
	pfOf := map[string]*tap.HostPortPlan{}
	for _, n := range names {
		x := ten[n]
		x.dpu = x.host + "/bf3-0"
		for _, hp := range plan.HostPorts {
			if hp.Device == x.host && hp.SwitchIf == "pf0hpf_if" && hp.Switch.Device.Path == x.dpu {
				pfOf[n] = hp
			}
		}
		if pfOf[n] == nil {
			t.Fatalf("%s: its host PF is not spliced onto %s's pf0hpf_if; host ports %d, skipped %v", x.host, x.dpu, len(plan.HostPorts), plan.HostSkipped)
		}
		t.Logf("%s: %s's NIC %s (%s) is %s:pf0hpf_if", n, x.host, pfOf[n].Ref, pfOf[n].MAC, x.dpu)
	}

	// each DPU's NVUE, beside it in its namespace, as NICo's agent reaches it
	var inputs []tap.SwitchInput
	b, err := os.ReadFile(filepath.Join(tw.Runtime.StateDir, "stack-inputs", tap.SwitchesFile))
	if err != nil || json.Unmarshal(b, &inputs) != nil {
		t.Fatalf("stack-inputs/%s: %v", tap.SwitchesFile, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(tw.Net.CAPEM)
	clients := map[string]*nvueClient{}
	for _, in := range inputs {
		if in.NOS != nos.HBN {
			continue
		}
		if in.NVUE != "127.0.0.1:8765" || in.Container == "" {
			t.Errorf("%s: NVUE at %q in %q, want 127.0.0.1:8765 in its container", in.Path, in.NVUE, in.Container)
		}
		clients[in.Path] = &nvueClient{t: t, in: in, pw: in.NVUEPassword, http: &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{
			DialContext: sub.DialIn(in.Path), TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: in.Hostname}}}}
	}
	if len(clients) != 3 {
		t.Fatalf("%d emulated BlueFields in stack-inputs, want 3", len(clients))
	}
	since := tw.Engine.Events(0, nil)
	var mark uint64
	if len(since) > 0 {
		mark = since[len(since)-1].Seq
	}
	for _, n := range names {
		c := clients[ten[n].dpu]
		if _, sys := c.do("GET", "system", nil); sys["build"] != nvue.HBNBuild {
			t.Fatalf("%s: NVUE's build %v, want %q (NICo's agent parses HBN's version from it)", n, sys["build"], nvue.HBNBuild)
		}
		st, out := c.push(fnnConfig(*ten[n], nil))
		if st != "applied" {
			conf, _ := sub.Exec(ctx, ten[n].dpu, "sh", "-c", "tail -n 40 /var/log/frr/frr-reload.log 2>/dev/null; vtysh -c 'show running-config'")
			t.Fatalf("%s: FNN configuration: %s %v\n%s", n, st, out["transition"], indent(conf))
		}
		t.Logf("%s: %s applied FNN for %s (VNI %d, AS %d)", n, ten[n].dpu, ten[n].vpc, ten[n].vni, ten[n].asn)
	}
	// NICo's health check: both uplinks' sessions established
	for _, n := range names {
		c := clients[ten[n].dpu]
		waitFor(t, 3*time.Minute, n+"'s DPU's unnumbered sessions on p0_if and p1_if", func() bool {
			st, ns := c.do("GET", "vrf/default/router/bgp/neighbor?include=/*/state", nil)
			if st != 200 {
				return false
			}
			for _, up := range []string{"p0_if", "p1_if"} {
				m, _ := ns[up].(map[string]any)
				if m["state"] != "established" {
					return false
				}
			}
			return true
		})
	}
	t.Logf("every DPU has both uplinks established (NICo's BgpPeeringTor check)")

	// the guests, through their host PF
	va := tw.Adapter("vm").(*vm.Adapter)
	ia := tw.Adapter("inhost").(*inhost.Adapter)
	waitFor(t, 5*time.Minute, "the guests' in-host agents to attach", func() bool {
		n := 0
		for _, g := range ia.Guests() {
			if g.Attached {
				n++
			}
		}
		return n == len(names)
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
	ifOf := func(n string) string {
		return fmt.Sprintf("$(grep -il %s /sys/class/net/*/address | cut -d/ -f5)", pfOf[n].MAC)
	}
	addr := func(n string) string { a, _, _ := strings.Cut(ten[n].addr, "/"); return a }
	address := func(n, cidr, gw string) {
		out, rc := run(ten[n].host, fmt.Sprintf("i=%s; ip link set $i mtu 1400 up; ip addr flush dev $i; ip addr add %s dev $i; ip route replace 10.0.0.0/8 via %s dev $i; ip -4 addr show $i",
			ifOf(n), cidr, gw))
		if rc != 0 {
			t.Fatalf("%s: addressing its host PF: %s", n, out)
		}
	}
	for _, n := range names {
		gw, _, _ := strings.Cut(ten[n].gw, "/")
		address(n, ten[n].addr, gw)
	}
	reach := func(from, to string) bool {
		_, rc := run(ten[from].host, "ping -c 2 -W 2 "+to)
		return rc == 0
	}
	waitFor(t, 3*time.Minute, "A1 to reach B1 in VPC 1, across the fabric", func() bool { return reach("A1", addr("B1")) })
	if !reach("B1", addr("A1")) {
		t.Errorf("B1 does not reach A1 in VPC 1")
	}
	out, _ := run(ten["A1"].host, "ping -c 3 -W 2 "+addr("B1"))
	t.Logf("A1 → B1 (VPC 1, DPU → leaf → spine → leaf → DPU over the L3 VNI):\n%s", indent(out))
	for _, l := range plan.Switches {
		if len(l.Config.Unnumbered) == 0 {
			continue
		}
		out, err := sub.RoutingExec(ctx, l.Device.Path, "vtysh", "-c", "show bgp l2vpn evpn route type prefix")
		if err != nil {
			t.Fatalf("%s: %v", l.Config.Hostname, err)
		}
		for _, pfx := range []string{"10.70.1.0", "10.70.3.0"} {
			if !strings.Contains(out, pfx) {
				t.Errorf("%s: no EVPN type-5 route for %s/24:\n%s", l.Config.Hostname, pfx, indent(out))
			}
		}
		if vrfs, _ := sub.RoutingExec(ctx, l.Device.Path, "ip", "-br", "link", "show", "type", "vrf"); strings.TrimSpace(vrfs) != "" {
			t.Errorf("%s holds a tenant VRF: %s", l.Config.Hostname, vrfs)
		}
	}

	// isolation: VPC 2 reaches nothing of VPC 1's, on the same leaf or another
	for _, c := range [][2]string{{"A2", "A1"}, {"A2", "B1"}, {"A1", "A2"}, {"B1", "A2"}} {
		if reach(c[0], addr(c[1])) {
			t.Errorf("%s (%s) reaches %s (%s)", c[0], ten[c[0]].vpc, c[1], ten[c[1]].vpc)
		}
	}
	for _, n := range names {
		if exec.Command("ip", "netns", "exec", p.name, "ping", "-c", "1", "-W", "2", addr(n)).Run() == nil {
			t.Errorf("the fabric's default VRF (the probe at the spine) reaches %s at %s", n, addr(n))
		}
	}
	t.Logf("VPC 2 (A2) reaches neither A1 on its own leaf nor B1; the underlay reaches no tenant")

	// a network security group on B1's DPU: TCP to A1:7000 dropped, the rest passes
	nc := func(from, to string, port int) bool {
		run(ten[to].host, fmt.Sprintf("pkill -f 'nc -l -p %d' ; (nc -l -p %d > /tmp/got%d 2>&1 &) ; sleep 0.5", port, port, port))
		_, rc := run(ten[from].host, fmt.Sprintf("echo hello | nc -w 3 %s %d", addr(to), port))
		return rc == 0
	}
	if !nc("B1", "A1", 7000) {
		t.Fatalf("B1 cannot open TCP to A1:7000 before any ACL")
	}
	acl := map[string]any{"nsg_b1": map[string]any{"type": "ipv4", "rule": map[string]any{
		"10": map[string]any{"action": map[string]any{"deny": map[string]any{}}, "hw-offload": "off",
			"match": map[string]any{"ip": map[string]any{"protocol": "tcp", "dest-ip": ten["A1"].addr, "dest-port": map[string]any{"7000": map[string]any{}}}}},
	}}}
	if st, out := clients[ten["B1"].dpu].push(fnnConfig(*ten["B1"], acl)); st != "applied" {
		t.Fatalf("B1's ACL: %s %v", st, out["transition"])
	}
	if nc("B1", "A1", 7000) {
		nft, _ := sub.Exec(ctx, ten["B1"].dpu, "nft", "list", "ruleset")
		t.Errorf("B1 still reaches A1:7000 with the ACL bound:\n%s", indent(nft))
	}
	if !nc("B1", "A1", 7001) || !reach("B1", addr("A1")) {
		t.Errorf("the ACL dropped more than TCP to port 7000")
	}
	if st, _ := clients[ten["B1"].dpu].push(fnnConfig(*ten["B1"], nil)); st != "applied" || !eventually(30*time.Second, func() bool { return nc("B1", "A1", 7000) }) {
		nft, _ := sub.Exec(ctx, ten["B1"].dpu, "nft", "list", "ruleset")
		t.Errorf("the ACL removed (%s): B1 still cannot reach A1:7000; B1's DPU's nftables:\n%s", st, indent(nft))
	}
	t.Logf("an ACL on B1's DPU dropped TCP to A1:7000 only; removed, the port is reached")

	// operational reads, idempotence, events
	waitFor(t, 30*time.Second, "B1's NIC in its DPU's MAC table", func() bool {
		_, mt := clients[ten["B1"].dpu].do("GET", "bridge/domain/br_default/mac-table", nil)
		for _, e := range mt {
			m, _ := e.(map[string]any)
			if strings.EqualFold(fmt.Sprint(m["mac"]), pfOf["B1"].MAC.String()) && m["interface"] == "pf0hpf_if" && m["entry-type"] != "permanent" {
				return true
			}
		}
		return false
	})
	if st, _ := clients[ten["A1"].dpu].push(fnnConfig(*ten["A1"], nil)); st != "" {
		t.Errorf("A1's unchanged configuration was applied again (%s); NICo's agent applies only a diff", st)
	}
	applies := map[string]int{}
	for _, ev := range tw.Engine.Events(mark, func(e schema.Event) bool { return e.Request != nil && e.Request.Protocol == "nvue" }) {
		if ev.Request.Canonical == "network.config.apply" {
			applies[ev.Path]++
			if ev.Source.Principal != nvue.User || ev.Source.Class != "stack" || ev.Request.Status != 200 {
				t.Errorf("an apply event %+v %+v", ev.Source, ev.Request)
			}
		}
	}
	for _, n := range names {
		if applies[ten[n].dpu] == 0 {
			t.Errorf("no network.config.apply event for %s", ten[n].dpu)
		}
	}
	t.Logf("NVUE applies committed as events: %v", applies)

	// a pulled uplink: that session down, the VPC up over the other
	var uplink *tap.LinkPlan
	for _, lp := range plan.Links {
		if (lp.A.Device.Path == ten["A1"].dpu && lp.APort == "p0_if") || (lp.B.Device.Path == ten["A1"].dpu && lp.BPort == "p0_if") {
			uplink = lp
		}
	}
	if uplink == nil {
		t.Fatalf("no cable on A1's DPU's p0_if")
	}
	if err := tw.Engine.SetLink(uplink.Link.ID, false, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "A1's DPU's p0_if session to go down", func() bool {
		_, ns := clients[ten["A1"].dpu].do("GET", "vrf/default/router/bgp/neighbor?include=/*/state", nil)
		m, _ := ns["p0_if"].(map[string]any)
		return m["state"] != "established"
	})
	if !eventually(60*time.Second, func() bool { return reach("A1", addr("B1")) }) {
		for _, path := range []string{ten["A1"].dpu, ten["B1"].dpu} {
			diag, _ := sub.Exec(ctx, path, "sh", "-c", "ip -br link show p0_if p1_if; ip route; ip route show vrf vpc_1; for i in p0_if p1_if; do cat /proc/sys/net/ipv4/conf/$i/ignore_routes_with_linkdown; done; vtysh -c 'show bgp summary' -c 'show bgp l2vpn evpn route type prefix'")
			t.Logf("%s:\n%s", path, indent(diag))
		}
		for _, l := range plan.Switches {
			if len(l.Config.Unnumbered) > 0 {
				diag, _ := sub.RoutingExec(ctx, l.Device.Path, "sh", "-c", "ip -br link; ip route; vtysh -c 'show bgp summary'")
				t.Logf("%s:\n%s", l.Config.Hostname, indent(diag))
			}
		}
		hold(t)
		t.Fatalf("A1 does not reach B1 with its DPU's p0 cable pulled")
	}
	if err := tw.Engine.SetLink(uplink.Link.ID, true, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 90*time.Second, "A1's DPU's p0_if session back", func() bool {
		_, ns := clients[ten["A1"].dpu].do("GET", "vrf/default/router/bgp/neighbor?include=/*/state", nil)
		m, _ := ns["p0_if"].(map[string]any)
		return m["state"] == "established"
	})
	t.Logf("A1's DPU lost p0 and kept VPC 1 over p1; p0 back")

	// A2 moved into VPC 1: reaches both
	a2 := *ten["A2"]
	a2.vpc, a2.vni = "vpc_1", 5001
	if st, out := clients[a2.dpu].push(fnnConfig(a2, nil)); st != "applied" {
		t.Fatalf("A2 into VPC 1: %s %v", st, out["transition"])
	}
	if !eventually(2*time.Minute, func() bool { return reach("A2", addr("A1")) && reach("A2", addr("B1")) }) {
		for _, n := range names {
			diag, _ := sub.Exec(ctx, ten[n].dpu, "sh", "-c", "ip -d link show type vxlan; ip route show vrf vpc_1; ip neigh show dev vlan4024; bridge fdb show dev vni5001 2>/dev/null; bridge fdb show dev vni5002 2>/dev/null; vtysh -c 'show evpn rmac vni all' -c 'show evpn next-hops vni all' -c 'show evpn vni'")
			t.Logf("%s's DPU:\n%s", n, indent(diag))
		}
		hostDiag, _ := run(ten["A2"].host, "ip addr; ip route; ping -c 2 -W 1 10.70.2.1")
		hold(t)
		t.Fatalf("A2, now in VPC 1, does not reach A1 and B1; A2:\n%s", indent(hostDiag))
	}
	if st, _ := clients[a2.dpu].push(fnnConfig(*ten["A2"], nil)); st != "applied" {
		t.Fatalf("A2 back into VPC 2: %s", st)
	}
	waitFor(t, 2*time.Minute, "A2, back in VPC 2, to lose A1", func() bool { return !reach("A2", addr("A1")) })
	t.Logf("A2 moved into VPC 1 reached A1 and B1; moved back, it is isolated again")

	// an Ethernet Virtualizer segment: one L2 VNI across A1's and B1's DPUs, a VRR gateway on each
	etv := func(n string, own string) map[string]any {
		x := *ten[n]
		x.vlan, x.gw = 200, own
		cfg := fnnConfig(x, nil)
		cfg["bridge"] = map[string]any{"domain": map[string]any{"br_default": map[string]any{"vlan": map[string]any{"200": map[string]any{"vni": map[string]any{"6200": map[string]any{}}}}}}}
		ifs := cfg["interface"].(map[string]any)
		ifs["vlan200"].(map[string]any)["ip"].(map[string]any)["vrr"] = map[string]any{"enable": "on", "mac-address": "00:00:5e:00:01:01",
			"address": map[string]any{"10.80.0.1/24": map[string]any{}}, "state": map[string]any{"up": map[string]any{}}}
		return cfg
	}
	for n, own := range map[string]string{"A1": "10.80.0.2/24", "B1": "10.80.0.3/24"} {
		if st, out := clients[ten[n].dpu].push(etv(n, own)); st != "applied" {
			conf, _ := sub.Exec(ctx, ten[n].dpu, "sh", "-c", "cat /etc/network/interfaces")
			t.Fatalf("%s: ETV segment: %s %v\n%s", n, st, out["transition"], indent(conf))
		}
	}
	address("A1", "10.80.0.11/24", "10.80.0.1")
	address("B1", "10.80.0.13/24", "10.80.0.1")
	waitFor(t, 2*time.Minute, "A1 to reach B1 on one subnet over the L2 VNI", func() bool { return reach("A1", "10.80.0.13") })
	if !reach("A1", "10.80.0.1") {
		t.Errorf("A1 does not reach the segment's VRR gateway")
	}
	out, _ = run(ten["A1"].host, "ip neigh show 10.80.0.13")
	if !strings.Contains(strings.ToLower(out), strings.ToLower(pfOf["B1"].MAC.String())) {
		t.Errorf("A1 does not hold B1's MAC for 10.80.0.13 (L2 across the VNI): %s", out)
	}
	if reach("A2", "10.80.0.13") {
		t.Errorf("A2 (VPC 2) reaches B1 on the ETV segment")
	}
	t.Logf("ETV: A1 and B1 on 10.80.0.0/24 across two DPUs over VNI 6200, the VRR gateway on each; A2 outside")
	for _, l := range va.Report().Lines {
		if strings.Contains(l, "network:") {
			t.Logf("report vm: %s", l)
		}
	}
}
