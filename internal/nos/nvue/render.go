package nvue

import (
	"crypto/sha256"
	"fmt"
	"net"
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
			if sw.Native {
				return p.If()
			}
			return "swp" + strconv.Itoa(i+1)
		}
	}
	return ""
}

// interfaces is every port interface the switch has: its cabled ports, or every panel port of a
// NOS whose ports are all present (Native).
func interfaces(sw nos.Switch) map[string]bool {
	out := map[string]bool{}
	if sw.Native {
		for _, p := range sw.Panel {
			out[p.If()] = true
		}
		return out
	}
	for _, p := range sw.Ports() {
		out[Port(sw, p)] = true
	}
	return out
}

// panel is every front-panel port the switch has, cabled or not: a port a configuration may name
// (a real switch has all of them). An uncabled one has no link in the twin, so nothing renders
// for it.
func panel(sw nos.Switch) map[string]bool {
	out := map[string]bool{}
	for _, p := range sw.Panel {
		out[Port(sw, p.Name)] = true
	}
	return out
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
	if sw.LLDP {
		set(Tree{}, "service", "lldp")
	}
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
		for _, st := range s.Serves {
			set("ipv4-address", "vrf", "default", "router", "static", st.Prefix.String(), "via", st.Via.String(), "type")
			set("ipv4-unicast", "vrf", "default", "router", "static", st.Prefix.String(), "address-family")
			if sw.ASN != 0 {
				set("on", "vrf", "default", "router", "bgp", "address-family", "ipv4-unicast", "redistribute", "static", "enable")
			}
		}
	}
	for _, u := range sw.Unnumbered {
		swp := Port(sw, u.Port)
		if swp == "" {
			return nil, fmt.Errorf("%s: port %s is not on the front panel", sw.Path, u.Port)
		}
		set("swp", "interface", swp, "type")
		set(u.PeerName, "interface", swp, "description")
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
			if sw.EVPN {
				set("on", "vrf", "default", "router", "bgp", "neighbor", r.Peer.String(), "address-family", "l2vpn-evpn", "enable")
			}
		}
		for _, u := range sw.Unnumbered {
			n := Port(sw, u.Port)
			set("external", "vrf", "default", "router", "bgp", "neighbor", n, "remote-as")
			set("unnumbered", "vrf", "default", "router", "bgp", "neighbor", n, "type")
			set(u.PeerName, "vrf", "default", "router", "bgp", "neighbor", n, "description")
			set("on", "vrf", "default", "router", "bgp", "neighbor", n, "address-family", "l2vpn-evpn", "enable")
			// a DPU takes only a default route from the underlay (NICo's FORGE_FROM_UNDERLAY): the
			// leaf originates one to it, as a site's leaves do
			set("on", "vrf", "default", "router", "bgp", "neighbor", n, "address-family", "ipv4-unicast", "default-route-origination", "enable")
		}
		if sw.EVPN || len(sw.Unnumbered) > 0 {
			set("on", "evpn", "enable")
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
	// built in 5.14's grammar; a switch running 5.15 or later has it in its own
	ver := switchVersion(sw)
	if !ver.Less(v515) {
		t = upgrade(t)
	}
	return t, ValidateAt(ver, nil, t)
}

// switchVersion is the release a switch's NVUE speaks: its build's, else DefaultVersion.
func switchVersion(sw nos.Switch) Version {
	if sw.Build == "" {
		return DefaultVersion
	}
	return BuildVersion(sw.Build)
}

// Files is what applying a configuration writes on the switch and runs.
type Files struct {
	// Interfaces is /etc/network/interfaces, for ifupdown2.
	Interfaces string
	// FRR is /etc/frr/frr.conf, for frr-reload.py.
	FRR string
	// Relay is dhcrelay's arguments; empty: no relay.
	Relay []string
	// Recreate are devices applying deletes first, for ifupdown2 to make again (a VLAN whose
	// VNI changed: apply.go's recreate).
	Recreate []string
	// ACL is the nftables ruleset the configuration's ACLs render to (acl.go): always a
	// replacement of the twin's own tables, so an empty configuration removes them.
	ACL string
	// System is what the system and service branches render to (system.go).
	System System
}

