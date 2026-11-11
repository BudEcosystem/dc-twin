//go:build linux

package linuxnet

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap"
	"github.com/BudEcosystem/dc-twin/internal/adapter/nicoagent"
	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/inhost"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// machineID is a NICo machine id: fm100, d (DPU) or h (host), s (from serials), and 52 base32hex
// characters of 32 bytes (crates/uuid/src/machine).
func machineID(kind byte, serial string) string {
	sum := sha256.Sum256([]byte(serial))
	return "fm100" + string(kind) + "s" + strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
}

// agentTenant is what the harness, standing in for NICo, decides for one DPU: its machine ids,
// its AS and loopback (NICo's fnn-asn and lo-ip pools), and its host's interface in a VPC, an FNN
// routed /31 linknet as NICo's core carves it (the DPU the first address, the host the second).
type agentTenant struct {
	dpuTenant
	serial, dpuID, hostID, hostIfID, ifUUID string
	linknet                                 netip.Prefix
	vpcPrefixes                             []string
	version, instVersion                    string
	mac                                     string // the BaseMAC: the host PF's
	// admin: the host is released, on NICo's admin network (use_admin_network) rather than in
	// a tenant's VPC
	admin bool
}

// adminAddr is a released host's address on the admin network, its gateway the admin VRR.
const adminGW, adminPrefix = "10.72.0.1", "10.72.0.0/24"

func (x *agentTenant) hostIP() netip.Addr { return x.linknet.Addr().Next() }

// networkConfig is NICo's ManagedHostNetworkConfigResponse for the DPU: FNN, one physical tenant
// interface (crates/rpc/proto/forge.proto; the fields NICo's agent reads, crates/agent).
func (x *agentTenant) networkConfig() obj {
	if x.admin {
		return x.adminConfig()
	}
	gw := netip.PrefixFrom(x.linknet.Addr(), 31).String()
	ip := x.hostIP().String()
	return obj{
		"asn":                             x.asn,
		"datacenter_asn":                  datacenterASN,
		"dhcp_servers":                    []any{},
		"vni_device":                      "vxlan48",
		"managed_host_config":             obj{"loopback_ip": x.lo},
		"managed_host_config_version":     x.version,
		"use_admin_network":               false,
		"is_primary_dpu":                  true,
		"network_virtualization_type":     "FNN",
		"vpc_vni":                         x.vni,
		"instance_id":                     obj{"value": "7d3c1f4e-0000-4000-8000-0000000000" + x.ifUUID[len(x.ifUUID)-2:]},
		"instance_network_config_version": x.instVersion,
		"tenant_interfaces": []any{obj{
			"function_type": "PHYSICAL", "vlan_id": x.vlan, "vni": 7000 + x.vlan, "vpc_vni": x.vni,
			"gateway": gw, "ip": ip, "interface_prefix": ip + "/32", "prefix": x.linknet.String(),
			"addresses":    []any{obj{"address_family": "ADDRESS_FAMILY_V4", "gateway": gw, "ip": ip, "interface_prefix": ip + "/32", "prefix": x.linknet.String()}},
			"vpc_prefixes": x.vpcPrefixes, "fqdn": "host-" + strings.ReplaceAll(ip, ".", "-") + ".tenant.local", "mtu": 1500,
			"is_l2_segment": false, "vpc_routing_profile": obj{}, "internal_uuid": obj{"value": x.ifUUID},
		}},
		"routing_profile":             obj{},
		"route_servers":               []any{},
		"deny_prefixes":               []any{},
		"site_fabric_prefixes":        []any{},
		"vpc_isolation_behavior":      "VPC_ISOLATION_OPEN",
		"vpc_peer_vnis_authoritative": true,
		"stateful_acls_enabled":       false,
		"enable_dhcp":                 true,
		"host_interface_id":           x.hostIfID,
		"min_dpu_functioning_links":   2,
	}
}

