// Package compile turns an estate file and the packs it pins into the runtime model, refusing
// the estate before anything starts if it breaks a rule of docs/spec/03-estate-model.md
// (Validation). Port allocation in every generator is a pure function of endpoint paths.
package compile

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/expr"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/selector"
)

type Options struct {
	HostArch string // aarch64 or x86_64
	AllowTCG bool
}

type Result struct {
	Model    *model.Model
	Lock     *pack.Lock
	Warnings []string
}

// Error carries every problem found, not just the first.
type Error struct{ Problems []string }

func (e *Error) Error() string {
	return fmt.Sprintf("estate refused (%d problems):\n  - %s", len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

var coreTemplates = map[string]string{"clos-2tier": "generic/clos-2tier", "clos-3tier": "generic/clos-2tier", "rail-optimised": "generic/rail-optimised", "oob-flat": "generic/oob-flat"}

type compiler struct {
	est      *schema.Estate
	reg      *pack.Registry
	opt      Options
	m        *model.Model
	id       *identity.Deriver
	enabled  map[string]bool
	errs     []string
	warns    []string
	over     []*override
	reserved map[*model.Rack]int // next free reserved U per rack
}

type override struct {
	sel   *selector.Selector
	level string
	os    string
}

func (c *compiler) errf(f string, a ...any)  { c.errs = append(c.errs, fmt.Sprintf(f, a...)) }
func (c *compiler) warnf(f string, a ...any) { c.warns = append(c.warns, fmt.Sprintf(f, a...)) }

// Compile builds the model. It returns a *Error listing every problem when the estate is refused.
func Compile(est *schema.Estate, reg *pack.Registry, opt Options) (*Result, error) {
	c := &compiler{est: est, reg: reg, opt: opt, reserved: map[*model.Rack]int{}}
	if err := schema.CheckHeader(schema.Header{APIVersion: est.APIVersion, Kind: est.Kind, Metadata: est.Metadata}, "Estate"); err != nil {
		return nil, &Error{[]string{err.Error()}}
	}
	sp := &est.Spec
	if sp.Clock == "" {
		sp.Clock = "wall"
	}
	if sp.Clock != "wall" && sp.Clock != "virtual" {
		c.errf("spec.clock %q: wall or virtual", sp.Clock)
	}
	if sp.TimeScale == 0 {
		sp.TimeScale = 1
	}
	if sp.Fidelity.Default == "" {
		sp.Fidelity.Default = "model"
	}
	lock, enabled, err := reg.Resolve(est.Metadata.Name, sp.Packs)
	if err != nil {
		c.errf("%v", err)
	}
	c.enabled = enabled
	if len(sp.Sites) != 1 {
		c.errf("spec.sites: exactly one site in v1alpha1 (D30), got %d", len(sp.Sites))
		return nil, &Error{c.errs}
	}
	site := sp.Sites[0]
	c.m = &model.Model{
		Name: est.Metadata.Name, Seed: sp.Seed, Site: site.Name, Estate: est, Clock: sp.Clock,
		RackBy: map[string]*model.Rack{}, DeviceBy: map[string]*model.Device{}, Ports: map[string]*model.Port{},
		LinkBy: map[string]*model.Link{}, Networks: map[string]*model.Network{}, Fabrics: map[string]*model.Fabric{},
		Feeds: map[string]*model.Feed{}, Loops: map[string]*model.Loop{},
	}
	c.id = identity.New(sp.Seed)
	for _, o := range sp.Fidelity.Overrides {
		sel, err := selector.Parse(o.Select)
		if err != nil {
			c.errf("fidelity.overrides: %v", err)
			continue
		}
		if !validLevel(o.Level) {
			c.errf("fidelity.overrides %q: level %q", o.Select, o.Level)
		}
		if o.OS != "" && o.OS != "twin" && o.OS != "stack" {
			c.errf("fidelity.overrides %q: os %q: twin or stack", o.Select, o.OS)
		}
		c.over = append(c.over, &override{sel: sel, level: o.Level, os: o.OS})
	}
	for _, f := range sp.Facility.Power.Feeds {
		c.m.Feeds[f.Name] = &model.Feed{Name: f.Name, Path: site.Name + "/facility/power/" + f.Name, CapacityW: f.CapacityW, Up: true}
	}
	c.halls(site)
	c.facilityDevices(site)
	c.fabrics()
	c.cabling()
	c.powerInputs()
	c.networks()
	c.domains()
	c.loops()
	c.validate()
	c.finish()
	if len(c.errs) > 0 {
		sort.Strings(c.errs)
		return &Result{Model: c.m, Lock: lock, Warnings: c.warns}, &Error{c.errs}
	}
	c.m.Warnings = c.warns
	return &Result{Model: c.m, Lock: lock, Warnings: c.warns}, nil
}

func validLevel(l string) bool { return l == "model" || l == "vm" || l == "hardware" }

// use resolves a profile and checks its pack is pinned.
func (c *compiler) profile(name, where string) *schema.DeviceProfile {
	p, ok := c.reg.Profiles[name]
	if !ok {
		c.errf("%s: profile %q not found", where, name)
		return nil
	}
	if !c.enabled[pack.PackOf(name)] {
		c.errf("%s: profile %q is in pack %q, which the estate does not pin", where, name, pack.PackOf(name))
		return nil
	}
	return p
}

// ---------------------------------------------------------------- halls, rows, racks

func (c *compiler) halls(site schema.Site) {
	for _, h := range site.Halls {
		hall := &model.Hall{Name: h.Name, Path: site.Name + "/" + h.Name}
		c.m.Halls = append(c.m.Halls, hall)
		names := map[string]bool{}
		for ri, r := range h.Rows {
			if r.Positions < 1 {
				c.errf("hall %s row %s: positions must be ≥ 1", h.Name, r.Name)
			}
			row := &model.Row{Name: r.Name, Index: ri, Positions: r.Positions}
			hall.Rows = append(hall.Rows, row)
			used := map[int]string{}
			for _, rk := range r.Racks {
				if names[rk.Name] {
					c.errf("hall %s: two racks named %s", h.Name, rk.Name)
					continue
				}
				names[rk.Name] = true
				if rk.Position < 1 || rk.Position > r.Positions {
					c.errf("rack %s: position %d out of range 1..%d", rk.Name, rk.Position, r.Positions)
				}
				if prev, dup := used[rk.Position]; dup {
					c.errf("rack %s: position %d already used by %s", rk.Name, rk.Position, prev)
				}
				used[rk.Position] = rk.Name
				c.rack(hall, row, rk)
			}
		}
	}
}

func (c *compiler) rack(hall *model.Hall, row *model.Row, spec schema.Rack) {
	f, ok := c.reg.Formats[spec.Format]
	if !ok {
		c.errf("rack %s: format %q not found", spec.Name, spec.Format)
		return
	}
	if !c.enabled[pack.PackOf(spec.Format)] {
		c.errf("rack %s: format %q is in pack %q, which the estate does not pin", spec.Name, spec.Format, pack.PackOf(spec.Format))
		return
	}
	r := &model.Rack{Name: spec.Name, Path: hall.Path + "/" + spec.Name, Hall: hall, Row: row, Position: spec.Position,
		FormatName: spec.Format, Format: f, Isolated: map[string]bool{}}
	r.Params = c.formatParams(spec, f)
	r.Feeds = spec.Feeds
	row.Racks = append(row.Racks, r)
	c.m.Racks = append(c.m.Racks, r)
	c.m.RackBy[r.Path] = r
	for name := range spec.Slots {
		_ = name
	}
	slotIdx := map[string]int{}
	var atDevices []struct {
		d  *model.Device
		at string
	}
	for _, slot := range f.Spec.Slots {
		if slot.When != "" {
			ok, err := expr.Bool(expr.EnvParams, slot.When, map[string]any{"params": r.Params}, nil)
			if err != nil {
				c.errf("format %s slot %s: %v", f.Metadata.Name, slot.Names, err)
				continue
			}
			if !ok {
				continue
			}
		}
		names, err := selector.Expand(subst(slot.Names, r.Params))
		if err != nil {
			c.errf("format %s slot %s: %v", f.Metadata.Name, slot.Names, err)
			continue
		}
		profName := slot.Profile
		if strings.HasPrefix(profName, "$") {
			profName = fmt.Sprint(r.Params[profName[1:]])
		}
		group := groupOf(slot.Names)
		for i, name := range names {
			pn := profName
			if ov, ok := spec.Slots[name]; ok {
				if ov.Empty {
					continue
				}
				if ov.Profile != "" {
					p := c.profile(ov.Profile, "rack "+spec.Name+" slot "+name)
					if p != nil && !accepts(slot.Accepts, p) {
						c.errf("rack %s slot %s: profile %s is not accepted by the slot (%v)", spec.Name, name, ov.Profile, slot.Accepts)
					}
					pn = ov.Profile
				}
			}
			if pn == "" || pn == "<nil>" {
				continue
			}
			p := c.profile(pn, "rack "+spec.Name+" slot "+name)
			if p == nil {
				continue
			}
			d := c.newDevice(r, hall, r.Path, name, p, nil)
			d.Slot = group
			d.SlotIndex = slotIdx[group]
			slotIdx[group]++
			d.Side = slot.Side
			if slot.U > 0 {
				d.U = slot.U
			}
			if len(slot.At) > 0 && i < len(slot.At) {
				atDevices = append(atDevices, struct {
					d  *model.Device
					at string
				}{d, slot.At[i]})
			}
		}
	}
	for name := range spec.Slots {
		found := false
		for _, d := range r.Devices {
			if d.Name == name {
				found = true
			}
		}
		if !found && !spec.Slots[name].Empty {
			c.errf("rack %s: slot override %q names no slot of %s", spec.Name, name, spec.Format)
		}
	}
	c.layoutRack(r)
	for _, a := range atDevices {
		for _, d := range r.Devices {
			if d.Name == a.at {
				a.d.U = d.U
			}
		}
	}
}

func groupOf(names string) string {
	if i := strings.IndexAny(names, "{$"); i > 0 {
		return strings.TrimRight(names[:i], "-")
	}
	return names
}

func subst(s string, params map[string]any) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		s = strings.ReplaceAll(s, "$"+k, fmt.Sprint(params[k]))
	}
	return s
}