type svi struct {
	id       int
	addrs    []string
	vrf      string
	vrrMAC   string // the VRR's MAC, the SVI's own
	vrrAddrs []string
	// l3: the SVI of a VRF's L3 VNI, which Cumulus names vlan<id>_l3 (a relay's
	// upstream-interface names it so); the twin's device is vlan<id>
	l3 bool
}

// Render takes a configuration, valid against the schema (Validate: every PATCH and Startup check
// it), to the files Cumulus Linux renders from it. It refuses what the switch cannot carry: an interface it has no port for, a VNI without an NVE source address, a
// VRF that does not exist, two VNIs on one VLAN. A configuration in 5.15's grammar is read as
// its 5.14 equivalent (normalize).
func Render(t Tree, sw nos.Switch) (Files, error) {
	t = normalize(t)
	cabled, ports := interfaces(sw), panel(sw)
	// on a BlueField's HBN, a VRF an interface is put in exists, as HBN's NVUE creates it (NICo's
	// templates name the VRF of the DPU's own function, pf0dpu1_if, only there); a switch refuses
	// a VRF that does not exist
	if sw.Native {
		t = Clone(t)
		for _, name := range keys(sub(t, "interface")) {
			if v := str(t, "interface", name, "ip", "vrf"); v != "" && v != "default" && sub(t, "vrf", v) == nil {
				Patch(t, []string{"vrf", v}, Tree{})
			}
		}
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
		if a == "dhcp" {
			return Files{}, fmt.Errorf("interface lo: a loopback has no DHCP")
		}
		w("    address %s", a)
	}
	for _, a := range keys(sub(vrfs, "default", "loopback", "ip", "address")) {
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
	// mgmtVRF: eth0 is in vrf mgmt, whose stanza renderMgmt writes
	mgmtVRF := false
	// bonds: each bond's members, which must be cabled ports in no other bond
	bonds, memberOf, err := bondsOf(ifaces, cabled)
	if err != nil {
		return Files{}, err
	}
	for _, name := range keys(ifaces) {
		if name == "lo" {
			continue
		}
		it := sub(ifaces, name)
		if name == nos.MgmtIf && sw.Mgmt {
			in, err := renderMgmt(w, it)
			if err != nil {
				return Files{}, err
			}
			mgmtVRF = in
			continue
		}
		isSVI := strings.HasPrefix(name, "vlan") && !cabled[name]
		_, isBond := bonds[name]
		if typ := str(it, "type"); typ != "" {
			switch {
			case typ == "svi" && !isSVI, typ == "swp" && !cabled[name] && !ports[name], typ == "bond" && !isBond, typ == "loopback":
				return Files{}, fmt.Errorf("interface %s: type %s: the switch has no such %s", name, typ, typ)
			case typ != "svi" && typ != "swp" && typ != "bond":
				return Files{}, fmt.Errorf("interface %s: type %s: the twin's NVUE has swp, bond, svi and loopback interfaces", name, typ)
			}
		}
		if b := memberOf[name]; b != "" && (len(keys(sub(it, "ip", "address"))) > 0 || sub(it, "bridge", "domain") != nil || str(it, "ip", "vrf") != "") {
			return Files{}, fmt.Errorf("interface %s: a member of bond %s has no address, bridge or VRF of its own", name, b)
		}
		switch {
		case cabled[name] || isBond:
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
			if isBond {
				renderBond(w, sub(it, "bond"), bonds[name])
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
			if mtu := str(it, "link", "mtu"); mtu != "" {
				w("    mtu %s", mtu)
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
		case isSVI:
			id, err := strconv.Atoi(strings.TrimPrefix(name, "vlan"))
			if err != nil || id < 1 || id > 4094 {
				return Files{}, fmt.Errorf("interface %s: an SVI is vlan<id>", name)
			}
			if v := str(it, "vlan"); v != "" && v != strconv.Itoa(id) {
				return Files{}, fmt.Errorf("interface %s: vlan %s: an SVI's VLAN is its name's", name, v)
			}
			s := &svi{id: id, addrs: keys(sub(it, "ip", "address")), vrf: str(it, "ip", "vrf")}
			if tenant(s.vrf) && sub(vrfs, s.vrf) == nil {
				return Files{}, fmt.Errorf("interface %s: vrf %s does not exist", name, s.vrf)
			}
			if vrr := sub(it, "ip", "vrr"); vrr != nil && str(vrr, "enable") != "off" {
				_, down := sub(vrr, "state")["down"]
				mac := str(vrr, "mac-address")
				if mac == "" || mac == "auto" {
					mac = vrrAutoMAC(t)
				}
				va := keys(sub(vrr, "address"))
				if len(va) > 0 && !down {
					if _, err := net.ParseMAC(mac); err != nil {
						return Files{}, fmt.Errorf("interface %s: ip vrr: mac-address %q", name, mac)
					}
					s.vrrMAC, s.vrrAddrs = mac, va
				}
			}
			sviByID[id] = s
			bridgeVLANs[id] = true
		case ports[name]:
			// an uncabled front-panel port: kept in the configuration, as the switch keeps it,
			// with no link to render
		case subportRE.MatchString(name):
			return Files{}, fmt.Errorf("interface %s: a breakout's subport: the twin records link breakout and cannot split a port, so no subport exists", name)
		default:
			return Files{}, fmt.Errorf("interface %s: the switch has no such port (its interfaces are its ports, vlan<id> SVIs and lo)", name)
		}
	}
	// a factory switch's front-panel ports are admin down until configured ("By default,
	// Cumulus Linux disables all data plane ports", vendor facts B): a cabled port the
	// configuration does not name is down. A port is named by an interface entry or as an
	// unnumbered BGP neighbour (a session that needs the port up). A switch the twin configures
	// keeps every cabled port up, as it always has.
	if sw.Factory != nil && !sw.Native {
		named := map[string]bool{}
		for m := range memberOf {
			named[m] = true
		}
		for _, v := range keys(vrfs) {
			for _, n := range keys(sub(vrfs, v, "router", "bgp", "neighbor")) {
				named[n] = true
			}
		}
		var down []string
		for p := range cabled {
			if ifaces[p] == nil && !named[p] {
				down = append(down, p)
			}
		}
		sort.Strings(down)
		for _, p := range down {
			w("auto %s", p)
			w("iface %s", p)
			w("    link-down yes")
			w("")
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
			// the VLAN the configuration gives the L3 VNI (vrf <x> evpn vlan), else the next of
			// NVUE's reserved range
			id := 0
			if want := str(vrfs, name, "evpn", "vlan"); want != "" {
				id, _ = strconv.Atoi(want)
				if id < 1 || id > 4094 || bridgeVLANs[id] {
					return Files{}, fmt.Errorf("vrf %s: evpn vlan %s: not a VLAN free for its L3 VNI", name, want)
				}
			} else {
				for bridgeVLANs[next] {
					next++
				}
				if next > 4094 {
					return Files{}, fmt.Errorf("vrf %s: L3 VNI %d: no VLAN left from %d to 4094 to carry it", name, v, l3VLANBase)
				}
				id = next
				next++
			}
			l3[name] = v
			vnis = append(vnis, vni{v, id})
			sviByID[id] = &svi{id: id, vrf: name, l3: true}
			bridgeVLANs[id] = true
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
		if !tenant(name) || (name == "mgmt" && mgmtVRF) {
			continue
		}
		w("auto %s", name)
		w("iface %s", name)
		w("    vrf-table auto")
		for _, a := range keys(sub(vrfs, name, "loopback", "ip", "address")) {
			w("    address %s", a)
		}
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
		// a fixed MAC, as Cumulus gives the bridge the switch's: the kernel takes a bridge's MAC
		// from its lowest port, so a VNI's VXLAN device made or removed would change it, and the
		// bridge's own entry in an L3 VNI's VLAN (where a remote VTEP's frames to this router MAC
		// are delivered) does not follow
		w("    hwaddress %s", bridgeMAC(sw))
		w("")
	}
	relay, err := renderRelays(t, cabled, sviByID)
	if err != nil {
		return Files{}, err
	}
	// a relay's upstream on a VRF's L3 VNI SVI (NVIDIA Config Manager's OOB-Leaf: vlan<id>_l3,
	// unnumbered in symmetric routing): ISC's dhcrelay discards a reply that arrives on an
	// interface with no IPv4 address, which Cumulus's own relay does not (NVIDIA's design relays
	// there). The twin gives such an SVI its VRF's loopback, a /32 the VRF already holds, so the
	// image's dhcrelay takes the server's reply there (UNCONFIRMED as Cumulus's mechanism; its
	// outcome, the reply relayed, is the design's).
	relayUp := map[string]bool{}
	for _, line := range relay {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "-iu" {
				relayUp[f[i+1]] = true
			}
		}
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
		if s.l3 && len(s.addrs) == 0 && relayUp[fmt.Sprintf("vlan%d", id)] {
			addrs := keys(sub(t, "vrf", s.vrf, "loopback", "ip", "address"))
			if len(addrs) == 0 {
				// a VRF with no loopback of its own: the switch's, a /32 no other interface of the
				// VRF holds
				addrs = keys(sub(t, "interface", "lo", "ip", "address"))
			}
			if len(addrs) == 0 && sw.Loopback.IsValid() {
				addrs = []string{sw.Loopback.String() + "/32"}
			}
			for _, a := range addrs {
				w("    address %s", a)
			}
		}
		w("    vlan-raw-device %s", Bridge)
		w("    vlan-id %d", id)
		if tenant(s.vrf) {
			w("    vrf %s", s.vrf)
		}
		if s.vrrMAC != "" {
			// a VRR (an anycast gateway, the same address and MAC on every DPU or leaf of the
			// segment): the SVI takes the VRR's MAC and addresses itself. Cumulus puts them on a
			// macvlan of the SVI (address-virtual), whose sysctls a container cannot write; to
			// the segment's hosts the gateway is the same.
			w("    hwaddress %s", s.vrrMAC)
			for _, a := range s.vrrAddrs {
				w("    address %s", a)
			}
		}
		w("")
	}

	frr, err := renderFRR(t, sw, l3)
	if err != nil {
		return Files{}, err
	}
	bridged := map[string]bool{}
	for _, m := range members {
		bridged[m] = true
	}
	acl, err := renderACL(t, cabled, bridged)
	if err != nil {
		return Files{}, err
	}
	system, err := renderSystem(t, sw)
	if err != nil {
		return Files{}, err
	}
	return Files{Interfaces: b.String(), FRR: frr, Relay: relay, ACL: acl, System: system}, nil
}

// renderRelays is the switch's DHCP relays, one dhcrelay per VRF that has one, each line of Relay
// "<vrf> <dhcrelay's arguments>": every downstream SVI with -id, every upstream interface with -iu,
// the servers last; `-a` appends option 82 and `-m discard` drops a request that already carries
// one, as Cumulus runs it. A VRF's relay is either the plain form (server, interface: SVIs are
// downstream, ports upstream) or server groups (NVIDIA Config Manager's: a group's servers and
// upstream interfaces, and the downstream interfaces relayed to it). A relay in a tenant VRF runs
// bound to that VRF (the switch's dctwin-vrf-exec, 18 — Z7a), as Cumulus runs dhcrelay@<vrf>.
//
// What the emulation does not carry is refused by name: two server groups in one VRF (one
// dhcrelay per VRF, as both would take its port 67), and the default VRF's relay beside another
// VRF's (an unbound socket on port 67 takes every VRF's, through udp_l3mdev_accept).
func renderRelays(t Tree, cabled map[string]bool, svis map[int]*svi) ([]string, error) {
	relays := sub(t, "service", "dhcp-relay")
	var out []string
	for _, vrf := range keys(relays) {
		args, err := renderRelay(t, vrf, sub(relays, vrf), cabled, svis)
		if err != nil {
			return nil, err
		}
		if args != nil {
			out = append(out, vrf+" "+strings.Join(args, " "))
		}
	}
	if len(out) > 1 {
		for _, l := range out {
			if strings.HasPrefix(l, "default ") {
				return nil, fmt.Errorf("service dhcp-relay default beside another VRF's relay: the emulation runs one relay on port 67 per VRF device, and the default VRF has none")
			}
		}
	}
	return out, nil
}

func renderRelay(t Tree, vrf string, r map[string]any, cabled map[string]bool, svis map[int]*svi) ([]string, error) {
	at := "service dhcp-relay " + vrf
	if vrf != "default" && sub(t, "vrf", vrf) == nil {
		return nil, fmt.Errorf("%s: vrf %s does not exist", at, vrf)
	}
	// an interface's VRF: an SVI's, a port's (ip vrf), or default
	vrfOf := func(i string) string {
		if id, ok := sviID(i); ok && svis[id] != nil {
			return orDefault(svis[id].vrf)
		}
		return orDefault(str(t, "interface", i, "ip", "vrf"))
	}
	// an interface the relay names, as the twin's device: vlan<id>_l3 is the L3 VNI's SVI
	resolve := func(i string, down bool) (string, error) {
		if id, ok := sviID(i); ok {
			s := svis[id]
			if s == nil || (strings.HasSuffix(i, "_l3") && !s.l3) {
				return "", fmt.Errorf("%s interface %s: no such SVI", at, i)
			}
			return "vlan" + strconv.Itoa(id), nil
		}
		if cabled[i] && !down {
			return i, nil
		}
		return "", fmt.Errorf("%s interface %s: no such interface", at, i)
	}
	inVRF := func(i string) error {
		if v := vrfOf(i); v != vrf {
			return fmt.Errorf("%s interface %s: in vrf %s, not %s", at, i, v, vrf)
		}
		return nil
	}
	var down, up, servers []string
	if groups := sub(r, "server-group"); len(groups) > 0 {
		if len(keys(sub(r, "server"))) > 0 || len(keys(sub(r, "interface"))) > 0 {
			return nil, fmt.Errorf("%s: server and interface beside server-group: one form per VRF", at)
		}
		if len(groups) > 1 {
			return nil, fmt.Errorf("%s: %d server groups: the emulation runs one dhcrelay per VRF, one group's", at, len(groups))
		}
		name := keys(groups)[0]
		g := sub(groups, name)
		servers = keys(sub(g, "server"))
		for _, i := range keys(sub(g, "upstream-interface")) {
			d, err := resolve(i, false)
			if err != nil {
				return nil, err
			}
			if err := inVRF(i); err != nil {
				return nil, err
			}
			up = append(up, d)
		}
		for _, i := range keys(sub(r, "downstream-interface")) {
			if gn := str(r, "downstream-interface", i, "server-group-name"); gn != name {
				return nil, fmt.Errorf("%s downstream-interface %s: server-group-name %q is not a group", at, i, gn)
			}
			if _, ok := sviID(i); !ok {
				return nil, fmt.Errorf("%s downstream-interface %s: the emulation relays from SVIs", at, i)
			}
			d, err := resolve(i, true)
			if err != nil {
				return nil, err
			}
			if err := inVRF(i); err != nil {
				return nil, err
			}
			down = append(down, d)
		}
	} else {
		if len(keys(sub(r, "downstream-interface"))) > 0 {
			return nil, fmt.Errorf("%s: downstream-interface names a server group, and there is none", at)
		}
		servers = keys(sub(r, "server"))
		for _, i := range keys(sub(r, "interface")) {
			id, isSVI := sviID(i)
			// a VRF's L3 VNI SVI is its relay's way to the server (Cumulus's EVPN relay names
			// vlan<id>_l3 in the plain form, upstream), whatever it is called
			l3 := isSVI && svis[id] != nil && svis[id].l3
			d, err := resolve(i, isSVI && !l3)
			if err != nil {
				return nil, err
			}
			if err := inVRF(i); err != nil {
				return nil, err
			}
			if isSVI && !l3 {
				down = append(down, d)
			} else {
				up = append(up, d)
			}
		}
	}
	if len(servers) == 0 {
		if len(down) > 0 {
			return nil, fmt.Errorf("%s: interfaces to relay from and no server", at)
		}
		return nil, nil
	}
	if len(down) == 0 {
		return nil, fmt.Errorf("%s: servers and no SVI to relay from", at)
	}
	return nos.DHCRelayArgs(down, up, servers), nil
}

// bondsOf is the configuration's bonds (an interface of type bond, or with a bond's settings), each
// with its members, and the bond each member is in. A member is a cabled port, in one bond.
func bondsOf(ifaces map[string]any, cabled map[string]bool) (map[string][]string, map[string]string, error) {
	bonds, memberOf := map[string][]string{}, map[string]string{}
	for _, name := range keys(ifaces) {
		it := sub(ifaces, name)
		if str(it, "type") != "bond" && sub(it, "bond") == nil {
			continue
		}
		if cabled[name] || len(name) > 15 || strings.ContainsAny(name, " /") {
			return nil, nil, fmt.Errorf("interface %s: not a name a bond can have", name)
		}
		members := keys(sub(it, "bond", "member"))
		if len(members) == 0 {
			return nil, nil, fmt.Errorf("interface %s: a bond has members (bond member <port>)", name)
		}
		for _, m := range members {
			if !cabled[m] {
				return nil, nil, fmt.Errorf("interface %s: bond member %s: the switch has no such port", name, m)
			}
			if o := memberOf[m]; o != "" {
				return nil, nil, fmt.Errorf("interface %s: bond member %s is in bond %s already", name, m, o)
			}
			memberOf[m] = name
		}
		bonds[name] = members
	}
	return bonds, memberOf, nil
}

// renderBond is a bond's ifupdown2 lines: its members, and LACP (802.3ad, Cumulus's default) or a
// static bond (balance-xor). lacp-bypass, which lets a bond forward before LACP is up, is
// Cumulus's own kernel change and is recorded only (18 — Fidelity).
func renderBond(w func(string, ...any), b map[string]any, members []string) {
	w("    bond-slaves %s", strings.Join(members, " "))
	if str(b, "mode") == "static" {
		w("    bond-mode balance-xor")
	} else {
		w("    bond-mode 802.3ad")
		rate := "1"
		if str(b, "lacp-rate") == "slow" {
			rate = "0"
		}
		w("    bond-lacp-rate %s", rate)
	}
	w("    bond-miimon 100")
	for _, k := range []struct{ nvue, ifup string }{{"down-delay", "bond-downdelay"}, {"up-delay", "bond-updelay"}} {
		if v := str(b, k.nvue); v != "" {
			w("    %s %s", k.ifup, v)
		}
	}
}

// sviID reads an SVI's VLAN from its name: vlan<id>, or vlan<id>_l3 (an L3 VNI's, as Cumulus
// names it).
func sviID(name string) (int, bool) {
	if !strings.HasPrefix(name, "vlan") {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "vlan"), "_l3"))
	return id, err == nil && id >= 1 && id <= 4094
}

func orDefault(vrf string) string {
	if vrf == "" {
		return "default"
	}
	return vrf
}

// vrrAutoMAC is VRR's MAC when the configuration gives none (mac-address auto, or unset): system
// global fabric-mac when set, else Cumulus's 00:00:5E:00:01:<fabric-id>, the VRRP range with the
// fabric id (system global fabric-id, 1 by default) as its last byte (Cumulus Linux 5.14's VRR
// page; UNCONFIRMED for an unset mac-address, which NVIDIA Config Manager's configurations leave)
func vrrAutoMAC(t Tree) string {
	if m := str(t, "system", "global", "fabric-mac"); m != "" {
		return strings.ToLower(m)
	}
	id := 1
	if f := str(t, "system", "global", "fabric-id"); f != "" {
		if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= 255 {
			id = n
		}
	}
	return fmt.Sprintf("00:00:5e:00:01:%02x", id)
}

// bridgeMAC is the switch's bridge MAC: a locally administered address from its hostname.
func bridgeMAC(sw nos.Switch) string {
	h := sha256.Sum256([]byte("dctwin-bridge/" + sw.Hostname))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3], h[4])
}

