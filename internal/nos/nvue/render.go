package nvue

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// Bridge is the one bridge domain Cumulus Linux has by default, VLAN-aware.
const Bridge = "br_default"

// l3VLANBase is the first VLAN an L3 VNI takes, as NVUE reserves VLANs for them.
const l3VLANBase = 4024

// Port is the name NVUE gives a model port of a switch: swp<n>, n its place on the front panel
// (eth0 is the management port), as Cumulus Linux names a switch's ports. "" when the port is not
// on the panel.
func Port(sw nos.Switch, model string) string {
	for i, p := range sw.Panel {
		if p.Name == model {
			return "swp" + strconv.Itoa(i+1)
		}
	}
	return ""
}

// vlanID reads a nos.Vlan's id from its name (Vlan1000).
func vlanID(v nos.Vlan) (int, error) {
	id, err := strconv.Atoi(strings.TrimPrefix(v.Name, "Vlan"))
	if err != nil || id < 1 || id > 4094 {
		return 0, fmt.Errorf("VLAN %q has no id", v.Name)
	}
	return id, nil
}

// Startup is the configuration the twin gives a switch at up, from the model, as an operator's
// `nv set` would: its hostname and Loopback0; an access port in the bridge for every port of a
// rack segment, with the segment's gateway on the segment VLAN's SVI; an address on every fabric
// and service port; BGP with a numbered eBGP neighbour per fabric link, connected routes
// redistributed; the DHCP relay to the segment's servers, upstream through the fabric ports.
func Startup(sw nos.Switch) (Tree, error) {
	t := Tree{}
	set := func(v any, path ...string) { Patch(t, path, v) }
	set(sw.Hostname, "system", "hostname")
	if sw.Loopback.IsValid() {
		set(Tree{}, "interface", "lo", "ip", "address", sw.Loopback.String()+"/32")
		set("loopback", "interface", "lo", "type")
	}
	relay := map[string]bool{}
	var servers []netip.Addr
	for _, v := range sw.Vlans {
		id, err := vlanID(v)
		if err != nil {
			return nil, err
		}
		svi := "vlan" + strconv.Itoa(id)
		set(Tree{}, "bridge", "domain", Bridge, "vlan", strconv.Itoa(id))
		for _, p := range v.Ports {
			swp := Port(sw, p)
			if swp == "" {
				return nil, fmt.Errorf("%s: port %s is not on the front panel", sw.Path, p)
			}
			set("swp", "interface", swp, "type")
			set(float64(id), "interface", swp, "bridge", "domain", Bridge, "access")
		}
		set("svi", "interface", svi, "type")
		set(Tree{}, "interface", svi, "ip", "address", v.Gateway.String())
		if len(v.Relay) > 0 {
			relay[svi] = true
			servers = append(servers, v.Relay...)
		}
	}
	for _, r := range sw.Routed {
		swp := Port(sw, r.Port)
		if swp == "" {
			return nil, fmt.Errorf("%s: port %s is not on the front panel", sw.Path, r.Port)
		}
		set("swp", "interface", swp, "type")
		set(r.PeerName, "interface", swp, "description")
		set(Tree{}, "interface", swp, "ip", "address", r.Addr.String())
		if len(servers) > 0 {
			relay[swp] = true
		}
	}
	for _, s := range sw.Service {
		swp := Port(sw, s.Port)
		if swp == "" {
			return nil, fmt.Errorf("%s: port %s is not on the front panel", sw.Path, s.Port)
		}
		set("swp", "interface", swp, "type")
		set(Tree{}, "interface", swp, "ip", "address", s.Gateway.String())
		if len(servers) > 0 {
			relay[swp] = true
		}
	}
	if sw.ASN != 0 {
		set("on", "router", "bgp", "enable")
		set(float64(sw.ASN), "router", "bgp", "autonomous-system")
		set(sw.RouterID.String(), "router", "bgp", "router-id")
		set("on", "vrf", "default", "router", "bgp", "enable")
		set("on", "vrf", "default", "router", "bgp", "address-family", "ipv4-unicast", "enable")
		set("on", "vrf", "default", "router", "bgp", "address-family", "ipv4-unicast", "redistribute", "connected", "enable")
		for _, r := range sw.Routed {
			set(float64(r.PeerASN), "vrf", "default", "router", "bgp", "neighbor", r.Peer.String(), "remote-as")
			set("numbered", "vrf", "default", "router", "bgp", "neighbor", r.Peer.String(), "type")
			set(r.PeerName, "vrf", "default", "router", "bgp", "neighbor", r.Peer.String(), "description")
		}
	}
	if len(servers) > 0 {
		for _, s := range servers {
			set(Tree{}, "service", "dhcp-relay", "default", "server", s.String())
		}
		for i := range relay {
			set(Tree{}, "service", "dhcp-relay", "default", "interface", i)
		}
	}
	return t, Validate(nil, t)
}

