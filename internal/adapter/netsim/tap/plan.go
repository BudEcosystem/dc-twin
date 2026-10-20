// Package tap is the TAP substrate (D20): a link layer for the twin's endpoints, Linux only.
//
// Every BMC's management port is its own NIC in a userspace network stack inside the twin
// process (gVisor's netstack), with the model's MAC, on its own TAP. The TAP's network device is
// the switch port it is cabled to: a port of its segment's Linux bridge when the switch is a
// model switch (nos: none, D21), or the matching interface of a real NOS, moved into the NOS
// container's network namespace and named as the NOS names the port (the model's name for
// generic/frr-dhcrelay, the container's eth<n> for SONiC-VS, D44). A model switch's segment
// gets the twin relay on its bridge, at the segment's gateway; a real NOS relays its own
// segments by giaddr, to the stack's DHCP server, with the stack's host attached to one of its
// ports (spec.adapters.network.attach). A fabric link between two real switches is a cable the
// twin holds the middle of (two veths and a two-port bridge), so their routing daemons peer for
// real and a pulled cable drops carrier at both ends.
//
// Every TAP, bridge and cable middle lives in a network namespace of the twin's own
// (dctwin-<estate>), so the host carries no address on any twin network and forwards nothing
// into one (15). Link state follows the model: a pulled cable, a port forced down by a fault or a
// BMC without power turns the TAP's carrier off, so the switch port (bridge or NOS) sees its
// link go down with its admin state untouched.
//
// This file plans the wiring from the model and is portable; the wiring itself is
// substrate_linux.go.
package tap

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Plan is the substrate's wiring, a pure function of the model and the estate's options.
type Plan struct {
	Estate    string
	Endpoints []*EndpointPlan
	Bridges   []*BridgePlan
	Switches  []*SwitchPlan
	Links     []*LinkPlan
	Attach    []*AttachPlan
	// Skipped counts BMC ports given no endpoint, by reason.
	Skipped map[string]int
}

// EndpointPlan is one BMC management port's endpoint.
type EndpointPlan struct {
	Index   int
	Ref     string // the BMC port, "device:port"
	Device  string
	MAC     net.HardwareAddr
	Network string
	Rack    string // rack path
	Peer    *model.Port
	Port    *model.Port
	Dev     *model.Device
	// TapIf is the TAP's name when the twin creates it in its namespace; SwitchIf the name it
	// has as the switch port: the same, as a bridge port, or the model port's name inside a real
	// NOS.
	TapIf, SwitchIf string
	Bridge          *BridgePlan // model switch
	Switch          *SwitchPlan // real NOS
}

// BridgePlan is a model switch segment: one Linux bridge per rack segment per network.
type BridgePlan struct {
	Index   int
	Name    string
	Network string
	Rack    string
	Segment dhcp.Segment
}

// SwitchPlan is a real NOS instance.
type SwitchPlan struct {
	Device    *model.Device
	NOS       nos.Entry
	Image     string
	Container string
	// Emulated: the image is not built for the host's architecture and runs under the Docker
	// host's binary translation for its platform (SONiC-VS's linux/amd64 on aarch64).
	Emulated bool
	Config   nos.Switch
}

// LinkPlan is a fabric link between two real switches. APort and BPort are the cable's ends as
// each switch's container names them (the model's port names for generic/frr-dhcrelay, eth<n>
// for SONiC-VS).
type LinkPlan struct {
	Link         *model.Link
	A, B         *SwitchPlan
	APort, BPort string
}

// AttachPlan cables a host outside the twin (a container) to a real switch's port.
type AttachPlan struct {
	Container string
	Netns     string
	Interface string
	Switch    *SwitchPlan
	Port      string // the model's port name
	SwitchIf  string // the port as the switch's container names it
	Address   netip.Prefix
	Gateway   netip.Addr
}

const defaultUnderlay = "10.255.0.0/16"

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// Hostname is a switch's hostname: "<rack>-<name>" for a switch in a rack, as the twin relay
// names a model switch, else its name.
func Hostname(d *model.Device) string {
	if r := d.RackOf(); r != nil {
		return r.Name + "-" + d.Name
	}
	return d.Name
}