// adminConfig is the ManagedHostNetworkConfigResponse of a released host's DPU: NICo's admin
// network, an FNN VPC of its own (VNI 5000, VLAN 100), the host's admin address on a stretched L2
// segment with a VRR gateway, as NICo's core builds it (api-core ethernet_virtualization admin).
func (x *agentTenant) adminConfig() obj {
	ip := x.adminIP()
	addr := obj{"address_family": "ADDRESS_FAMILY_V4", "gateway": adminGW + "/24", "ip": ip, "interface_prefix": ip + "/32", "prefix": adminPrefix}
	return obj{
		"asn":                             x.asn,
		"datacenter_asn":                  datacenterASN,
		"dhcp_servers":                    []any{},
		"vni_device":                      "",
		"managed_host_config":             obj{"loopback_ip": x.lo},
		"managed_host_config_version":     x.version,
		"use_admin_network":               true,
		"is_primary_dpu":                  true,
		"network_virtualization_type":     "FNN",
		"instance_network_config_version": "",
		"admin_interface": obj{
			"function_type": "PHYSICAL", "vlan_id": 100, "vni": 7100, "vpc_vni": 5000,
			"gateway": adminGW + "/24", "ip": ip, "interface_prefix": ip + "/32", "prefix": adminPrefix,
			"svi_ip": x.adminSVI() + "/24", "addresses": []any{addr}, "vpc_prefixes": []any{ip + "/32"},
			"fqdn": "host-" + strings.ReplaceAll(ip, ".", "-") + ".admin.local", "is_l2_segment": true, "vpc_routing_profile": obj{},
		},
		"tenant_interfaces":           []any{},
		"routing_profile":             obj{},
		"route_servers":               []any{},
		"deny_prefixes":               []any{},
		"site_fabric_prefixes":        []any{},
		"vpc_isolation_behavior":      "VPC_ISOLATION_OPEN",
		"vpc_peer_vnis_authoritative": true,
		"enable_dhcp":                 true,
		"host_interface_id":           x.hostIfID,
		"min_dpu_functioning_links":   2,
	}
}

// adminIP and adminSVI are the released host's admin address and its DPU's own on the segment.
func (x *agentTenant) adminIP() string  { return fmt.Sprintf("10.72.0.%d", 100+x.lo[len(x.lo)-1]-'0') }
func (x *agentTenant) adminSVI() string { return fmt.Sprintf("10.72.0.%d", 200+x.lo[len(x.lo)-1]-'0') }

// discoveryInfo is the BlueField's DiscoveryInfo as DPF's init container saves it for the agent
// (/data/hw_output.json; crates/host-support/src/hardware_enumeration.rs): an aarch64 BlueField,
// its factory MAC its BaseMAC.
func (x *agentTenant) discoveryInfo() obj {
	return obj{
		"network_interfaces": []any{}, "infiniband_interfaces": []any{}, "cpu_info": []any{}, "block_devices": []any{},
		"nvme_devices": []any{}, "gpus": []any{}, "memory_devices": []any{},
		"machine_type": "aarch64", "machine_arch": 0,
		"dmi_data": obj{"board_name": "BlueField-3 SmartNIC Main Card", "board_version": "A1", "bios_version": "4.9.0", "product_serial": x.serial,
			"board_serial": x.serial, "chassis_serial": x.serial, "bios_date": "2026-01-01", "product_name": "BlueField-3 DPU", "sys_vendor": "https://www.mellanox.com"},
		"dpu_info": obj{"part_number": "900-9D3B6-00CV-AA0", "part_description": "BlueField-3 B3220", "product_version": "32.43.1014",
			"factory_mac_address": x.mac, "firmware_version": "32.43.1014", "firmware_date": "2026-01-01", "switches": []any{}},
	}
}