func accepts(a map[string]any, p *schema.DeviceProfile) bool {
	if a == nil {
		return true
	}
	if v, ok := a["class"]; ok && fmt.Sprint(v) != p.Spec.Class {
		return false
	}
	if v, ok := a["mounting"]; ok && fmt.Sprint(v) != p.Spec.Form.Mounting {
		return false
	}
	if v, ok := a["cooling"]; ok && fmt.Sprint(v) != p.Spec.Cooling.Kind {
		return false
	}
	return true
}

func (c *compiler) formatParams(spec schema.Rack, f *schema.RackFormat) map[string]any {
	out := map[string]any{}
	for k := range spec.Params {
		if _, ok := f.Spec.Params[k]; !ok {
			c.errf("rack %s: parameter %q is not declared by %s", spec.Name, k, f.Metadata.Name)
		}
	}
	for k, decl := range f.Spec.Params {
		v, set := spec.Params[k]
		if !set {
			v = decl.Default
		}
		switch decl.Type {
		case "int":
			n, ok := toInt(v)
			if !ok {
				c.errf("rack %s: parameter %s: want an integer, got %v", spec.Name, k, v)
				continue
			}
			if len(decl.Range) == 2 && (n < decl.Range[0] || n > decl.Range[1]) {
				c.errf("rack %s: parameter %s=%d out of range %v", spec.Name, k, n, decl.Range)
			}
			if len(decl.Allowed) > 0 {
				ok := false
				for _, a := range decl.Allowed {
					if an, _ := toInt(a); an == n {
						ok = true
					}
				}
				if !ok {
					c.errf("rack %s: parameter %s=%d not in %v", spec.Name, k, n, decl.Allowed)
				}
			}
			out[k] = n
		case "profile":
			s := ""
			if v != nil {
				s = fmt.Sprint(v)
			}
			if s != "" {
				p := c.profile(s, "rack "+spec.Name+" parameter "+k)
				if p != nil && decl.Class != "" && p.Spec.Class != decl.Class {
					c.errf("rack %s: parameter %s: %s is a %s, the format wants a %s", spec.Name, k, s, p.Spec.Class, decl.Class)
				}
			}
			out[k] = s
		default:
			if v == nil {
				v = ""
			}
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), x == float64(int(x))
	case string:
		n, err := strconv.Atoi(x)
		return n, err == nil
	}
	return 0, false
}

