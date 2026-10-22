package compile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/selector"
)

// rackOrdinal is a pure function of a rack's place in the hall (row and position), so adding
// or removing a rack never moves another rack's links (03 — Changing a running estate).
func rackOrdinal(r *model.Rack) int { return r.Row.Index*maxPositions + r.Position - 1 }

// maxPositions is the widest a row may be: rackOrdinal gives each row this many positions.
const maxPositions = 16

func (c *compiler) fabrics() {
	// fixed links of rack formats first: they are inside racks and need no switches
	for _, r := range c.m.Racks {
		for _, fl := range r.Format.Spec.FixedLinks {
			if fl.Template == "" {
				continue
			}
			t, ok := c.reg.Templates[fl.Template]
			if !ok {
				c.errf("format %s: fixed-link template %q not found", r.FormatName, fl.Template)
				continue
			}
			switch t.Spec.Generator {
			case "nvlink-spine":
				c.genNVLinkSpine(r)
			case "node-mesh":
				c.genNodeMesh(r)
			default:
				c.errf("format %s: template %s generator %q is not a fixed-link generator", r.FormatName, fl.Template, t.Spec.Generator)
			}
		}
	}
	var oob []*model.Fabric
	for _, fs := range c.est.Spec.Fabrics {
		tname := fs.Template
		if core, ok := coreTemplates[tname]; ok {
			tname = core
		}
		t, ok := c.reg.Templates[tname]
		if !ok {
			c.errf("fabric %s: template %q not found", fs.Name, fs.Template)
			continue
		}
		if fs.NOS == "" {
			c.errf("fabric %s: nos is required (%s)", fs.Name, strings.Join(nos.Names(), ", "))
		} else if _, ok := nos.Lookup(fs.NOS); !ok {
			// whether a registered NOS can run on this host is the substrate's question, at up
			c.errf("fabric %s: nos %q is not in the NOS registry (%s)", fs.Name, fs.NOS, strings.Join(nos.Names(), ", "))
		}
		tech := fs.Technology
		if tech == "" {
			tech = "ethernet"
		}
		f := &model.Fabric{Name: fs.Name, Template: tname, NOS: fs.NOS, Technology: tech, Manager: fs.Manager,
			Params: c.checkParams("fabric "+fs.Name, tname, t.Spec.Params, fs.Params, false, false)}
		level := "none"
		if fs.NOS != "none" {
			level = "vm"
		}
		f.Provides = t.Spec.Provides[level]
		sel, err := selector.Parse(fs.Select)
		if err != nil {
			c.errf("fabric %s: %v", fs.Name, err)
			continue
		}
		for _, r := range c.m.Racks {
			if fs.Select == "" || sel.MatchPath(r.Path) {
				f.Racks = append(f.Racks, r)
			}
		}
		c.m.Fabrics[f.Name] = f
		place := c.placeIn(fs)
		switch t.Spec.Generator {
		case "clos":
			c.genClos(f, place)
		case "rail":
			c.genRail(f, place)
		case "oob":
			oob = append(oob, f)
			_ = place
		default:
			c.errf("fabric %s: template %s has unknown generator %q", fs.Name, tname, t.Spec.Generator)
		}
	}
	// the OOB fabric is cabled last, so it reaches the management ports of generated switches
	for _, f := range oob {
		var fs schema.Fabric
		for _, x := range c.est.Spec.Fabrics {
			if x.Name == f.Name {
				fs = x
			}
		}
		c.genOOB(f, c.placeIn(fs))
	}
}

// placeIn finds the rack generated switches go into: the fabric's placeIn, else the hall's
// first network rack.
func (c *compiler) placeIn(fs schema.Fabric) *model.Rack {
	if fs.PlaceIn != "" {
		sel, err := selector.Parse(fs.PlaceIn)
		if err != nil {
			c.errf("fabric %s placeIn: %v", fs.Name, err)
			return nil
		}
		for _, r := range c.m.Racks {
			if sel.MatchPath(r.Path) {
				return r
			}
		}
		c.errf("fabric %s: placeIn %q matches no rack", fs.Name, fs.PlaceIn)
		return nil
	}
	for _, r := range c.m.Racks {
		if r.Format.Spec.Archetype == "network" {
			return r
		}
	}
	return nil
}