// BuildPlan derives the wiring. hostArch is the host's architecture in the spec's spelling.
func BuildPlan(m *model.Model, est *schema.Estate, outputs map[string]string, hostArch string) (*Plan, error) {
	opts := est.Spec.Adapters.Network
	if rt := opts.NOS.Runtime; rt != "" && rt != "docker" {
		return nil, fmt.Errorf("spec.adapters.network.nos.runtime %q: the runtime is docker (the Docker Engine's socket)", rt)
	}
	p := &Plan{Estate: est.Metadata.Name, Skipped: map[string]int{}}
	segs := map[string]dhcp.Segment{}
	for _, s := range dhcp.PlanSegments(m) {
		segs[s.Network+"|"+s.Rack] = s
	}
	nosSwitches := twin.NOSSwitches(m)

	// the real switches first, in path order: their names, images and containers
	var swDevs []*model.Device
	for d := range nosSwitches {
		swDevs = append(swDevs, d)
	}
	sort.Slice(swDevs, func(i, j int) bool { return swDevs[i].Path < swDevs[j].Path })
	byDev := map[*model.Device]*SwitchPlan{}
	for _, d := range swDevs {
		name := nosSwitches[d]
		e, ok := nos.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("switch %s: nos %q is not in the registry (%s)", d.Path, name, strings.Join(nos.Names(), ", "))
		}
		if !e.Runnable {
			return nil, fmt.Errorf("switch %s: nos %s cannot run here: %s; %s runs on any architecture", d.Path, name, e.Why, nos.FRRDHCRelay)
		}
		emulated := false
		if !e.RunsOn(hostArch) {
			if !e.RunsEmulated(hostArch) {
				return nil, fmt.Errorf("switch %s: nos %s's image is not built for %s", d.Path, name, hostArch)
			}
			emulated = true // checked against the Docker host at up (nos.Docker.CanRun)
		}
		image := e.Image
		if opts.NOS.Image != "" {
			image = opts.NOS.Image
		}
		host := Hostname(d)
		sp := &SwitchPlan{Device: d, NOS: e, Image: image, Emulated: emulated,
			Container: unsafeName.ReplaceAllString("dctwin-"+est.Metadata.Name+"-"+host, "-"),
			Config:    nos.Switch{Path: d.Path, Hostname: host, Profile: d.ProfileName, MAC: switchMAC(d), Panel: frontPanel(d)}}
		p.Switches = append(p.Switches, sp)
		byDev[d] = sp
	}

	// every BMC's management port on a DHCP network, cabled to a switch
	var devs []*model.Device
	for _, d := range m.Devices {
		if d.HasBMC() {
			devs = append(devs, d)
		}
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Path < devs[j].Path })
	bridges := map[string]*BridgePlan{}
	vlans := map[*SwitchPlan]map[string]*nos.Vlan{}
	for _, d := range devs {
		port := compile.BMCPort(d)
		switch {
		case port == nil:
			p.Skipped["the profile has a BMC but no management port"]++
			continue
		case port.Link == nil:
			p.Skipped["the BMC port is not cabled"]++
			continue
		}
		n := m.Networks[port.Network]
		if n == nil || (n.DHCP != "twin" && n.DHCP != "stack") {
			p.Skipped["the BMC port is on no network with DHCP"]++
			continue
		}
		peer := port.Link.Peer(port)
		if peer.Device.BaseClass != "switch" {
			p.Skipped["the BMC port is cabled to something that is not a switch"]++
			continue
		}
		r := d.RackOf()
		if r == nil {
			p.Skipped["the device is in no rack"]++
			continue
		}
		seg, ok := segs[port.Network+"|"+r.Path]
		if !ok {
			p.Skipped["its rack has no segment on the network (the CIDR holds too few /24s)"]++
			continue
		}
		mac, err := net.ParseMAC(port.MAC)
		if err != nil {
			p.Skipped["the port has no MAC"]++
			continue
		}
		ep := &EndpointPlan{Index: len(p.Endpoints), Ref: port.Ref, Device: d.Path, MAC: mac, Network: port.Network, Rack: r.Path, Peer: peer, Port: port, Dev: d}
		ep.TapIf = fmt.Sprintf("p%05x", ep.Index)
		if sp := byDev[peer.Device]; sp != nil {
			if !seg.Gateway.IsValid() {
				return nil, fmt.Errorf("network %s has no IPv4 cidr: a real NOS (%s) needs the segment's gateway to relay %s", n.Name, sp.NOS.Name, port.Ref)
			}
			ep.Switch, ep.SwitchIf = sp, sp.NOS.Interface(sp.Config, peer.Name)
			if ep.SwitchIf == "" {
				return nil, fmt.Errorf("switch %s: %s's port %s is not a front-panel port %s can carry", sp.Device.Path, port.Ref, peer.Name, sp.NOS.Name)
			}
			if vlans[sp] == nil {
				vlans[sp] = map[string]*nos.Vlan{}
			}
			key := seg.Network + "|" + seg.Rack
			v := vlans[sp][key]
			if v == nil {
				v = &nos.Vlan{Name: fmt.Sprintf("Vlan%d", 1000+seg.Index), Network: seg.Network, Rack: seg.Rack,
					Gateway: netip.PrefixFrom(seg.Gateway, seg.Prefix.Bits())}
				relay, err := relayTargets(n, outputs)
				if err != nil {
					return nil, fmt.Errorf("switch %s: %w", sp.Device.Path, err)
				}
				v.Relay = relay
				vlans[sp][key] = v
			}
			v.Ports = append(v.Ports, peer.Name)
		} else {
			key := seg.Network + "|" + seg.Rack
			b := bridges[key]
			if b == nil {
				b = &BridgePlan{Index: len(p.Bridges), Network: seg.Network, Rack: seg.Rack, Segment: seg}
				b.Name = fmt.Sprintf("b%04x", b.Index)
				bridges[key] = b
				p.Bridges = append(p.Bridges, b)
			}
			ep.Bridge, ep.SwitchIf = b, ep.TapIf
		}
		p.Endpoints = append(p.Endpoints, ep)
	}
	for _, sp := range p.Switches {
		var keys []string
		for k := range vlans[sp] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := vlans[sp][k]
			sort.Strings(v.Ports)
			sp.Config.Vlans = append(sp.Config.Vlans, *v)
		}
	}

	// fabric links between real switches: a /31 each from the underlay, eBGP across it
	underlay := opts.NOS.Underlay
	if underlay == "" {
		underlay = defaultUnderlay
	}
	ul, err := netip.ParsePrefix(underlay)
	if err != nil || !ul.Addr().Is4() {
		return nil, fmt.Errorf("spec.adapters.network.nos.underlay %q is not an IPv4 prefix", underlay)
	}
	ul = ul.Masked()
	links := append([]*model.Link{}, m.Links...)
	sort.Slice(links, func(i, j int) bool { return links[i].ID < links[j].ID })
	asn := map[*SwitchPlan]uint32{}
	leaf := uint32(0)
	for _, sp := range p.Switches {
		if len(sp.Config.Vlans) > 0 {
			asn[sp] = 65101 + leaf // a leaf: its own AS (RFC 7938)
			leaf++
		} else {
			asn[sp] = 65000 // the aggregation or spine tier shares one
		}
		sp.Config.ASN = asn[sp]
	}
	for _, l := range links {
		a, b := byDev[l.A.Device], byDev[l.B.Device]
		if a == nil || b == nil {
			continue
		}
		i := len(p.Links)
		base := ul.Addr().As4()
		off := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
		off += uint32(2 * i)
		if !ul.Contains(v4(off + 1)) {
			return nil, fmt.Errorf("the underlay %s holds too few /31s for %d fabric links", ul, i+1)
		}
		lp := &LinkPlan{Link: l, A: a, B: b, APort: a.NOS.Interface(a.Config, l.A.Name), BPort: b.NOS.Interface(b.Config, l.B.Name)}
		if lp.APort == "" || lp.BPort == "" {
			return nil, fmt.Errorf("fabric link %s: %s or %s is not a front-panel port its NOS can carry", l.ID, l.A.Ref, l.B.Ref)
		}
		p.Links = append(p.Links, lp)
		a.Config.Routed = append(a.Config.Routed, nos.Routed{Port: l.A.Name, Addr: netip.PrefixFrom(v4(off), 31), Peer: v4(off + 1),
			PeerASN: asn[b], PeerName: b.Config.Hostname + ":" + l.B.Name})
		b.Config.Routed = append(b.Config.Routed, nos.Routed{Port: l.B.Name, Addr: netip.PrefixFrom(v4(off+1), 31), Peer: v4(off),
			PeerASN: asn[a], PeerName: a.Config.Hostname + ":" + l.A.Name})
	}

	// hosts outside the twin, cabled to a real switch's port
	for i, at := range opts.Attach {
		dev, portName, ok := strings.Cut(at.Port, ":")
		if !ok {
			return nil, fmt.Errorf("spec.adapters.network.attach[%d].port %q: a port ref, <switch path>:<port>", i, at.Port)
		}
		d := m.DeviceBy[dev]
		if d == nil {
			for _, x := range m.Devices { // a suffix, as the CLI takes paths
				if strings.HasSuffix(x.Path, "/"+dev) {
					d = x
				}
			}
		}
		sp := byDev[d]
		if d == nil || sp == nil {
			return nil, fmt.Errorf("spec.adapters.network.attach[%d]: %s is not a switch a real NOS serves", i, dev)
		}
		mp := d.PortBy[portName]
		if mp == nil {
			return nil, fmt.Errorf("spec.adapters.network.attach[%d]: %s has no port %s", i, d.Path, portName)
		}
		if mp.Link != nil {
			return nil, fmt.Errorf("spec.adapters.network.attach[%d]: %s is cabled already (%s)", i, mp.Ref, mp.Link.ID)
		}
		addr, err := netip.ParsePrefix(at.Address)
		if err != nil || !addr.Addr().Is4() {
			return nil, fmt.Errorf("spec.adapters.network.attach[%d].address %q: an IPv4 address with its prefix", i, at.Address)
		}
		gw, err := netip.ParseAddr(at.Gateway)
		if err != nil || !addr.Masked().Contains(gw) {
			return nil, fmt.Errorf("spec.adapters.network.attach[%d].gateway %q: an address in %s", i, at.Gateway, addr.Masked())
		}
		ifname := at.Interface
		if ifname == "" {
			ifname = "svc0"
		}
		if (at.Container == "") == (at.Netns == "") {
			return nil, fmt.Errorf("spec.adapters.network.attach[%d]: name a container or a netns", i)
		}
		swIf := sp.NOS.Interface(sp.Config, portName)
		if swIf == "" {
			return nil, fmt.Errorf("spec.adapters.network.attach[%d]: %s is not a front-panel port %s can carry", i, mp.Ref, sp.NOS.Name)
		}
		p.Attach = append(p.Attach, &AttachPlan{Container: at.Container, Netns: at.Netns, Interface: ifname, Switch: sp, Port: portName, SwitchIf: swIf, Address: addr, Gateway: gw})
		sp.Config.Service = append(sp.Config.Service, nos.Service{Port: portName, Gateway: netip.PrefixFrom(gw, addr.Bits())})
	}
	for _, sp := range p.Switches {
		c := &sp.Config
		switch {
		case len(c.Routed) > 0:
			c.RouterID = c.Routed[0].Addr.Addr()
		case len(c.Vlans) > 0:
			c.RouterID = c.Vlans[0].Gateway.Addr()
		case len(c.Service) > 0:
			c.RouterID = c.Service[0].Gateway.Addr()
		}
		if _, err := sp.NOS.StartScript(*c); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// switchMAC is a switch's own MAC: its management port's, which a NOS takes as its system MAC
// (SONiC's DEVICE_METADATA mac, the router MAC of its VLAN interfaces).
func switchMAC(d *model.Device) string {
	for _, p := range d.Ports {
		if p.Kind == "ethernet" && p.Role == "management" && p.Component == nil && p.MAC != "" {
			return p.MAC
		}
	}
	return ""
}

// frontPanel is a switch's front panel: its Ethernet ports that are not management ports, in the
// profile's order, cabled or not.
func frontPanel(d *model.Device) []nos.PanelPort {
	var out []nos.PanelPort
	for _, p := range d.Ports {
		if p.Kind == "ethernet" && p.Role != "management" && p.Component == nil {
			out = append(out, nos.PanelPort{Name: p.Name, Speed: p.Speed})
		}
	}
	return out
}

func v4(u uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)})
}

// relayTargets is where a real NOS relays a network's DHCP: the stack's server, by address (a
// relay forwards to port 67).
func relayTargets(n *model.Network, outputs map[string]string) ([]netip.Addr, error) {
	if n.DHCP == "twin" {
		return nil, fmt.Errorf("network %s is dhcp: twin, which a real NOS cannot relay to: the twin site DHCP is on no twin network; use dhcp: stack with the server attached (spec.adapters.network.attach)", n.Name)
	}
	addr := n.Server
	if addr == "" {
		addr = outputs["dhcpServer"]
	}
	if addr == "" {
		return nil, nil // nothing to relay to: the VLAN has no relay, and the dhcp adapter reports it
	}
	host := addr
	if h, port, err := net.SplitHostPort(addr); err == nil {
		if port != "67" {
			return nil, fmt.Errorf("network %s: server %s: a relay forwards to port 67", n.Name, addr)
		}
		host = h
	}
	a, err := netip.ParseAddr(host)
	if err != nil || !a.Is4() {
		return nil, fmt.Errorf("network %s: server %q is not an IPv4 address a relay can forward to", n.Name, addr)
	}
	return []netip.Addr{a}, nil
}