// layoutRack assigns U positions from layout.front, top to bottom, EIA numbering (U1 at the
// bottom). Zero-U and unmounted devices get U 0.
func (c *compiler) layoutRack(r *model.Rack) {
	lay := r.Format.Spec.Layout
	cursor := lay.Height
	placed := map[*model.Device]bool{}
	place := func(d *model.Device) {
		if placed[d] || d.Parent != nil {
			return
		}
		placed[d] = true
		mount := d.Profile.Spec.Form.Mounting
		if mount == "zero-u" || mount == "none" || d.Units == 0 {
			return
		}
		if d.U > 0 {
			return
		}
		d.U = cursor - d.Units + 1
		cursor -= d.Units
		if d.U < 1 {
			c.errf("rack %s: %s does not fit in %dU", r.Name, d.Name, lay.Height)
		}
	}
	for _, g := range lay.Front {
		names, err := selector.Expand(g.Slots)
		if err != nil {
			c.errf("format %s layout %s: %v", r.FormatName, g.Slots, err)
			continue
		}
		for _, n := range names {
			for _, d := range r.Devices {
				if d.Name == n {
					place(d)
				}
			}
		}
	}
	for _, d := range r.Devices {
		place(d)
	}
	c.reserved[r] = 0
	for _, res := range lay.Reserved {
		if res.For == "fabric" {
			c.reserved[r] = res.U
		}
	}
}

// ---------------------------------------------------------------- devices