// renderMgmt renders a switch's management port, eth0 (D73), as Cumulus Linux has it: in the
// management VRF, mgmt, which Cumulus creates for it (its loopback addresses and its own table),
// with DHCP (`nv set interface eth0 ip address dhcp`) or the addresses it is given. A management
// port in the default VRF is rendered there.
func renderMgmt(w func(string, ...any), it Tree) (bool, error) {
	if typ := str(it, "type"); typ != "" && typ != "eth" {
		return false, fmt.Errorf("interface %s: type %s: the management port is type eth", nos.MgmtIf, typ)
	}
	// an eth0 whose configuration names no VRF is in mgmt, as Cumulus's NVUE has it: NVIDIA Config
	// Manager's configurations never set interface eth0 ip vrf and route the management network
	// in vrf mgmt (18, Z7a); `ip vrf default` takes it out
	vrf := str(it, "ip", "vrf")
	if vrf == "" {
		vrf = "mgmt"
	}
	switch vrf {
	case "default", "mgmt":
	default:
		return false, fmt.Errorf("interface %s: vrf %s: the management port is in vrf mgmt or the default VRF", nos.MgmtIf, vrf)
	}
	if vrf == "mgmt" {
		w("auto mgmt")
		w("iface mgmt")
		w("    address 127.0.0.1/8")
		w("    vrf-table auto")
		w("")
	}
	addrs := keys(sub(it, "ip", "address"))
	dhcp := false
	var static []string
	for _, a := range addrs {
		if a == "dhcp" {
			dhcp = true
		} else {
			static = append(static, a)
		}
	}
	w("auto %s", nos.MgmtIf)
	switch {
	case dhcp && vrf == "mgmt":
		// ifupdown2 runs a VRF slave's dhclient under `ip vrf exec mgmt`, which needs a cgroup a
		// container cannot make (/sys/fs/cgroup is read-only there): dhclient runs on eth0
		// itself, its exchange over its raw socket, which no VRF holds, its replies taken in
		// through udp_l3mdev_accept
		w("iface %s", nos.MgmtIf)
		// started once: its pid file names a live dhclient (a pgrep of the command line would
		// find this very shell, which carries it)
		w("    post-up kill -0 $(cat /run/dhclient.%[1]s.pid 2>/dev/null) 2>/dev/null || dhclient -4 -nw -pf /run/dhclient.%[1]s.pid -lf /var/lib/dhcp/dhclient.%[1]s.leases %[1]s", nos.MgmtIf)
		w("    pre-down dhclient -4 -x -pf /run/dhclient.%[1]s.pid %[1]s || true", nos.MgmtIf)
	case dhcp:
		w("iface %s inet dhcp", nos.MgmtIf)
	default:
		w("iface %s", nos.MgmtIf)
	}
	for _, a := range static {
		w("    address %s", a)
	}
	for _, gw := range keys(sub(it, "ip", "gateway")) {
		w("    gateway %s", gw)
	}
	if vrf == "mgmt" {
		w("    vrf mgmt")
	}
	if _, down := sub(it, "link", "state")["down"]; down {
		w("    link-down yes")
	}
	w("")
	return vrf == "mgmt", nil
}