// (i) NICo's own DPU agent in each emulated BlueField (D71, P3 and P4 of the DPU isolation
// design), configured by a Forge API harness that answers as NICo would for a site with two
// tenants' FNN VPCs: A1 (r01) and B1 (r02) in VPC 5001, A2 (r01) in VPC 5002. NICo's agent and
// DHCP server, built from NICo's source, run in each BlueField's container, containerized and
// configuring HBN through NVUE's REST API, as DPF runs them. Checked:
//
//   - the agent registers (DiscoverMachine with the BlueField's identity from hw_output.json, the
//     host PF's MAC as its factory MAC) and is given its machine certificate;
//   - it renders NICo's FNN template from the harness's network configuration, pushes it through
//     the emulated NVUE (committed request events by NVUE's user), and acknowledges the version
//     (RecordDpuNetworkStatus network_config_version), healthy (NvueApiRunning, no alert);
//   - the host leases its address from forge-dhcp-server on its DPU, through the host PF;
//   - hosts in one VPC reach each other across racks; A2 reaches neither;
//   - A2 re-tenanted into VPC 5001 by a new configuration version is reconfigured by its agent
//     and reaches both;
//   - A2 released onto NICo's admin network: its agent applies the admin configuration, the host
//     leases its admin address from its DPU and reaches the admin gateway, and no tenant;
//   - B2, whose OS is the stack's, reset: its firmware's PXE DHCP on the host PF is answered by
//     NICo's DHCP server on its DPU;
//   - A1's DPU with a pulled uplink reports BgpPeeringTor failing to NICo;
//   - throughout, the agent reaches NICo only through its mgmt VRF (oob_net0), as on a DPU, over
//     the connections the substrate carries for it (StartHBNAgent, as the stand-in hands off).
func TestDPUAgent(t *testing.T) {
	need(t)
	bins := os.Getenv("DCTWIN_HBN_BINARIES")
	if bins == "" {
		t.Skip("DCTWIN_HBN_BINARIES is not set: NICo's forge-dpu-agent and forge-dhcp-server (compat/nico/hbn-build.sh)")
	}
	for _, b := range []string{"forge-dpu-agent", "forge-dhcp-server"} {
		if _, err := os.Stat(filepath.Join(bins, b)); err != nil {
			t.Skipf("NICo's %s is not in %s: %v", b, bins, err)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("vm hosts need KVM: %v", err)
	}
	if _, err := qemu.Find("x86_64", "", false); err != nil {
		t.Skipf("vm hosts need QEMU for x86_64: %v", err)
	}
	if g := vm.Guests["x86_64"]; g.Present(vm.GuestDir(twin.AssetsDir(), "", g)) != nil {
		t.Skipf("the twin's x86_64 guest image %s is not in %s", g.Name, twin.AssetsDir())
	}
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	forge, err := nicoagent.LoadForge(reg)
	if err != nil {
		t.Skipf("NICo's Forge API definition: %v", err)
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
	probeName := "dctwin-probe-agent"
	newProbe(t, probeName)
	est := loadEstate(t, "tap-dpu-agent.yaml", map[string]string{"PROBE_NETNS": probeName, "HBN_BINARIES": bins})
	tw, sub := upWith(t, est, "dhcp", "redfish", "vm", "inhost")
	plan := sub.Plan()

	// NICo's side: the Forge API harness, at an address of the site's (10.99.0.1:443) that each
	// DPU reaches over its own out-of-band path: the substrate carries the agent's connections
	// there, as the stand-in's NIC would to nico-api (D71)
	h := newForgeHarness(t, forge)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := h.serve(ln)
	t.Cleanup(gs.Stop)
	const apiURL, pxeHost = "https://10.99.0.1:443", "10.99.0.1"
	toNICo := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "10.99.0.1:443" {
			return nil, fmt.Errorf("the agent dialled %s, not nico-api", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	}

	const r01, r02 = "site-1/hall-a/r01/", "site-1/hall-a/r02/"
	ten := map[string]*agentTenant{
		"A1": {dpuTenant: dpuTenant{host: r01 + "server-01", asn: 4200000001, lo: "10.71.0.1", vni: 5001}, linknet: netip.MustParsePrefix("10.70.1.0/31"), ifUUID: "4b1e0f2a-0000-4000-8000-000000000011"},
		"A2": {dpuTenant: dpuTenant{host: r01 + "server-02", asn: 4200000002, lo: "10.71.0.2", vni: 5002}, linknet: netip.MustParsePrefix("10.70.2.0/31"), ifUUID: "4b1e0f2a-0000-4000-8000-000000000012"},
		"B1": {dpuTenant: dpuTenant{host: r02 + "server-01", asn: 4200000003, lo: "10.71.0.3", vni: 5001}, linknet: netip.MustParsePrefix("10.70.3.0/31"), ifUUID: "4b1e0f2a-0000-4000-8000-000000000013"},
		"B2": {dpuTenant: dpuTenant{host: r02 + "server-02", asn: 4200000004, lo: "10.71.0.4", vni: 5001}, linknet: netip.MustParsePrefix("10.70.4.0/31"), ifUUID: "4b1e0f2a-0000-4000-8000-000000000014"},
	}
	names := []string{"A1", "A2", "B1", "B2"}
	guests := []string{"A1", "A2", "B1"} // B2's OS is the stack's: it has no in-host agent
	vpcPrefixes := map[int][]string{5001: {"10.70.1.0/24", "10.70.3.0/24", "10.70.4.0/24"}, 5002: {"10.70.2.0/24"}}
	pfOf := map[string]*tap.HostPortPlan{}
	byDPU := map[string]*agentTenant{}
	var mu sync.Mutex
	for i, n := range names {
		x := ten[n]
		x.dpu = x.host + "/bf3-0"
		x.vlan = 101 + i
		dev := tw.Engine.M.DeviceBy[x.dpu]
		if dev == nil {
			t.Fatalf("no %s in the model", x.dpu)
		}
		x.serial = dev.Identity.Serial
		x.dpuID, x.hostID = machineID('d', x.serial), machineID('h', tw.Engine.M.DeviceBy[x.host].Identity.Serial)
		x.hostIfID = fmt.Sprintf("9a8b7c6d-0000-4000-8000-0000000000%02d", 31+i)
		x.vpcPrefixes = vpcPrefixes[x.vni]
		x.version, x.instVersion = "V1-T1762732800000000", "V1-T1762732800000000"
		for _, hp := range plan.HostPorts {
			if hp.Device == x.host && hp.SwitchIf == "pf0hpf_if" {
				pfOf[n] = hp
			}
		}
		if pfOf[n] == nil {
			t.Fatalf("%s: no host PF on its DPU", n)
		}
		x.mac = pfOf[n].MAC.String()
		byDPU[x.dpuID] = x
	}
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	h.handle("DiscoverMachine", func(req obj, _ bool) (obj, error) {
		info, _ := req["info"].(map[string]any)
		serial := fmt.Sprint(nvueGet(info, "dmi_data", "product_serial"))
		for _, x := range ten {
			if x.serial == serial {
				cert, key := h.issue(x.dpuID, nil, nil, x509.ExtKeyUsageClientAuth)
				return obj{"machine_id": obj{"id": x.dpuID}, "machine_interface_id": obj{"value": x.hostIfID},
					"machine_certificate": obj{"public_key": b64(cert), "private_key": b64(key), "issuing_ca": b64(h.CAPEM)}}, nil
			}
		}
		return nil, fmt.Errorf("no DPU with serial %q", serial)
	})
	h.handle("Version", func(obj, bool) (obj, error) { return obj{"build_version": "dc-twin forge harness"}, nil })
	h.handle("GetManagedHostNetworkConfig", func(req obj, _ bool) (obj, error) {
		mu.Lock()
		defer mu.Unlock()
		x := byDPU[fmt.Sprint(nvueGet(req, "dpu_machine_id", "id"))]
		if x == nil {
			return nil, fmt.Errorf("no such DPU %v", req)
		}
		return x.networkConfig(), nil
	})
	h.handle("FindInterfaces", func(req obj, _ bool) (obj, error) {
		id := fmt.Sprint(nvueGet(req, "id", "value"))
		for _, x := range ten {
			if x.hostIfID == id {
				return obj{"interfaces": []any{obj{"id": obj{"value": id}, "machine_id": obj{"id": x.hostID}, "primary_interface": true, "mac_address": x.mac}}}, nil
			}
		}
		return obj{"interfaces": []any{}}, nil
	})
	for _, m := range []string{"RecordDpuNetworkStatus", "UpdateAgentReportedInventory", "GetNextRemediationForMachine"} {
		h.handle(m, func(obj, bool) (obj, error) { return obj{}, nil })
	}

	// each BlueField: NICo's agent and DHCP server started in its emulated HBN, as the stand-in
	// starts them once NICo's image has run (netsim.HBNAgents)
	for _, n := range names {
		x := ten[n]
		hw, _ := json.Marshal(x.discoveryInfo())
		if err := sub.StartHBNAgent(x.dpu, netsim.HBNAgent{APIURL: apiURL, PXEHost: pxeHost, RootCA: h.CAPEM, Discovery: hw, Dial: toNICo}); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		t.Logf("%s: %s runs NICo's forge-dpu-agent and forge-dhcp-server, reaching nico-api through its mgmt VRF", n, x.dpu)
	}
	agentLog := func(n string) string {
		out, _ := sub.Exec(ctx, ten[n].dpu, "sh", "-c", "tail -n 60 /var/log/forge-dpu-agent.log; echo == dhcp; tail -n 20 /var/log/forge-dhcp-server.log")
		return out
	}
	since := tw.Engine.Events(0, nil)
	var mark uint64
	if len(since) > 0 {
		mark = since[len(since)-1].Seq
	}

	// registration, then the configuration acknowledged
	statusOf := func(x *agentTenant) obj {
		var last obj
		for _, r := range h.requests("RecordDpuNetworkStatus") {
			if nvueGet(r, "dpu_machine_id", "id") == x.dpuID {
				last = r
			}
		}
		return last
	}
	acked := func(x *agentTenant, version string) bool {
		s := statusOf(x)
		return s != nil && s["network_config_version"] == version
	}
	for _, n := range names {
		x := ten[n]
		if !eventually(4*time.Minute, func() bool { return acked(x, x.version) }) {
			t.Fatalf("%s: NICo's agent has not acknowledged %s; its last status %v\nits log:\n%s", n, x.version, statusOf(x), indent(agentLog(n)))
		}
		t.Logf("%s: NICo's agent acknowledged network config %s", n, x.version)
	}
	for _, r := range h.requests("DiscoverMachine") {
		if r["discovery_reporter"] != "MACHINE_DISCOVERY_REPORTER_DPU_AGENT" {
			t.Errorf("DiscoverMachine from %v", r["discovery_reporter"])
		}
		if mac := nvueGet(r, "info", "dpu_info", "factory_mac_address"); byMAC(ten, fmt.Sprint(mac)) == nil {
			t.Errorf("DiscoverMachine with factory MAC %v, not a host PF's", mac)
		}
	}
	applies := map[string]int{}
	for _, ev := range tw.Engine.Events(mark, func(e schema.Event) bool { return e.Request != nil && e.Request.Protocol == "nvue" }) {
		if ev.Request.Canonical == "network.config.apply" && ev.Request.Status == 200 {
			applies[ev.Path]++
		}
	}
	for _, n := range names {
		x := ten[n]
		if applies[x.dpu] == 0 {
			t.Errorf("%s: no NVUE apply event from NICo's agent", n)
		}
		applied := sub.NVUE(x.dpu).Applied()
		if nvueGet(applied, "vrf", fmt.Sprintf("vpc_%d", x.vni), "evpn") == nil || nvueGet(applied, "router", "bgp", "autonomous-system") == nil {
			t.Errorf("%s: the applied NVUE configuration is not NICo's FNN for VPC %d", n, x.vni)
		}
	}
	healthy := func(x *agentTenant) (bool, string) {
		s := statusOf(x)
		var ok []string
		for _, c := range asList(nvueGet(s, "dpu_health", "successes")) {
			ok = append(ok, fmt.Sprint(nvueGet(c, "id")))
		}
		joined := strings.Join(ok, ",")
		// NICo's BGP uplink check adds an alert when it fails and nothing when it passes
		// (crates/agent/src/health/nvue.rs)
		return strings.Contains(joined, "NvueApiRunning") && len(asList(nvueGet(s, "dpu_health", "alerts"))) == 0, fmt.Sprint(nvueGet(s, "dpu_health"))
	}
	for _, n := range names {
		var why string
		if !eventually(2*time.Minute, func() bool { ok, w := healthy(ten[n]); why = w; return ok }) {
			t.Errorf("%s: NICo's agent does not report its DPU healthy: %s", n, why)
		}
	}
	t.Logf("NICo's agents report every DPU healthy: NVUE up, no BgpPeeringTor alert (both uplinks established)")

	// the hosts lease from their DPU's DHCP server, through the host PF
	ia := tw.Adapter("inhost").(*inhost.Adapter)
	waitFor(t, 5*time.Minute, "the guests' in-host agents to attach", func() bool {
		n := 0
		for _, g := range ia.Guests() {
			if g.Attached {
				n++
			}
		}
		return n == len(guests)
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
	const udhcpcScript = `cat > /tmp/udhcpc.sh <<'X'
#!/bin/sh
case $1 in bound|renew)
	ip addr flush dev $interface
	ip addr add $ip/$mask dev $interface
	[ -n "$router" ] && ip route replace 10.0.0.0/8 via ${router%% *} dev $interface onlink ;;
esac
X
chmod +x /tmp/udhcpc.sh
`
	for _, n := range guests {
		x := ten[n]
		out, _ := run(x.host, udhcpcScript+fmt.Sprintf("i=$(grep -il %s /sys/class/net/*/address | cut -d/ -f5); ip link set $i mtu 1400 up; udhcpc -i $i -n -q -t 20 -T 2 -s /tmp/udhcpc.sh 2>&1 | tail -n 3; ip -4 addr show $i", x.mac))
		if !strings.Contains(out, x.hostIP().String()+"/31") {
			t.Fatalf("%s: no lease of %s/31 from its DPU's forge-dhcp-server:\n%s\nthe DPU:\n%s", n, x.hostIP(), indent(out), indent(agentLog(n)))
		}
		t.Logf("%s: leased %s/31 from forge-dhcp-server on its DPU", n, x.hostIP())
	}
	reach := func(from, to string) bool {
		_, rc := run(ten[from].host, "ping -c 2 -W 2 "+ten[to].hostIP().String())
		return rc == 0
	}
	if !eventually(3*time.Minute, func() bool { return reach("A1", "B1") && reach("B1", "A1") }) {
		t.Fatalf("A1 and B1, both in VPC 5001, do not reach each other")
	}
	for _, c := range [][2]string{{"A2", "A1"}, {"A2", "B1"}, {"A1", "A2"}} {
		if reach(c[0], c[1]) {
			t.Errorf("%s reaches %s across VPCs", c[0], c[1])
		}
	}
	t.Logf("VPC 5001 (A1, B1) is one routed network across racks; A2 (VPC 5002) reaches neither")

	// A2 re-tenanted into VPC 5001: a new version, applied by its agent
	mu.Lock()
	a2 := ten["A2"]
	a2.vni, a2.vpcPrefixes, a2.version, a2.instVersion = 5001, vpcPrefixes[5001], "V2-T1762732900000000", "V2-T1762732900000000"
	mu.Unlock()
	if !eventually(3*time.Minute, func() bool { return acked(a2, a2.version) }) {
		t.Fatalf("A2's agent has not acknowledged %s:\n%s", a2.version, indent(agentLog("A2")))
	}
	if !eventually(3*time.Minute, func() bool { return reach("A2", "A1") && reach("A2", "B1") }) {
		t.Fatalf("A2, re-tenanted into VPC 5001, does not reach A1 and B1")
	}
	t.Logf("A2 re-tenanted into VPC 5001 by NICo's configuration: its agent applied %s and A2 reaches A1 and B1", a2.version)

	// A2 released: NICo puts its host on the admin network; its agent applies the admin
	// configuration, the host leases its admin address from its DPU, and no tenant reaches it
	mu.Lock()
	a2.admin, a2.version = true, "V3-T1762733000000000"
	mu.Unlock()
	if !eventually(3*time.Minute, func() bool { return acked(a2, a2.version) }) {
		t.Fatalf("A2's agent has not acknowledged the admin network (%s):\n%s", a2.version, indent(agentLog("A2")))
	}
	if applied := sub.NVUE(a2.dpu).Applied(); nvueGet(applied, "vrf", "vpc_5000") == nil || nvueGet(applied, "vrf", "vpc_5001") != nil {
		t.Errorf("A2's DPU's applied configuration is not the admin network's: vrfs %v", keysOf(nvueGet(applied, "vrf")))
	}
	out, _ := run(a2.host, udhcpcScript+fmt.Sprintf("i=$(grep -il %s /sys/class/net/*/address | cut -d/ -f5); udhcpc -i $i -n -q -t 20 -T 2 -s /tmp/udhcpc.sh 2>&1 | tail -n 3; ip -4 addr show $i; ping -c 2 -W 2 %s", a2.mac, adminGW))
	if !strings.Contains(out, a2.adminIP()+"/24") || !strings.Contains(out, " 0% packet loss") {
		hold(t)
		t.Fatalf("A2, released, has no admin lease of %s/24 or does not reach the admin gateway:\n%s\nits DPU:\n%s", a2.adminIP(), indent(out), indent(agentLog("A2")))
	}
	for _, other := range []string{"A1", "B1"} {
		if reach("A2", other) || reach(other, "A2") {
			t.Errorf("A2 on the admin network and %s in VPC 5001 reach each other", other)
		}
	}
	t.Logf("A2 released: its agent applied the admin network (%s), the host leased %s from its DPU and reaches the admin gateway, and no tenant", a2.version, a2.adminIP())

	// B2's firmware boots from the network through its DPU: reset, its UEFI PXE client asks DHCP
	// on the host PF, and NICo's DHCP server on the DPU answers it with the host's address and
	// NICo's boot service (its PXE service, which is NICo's and not here, then serves nothing)
	b2 := ten["B2"]
	if err := tw.Engine.Power(engine.Request{Path: b2.host, Canonical: "power.restart", Protocol: "test", Source: testSrc}); err != nil {
		t.Fatal(err)
	}
	va := tw.Adapter("vm").(*vm.Adapter)
	// UEFI tries network boot on each NIC in its order, IPv4 and IPv6 PXE and HTTP, each timing
	// out: the host's own provisioning NIC (eth0, where nothing answers) comes first
	var pf vm.NIC
	if !eventually(12*time.Minute, func() bool {
		for _, x := range va.NICs(b2.host) {
			if x.Port == pfOf["B2"].Ref {
				pf = x
			}
		}
		for c := range pf.Classes {
			if strings.HasPrefix(c, "PXEClient") && pf.Replies > 0 {
				return true
			}
		}
		return false
	}) {
		var all []string
		for _, x := range va.NICs(b2.host) {
			all = append(all, fmt.Sprintf("%s: %d requests %v, %d replies", x.Port, x.DHCPRequests, x.Classes, x.Replies))
		}
		t.Fatalf("B2's firmware's PXE DHCP on its host PF is not answered through its DPU: %v\n%s", all, indent(agentLog("B2")))
	}
	dlog, _ := sub.Exec(ctx, b2.dpu, "sh", "-c", "grep -a 'Sent DHCP reply' /var/log/forge-dhcp-server.log | tail -n 3")
	if !strings.Contains(dlog, strings.ToLower(b2.mac)) && !strings.Contains(strings.ToLower(dlog), strings.ToLower(b2.mac)) {
		t.Errorf("forge-dhcp-server on B2's DPU logged no reply to B2's NIC %s:\n%s", b2.mac, indent(dlog))
	}
	t.Logf("B2's firmware network-booted through its DPU: its UEFI PXE client's DHCP (%v) answered by NICo's DHCP server on the DPU (%d replies)", pf.Classes, pf.Replies)

	// A1's DPU with a pulled uplink: its agent reports it
	var uplink *tap.LinkPlan
	for _, lp := range plan.Links {
		if (lp.A.Device.Path == ten["A1"].dpu && lp.APort == "p0_if") || (lp.B.Device.Path == ten["A1"].dpu && lp.BPort == "p0_if") {
			uplink = lp
		}
	}
	if uplink == nil {
		t.Fatal("no cable on A1's DPU's p0_if")
	}
	if err := tw.Engine.SetLink(uplink.Link.ID, false, testSrc); err != nil {
		t.Fatal(err)
	}
	alerted := func() bool {
		for _, a := range asList(nvueGet(statusOf(ten["A1"]), "dpu_health", "alerts")) {
			if nvueGet(a, "id") == "BgpPeeringTor" {
				return true
			}
		}
		return false
	}
	if !eventually(2*time.Minute, alerted) {
		t.Errorf("A1's agent does not report BgpPeeringTor with p0 pulled: %v", nvueGet(statusOf(ten["A1"]), "dpu_health"))
	}
	if err := tw.Engine.SetLink(uplink.Link.ID, true, testSrc); err != nil {
		t.Fatal(err)
	}
	if !eventually(2*time.Minute, func() bool { ok, _ := healthy(ten["A1"]); return ok }) {
		t.Errorf("A1's agent does not report its DPU healthy again with p0 back")
	}
	t.Logf("A1's agent reported BgpPeeringTor with p0 pulled to NICo, and healthy again with it back")
}

func asList(v any) []any { l, _ := v.([]any); return l }

func keysOf(v any) []string {
	var out []string
	for k := range asMap(v) {
		out = append(out, k)
	}
	return out
}

func byMAC(ten map[string]*agentTenant, mac string) *agentTenant {
	for _, x := range ten {
		if strings.EqualFold(x.mac, mac) {
			return x
		}
	}
	return nil
}