func (c *compiler) fidelity(path string, d *model.Device, inherit *model.Device) (string, string) {
	level, os := c.est.Spec.Fidelity.Default, ""
	if inherit != nil {
		level, os = inherit.Fidelity, inherit.OS
	}
	for _, o := range c.over {
		if o.sel.MatchPath(path) && o.sel.MatchAttrs(d.Attr) {
			level, os = o.level, o.os
		}
	}
	if level == "vm" && os == "" {
		os = "twin"
	}
	if level != "vm" {
		os = ""
	}
	return level, os
}

func (c *compiler) newDevice(r *model.Rack, hall *model.Hall, parentPath, name string, p *schema.DeviceProfile, parent *model.Device) *model.Device {
	path := parentPath + "/" + name
	if _, dup := c.m.DeviceBy[path]; dup {
		c.errf("two devices at %s", path)
		return c.m.DeviceBy[path]
	}
	d := &model.Device{Path: path, Name: name, Rack: r, Hall: hall, ProfileName: p.Metadata.Name, ProfileVersion: p.Metadata.Version,
		Profile: p, Class: p.Spec.Class, BaseClass: p.Spec.Class, Units: p.Spec.Form.Units, Parent: parent,
		PortBy: map[string]*model.Port{}}
	if parent != nil {
		d.Rack = nil
	}
	d.Fidelity, d.OS = c.fidelity(path, d, parent)
	if _, ok := p.Spec.Fidelity[d.Fidelity]; !ok {
		c.errf("%s: profile %s does not support fidelity %s", path, p.Metadata.Name, d.Fidelity)
	}
	if d.Fidelity == "vm" {
		arch, _ := p.Spec.Fidelity["vm"]["arch"].(string)
		if arch != "" && c.opt.HostArch != "" && arch != c.opt.HostArch && !c.opt.AllowTCG {
			c.errf("%s: a vm %s guest on a %s host runs under TCG; pass --allow-tcg to accept it (09, Q17)", path, arch, c.opt.HostArch)
		}
	}
	major := identity.ProfileMajor(p.Metadata.Name, p.Metadata.Version)
	idspec := p.Spec.Identity
	if idspec == nil {
		idspec = &schema.IdentitySpec{}
	}
	d.Identity = model.Identity{ProfileMajor: major, Serial: c.id.Serial(path, major, idspec.SerialFormat, 0),
		UUID: c.id.UUID(path, major, 0), MACs: map[string]string{}, GUIDs: map[string]string{}}
	if p.Spec.BMC != nil {
		d.Dialect = p.Spec.BMC.Dialect
		dl, _ := c.reg.Dialect(d.Dialect)
		if dl != nil {
			d.Identity.BMCUser = dl.Spec.Factory.User
			d.Identity.BMCPassword = dl.Spec.Factory.Password
		}
		if d.Identity.BMCPassword == "" {
			d.Identity.BMCPassword = c.id.Password(path, major)
		}
		if !c.enabled[pack.PackOf(d.Dialect)] {
			c.errf("%s: dialect %s is in pack %q, which the estate does not pin", path, d.Dialect, pack.PackOf(d.Dialect))
		}
	}
	c.m.DeviceBy[path] = d
	c.m.Devices = append(c.m.Devices, d)
	if r != nil && parent == nil {
		r.Devices = append(r.Devices, d)
	}
	// ports
	for _, ps := range p.Spec.Ports {
		names, err := selector.Expand(ps.Name)
		if err != nil {
			c.errf("profile %s port %s: %v", p.Metadata.Name, ps.Name, err)
			continue
		}
		if ps.Count > 1 && len(names) == 1 {
			names = nil
			for i := 0; i < ps.Count; i++ {
				names = append(names, fmt.Sprintf("%s%d", ps.Name, i))
			}
		}
		for _, n := range names {
			c.addPort(d, nil, n, ps)
		}
	}
	// components (and sub-devices)
	for _, cs := range p.Spec.Components {
		for i := 0; i < cs.Count; i++ {
			cname := fmt.Sprintf("%s-%0*d", cs.Name, cs.Pad, i)
			comp := &model.Component{Path: path + "/" + cname, Name: cname, Group: cs.Name, Kind: cs.Kind, Model: cs.Model,
				Vendor: cs.Vendor, Index: i, Device: d, MaxW: cs.MaxW, MemoryMiB: accelMemoryMiB(cs), SerialShare: cs.SerialShare, State: model.ComponentState{Counters: map[string]float64{}}}
			d.Components = append(d.Components, comp)
			for _, ps := range cs.Ports {
				lanes := ps.Lanes
				if lanes == 0 {
					lanes = 1
				}
				for l := 0; l < lanes; l++ {
					c.addPort(d, comp, fmt.Sprintf("%s-%s-%d", cname, ps.Name, l), ps)
				}
			}
			if cs.Profile != "" {
				sp := c.profile(cs.Profile, path+" component "+cs.Name)
				if sp != nil {
					sub := c.newDevice(r, hall, path, cname, sp, d)
					sub.Rack = nil
					comp.Sub = sub
					d.Children = append(d.Children, sub)
				}
			}
		}
	}
	// the BMC's MAC is its management port's
	if d.HasBMC() {
		if bp := bmcPort(d); bp != nil {
			d.Identity.BMCMAC = bp.MAC
		} else {
			c.errf("%s: profile %s has a BMC but no management port", path, p.Metadata.Name)
		}
	}
	return d
}

