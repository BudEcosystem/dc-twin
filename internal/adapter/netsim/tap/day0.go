package tap

import (
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/day0"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// Factory reports whether any switch of the plan starts in factory state (D72).
func (p *Plan) Factory() bool {
	for _, sp := range p.Switches {
		if sp.Config.Factory != nil {
			return true
		}
	}
	return false
}

// Day0Design is the plan as the design a day-0 tool is handed (D77): every real switch but an
// emulated BlueField (whose HBN is NICo's agent's), its identity and, with plan: given, the
// addresses, sessions and segments the twin would have configured itself, which the day-0
// oracle compares the switches with.
func (p *Plan) Day0Design(m *model.Model) day0.Design {
	d := day0.Design{Estate: p.Estate, Plan: "given"}
	for _, sp := range p.Switches {
		if b := sp.Config.Build; b != "" {
			d.Release = nvue.BuildVersion(b).String()
			break
		}
	}
	for _, f := range m.Fabrics {
		if f.Factory != nil && f.Factory.Plan == "tool" {
			d.Plan = "tool"
		}
	}
	byHost := map[string]*SwitchPlan{}
	fabricOf := map[*model.Device]string{}
	for _, f := range m.Fabrics {
		for _, dev := range f.Devices {
			fabricOf[dev] = f.Name
		}
	}
	for _, l := range m.Links {
		for _, pt := range []*model.Port{l.A, l.B} {
			if pt.Role != "management" && fabricOf[pt.Device] == "" {
				fabricOf[pt.Device] = l.Fabric
			}
		}
	}
	for _, sp := range p.Switches {
		if sp.Config.Native {
			continue
		}
		byHost[sp.Config.Hostname] = sp
		c := sp.Config
		dev := sp.Device
		s := day0.Switch{Path: dev.Path, Hostname: c.Hostname, Role: role(dev), Fabric: fabricOf[dev], Profile: dev.ProfileName,
			Serial: dev.Identity.Serial, BaseMAC: c.MAC, MgmtMAC: c.MAC, NOS: sp.NOS.Name, Factory: c.Factory != nil,
			ASN: c.ASN, EVPN: c.EVPN}
		if r := dev.RackOf(); r != nil {
			s.Rack = r.Name
		}
		s.U = dev.U
		if pr := dev.Profile; pr != nil {
			s.Manufacturer, s.Model, s.PartNumber, s.Platform = pr.Spec.Identity.Manufacturer, pr.Spec.Identity.Model, pr.Spec.Identity.PartNumber, pr.Spec.Identity.Platform
		}
		if c.Factory != nil {
			s.Platform = c.Factory.Identity.Platform
		}
		if c.RouterID.IsValid() {
			s.RouterID = c.RouterID.String()
		}
		if c.Loopback.IsValid() {
			s.Loopback = c.Loopback.String() + "/32"
		}
		speed := map[string]string{}
		for _, pp := range c.Panel {
			speed[pp.Name] = pp.Speed
		}
		ifn := func(port string) string { return osName(sp, port) }
		for _, r := range c.Routed {
			s.Interfaces = append(s.Interfaces, day0.Interface{Name: ifn(r.Port), Model: r.Port, Speed: speed[r.Port], Kind: "fabric",
				Address: r.Addr.String(), PeerAddr: r.Peer.String(), PeerASN: r.PeerASN, Peer: r.PeerName})
		}
		for _, u := range c.Unnumbered {
			s.Interfaces = append(s.Interfaces, day0.Interface{Name: ifn(u.Port), Model: u.Port, Speed: speed[u.Port], Kind: "unnumbered", Peer: u.PeerName})
		}
		for _, sv := range c.Service {
			s.Interfaces = append(s.Interfaces, day0.Interface{Name: ifn(sv.Port), Model: sv.Port, Speed: speed[sv.Port], Kind: "service", Address: sv.Gateway.String()})
		}
		for _, v := range c.Vlans {
			id, _ := strconv.Atoi(strings.TrimPrefix(v.Name, "Vlan"))
			dv := day0.Vlan{Name: v.Name, ID: id, Network: v.Network, Gateway: v.Gateway.String()}
			for _, r := range v.Relay {
				dv.Relay = append(dv.Relay, r.String())
			}
			for _, port := range v.Ports {
				dv.Ports = append(dv.Ports, ifn(port))
				s.Interfaces = append(s.Interfaces, day0.Interface{Name: ifn(port), Model: port, Speed: speed[port], Kind: "access", Vlan: v.Name})
			}
			s.Vlans = append(s.Vlans, dv)
		}
		sort.Slice(s.Interfaces, func(i, j int) bool { return s.Interfaces[i].Name < s.Interfaces[j].Name })
		d.Switches = append(d.Switches, s)
	}
	// the peers' interface names, now every switch is known
	for i := range d.Switches {
		for j, it := range d.Switches[i].Interfaces {
			host, port, ok := strings.Cut(it.Peer, ":")
			if sp := byHost[host]; ok && sp != nil {
				if n := osName(sp, port); n != "" {
					d.Switches[i].Interfaces[j].Peer = host + ":" + n
				}
			}
		}
	}
	// the cables between real switches, and each management cable
	real := map[*model.Device]*SwitchPlan{}
	for _, sp := range p.Switches {
		if !sp.Config.Native {
			real[sp.Device] = sp
		}
	}
	name := func(dev *model.Device) string {
		if sp := real[dev]; sp != nil {
			return sp.Config.Hostname
		}
		return Hostname(dev)
	}
	ifName := func(pt *model.Port) string {
		if sp := real[pt.Device]; sp != nil {
			if pt.Role == "management" {
				return nos.MgmtIf
			}
			return osName(sp, pt.Name)
		}
		return ""
	}
	for _, l := range m.Links {
		if real[l.A.Device] == nil && real[l.B.Device] == nil {
			continue
		}
		d.Cables = append(d.Cables, day0.Cable{ID: l.ID, Fabric: l.Fabric, ADevice: name(l.A.Device), APort: l.A.Name, AIf: ifName(l.A),
			BDevice: name(l.B.Device), BPort: l.B.Name, BIf: ifName(l.B), Speed: l.A.Speed})
	}
	sort.Slice(d.Cables, func(i, j int) bool { return d.Cables[i].ID < d.Cables[j].ID })
	seen := map[string]bool{}
	for _, mp := range p.Mgmt {
		var seg *BridgePlan
		if mp.Bridge != nil {
			seg = mp.Bridge
		}
		key := mp.Network + "|" + mp.Rack
		if seen[key] {
			continue
		}
		seen[key] = true
		mn := day0.MgmtNet{Network: mp.Network, Rack: mp.Rack}
		if seg != nil {
			mn.Prefix, mn.Gateway = seg.Segment.Prefix.String(), seg.Segment.Gateway.String()
		} else if mp.OOB != nil {
			for _, v := range mp.OOB.Config.Vlans {
				if v.Network == mp.Network && v.Rack == mp.Rack {
					mn.Prefix, mn.Gateway = v.Gateway.Masked().String(), v.Gateway.Addr().String()
				}
			}
		}
		d.Mgmt = append(d.Mgmt, mn)
	}
	return d
}

// role is a switch's role in the design, from its place in its fabric: the OOB fabric's switches
// are oob, a Clos fabric's spines and super-spines by name, the rest leaves.
func role(d *model.Device) string {
	switch {
	case strings.HasPrefix(d.Name, "mgmt-switch"), strings.HasPrefix(d.Name, "oob-"):
		return "oob"
	case strings.HasPrefix(d.Name, "super-spine"):
		return "super-spine"
	case strings.HasPrefix(d.Name, "spine"):
		return "spine"
	}
	return "leaf"
}

// osName is a model port's name as the switch's NOS names it to its users: swp<n> on Cumulus,
// Ethernet<first lane> on SONiC (whose container's eth<n> is only what lies behind it).
func osName(sp *SwitchPlan, port string) string {
	if sp.NOS.Name == nos.SonicVS {
		ports, err := nos.SonicPorts(sp.Config)
		if err != nil {
			return ""
		}
		for _, p := range ports {
			if p.Model == port {
				return p.Name
			}
		}
		return ""
	}
	return sp.NOS.Interface(sp.Config, port)
}
