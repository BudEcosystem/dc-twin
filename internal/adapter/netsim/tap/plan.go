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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Plan is the substrate's wiring, a pure function of the model and the estate's options.
type Plan struct {
	Estate    string
	Endpoints []*EndpointPlan
	// HostPorts are vm hosts' data ports cabled to a real NOS (D57).
	HostPorts []*HostPortPlan
	// Mgmt are factory switches' management ports on the OOB network (D73).
	Mgmt     []*MgmtPlan
	Bridges  []*BridgePlan
	Switches []*SwitchPlan
	Links    []*LinkPlan
	Attach   []*AttachPlan
	// Skipped counts BMC ports given no endpoint, by reason.
	Skipped map[string]int
	// HostSkipped counts vm hosts' data ports not spliced onto a real NOS, by reason.
	HostSkipped map[string]int
	// NOSMemory is every NOS container's memory limit in bytes (0: none).
	NOSMemory int64
	// Day0Binaries is the directory of the twin's day-0 programs on the Docker host, mounted in
	// every factory switch (spec.adapters.network.nos.day0Binaries, else beside the twin's own
	// executable); "" when there is none.
	Day0Binaries string
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

// HostPortPlan is one data port of a vm host cabled to a real NOS's front-panel port (D57): a TAP
// whose device is the switch port, as a BMC's is, but with no NIC of the twin's behind it: the
// twin splices the guest's NIC (its QEMU stream cable, D48) onto the TAP, so the guest's frames
// are the switch's, the NOS relays its DHCP and routes its traffic.
type HostPortPlan struct {
	Index   int
	Ref     string // the host port, "device:port"
	Device  string
	MAC     net.HardwareAddr
	Network string
	Rack    string
	Port    *model.Port
	Peer    *model.Port
	Dev     *model.Device
	// TapIf is the TAP's name in the twin's namespace; SwitchIf its name in the NOS's.
	TapIf, SwitchIf string
	Switch          *SwitchPlan
	// Vlan is the switch's VLAN for the port's network and rack.
	Vlan string
}

// MgmtPlan is a real switch's management port, eth0 in its container, cabled to the OOB
// network as a BMC's port is (D73): a veth whose switch end is eth0 with the port's MAC and
// whose other end is the OOB switch's port: a port of its segment's bridge on a model switch, or
// the OOB NOS's front-panel port, an access port of its segment's VLAN. The NOS's own DHCP client
// leases on it.
type MgmtPlan struct {
	Index   int
	Ref     string // the management port, "device:port"
	MAC     net.HardwareAddr
	Network string
	Rack    string
	Port    *model.Port
	Peer    *model.Port
	Switch  *SwitchPlan
	// TwinIf is the veth end's name in the twin's namespace (a bridge port) or, on a real OOB
	// NOS, PeerIf its name there.
	TwinIf string
	Bridge *BridgePlan
	OOB    *SwitchPlan
	PeerIf string
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
	// Binds are host paths mounted in the container ("<host path>:<path>:ro").
	Binds []string
	// Emulated: the image is not built for the host's architecture and runs under the Docker
	// host's binary translation for its platform (SONiC-VS's linux/amd64 on aarch64).
	Emulated bool
	Config   nos.Switch
	// nvueToken is a factory switch's local token for `nv` (nvue.SetLocalToken), made at up
	nvueToken string
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
	// Routes are what the host routes through the port (none: a default route).
	Routes []netip.Prefix
}

// kernelProbes are the overlay devices a NOS that provides VRFs creates, and how to make one.
var kernelProbes = []struct{ feature, add string }{
	{"vrf", "ip link add dctwin-pvrf type vrf table 4093"},
	{"vxlan", "ip link add dctwin-pvx type vxlan id 16777000 dstport 4789 nolearning"},
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
	p := &Plan{Estate: est.Metadata.Name, Skipped: map[string]int{}, HostSkipped: map[string]int{}}
	mem, err := nos.ParseMemory(opts.NOS.Memory)
	if err != nil {
		return nil, fmt.Errorf("spec.adapters.network.nos.memory: %w", err)
	}
	p.NOSMemory = mem
	// the Cumulus release generic/nvue-frr's switches run: their NVUE's grammar and build
	cumulusBuild := ""
	if v := opts.NOS.CumulusVersion; v != "" {
		ver, err := nvue.ParseVersion(v)
		if err != nil {
			return nil, fmt.Errorf("spec.adapters.network.nos.cumulusVersion: %w", err)
		}
		cumulusBuild = nvue.Build(ver)
	}
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
		if e.Name == nos.NVUEFRR {
			sp.Config.Build = cumulusBuild
		}
		p.Switches = append(p.Switches, sp)
		byDev[d] = sp
	}
	// BlueFields whose HBN is emulated (D71): a NOS instance each, unconfigured until NICo's
	// agent (or a client) configures it through NVUE
	hbn := map[*SwitchPlan]bool{}
	var hbnDevs []*model.Device
	for _, d := range m.Devices {
		if d.HBN {
			hbnDevs = append(hbnDevs, d)
		}
	}
	sort.Slice(hbnDevs, func(i, j int) bool { return hbnDevs[i].Path < hbnDevs[j].Path })
	for _, d := range hbnDevs {
		e, _ := nos.Lookup(nos.HBN)
		host := Hostname(d.Parent) + "-" + d.Name
		sp := &SwitchPlan{Device: d, NOS: e, Image: e.Image,
			Container: unsafeName.ReplaceAllString("dctwin-"+est.Metadata.Name+"-"+host, "-"),
			Config:    nvue.HBNSwitch(d.Path, host)}
		sp.Config.Profile, sp.Config.MAC = d.ProfileName, switchMAC(d)
		if dir := opts.NOS.HBNBinaries; dir != "" {
			if !filepath.IsAbs(dir) {
				return nil, fmt.Errorf("spec.adapters.network.nos.hbnBinaries %q: an absolute path on the Docker host", dir)
			}
			sp.Binds = append(sp.Binds, dir+":/opt/forge/bin:ro")
		}
		p.Switches = append(p.Switches, sp)
		byDev[d] = sp
		hbn[sp] = true
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
	vlans := vlanSet{}
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
			v, err := vlans.get(sp, seg, n, outputs)
			if err != nil {
				return nil, err
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
	if err := planHostPorts(p, m, byDev, vlans, outputs); err != nil {
		return nil, err
	}
	if err := planFactory(p, m, est, byDev, hbn, segs, bridges, vlans, outputs); err != nil {
		return nil, err
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
	// a switch cabled to an emulated BlueField is a leaf, whatever else it carries
	facesDPU := map[*SwitchPlan]bool{}
	for _, l := range links {
		a, b := byDev[l.A.Device], byDev[l.B.Device]
		if l.A.Role == "management" || l.B.Role == "management" {
			continue
		}
		if a != nil && b != nil && hbn[a] != hbn[b] {
			facesDPU[a], facesDPU[b] = !hbn[a], !hbn[b]
		}
	}
	for _, sp := range p.Switches {
		if hbn[sp] {
			continue // its AS is NICo's (the fnn-asn pool), set through NVUE
		}
		sp.Config.EVPN = len(hbn) > 0
		if len(sp.Config.Vlans) > 0 || facesDPU[sp] || strings.HasPrefix(sp.Device.Name, "leaf-") {
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
		if l.A.Role == "management" || l.B.Role == "management" {
			// a switch's management port on the OOB network is not a fabric link: a factory
			// switch's is planned as its management port (D73), another's is not cabled in
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
		if hbn[a] || hbn[b] {
			// a BlueField's uplink: an unnumbered eBGP session the leaf accepts from any AS (NICo's
			// DPUs take theirs from its fnn-asn pool), carrying IPv4 and EVPN (D71)
			switch {
			case hbn[a] && hbn[b]:
				return nil, fmt.Errorf("fabric link %s: two BlueFields cabled to each other", l.ID)
			case hbn[a]:
				a.Config.Cabled = append(a.Config.Cabled, l.A.Name)
				b.Config.Unnumbered = append(b.Config.Unnumbered, nos.Unnumbered{Port: l.B.Name, PeerName: a.Config.Hostname + ":" + lp.APort})
			default:
				b.Config.Cabled = append(b.Config.Cabled, l.B.Name)
				a.Config.Unnumbered = append(a.Config.Unnumbered, nos.Unnumbered{Port: l.A.Name, PeerName: b.Config.Hostname + ":" + lp.BPort})
			}
			continue
		}
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
		ap := &AttachPlan{Container: at.Container, Netns: at.Netns, Interface: ifname, Switch: sp, Port: portName, SwitchIf: swIf, Address: addr, Gateway: gw}
		svc := nos.Service{Port: portName, Gateway: netip.PrefixFrom(gw, addr.Bits())}
		for _, r := range at.Routes {
			pfx, err := netip.ParsePrefix(r)
			if err != nil || !pfx.Addr().Is4() {
				return nil, fmt.Errorf("spec.adapters.network.attach[%d].routes %q: an IPv4 prefix", i, r)
			}
			ap.Routes = append(ap.Routes, pfx.Masked())
		}
		for _, r := range at.Serves {
			pfx, err := netip.ParsePrefix(r)
			if err != nil || !pfx.Addr().Is4() {
				return nil, fmt.Errorf("spec.adapters.network.attach[%d].serves %q: an IPv4 prefix", i, r)
			}
			svc.Serves = append(svc.Serves, nos.Static{Prefix: pfx.Masked(), Via: addr.Addr()})
		}
		p.Attach = append(p.Attach, ap)
		sp.Config.Service = append(sp.Config.Service, svc)
	}
	// every real switch's Loopback0, its router id, from the top of the underlay down: where its
	// management interfaces answer in band (and a VTEP's source), reached across the fabric as a
	// connected route its BGP redistributes
	// in 64 bits: a /31 or /32 underlay has no host addresses to count down from
	b4 := ul.Addr().As4()
	first := uint64(b4[0])<<24 | uint64(b4[1])<<16 | uint64(b4[2])<<8 | uint64(b4[3])
	size := uint64(1) << (32 - ul.Bits())
	if n := uint64(len(p.Switches)); n > 0 && (size < 4 || first+size-1-n < first+uint64(2*len(p.Links))) {
		return nil, fmt.Errorf("the underlay %s holds too few addresses for %d fabric links and %d loopbacks", ul, len(p.Links), n)
	}
	top := first + size - 2 // the last host address
	i := 0
	for _, sp := range p.Switches {
		if hbn[sp] {
			continue // a DPU's loopback is NICo's (its lo-ip pool), set through NVUE
		}
		lo := v4(uint32(top - uint64(i)))
		sp.Config.Loopback, sp.Config.RouterID = lo, lo
		i++
	}
	for _, sp := range p.Switches {
		if _, err := sp.NOS.StartScript(sp.Config); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// vlanSet is each real switch's VLANs, by "network|rack".
type vlanSet map[*SwitchPlan]map[string]*nos.Vlan

// get is a switch's VLAN for a segment, made the first time: the segment's gateway, a relay to
// the stack's server when the network has DHCP, and a VLAN id the switch has not used: 1000 + the
// segment's index, or the next one free (a switch carrying two networks' segments of one rack).
func (vs vlanSet) get(sp *SwitchPlan, seg dhcp.Segment, n *model.Network, outputs map[string]string) (*nos.Vlan, error) {
	if vs[sp] == nil {
		vs[sp] = map[string]*nos.Vlan{}
	}
	key := seg.Network + "|" + seg.Rack
	if v := vs[sp][key]; v != nil {
		return v, nil
	}
	used := map[string]bool{}
	for _, v := range vs[sp] {
		used[v.Name] = true
	}
	id := 1000 + seg.Index
	for used[fmt.Sprintf("Vlan%d", id)] {
		id++
	}
	if id > 4094 {
		return nil, fmt.Errorf("switch %s: no VLAN id is free for %s's segment of %s", sp.Device.Path, seg.Network, seg.Rack)
	}
	v := &nos.Vlan{Name: fmt.Sprintf("Vlan%d", id), Network: seg.Network, Rack: seg.Rack, Gateway: netip.PrefixFrom(seg.Gateway, seg.Prefix.Bits())}
	if n.DHCP == "twin" || n.DHCP == "stack" {
		relay, err := relayTargets(n, outputs)
		if err != nil {
			return nil, fmt.Errorf("switch %s: %w", sp.Device.Path, err)
		}
		v.Relay = relay
	}
	vs[sp][key] = v
	return v, nil
}

// planHostPorts plans every vm host's data ports cabled to a real NOS (D57): each joins its
// switch's VLAN for its network's rack segment. A port on a model switch keeps the twin's
// gateway (D48) and is counted, as are ports that cannot be spliced.
func planHostPorts(p *Plan, m *model.Model, byDev map[*model.Device]*SwitchPlan, vlans vlanSet, outputs map[string]string) error {
	var devs []*model.Device
	for _, d := range m.Devices {
		if d.Fidelity == "vm" && d.IsHost() { // a real NOS's switch is vm by its NOS (D21), not a host
			devs = append(devs, d)
		}
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Path < devs[j].Path })
	segs := map[string]map[string]dhcp.Segment{} // network -> rack path -> segment
	for _, d := range devs {
		for _, port := range compile.HostPorts(d) {
			if port.Device.HBN {
				// the host's PF, spliced onto the emulated BlueField's pf0hpf_if (D71)
				sp := byDev[port.Device]
				mac, err := net.ParseMAC(port.MAC)
				if sp == nil || err != nil {
					p.HostSkipped["a vm host's PF on an emulated BlueField has no NOS or no MAC"]++
					continue
				}
				r := d.RackOf()
				hp := &HostPortPlan{Index: len(p.HostPorts), Ref: port.Ref, Device: d.Path, MAC: mac, Port: port, Dev: d, Switch: sp,
					SwitchIf: sp.NOS.Interface(sp.Config, port.Name)}
				sp.Config.Cabled = append(sp.Config.Cabled, port.Name)
				if r != nil {
					hp.Rack = r.Path
				}
				hp.TapIf = fmt.Sprintf("h%05x", hp.Index)
				p.HostPorts = append(p.HostPorts, hp)
				continue
			}
			if port.Link == nil {
				p.HostSkipped["a vm host's port is not cabled"]++
				continue
			}
			peer := port.Link.Peer(port)
			sp := byDev[peer.Device]
			if sp == nil {
				if peer.Device.BaseClass == "switch" {
					p.HostSkipped["a vm host's port on a model switch (the twin's gateway serves it, D48)"]++
				} else {
					p.HostSkipped["a vm host's port is cabled to something that is not a switch"]++
				}
				continue
			}
			n := m.Networks[port.Network]
			r := port.Device.RackOf()
			if n == nil || r == nil {
				p.HostSkipped["a vm host's port on a real NOS is on no network, or in no rack"]++
				continue
			}
			if segs[n.Name] == nil {
				segs[n.Name] = map[string]dhcp.Segment{}
				for _, s := range dhcp.NetworkSegments(m, n.Name) {
					segs[n.Name][s.Rack] = s
				}
			}
			seg, ok := segs[n.Name][r.Path]
			if !ok || !seg.Gateway.IsValid() {
				p.HostSkipped["a vm host's port on a real NOS is on a network with no IPv4 segment for its rack (no cidr)"]++
				continue
			}
			mac, err := net.ParseMAC(port.MAC)
			if err != nil {
				p.HostSkipped["a vm host's port has no MAC"]++
				continue
			}
			swIf := sp.NOS.Interface(sp.Config, peer.Name)
			if swIf == "" {
				return fmt.Errorf("switch %s: %s's port %s is not a front-panel port %s can carry", sp.Device.Path, port.Ref, peer.Name, sp.NOS.Name)
			}
			v, err := vlans.get(sp, seg, n, outputs)
			if err != nil {
				return err
			}
			v.Ports = append(v.Ports, peer.Name)
			hp := &HostPortPlan{Index: len(p.HostPorts), Ref: port.Ref, Device: d.Path, MAC: mac, Network: n.Name, Rack: r.Path,
				Port: port, Peer: peer, Dev: d, Switch: sp, SwitchIf: swIf, Vlan: v.Name}
			hp.TapIf = fmt.Sprintf("h%05x", hp.Index)
			p.HostPorts = append(p.HostPorts, hp)
		}
	}
	return nil
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

// planFactory marks the switches of factory fabrics (D72) and plans their management ports onto
// the OOB network (D73).
func planFactory(p *Plan, m *model.Model, est *schema.Estate, byDev map[*model.Device]*SwitchPlan, hbn map[*SwitchPlan]bool,
	segs map[string]dhcp.Segment, bridges map[string]*BridgePlan, vlans vlanSet, outputs map[string]string) error {
	scale := est.Spec.TimeScale
	if scale <= 0 {
		scale = 1
	}
	if dir := est.Spec.Adapters.Network.NOS.Day0Binaries; dir != "" {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("spec.adapters.network.nos.day0Binaries %q: an absolute path on the Docker host", dir)
		}
		p.Day0Binaries = dir
	} else if exe, err := os.Executable(); err == nil {
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "dctwin-day0")); err == nil {
			p.Day0Binaries = filepath.Dir(exe)
		}
	}
	var images []nos.Image
	for _, im := range est.Spec.Adapters.Network.NOS.Images {
		images = append(images, nos.Image{SHA256: strings.ToLower(im.Match.SHA256), Name: im.Match.Name, NOS: im.NOS, Version: im.Version})
	}
	var names []string
	for n := range m.Fabrics {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f := m.Fabrics[name]
		if f.Factory == nil {
			continue
		}
		for _, sp := range p.Switches {
			if hbn[sp] || !f.Factory.Devices[sp.Device] {
				continue
			}
			if why := sp.NOS.FactoryCapable(); why != "" {
				return fmt.Errorf("fabric %s: switch %s cannot start in factory state: %s", f.Name, sp.Device.Path, why)
			}
			d := sp.Device
			id := nos.Identity{Serial: d.Identity.Serial, BaseMAC: sp.Config.MAC, MgmtMAC: sp.Config.MAC,
				Arch: map[string]string{"aarch64": "arm64"}[compile.HostArch()]}
			if id.Arch == "" {
				id.Arch = "x86_64"
			}
			if pr := d.Profile; pr != nil {
				id.Manufacturer, id.Model, id.PartNumber, id.Platform = pr.Spec.Identity.Manufacturer, pr.Spec.Identity.Model, pr.Spec.Identity.PartNumber, pr.Spec.Identity.Platform
			}
			if id.Platform == "" {
				// ONIE's generic form for a platform the profile does not name
				id.Platform = id.Arch + "-dctwin_" + unsafeName.ReplaceAllString(strings.ToLower(strings.TrimPrefix(d.ProfileName, "generic/")), "_") + "-r0"
			}
			sp.Config.Factory = &nos.Factory{Install: f.Factory.Install, ZTP: f.Factory.ZTP, Identity: id, Images: images,
				BootTime: time.Duration(float64(f.Factory.BootTime) * scale), InstallTime: time.Duration(float64(f.Factory.InstallTime) * scale)}
			if err := planMgmt(p, m, sp, byDev, segs, bridges, vlans, outputs); err != nil {
				return err
			}
		}
	}
	// a switch the twin configures that a factory switch's front panel is cabled to runs LLDP, as
	// Cumulus does by default: a day-0 tool validates the factory switch's cabling by its
	// neighbours (18 — Z7d: the OOB leaves' uplinks to the aggregation switch)
	for _, sp := range p.Switches {
		if sp.Config.Factory == nil {
			continue
		}
		for _, port := range sp.Device.Ports {
			if port.Link == nil || port.Role == "management" {
				continue
			}
			if peer := byDev[port.Link.Peer(port).Device]; peer != nil && peer.Config.Factory == nil {
				peer.Config.LLDP = true
			}
		}
	}
	return nil
}

// planMgmt cables a factory switch's management port (its first management-role Ethernet port,
// mgmt0) to the OOB network it is cabled to in the model.
func planMgmt(p *Plan, m *model.Model, sp *SwitchPlan, byDev map[*model.Device]*SwitchPlan,
	segs map[string]dhcp.Segment, bridges map[string]*BridgePlan, vlans vlanSet, outputs map[string]string) error {
	d := sp.Device
	var port *model.Port
	for _, x := range d.Ports {
		if x.Kind == "ethernet" && x.Role == "management" && x.Component == nil {
			port = x
			break
		}
	}
	switch {
	case port == nil:
		return fmt.Errorf("switch %s: its profile has no management port, which a factory switch's DHCP and day-0 tooling need (D73)", d.Path)
	case port.Link == nil:
		return fmt.Errorf("switch %s: its management port %s is not cabled: put the switch in a rack with a management switch (oob-flat cables it)", d.Path, port.Name)
	}
	n := m.Networks[port.Network]
	if n == nil || (n.DHCP != "twin" && n.DHCP != "stack") {
		return fmt.Errorf("switch %s: its management port %s is on no network with DHCP", d.Path, port.Ref)
	}
	r := d.RackOf()
	if r == nil {
		return fmt.Errorf("switch %s: a factory switch must be in a rack", d.Path)
	}
	seg, ok := segs[port.Network+"|"+r.Path]
	if !ok {
		return fmt.Errorf("switch %s: rack %s has no segment on network %s", d.Path, r.Name, port.Network)
	}
	mac, err := net.ParseMAC(port.MAC)
	if err != nil {
		return fmt.Errorf("switch %s: its management port has no MAC", d.Path)
	}
	peer := port.Link.Peer(port)
	mp := &MgmtPlan{Index: len(p.Mgmt), Ref: port.Ref, MAC: mac, Network: port.Network, Rack: r.Path, Port: port, Peer: peer, Switch: sp}
	mp.TwinIf = fmt.Sprintf("m%05x", mp.Index)
	if osp := byDev[peer.Device]; osp != nil {
		mp.OOB, mp.PeerIf = osp, osp.NOS.Interface(osp.Config, peer.Name)
		if mp.PeerIf == "" {
			return fmt.Errorf("switch %s: %s's port %s is not a front-panel port %s can carry", osp.Device.Path, port.Ref, peer.Name, osp.NOS.Name)
		}
		v, err := vlans.get(osp, seg, n, outputs)
		if err != nil {
			return err
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
		mp.Bridge = b
	}
	sp.Config.Mgmt = true
	p.Mgmt = append(p.Mgmt, mp)
	return nil
}