// bmcPort is the port the BMC is reached on: bmc0, else the first management-role port.
func bmcPort(d *model.Device) *model.Port {
	if p := d.PortBy["bmc0"]; p != nil {
		return p
	}
	for _, p := range d.Ports {
		if p.Role == "management" && p.Component == nil {
			return p
		}
	}
	return nil
}

// BMCPort is exported for adapters.
func BMCPort(d *model.Device) *model.Port { return bmcPort(d) }

func (c *compiler) addPort(d *model.Device, comp *model.Component, name string, ps schema.PortSpec) {
	if _, dup := d.PortBy[name]; dup {
		c.errf("%s: two ports named %s", d.Path, name)
		return
	}
	pt := &model.Port{Ref: d.Path + ":" + name, Name: name, Device: d, Component: comp, Kind: ps.Kind, Role: ps.Role,
		Speed: ps.Speed, Technology: ps.Technology, AdminUp: true}
	idspec := d.Profile.Spec.Identity
	oui, guid := "", ""
	if idspec != nil {
		oui, guid = idspec.MacOUI, idspec.GUIDPrefix
	}
	switch ps.Kind {
	case "ethernet":
		pt.MAC = c.id.MAC(d.Path, d.Identity.ProfileMajor, name, oui, 0)
		d.Identity.MACs[name] = pt.MAC
	case "infiniband":
		pt.GUID = c.id.GUID(d.Path, d.Identity.ProfileMajor, name, guid, 0)
		d.Identity.GUIDs[name] = pt.GUID
	}
	d.Ports = append(d.Ports, pt)
	d.PortBy[name] = pt
	if comp != nil {
		comp.Ports = append(comp.Ports, pt)
	}
	c.m.Ports[pt.Ref] = pt
}

func (c *compiler) link(a, b *model.Port, kind, fabric string) *model.Link {
	if a == nil || b == nil {
		return nil
	}
	for _, p := range []*model.Port{a, b} {
		if p.Link != nil {
			c.errf("port %s has two cables (%s and %s)", p.Ref, p.Link.ID, linkID(a, b))
			return nil
		}
	}
	if a.Kind != b.Kind && !(a.Kind == "power-in" || b.Kind == "power-in") {
		c.errf("cable %s joins incompatible port kinds %s and %s", linkID(a, b), a.Kind, b.Kind)
		return nil
	}
	l := &model.Link{ID: linkID(a, b), A: a, B: b, Kind: kind, Fabric: fabric, Up: true}
	a.Link, b.Link = l, l
	c.m.Links = append(c.m.Links, l)
	c.m.LinkBy[l.ID] = l
	return l
}

func linkID(a, b *model.Port) string {
	if a.Ref > b.Ref {
		a, b = b, a
	}
	return a.Ref + "~" + b.Ref
}

func (c *compiler) facilityDevices(site schema.Site) {
	for _, fd := range c.est.Spec.Facility.Devices {
		p := c.profile(fd.Profile, "facility device "+fd.Name)
		if p == nil {
			continue
		}
		var hall *model.Hall
		if len(c.m.Halls) > 0 {
			hall = c.m.Halls[0]
		}
		c.newDevice(nil, hall, hall.Path+"/facility", fd.Name, p, nil)
	}
	for _, lp := range c.est.Spec.Facility.Cooling.Loops {
		if lp.Profile == "" {
			continue
		}
		p := c.profile(lp.Profile, "cooling loop "+lp.Name)
		if p == nil || len(c.m.Halls) == 0 {
			continue
		}
		c.newDevice(nil, c.m.Halls[0], c.m.Halls[0].Path+"/facility", lp.Name, p, nil)
	}
}

// ---------------------------------------------------------------- power

