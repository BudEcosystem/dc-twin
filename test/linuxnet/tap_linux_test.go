//go:build linux

package linuxnet

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	"github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

var testSrc = schema.EventSource{Class: "scenario", Principal: "linuxnet"}

// (a) The TAP substrate on model switches: every BMC leases over real L2 (from the twin site
// DHCP, then from Kea through the twin relay), answers Redfish on its own address at :443 with
// its own MAC (the probe's ARP table and the segment bridge's forwarding database say so), and
// a pulled cable takes it off the segment until the cable is replaced.
func TestTapModelSwitches(t *testing.T) {
	need(t)
	for _, server := range []string{"twin", "kea"} {
		t.Run(server, func(t *testing.T) { runModelSwitches(t, server) })
	}
}

func runModelSwitches(t *testing.T, server string) {
	est := loadEstate(t, "tap-model.yaml", nil)
	var kea *keaLocal
	if server == "kea" {
		reg, err := compile.Registry()
		if err != nil {
			t.Fatal(err)
		}
		res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch()})
		if err != nil {
			t.Fatal(err)
		}
		kea = startKeaLocal(t, dhcp.PlanSegments(res.Model))
		n := est.Spec.Networks["oob"]
		n.DHCP, n.Server = "stack", kea.addr
		est.Spec.Networks["oob"] = n
	}
	tw, sub, a := up(t, est)
	plan := sub.Plan()
	took := waitLeased(t, a, len(plan.Endpoints), 90*time.Second)
	t.Logf("%d BMCs leased over L2 from the %s DHCP in %s", len(plan.Endpoints), server, took.Round(time.Millisecond))
	t.Logf("%-46s %-17s %-12s %-28s %s", "BMC port", "MAC (model)", "lease", "circuit id (twin relay)", "server")
	for _, ep := range plan.Endpoints {
		l := leaseOf(tw, ep.Device)
		t.Logf("%-46s %-17s %-12s %-28s %s", ep.Ref, ep.MAC, l.IP, l.Circuit, l.Server)
		if p := sub.Port(ep.Ref); p == nil || p.Address().Addr().String() != l.IP {
			t.Errorf("%s: the endpoint's NIC holds %v, the lease is %s", ep.Ref, p.Address(), l.IP)
		}
	}

	// a client on each segment: a kernel namespace cabled into the segment's bridge
	p := newProbe(t, "dctwin-probe-"+server)
	for i, b := range plan.Bridges {
		addr := fmt.Sprintf("%s/%d", hostAt(b.Segment.Prefix, 253), b.Segment.Prefix.Bits())
		p.cableToBridge(t, sub.Netns(), b.Name, fmt.Sprintf("pr%d", i), addr)
		t.Logf("probe %s on bridge %s (%s, %s)", addr, b.Name, b.Rack, b.Network)
	}
	for _, ep := range plan.Endpoints {
		l := leaseOf(tw, ep.Device)
		code, cn, err := p.redfishRoot(l.IP, 10*time.Second)
		if err != nil || code != 200 {
			t.Errorf("%s: GET https://%s/redfish/v1/ = %d %v", ep.Ref, l.IP, code, err)
			continue
		}
		if want := redfish.Hostname(ep.Dev); cn != want {
			t.Errorf("%s: https://%s answered with %q's certificate, want %q", ep.Ref, l.IP, cn, want)
		}
	}
	t.Logf("every BMC answered GET https://<its lease>/redfish/v1/ with 200 and its own certificate")

	arp := p.neigh(t)
	fdb := bridgeFDB(t, sub.Netns())
	for _, ep := range plan.Endpoints {
		l := leaseOf(tw, ep.Device)
		if arp[l.IP] != ep.MAC.String() {
			t.Errorf("%s: the probe's ARP table has %s at %q, want the model's MAC %s", ep.Ref, l.IP, arp[l.IP], ep.MAC)
		}
		if got := fdb[ep.MAC.String()]; got != ep.SwitchIf {
			t.Errorf("%s: bridge %s learned %s on %q, want its own port %s", ep.Ref, ep.Bridge.Name, ep.MAC, got, ep.SwitchIf)
		}
	}
	t.Logf("probe ARP table (ip neigh):\n%s", indent(sh("ip", "-n", p.name, "neigh", "show")))
	t.Logf("segment bridge %s forwarding database (bridge fdb, learned entries):\n%s", plan.Bridges[0].Name,
		indent(grepNot(sh("bridge", "-n", sub.Netns(), "fdb", "show", "br", plan.Bridges[0].Name), "permanent")))

	// a pulled cable takes the BMC off the segment; the others keep answering
	victim := plan.Endpoints[0]
	for _, ep := range plan.Endpoints {
		if strings.Contains(ep.Device, "/server-01") {
			victim = ep
			break
		}
	}
	vip := leaseOf(tw, victim.Device).IP
	if err := tw.Engine.SetLink(victim.Ref, false, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the pulled cable's bridge port to lose carrier", func() bool { return !carrier(t, sub.Netns(), victim.SwitchIf) })
	t.Logf("cable pulled at %s:\n%s", victim.Ref, indent(sh("ip", "-n", sub.Netns(), "link", "show", victim.SwitchIf)))
	if code, _, err := p.redfishRoot(vip, 3*time.Second); err == nil {
		t.Errorf("%s at %s still answered (%d) with its cable pulled", victim.Ref, vip, code)
	} else {
		t.Logf("with the cable pulled, https://%s/redfish/v1/: %v", vip, err)
	}
	for _, ep := range plan.Endpoints {
		if ep == victim || ep.Bridge != victim.Bridge {
			continue
		}
		ip := leaseOf(tw, ep.Device).IP
		if code, _, err := p.redfishRoot(ip, 10*time.Second); err != nil || code != 200 {
			t.Errorf("%s at %s stopped answering when another port's cable was pulled: %d %v", ep.Ref, ip, code, err)
		}
	}
	if t.Failed() {
		t.Logf("bridge ports:\n%s", indent(sh("bridge", "-n", sub.Netns(), "link", "show")))
		t.Logf("fdb:\n%s", indent(grepNot(sh("bridge", "-n", sub.Netns(), "fdb", "show", "br", victim.Bridge.Name), "permanent")))
		t.Logf("probe neigh:\n%s", indent(sh("ip", "-n", p.name, "neigh", "show")))
		for _, k := range sortedKeys(sub.NICStats()) {
			t.Logf("NIC %s rx/tx %v", k, sub.NICStats()[k])
		}
		t.FailNow()
	}
	if err := tw.Engine.SetLink(victim.Ref, true, testSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "the BMC to answer again after the cable is replaced", func() bool {
		code, _, err := p.redfishRoot(vip, 2*time.Second)
		return err == nil && code == 200
	})
	t.Logf("cable replaced: https://%s/redfish/v1/ answers again", vip)

	for _, l := range tw.Net.SubstrateIsNot() {
		t.Logf("report substrate: %s", l)
	}
	for _, l := range a.Report().Lines {
		t.Logf("report dhcp: %s", l)
	}
	if kea != nil {
		b, _ := os.ReadFile(kea.leases)
		t.Logf("Kea's lease file (%s):\n%s", kea.leases, indent(string(b)))
	}
}