// generated creates a template-generated switch in a rack's reserved U.
func (c *compiler) generated(f *model.Fabric, rack *model.Rack, name, profile string) *model.Device {
	p := c.profile(profile, "fabric "+f.Name)
	if p == nil {
		return nil
	}
	var hall *model.Hall
	parent := ""
	if rack != nil {
		hall, parent = rack.Hall, rack.Path
	} else if len(c.m.Halls) > 0 {
		hall, parent = c.m.Halls[0], c.m.Halls[0].Path+"/"+f.Name
		c.warnf("fabric %s: no rack to place %s in; it has no elevation", f.Name, name)
	}
	if rack != nil {
		if existing := c.m.DeviceBy[parent+"/"+name]; existing != nil {
			return existing
		}
	}
	d := c.newDevice(rack, hall, parent, name, p, nil)
	d.Generated = f.Name
	d.Slot = "fabric"
	if f.NOS == "none" {
		d.Fidelity = "model"
	} else {
		d.Fidelity = "vm"
	}
	if rack != nil && c.reserved[rack] > 0 && d.Units > 0 {
		d.U = c.reserved[rack]
		c.reserved[rack] += d.Units
	}
	f.Devices = append(f.Devices, d)
	return d
}

func intParam(f *model.Fabric, k string, def int) int {
	if n, ok := toInt(f.Params[k]); ok && n > 0 {
		return n
	}
	return def
}