func (c *compiler) powerInputs() {
	for _, r := range c.m.Racks {
		for _, in := range r.Format.Spec.Power.Inputs {
			pi := &model.PowerInput{Name: in.Name, Kind: in.Kind, CapacityW: in.CapacityW, Redundancy: in.Redundancy,
				MinSources: in.MinSources, SourceFeed: map[string]string{}}
			if pi.MinSources == 0 {
				pi.MinSources = 1
			}
			for _, d := range r.Devices {
				if in.Sources != "" && selector.Glob(in.Sources, d.Name) {
					pi.Sources = append(pi.Sources, d)
				}
			}
			for _, d := range r.Devices {
				if contains(pi.Sources, d) {
					continue
				}
				for _, p := range d.Ports {
					if p.Kind == "power-in" && sourceOf(d, p) == in.Kind {
						pi.Serves = append(pi.Serves, d)
						d.Input = pi
						break
					}
				}
			}
			feeds := r.Feeds
			if len(feeds) == 0 {
				for i, f := range c.est.Spec.Facility.Power.Feeds {
					if i < max(1, len(in.Feeds)) {
						feeds = append(feeds, f.Name)
					}
				}
			}
			for _, f := range feeds {
				if _, ok := c.m.Feeds[f]; !ok {
					c.errf("rack %s: feed %q is not in facility.power.feeds", r.Name, f)
				}
			}
			if len(pi.Sources) > 0 && len(pi.Sources) < pi.MinSources {
				c.errf("rack %s: input %s has %d sources, fewer than its minSources %d", r.Name, in.Name, len(pi.Sources), pi.MinSources)
			}
			for i, s := range pi.Sources {
				if len(feeds) > 0 {
					side := i * len(feeds) / len(pi.Sources)
					pi.SourceFeed[s.Path] = feeds[side]
				}
			}
			r.Inputs = append(r.Inputs, pi)
		}
	}
}

func sourceOf(d *model.Device, p *model.Port) string {
	for _, ps := range d.Profile.Spec.Ports {
		names, _ := selector.Expand(ps.Name)
		for _, n := range names {
			if n == p.Name {
				return ps.Source
			}
		}
	}
	return ""
}

func contains(ds []*model.Device, d *model.Device) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- networks, cabling, domains

func (c *compiler) networks() {
	roleNets := map[string][]string{}
	names := make([]string, 0, len(c.est.Spec.Networks))
	for n := range c.est.Spec.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		ns := c.est.Spec.Networks[name]
		switch ns.Role {
		case "management", "provisioning", "frontend", "scale-out", "storage", "service":
		default:
			c.errf("network %s: role %q", name, ns.Role)
		}
		if ns.DHCP == "" {
			ns.DHCP = "stack"
		}
		if ns.DHCP != "stack" && ns.DHCP != "twin" && ns.DHCP != "none" {
			c.errf("network %s: dhcp %q: stack, twin or none", name, ns.DHCP)
		}
		if ns.CIDR != "" {
			if _, err := netip.ParsePrefix(ns.CIDR); err != nil {
				c.errf("network %s: cidr %q: %v", name, ns.CIDR, err)
			}
		}
		if (ns.DHCP == "twin" || ns.DHCP == "none") && ns.CIDR == "" && ns.Role != "service" {
			c.errf("network %s: dhcp %s needs a cidr", name, ns.DHCP)
		}
		if ns.Fabric != "" {
			if _, ok := c.m.Fabrics[ns.Fabric]; !ok {
				c.errf("network %s: fabric %q is not declared", name, ns.Fabric)
			}
		} else if ns.DHCP == "stack" && ns.Role != "service" {
			c.errf("network %s: a relayed network names no fabric", name)
		}
		n := &model.Network{Name: name, Role: ns.Role, CIDR: ns.CIDR, DHCP: ns.DHCP, Fabric: ns.Fabric, Server: ns.Server}
		roleNets[ns.Role] = append(roleNets[ns.Role], name)
		c.m.Networks[name] = n
	}
	for role, nets := range roleNets {
		if len(nets) > 1 {
			// a role maps to at most one network per rack: the fabrics must select disjoint racks
			seen := map[string]string{}
			for _, nn := range nets {
				f := c.m.Fabrics[c.m.Networks[nn].Fabric]
				if f == nil {
					continue
				}
				for _, r := range f.Racks {
					if prev, dup := seen[r.Path]; dup {
						c.errf("rack %s: role %s maps to two networks (%s, %s)", r.Name, role, prev, nn)
					}
					seen[r.Path] = nn
				}
			}
		}
	}
	for _, d := range c.m.Devices {
		for _, p := range d.Ports {
			if p.Role == "" {
				continue
			}
			for _, nn := range roleNets[p.Role] {
				n := c.m.Networks[nn]
				f := c.m.Fabrics[n.Fabric]
				r := d.RackOf()
				if f == nil || r == nil || rackIn(f.Racks, r) || len(roleNets[p.Role]) == 1 {
					p.Network = nn
					n.Ports = append(n.Ports, p)
					break
				}
			}
		}
	}
}

func rackIn(rs []*model.Rack, r *model.Rack) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