func hostAt(p netip.Prefix, off int) netip.Addr {
	b := p.Addr().As4()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])+uint32(off))
	return netip.AddrFrom4(b)
}

func grepNot(s, word string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" && !strings.Contains(l, word) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// bridgeFDB is the learned entries of every bridge in a namespace: MAC -> bridge port name.
func bridgeFDB(t *testing.T, nsName string) map[string]string {
	t.Helper()
	ns, err := netns.GetFromName(nsName)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	links, _ := h.LinkList()
	names := map[int]string{}
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
	}
	neighs, err := h.NeighList(0, unix.AF_BRIDGE)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, n := range neighs {
		if n.MasterIndex != 0 && n.State&netlink.NUD_PERMANENT == 0 {
			out[n.HardwareAddr.String()] = names[n.LinkIndex]
		}
	}
	return out
}

// carrier reports whether an interface has its lower layer up (IFF_LOWER_UP): a TAP's carrier.
func carrier(t *testing.T, nsName, ifname string) bool {
	ns, err := netns.GetFromName(nsName)
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	l, err := h.LinkByName(ifname)
	if err != nil {
		t.Fatal(err)
	}
	return l.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
}

// keaLocal is ISC Kea in the test's own namespace, as the stack's DHCP behind the twin relays:
// one subnet per segment, selected by the relay's link selection.
type keaLocal struct{ addr, leases, log string }

func startKeaLocal(t *testing.T, segs []dhcp.Segment) *keaLocal {
	t.Helper()
	bin, err := exec.LookPath("kea-dhcp4")
	if err != nil {
		t.Skip("kea-dhcp4 is not on PATH")
	}
	dir := t.TempDir()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	k := &keaLocal{addr: "127.0.0.1:" + strconv.Itoa(port), leases: filepath.Join(dir, "kea-leases4.csv"), log: filepath.Join(dir, "kea.log")}
	var subnets []any
	for _, s := range segs {
		subnets = append(subnets, map[string]any{"id": s.Index + 1, "subnet": s.Prefix.String(),
			"pools":       []any{map[string]any{"pool": hostAt(s.Prefix, 10).String() + " - " + hostAt(s.Prefix, 250).String()}},
			"option-data": []any{map[string]any{"name": "routers", "data": s.Gateway.String()}}})
	}
	cfg := map[string]any{"Dhcp4": map[string]any{
		"interfaces-config": map[string]any{"interfaces": []any{"lo"}, "dhcp-socket-type": "udp"},
		"lease-database":    map[string]any{"type": "memfile", "persist": true, "name": k.leases, "lfc-interval": 0},
		"valid-lifetime":    600, "store-extended-info": true, "subnet4": subnets,
		"loggers": []any{map[string]any{"name": "kea-dhcp4", "severity": "INFO", "output-options": []any{map[string]any{"output": k.log}}}},
	}}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	cfgPath := filepath.Join(dir, "kea-dhcp4.json")
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-c", cfgPath, "-p", strconv.Itoa(port))
	cmd.Env = append(os.Environ(), "KEA_PIDFILE_DIR="+dir, "KEA_LOCKFILE_DIR="+dir)
	out, _ := os.Create(filepath.Join(dir, "kea.stdout"))
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	waitFor(t, 20*time.Second, "kea-dhcp4 to start", func() bool {
		lb, _ := os.ReadFile(k.log)
		return strings.Contains(string(lb), "DHCP4_STARTED")
	})
	t.Logf("Kea listening on %s with %d subnets, selected by the relay's link selection", k.addr, len(subnets))
	return k
}