// Files is what applying a configuration writes on the switch and runs.
type Files struct {
	// Interfaces is /etc/network/interfaces, for ifupdown2.
	Interfaces string
	// FRR is /etc/frr/frr.conf, for frr-reload.py.
	FRR string
	// Relay is dhcrelay's arguments; empty: no relay.
	Relay []string
}

type svi struct {
	id    int
	addrs []string
	vrf   string
}

// Render takes a configuration to the files Cumulus Linux renders from it. It refuses what the
// switch cannot carry: an interface it has no port for, a VNI without an NVE source address, a
// VRF that does not exist, two VNIs on one VLAN.
func Render(t Tree, sw nos.Switch) (Files, error) {
	if err := Validate(nil, t); err != nil {
		return Files{}, err
	}
	cabled := map[string]bool{}
	for _, p := range sw.Ports() {
		cabled[Port(sw, p)] = true
	}
	vrfs := sub(t, "vrf")
	tenant := func(name string) bool { return name != "" && name != "default" }
	for _, v := range keys(vrfs) {
		if tenant(v) && (len(v) > 15 || strings.ContainsAny(v, " /")) {
			return Files{}, fmt.Errorf("vrf %q: not a usable device name", v)
		}
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# rendered by the twin's NVUE (generic/nvue-frr) from the applied revision, as Cumulus Linux's NVUE renders it")
	w("source /etc/network/interfaces.d/*")
	w("")
	ifaces := sub(t, "interface")
	lo := sub(ifaces, "lo")
	w("auto lo")
	w("iface lo inet loopback")
	for _, a := range keys(sub(lo, "ip", "address")) {
		w("    address %s", a)
	}
	w("")

	// the bridge: access ports, VLANs, VNIs
	bridgeVLANs := map[int]bool{}
	for _, k := range keys(sub(t, "bridge", "domain", Bridge, "vlan")) {
		id, _ := strconv.Atoi(k)
		bridgeVLANs[id] = true
	}
	var members []string
	sviByID := map[int]*svi{}
	for _, name := range keys(ifaces) {
		if name == "lo" {
			continue
		}
		it := sub(ifaces, name)
		switch {
		case strings.HasPrefix(name, "swp"):
			if !cabled[name] {
				return Files{}, fmt.Errorf("interface %s: the switch has no such port (its cabled ports are its interfaces)", name)
			}
			for _, d := range keys(sub(it, "bridge", "domain")) {
				if d != Bridge {
					return Files{}, fmt.Errorf("interface %s: bridge domain %s: only %s is emulated", name, d, Bridge)
				}
			}
			access := str(it, "bridge", "domain", Bridge, "access")
			addrs := keys(sub(it, "ip", "address"))
			if access != "" && len(addrs) > 0 {
				return Files{}, fmt.Errorf("interface %s: a bridge access port has no address", name)
			}
			w("auto %s", name)
			w("iface %s", name)
			if d := str(it, "description"); d != "" {
				w("    alias %s", d)
			}
			for _, a := range addrs {
				w("    address %s", a)
			}
			if vrf := str(it, "ip", "vrf"); tenant(vrf) {
				if sub(vrfs, vrf) == nil {
					return Files{}, fmt.Errorf("interface %s: vrf %s does not exist", name, vrf)
				}
				w("    vrf %s", vrf)
			}
			if access != "" {
				id, _ := strconv.Atoi(access)
				if id < 1 || id > 4094 {
					return Files{}, fmt.Errorf("interface %s: access VLAN %s", name, access)
				}
				w("    bridge-access %d", id)
				bridgeVLANs[id] = true
				members = append(members, name)
			}
			if _, down := sub(it, "link", "state")["down"]; down {
				w("    link-down yes")
			}
			w("")
		case strings.HasPrefix(name, "vlan"):
			id, err := strconv.Atoi(strings.TrimPrefix(name, "vlan"))
			if err != nil || id < 1 || id > 4094 {
				return Files{}, fmt.Errorf("interface %s: an SVI is vlan<id>", name)
			}
			s := &svi{id: id, addrs: keys(sub(it, "ip", "address")), vrf: str(it, "ip", "vrf")}
			if tenant(s.vrf) && sub(vrfs, s.vrf) == nil {
				return Files{}, fmt.Errorf("interface %s: vrf %s does not exist", name, s.vrf)
			}
			sviByID[id] = s
			bridgeVLANs[id] = true
		default:
			return Files{}, fmt.Errorf("interface %s: the twin's NVUE has swp<n> ports, vlan<id> SVIs and lo", name)
		}
	}

	// VNIs: an L2 VNI per bridge VLAN that has one, an L3 VNI per VRF that has one (on a VLAN of
	// its own from NVUE's reserved range, with an SVI in the VRF)
	src := str(t, "nve", "vxlan", "source", "address")
	type vni struct{ id, vlan int }
	var vnis []vni
	vniByVLAN := map[int]int{}
	for _, k := range keys(sub(t, "bridge", "domain", Bridge, "vlan")) {
		id, _ := strconv.Atoi(k)
		for _, n := range keys(sub(t, "bridge", "domain", Bridge, "vlan", k, "vni")) {
			v, _ := strconv.Atoi(n)
			if _, dup := vniByVLAN[id]; dup {
				return Files{}, fmt.Errorf("bridge domain %s vlan %d: one VNI per VLAN", Bridge, id)
			}
			vniByVLAN[id] = v
			vnis = append(vnis, vni{v, id})
		}
	}
	l3 := map[string]int{}
	next := l3VLANBase
	for _, name := range keys(vrfs) {
		for _, n := range keys(sub(vrfs, name, "evpn", "vni")) {
			if !tenant(name) {
				return Files{}, fmt.Errorf("vrf default: an L3 VNI is a tenant VRF's")
			}
			v, _ := strconv.Atoi(n)
			for bridgeVLANs[next] {
				next++
			}
			l3[name] = v
			vnis = append(vnis, vni{v, next})
			sviByID[next] = &svi{id: next, vrf: name}
			bridgeVLANs[next] = true
			next++
		}
	}
	seen := map[int]bool{}
	for _, v := range vnis {
		if seen[v.id] {
			return Files{}, fmt.Errorf("VNI %d is used twice", v.id)
		}
		seen[v.id] = true
	}
	if len(vnis) > 0 {
		if str(t, "nve", "vxlan", "enable") == "off" {
			return Files{}, fmt.Errorf("nve vxlan is off, and %d VNIs are configured", len(vnis))
		}
		if src == "" {
			return Files{}, fmt.Errorf("a VNI needs the NVE's source address (nve vxlan source address)")
		}
	}
	sort.Slice(vnis, func(i, j int) bool { return vnis[i].id < vnis[j].id })
	for _, v := range vnis {
		dev := "vni" + strconv.Itoa(v.id)
		w("auto %s", dev)
		w("iface %s", dev)
		w("    vxlan-id %d", v.id)
		w("    vxlan-local-tunnelip %s", src)
		w("    bridge-access %d", v.vlan)
		w("    bridge-learning off")
		if on(t, "nve", "vxlan", "arp-nd-suppress") {
			w("    bridge-arp-nd-suppress on")
		}
		w("")
		members = append(members, dev)
	}
	for _, name := range keys(vrfs) {
		if !tenant(name) {
			continue
		}
		w("auto %s", name)
		w("iface %s", name)
		w("    vrf-table auto")
		w("")
	}
	if len(members) > 0 || len(bridgeVLANs) > 0 {
		var ids []int
		for id := range bridgeVLANs {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		var vids []string
		for _, id := range ids {
			vids = append(vids, strconv.Itoa(id))
		}
		w("auto %s", Bridge)
		w("iface %s", Bridge)
		if len(members) > 0 {
			w("    bridge-ports %s", strings.Join(members, " "))
		} else {
			w("    bridge-ports none")
		}
		w("    bridge-vlan-aware yes")
		w("    bridge-vids %s", strings.Join(vids, " "))
		w("    bridge-pvid 1")
		w("")
	}
	var sviIDs []int
	for id := range sviByID {
		sviIDs = append(sviIDs, id)
	}
	sort.Ints(sviIDs)
	for _, id := range sviIDs {
		s := sviByID[id]
		w("auto vlan%d", id)
		w("iface vlan%d", id)
		for _, a := range s.addrs {
			w("    address %s", a)
		}
		w("    vlan-raw-device %s", Bridge)
		w("    vlan-id %d", id)
		if tenant(s.vrf) {
			w("    vrf %s", s.vrf)
		}
		w("")
	}

	frr, err := renderFRR(t, sw, l3)
	if err != nil {
		return Files{}, err
	}
	relay, err := renderRelay(t, cabled, sviByID)
	if err != nil {
		return Files{}, err
	}
	return Files{Interfaces: b.String(), FRR: frr, Relay: relay}, nil
}

func renderFRR(t Tree, sw nos.Switch, l3 map[string]int) (string, error) {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("frr version 8.4.4")
	w("frr defaults datacenter")
	host := str(t, "system", "hostname")
	if host == "" {
		host = sw.Hostname
	}
	w("hostname %s", host)
	w("log syslog informational")
	w("service integrated-vtysh-config")
	w("!")
	var vrfNames []string
	for name := range l3 {
		vrfNames = append(vrfNames, name)
	}
	sort.Strings(vrfNames)
	for _, name := range vrfNames {
		w("vrf %s", name)
		w(" vni %d", l3[name])
		w("exit-vrf")
		w("!")
	}
	asn := str(t, "router", "bgp", "autonomous-system")
	rid := str(t, "router", "bgp", "router-id")
	if !on(t, "router", "bgp", "enable") || asn == "" {
		for _, name := range keys(sub(t, "vrf")) {
			if on(t, "vrf", name, "router", "bgp", "enable") {
				return "", fmt.Errorf("vrf %s router bgp is on and router bgp has no autonomous-system or is off", name)
			}
		}
		return b.String(), nil
	}
	evpn := on(t, "evpn", "enable")
	for _, name := range append([]string{"default"}, without(keys(sub(t, "vrf")), "default")...) {
		bgp := sub(t, "vrf", name, "router", "bgp")
		if name != "default" && !on(bgp, "enable") {
			continue
		}
		vasn, vrid := asn, rid
		if a := str(bgp, "autonomous-system"); a != "" {
			vasn = a
		}
		if r := str(bgp, "router-id"); r != "" {
			vrid = r
		}
		if name == "default" {
			w("router bgp %s", vasn)
		} else {
			w("router bgp %s vrf %s", vasn, name)
		}
		if vrid != "" {
			w(" bgp router-id %s", vrid)
		}
		w(" no bgp ebgp-requires-policy")
		w(" bgp bestpath as-path multipath-relax")
		neighbors := sub(bgp, "neighbor")
		for _, n := range keys(neighbors) {
			ra := str(neighbors, n, "remote-as")
			if ra == "" {
				return "", fmt.Errorf("vrf %s router bgp neighbor %s: no remote-as", name, n)
			}
			w(" neighbor %s remote-as %s", n, ra)
			if d := str(neighbors, n, "description"); d != "" {
				w(" neighbor %s description %s", n, d)
			}
			w(" neighbor %s timers 3 9", n)
			w(" neighbor %s timers connect 10", n)
		}
		w(" !")
		w(" address-family ipv4 unicast")
		if on(bgp, "address-family", "ipv4-unicast", "redistribute", "connected", "enable") {
			w("  redistribute connected")
		}
		if on(bgp, "address-family", "ipv4-unicast", "redistribute", "static", "enable") {
			w("  redistribute static")
		}
		for _, n := range keys(neighbors) {
			if str(neighbors, n, "address-family", "ipv4-unicast", "enable") != "off" {
				w("  neighbor %s activate", n)
			}
		}
		w(" exit-address-family")
		switch {
		case name == "default" && evpn:
			w(" !")
			w(" address-family l2vpn evpn")
			for _, n := range keys(neighbors) {
				if on(neighbors, n, "address-family", "l2vpn-evpn", "enable") {
					w("  neighbor %s activate", n)
				}
			}
			w("  advertise-all-vni")
			w(" exit-address-family")
		case name != "default" && (on(bgp, "address-family", "ipv4-unicast", "route-export", "to-evpn", "enable") || l3[name] != 0):
			w(" !")
			w(" address-family l2vpn evpn")
			w("  advertise ipv4 unicast")
			w(" exit-address-family")
		}
		w("exit")
		w("!")
	}
	return b.String(), nil
}

func without(list []string, v string) []string {
	var out []string
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// renderRelay is dhcrelay's command line for the default relay: every SVI downstream, every
// port upstream, the servers last; `-a` appends option 82 and `-m discard` drops a request that
// already carries one, as Cumulus runs it.
func renderRelay(t Tree, cabled map[string]bool, svis map[int]*svi) ([]string, error) {
	relays := sub(t, "service", "dhcp-relay")
	for _, k := range keys(relays) {
		if k != "default" {
			return nil, fmt.Errorf("service dhcp-relay %s: only the default VRF's relay is emulated", k)
		}
	}
	r := sub(relays, "default")
	servers := keys(sub(r, "server"))
	if len(servers) == 0 {
		return nil, nil
	}
	args := []string{"-4", "-d", "-a", "-m", "discard"}
	var down, up []string
	for _, i := range keys(sub(r, "interface")) {
		switch {
		case strings.HasPrefix(i, "vlan"):
			id, _ := strconv.Atoi(strings.TrimPrefix(i, "vlan"))
			if svis[id] == nil {
				return nil, fmt.Errorf("service dhcp-relay default interface %s: no such SVI", i)
			}
			down = append(down, "-id", i)
		case cabled[i]:
			up = append(up, "-iu", i)
		default:
			return nil, fmt.Errorf("service dhcp-relay default interface %s: no such interface", i)
		}
	}
	if len(down) == 0 {
		return nil, fmt.Errorf("service dhcp-relay default: servers and no SVI to relay from")
	}
	return append(append(append(args, down...), up...), servers...), nil
}