func (c *compiler) cabling() {
	cb := c.est.Spec.Cabling
	if cb == nil {
		return
	}
	resolve := func(ref string) *model.Port {
		sel, err := selector.Parse(ref)
		if err != nil {
			c.errf("cabling %q: %v", ref, err)
			return nil
		}
		var hit *model.Port
		for r, p := range c.m.Ports {
			if sel.MatchPort(r) {
				if hit != nil {
					c.errf("cabling %q matches more than one port", ref)
					return nil
				}
				hit = p
			}
		}
		if hit == nil {
			c.errf("cabling %q matches no port", ref)
		}
		return hit
	}
	for _, ref := range cb.Remove {
		if p := resolve(ref); p != nil && p.Link != nil {
			l := p.Link
			l.A.Link, l.B.Link = nil, nil
			delete(c.m.LinkBy, l.ID)
			for i, x := range c.m.Links {
				if x == l {
					c.m.Links = append(c.m.Links[:i], c.m.Links[i+1:]...)
					break
				}
			}
		}
	}
	for _, a := range cb.Add {
		pa, pb := resolve(a.A), resolve(a.B)
		kind := a.Kind
		if kind == "" {
			kind = "cable"
		}
		c.link(pa, pb, kind, "")
	}
}

func (c *compiler) domains() {
	made := map[string]bool{}
	for _, r := range c.m.Racks {
		for _, ds := range r.Format.Spec.Domains {
			if ds.Formation != "" {
				if err := expr.Check(expr.EnvRack, ds.Formation); err != nil {
					c.errf("format %s domain %s: %v", r.FormatName, ds.Kind, err)
				}
			}
			racks := []*model.Rack{r}
			if ds.Spans != "" {
				peerName := subst(ds.Spans, r.Params)
				peer := c.m.RackBy[r.Hall.Path+"/"+peerName]
				if peer == nil {
					c.errf("rack %s: peer rack %q not found", r.Name, peerName)
					continue
				}
				back := subst(ds.Spans, peer.Params)
				if back != r.Name {
					c.errf("rack %s: peer %s names %q as its peer; a spanning domain's peer must be mutual", r.Name, peerName, back)
					continue
				}
				racks = append(racks, peer)
				sort.Slice(racks, func(i, j int) bool { return racks[i].Path < racks[j].Path })
			}
			if ds.Unit == "server" {
				for _, d := range r.Devices {
					if d.BaseClass != "server" {
						continue
					}
					dom := &model.Domain{Path: d.Path + "/domain/" + ds.Kind, Kind: ds.Kind, Technology: ds.Technology, Racks: []*model.Rack{r}, State: "formed"}
					for _, comp := range d.Components {
						if comp.Kind == "accelerator" {
							dom.Members = append(dom.Members, comp.Path)
						}
					}
					c.m.Domains = append(c.m.Domains, dom)
					r.Domains = append(r.Domains, dom)
				}
				continue
			}
			path := racks[0].Path + "/domain/" + ds.Kind
			if ds.Technology != "" {
				path += "-" + ds.Technology
			}
			if made[path] {
				continue
			}
			made[path] = true
			dom := &model.Domain{Path: path, Kind: ds.Kind, Technology: ds.Technology, Formation: ds.Formation, Racks: racks, State: "formed"}
			if ds.Manager != "" {
				dom.Manager = racks[0].Path + "/" + ds.Manager
			}
			msel, err := selector.Parse(ds.Members)
			if err == nil {
				for _, rr := range racks {
					for _, d := range rr.Devices {
						for _, comp := range d.Components {
							if msel.MatchPath(strings.TrimPrefix(comp.Path, rr.Path+"/")) || ds.Members == "*" {
								dom.Members = append(dom.Members, comp.Path)
							}
						}
					}
				}
			}
			c.m.Domains = append(c.m.Domains, dom)
			for _, rr := range racks {
				rr.Domains = append(rr.Domains, dom)
			}
		}
	}
}

func (c *compiler) loops() {
	for _, lp := range c.est.Spec.Facility.Cooling.Loops {
		l := &model.Loop{Name: lp.Name, Profile: lp.Profile, CapacityKW: lp.CapacityKW, SupplyC: lp.SupplyC, Flow: true}
		if l.SupplyC == 0 {
			l.SupplyC = 30
		}
		for _, rn := range lp.Racks {
			var found *model.Rack
			for _, r := range c.m.Racks {
				if r.Name == rn {
					found = r
				}
			}
			if found == nil {
				c.errf("cooling loop %s: rack %s not found", lp.Name, rn)
				continue
			}
			l.Racks = append(l.Racks, found)
			found.Loop = l
		}
		c.m.Loops[lp.Name] = l
	}
	for _, r := range c.m.Racks {
		if fmt.Sprint(r.Format.Spec.Cooling["kind"]) == "liquid" && r.Loop == nil {
			c.warnf("rack %s is liquid-cooled and on no cooling loop; coolant temperatures use the default supply", r.Name)
		}
	}
}

// ---------------------------------------------------------------- validation and finish