func strParam(f *model.Fabric, k string) string {
	if v, ok := f.Params[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// rackPorts lists the ports of a rack's devices (and their sub-devices) with one of the roles,
// in a stable order: device path, then port name.
func rackPorts(r *model.Rack, roles map[string]bool, kind string) []*model.Port {
	var out []*model.Port
	var walk func(d *model.Device)
	walk = func(d *model.Device) {
		for _, p := range d.Ports {
			if p.Component == nil && roles[p.Role] && (kind == "" || p.Kind == kind) {
				out = append(out, p)
			}
		}
		for _, ch := range d.Children {
			walk(ch)
		}
	}
	devs := append([]*model.Device{}, r.Devices...)
	sort.SliceStable(devs, func(i, j int) bool { return naturalLess(devs[i].Name, devs[j].Name) })
	for _, d := range devs {
		if d.Generated != "" || d.BaseClass == "switch" {
			continue
		}
		walk(d)
	}
	return out
}

func naturalLess(a, b string) bool { return a < b }

// genOOB cables every management-role port of a rack to the rack's management switches, and
// each management switch's first uplink to the OOB aggregation switch.
func (c *compiler) genOOB(f *model.Fabric, place *model.Rack) {
	var aggs []*model.Device
	aggProfile := strParam(f, "aggregation")
	for _, r := range c.m.Racks {
		var mgmt []*model.Device
		for _, d := range r.Devices {
			if strings.HasPrefix(d.Name, "mgmt-switch") {
				mgmt = append(mgmt, d)
			}
		}
		if len(mgmt) == 0 {
			continue
		}
		ports := rackPorts(r, map[string]bool{"management": true}, "ethernet")
		// generated switches placed in this rack reach the OOB too
		for _, d := range r.Devices {
			if d.Generated != "" {
				if p := d.PortBy["mgmt0"]; p != nil {
					ports = append(ports, p)
				}
			}
		}
		k := 0
		for _, p := range ports {
			if p.Link != nil {
				continue
			}
			sw := k / 48
			if sw >= len(mgmt) {
				c.errf("rack %s: %d management ports exceed its %d management switches' ports", r.Name, len(ports), len(mgmt))
				break
			}
			c.link(p, mgmt[sw].PortBy[fmt.Sprintf("eth%d", k%48+1)], "cable", f.Name)
			k++
		}
		if aggProfile != "" {
			// aggregation switch k takes uplinks [k*128, (k+1)*128): a pure function of the
			// rack's place, so a new rack never moves another rack's uplink
			for i, m := range mgmt {
				n := rackOrdinal(r)*2 + i
				k := n / 128
				for len(aggs) <= k {
					aggs = append(aggs, c.generated(f, place, fmt.Sprintf("oob-agg-%d", len(aggs)+1), aggProfile))
				}
				agg := aggs[k]
				if agg == nil {
					break
				}
				port := fmt.Sprintf("eth%d", n%128+1)
				if agg.PortBy[port] == nil {
					c.errf("rack %s: OOB aggregation %s has no port %s", r.Name, agg.Name, port)
					continue
				}
				c.link(m.PortBy["up1"], agg.PortBy[port], "cable", f.Name)
			}
		}
		f.Racks = appendRack(f.Racks, r)
	}
}

func appendRack(rs []*model.Rack, r *model.Rack) []*model.Rack {
	if rackIn(rs, r) {
		return rs
	}
	return append(rs, r)
}

// genClos builds leafsPerRack leaves per rack and a spine tier; every frontend or
// provisioning port goes to the rack's leaves, alternating, in a stable order.
func (c *compiler) genClos(f *model.Fabric, place *model.Rack) {
	L, S := intParam(f, "leafsPerRack", 2), intParam(f, "spines", 2)
	leafProfile, spineProfile := strParam(f, "leaf"), strParam(f, "spine")
	var spines []*model.Device
	for s := 1; s <= S; s++ {
		spines = append(spines, c.generated(f, place, fmt.Sprintf("spine-%d", s), spineProfile))
	}
	roles := map[string]bool{"frontend": true, "provisioning": true, "storage": true}
	for _, r := range f.Racks {
		ports := rackPorts(r, roles, "ethernet")
		if len(ports) == 0 {
			continue
		}
		target := place
		if c.reserved[r] > 0 {
			target = r
		}
		var leaves []*model.Device
		for l := 1; l <= L; l++ {
			leaves = append(leaves, c.generated(f, target, fmt.Sprintf("leaf-%s-%d", r.Name, l), leafProfile))
		}
		if leaves[0] == nil {
			continue
		}
		hostPorts := 64 - S
		for k, p := range ports {
			idx := k/L + 1
			if idx > hostPorts {
				c.errf("rack %s: %d host ports exceed %d leaves of %d host-facing ports", r.Name, len(ports), L, hostPorts)
				break
			}
			c.link(p, leaves[k%L].PortBy[fmt.Sprintf("eth%d", idx)], "cable", f.Name)
		}
		for li, leaf := range leaves {
			for s, sp := range spines {
				if sp == nil {
					continue
				}
				sport := fmt.Sprintf("eth%d", rackOrdinal(r)*L+li+1)
				if sp.PortBy[sport] == nil {
					c.errf("rack %s: spine %s has no port %s", r.Name, sp.Name, sport)
					continue
				}
				c.link(leaf.PortBy[fmt.Sprintf("eth%d", 64-s)], sp.PortBy[sport], "cable", f.Name)
			}
		}
	}
}

// genRail builds `rails` leaves per selected rack; scale-out port j of every device goes to rail
// j mod rails.
func (c *compiler) genRail(f *model.Fabric, place *model.Rack) {
	R := intParam(f, "rails", 8)
	ib := f.Technology == "infiniband"
	profile, prefix := strParam(f, "leaf"), "eth"
	if ib {
		profile, prefix = strParam(f, "ibLeaf"), "ib"
	}
	var first *model.Device
	var allLeaves []*model.Device
	for _, r := range f.Racks {
		kind := "ethernet"
		if ib {
			kind = "infiniband"
		}
		var devs []*model.Device
		for _, d := range r.Devices {
			if d.Generated == "" && d.BaseClass != "switch" && d.BaseClass != "appliance" {
				devs = append(devs, d)
			}
		}
		sort.Slice(devs, func(i, j int) bool { return devs[i].Name < devs[j].Name })
		var leaves []*model.Device
		any := false
		for i, d := range devs {
			var ports []*model.Port
			for _, p := range d.Ports {
				if p.Role == "scale-out" && p.Kind == kind && p.Component == nil {
					ports = append(ports, p)
				}
			}
			if len(ports) == 0 {
				continue
			}
			if !any {
				any = true
				for rr := 1; rr <= R; rr++ {
					leaves = append(leaves, c.generated(f, place, fmt.Sprintf("%s-rail-%s-%d", f.Name, r.Name, rr), profile))
				}
				allLeaves = append(allLeaves, leaves...)
				if first == nil {
					first = leaves[0]
				}
			}
			per := (len(ports) + R - 1) / R
			for j, p := range ports {
				leaf := leaves[j%R]
				if leaf == nil {
					continue
				}
				port := fmt.Sprintf("%s%d", prefix, i*per+j/R+1)
				if leaf.PortBy[port] == nil {
					c.errf("rack %s: rail leaf %s has no port %s", r.Name, leaf.Name, port)
					continue
				}
				c.link(p, leaf.PortBy[port], "cable", f.Name)
			}
		}
	}
	// The spine tier: every rail leaf uplinks to every spine, from its port 33 on (the downlinks
	// take the lower half; port 64 of the first leaf is the fabric manager's), and spine port n is
	// the n-th leaf. Without it a rail-optimised InfiniBand fabric is one subnet per leaf, which
	// no SuperPOD is (found by running OpenSM against the twin, compat/opensm). spines: 0 keeps
	// single-tier rails.
	S := 2
	if n, ok := toInt(f.Params["spines"]); ok && n >= 0 {
		S = n
	}
	spineProfile := strParam(f, "spine")
	if ib {
		spineProfile = strParam(f, "ibSpine")
	}
	if spineProfile == "" {
		spineProfile = profile
	}
	var spines []*model.Device
	for s := 1; s <= S && len(allLeaves) > 0; s++ {
		if sp := c.generated(f, place, fmt.Sprintf("%s-spine-%d", f.Name, s), spineProfile); sp != nil {
			spines = append(spines, sp)
		}
	}
	for li, leaf := range allLeaves {
		if leaf == nil {
			continue
		}
		for si, sp := range spines {
			up, down := leaf.PortBy[fmt.Sprintf("%s%d", prefix, 33+si)], sp.PortBy[fmt.Sprintf("%s%d", prefix, li+1)]
			if up == nil || down == nil {
				c.errf("fabric %s: %s or %s has no port for the spine tier (leaf %s%d, spine %s%d)", f.Name, leaf.Name, sp.Name, prefix, 33+si, prefix, li+1)
				continue
			}
			c.link(up, down, "cable", f.Name)
		}
	}
	if f.Manager != "" && first != nil {
		for _, d := range c.m.Devices {
			if d.BaseClass == "appliance" && d.PortBy["ib0"] != nil && d.PortBy["ib0"].Link == nil && ib {
				c.link(d.PortBy["ib0"], first.PortBy[prefix+"64"], "cable", f.Name)
			}
		}
	}
}

// genNVLinkSpine wires lane l of rack GPU g to switch tray l/2, NVLink Switch chip l mod 2,
// port g. In a two-rack (NVL36x2) domain, ports 36..71 of each chip cross to the peer rack.
func (c *compiler) genNVLinkSpine(r *model.Rack) {
	var trays, switches []*model.Device
	for _, d := range r.Devices {
		switch d.BaseClass {
		case "compute-tray":
			trays = append(trays, d)
		case "switch-tray":
			switches = append(switches, d)
		}
	}
	sort.Slice(trays, func(i, j int) bool { return trays[i].Name < trays[j].Name })
	sort.Slice(switches, func(i, j int) bool { return switches[i].Name < switches[j].Name })
	g := 0
	for _, t := range trays {
		for _, comp := range t.Components {
			if comp.Kind != "accelerator" {
				continue
			}
			for l, p := range comp.Ports {
				if p.Kind != "scale-up" {
					continue
				}
				st := l / 2
				if st >= len(switches) {
					continue
				}
				sp := switches[st].PortBy[fmt.Sprintf("nvswitch-%d-nvl-%d", l%2, g)]
				c.link(p, sp, "fixed", "nvlink")
			}
			g++
		}
	}
	peerName, _ := r.Params["peer"].(string)
	if peerName == "" || g > 36 {
		return
	}
	peer := c.m.RackBy[r.Hall.Path+"/"+peerName]
	if peer == nil || peer.Path < r.Path {
		return // the first rack in sorted order wires the cross-rack links
	}
	var peerSwitches []*model.Device
	for _, d := range peer.Devices {
		if d.BaseClass == "switch-tray" {
			peerSwitches = append(peerSwitches, d)
		}
	}
	sort.Slice(peerSwitches, func(i, j int) bool { return peerSwitches[i].Name < peerSwitches[j].Name })
	for k := 0; k < len(switches) && k < len(peerSwitches); k++ {
		for a := 0; a < 2; a++ {
			for p := 36; p < 72; p++ {
				name := fmt.Sprintf("nvswitch-%d-nvl-%d", a, p)
				c.link(switches[k].PortBy[name], peerSwitches[k].PortBy[name], "fixed", "nvlink-cross-rack")
			}
		}
	}
}

// genNodeMesh wires a server's accelerators all-to-all over their scale-up ports (OAM xGMI,
// Gaudi in-node RoCE): lanes split evenly across the peers.
func (c *compiler) genNodeMesh(r *model.Rack) {
	for _, d := range r.Devices {
		var accs []*model.Component
		for _, comp := range d.Components {
			if comp.Kind == "accelerator" {
				accs = append(accs, comp)
			}
		}
		n := len(accs)
		if n < 2 {
			continue
		}
		up := func(comp *model.Component) []*model.Port {
			var out []*model.Port
			for _, p := range comp.Ports {
				if p.Kind == "scale-up" {
					out = append(out, p)
				}
			}
			return out
		}
		lanes := len(up(accs[0]))
		per := lanes / (n - 1)
		if per == 0 {
			continue
		}
		slot := func(i, j int) int { // position of peer j among i's peers
			if j < i {
				return j
			}
			return j - 1
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				pi, pj := up(accs[i]), up(accs[j])
				for k := 0; k < per; k++ {
					c.link(pi[slot(i, j)*per+k], pj[slot(j, i)*per+k], "fixed", "node-mesh")
				}
			}
		}
	}
}