func (c *compiler) validate() {
	sp := c.est.Spec
	if sp.Clock == "virtual" {
		for _, d := range c.m.Devices {
			if d.Fidelity == "vm" || d.Fidelity == "hardware" {
				c.errf("clock: virtual is refused: %s is at %s fidelity (D22)", d.Path, d.Fidelity)
				break
			}
		}
		for _, f := range sp.Fabrics {
			if f.NOS != "none" {
				c.errf("clock: virtual is refused: fabric %s runs a real NOS (%s) (D22)", f.Name, f.NOS)
			}
		}
		if len(sp.Stacks) > 0 {
			c.errf("clock: virtual is refused: a stack is installed (D22)")
		}
	}
	for _, o := range c.over {
		for _, d := range c.m.Devices {
			if d.Generated != "" && o.sel.HasPatterns() && o.sel.MatchPath(d.Path) && o.level != d.Fidelity {
				c.errf("fidelity override %q names switch %s, whose fidelity derives from its fabric's nos (D21)", o.sel.Raw, d.Path)
			}
		}
	}
	for _, s := range sp.Stacks {
		name, _, _ := strings.Cut(s.Name, "@")
		st, ok := c.reg.Stacks[name]
		if !ok {
			c.errf("stack %s: not found", s.Name)
			continue
		}
		for _, imp := range s.Impersonators {
			found := false
			for _, i := range st.Spec.Impersonators {
				if i.Name == imp {
					found = true
				}
			}
			if !found {
				c.errf("stack %s: impersonator %q is not one the stack lists", s.Name, imp)
			}
		}
	}
	if sp.Facility.BMS != nil {
		if _, ok := c.reg.Topics[sp.Facility.BMS.Dialect]; !ok {
			c.errf("facility.bms: dialect %q not found", sp.Facility.BMS.Dialect)
		}
	}
	for _, d := range c.m.Devices {
		for _, beh := range d.Profile.Spec.Behaviours {
			if req, ok := beh["requires"].(string); ok {
				if err := expr.Check(expr.EnvBehaviour, req); err != nil {
					c.errf("profile %s: %v", d.ProfileName, err)
				}
			}
		}
	}
}

func (c *compiler) finish() {
	sort.Slice(c.m.Devices, func(i, j int) bool { return c.m.Devices[i].Path < c.m.Devices[j].Path })
	sort.Slice(c.m.Links, func(i, j int) bool { return c.m.Links[i].ID < c.m.Links[j].ID })
	sort.Slice(c.m.Racks, func(i, j int) bool { return c.m.Racks[i].Path < c.m.Racks[j].Path })
	initial := c.est.Spec.InitialState
	for _, d := range c.m.Devices {
		st := &d.State
		st.Power = model.On
		if initial == "off" {
			st.Power = model.Off
		}
		st.AC, st.BMCUp = true, d.HasBMC()
		st.Booted = "os"
		st.Bios, st.BiosPending, st.Patches = map[string]any{}, map[string]any{}, map[string]map[string]any{}
		st.Sessions, st.Counters, st.Outlets = map[string]model.Session{}, map[string]float64{}, map[int]bool{}
		st.Firmware = map[string]*model.FirmwareState{}
		st.Utilisation = 0.5
		st.Boot = model.BootState{OverrideTarget: "None", OverrideEnabled: "Disabled", Order: []string{"Pxe", "Hdd"}}
		for _, fw := range d.Profile.Spec.Firmware {
			st.Firmware[fw.Component] = &model.FirmwareState{Component: fw.Component, Version: fw.Baseline, Baseline: fw.Baseline, ApplyOn: fw.ApplyOn}
		}
		if d.HasBMC() {
			st.Accounts = []model.Account{{ID: "1", User: d.Identity.BMCUser, Password: d.Identity.BMCPassword, Role: "Administrator", Enabled: true}}
		}
		if d.Profile.Spec.SNMP != nil {
			for i := 1; i <= d.Profile.Spec.SNMP.Outlets; i++ {
				st.Outlets[i] = true
			}
		}
	}
}

// Fail reports whether err is a refusal.
func Fail(err error) (*Error, bool) {
	e, ok := err.(*Error)
	return e, ok
}

var accelGBRe = regexp.MustCompile(`(?i)(\d+)\s*GB\b`)

// accelMemoryMiB is an accelerator's own memory: the profile's memoryGiB, else a "<n>GB" in its
// model string ("NVIDIA H100 80GB HBM3"), else 0 (unknown).
func accelMemoryMiB(cs schema.ComponentSpec) float64 {
	if cs.Kind != "accelerator" {
		return 0
	}
	if cs.MemoryMiB > 0 {
		return cs.MemoryMiB
	}
	if cs.MemoryGiB > 0 {
		return cs.MemoryGiB * 1024
	}
	if m := accelGBRe.FindStringSubmatch(cs.Model); m != nil {
		gb, _ := strconv.ParseFloat(m[1], 64)
		return gb * 1024
	}
	return 0
}
