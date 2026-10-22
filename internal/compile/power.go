package compile

import (
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/selector"
)

// powerChain compiles facility.power.topology, the one-line diagram between the feeds and the
// racks (13): each UPS, busway and power panel becomes a facility device at
// <hall>/facility/power/<name>, and each rack power source learns the node it draws through.
// A source draws through the deepest node of its feed's chain that serves its rack; a source
// whose feed reaches its rack through no node is fed by the feed itself, as before.
func (c *compiler) powerChain() {
	top := c.est.Spec.Facility.Power.Topology
	c.m.PowerBy = map[string]*model.PowerNode{}
	c.idleFeeds()
	if len(top) == 0 {
		return
	}
	if len(c.m.Halls) == 0 {
		c.errf("facility.power.topology: the estate has no hall to place its nodes in")
		return
	}
	hall := c.m.Halls[0]
	byName := map[string]*model.PowerNode{}
	from := map[string]string{}
	for _, pn := range top {
		where := "facility.power.topology " + pn.Name
		switch {
		case pn.Name == "":
			c.errf("facility.power.topology: a node has no name")
			continue
		case byName[pn.Name] != nil:
			c.errf("%s: the name is used twice", where)
			continue
		case c.m.Feeds[pn.Name] != nil:
			c.errf("%s: a feed has the same name", where)
			continue
		}
		if pn.Kind != "ups" && pn.Kind != "busway" && pn.Kind != "pdu-panel" {
			c.errf("%s: kind %q: ups, busway or pdu-panel", where, pn.Kind)
			continue
		}
		if pn.CapacityW < 0 || pn.RuntimeS < 0 {
			c.errf("%s: capacityW and runtimeS are not negative", where)
		}
		if pn.Kind == "ups" && pn.RuntimeS == 0 {
			c.warnf("%s: a UPS with no runtimeS rides through nothing: its battery is empty as soon as its input is lost", where)
		}
		if pn.Kind != "ups" && pn.RuntimeS != 0 {
			c.errf("%s: runtimeS is a UPS's; a %s has no battery", where, pn.Kind)
		}
		prof := pn.Profile
		if prof == "" {
			prof = "generic/" + pn.Kind
		}
		p := c.profile(prof, where)
		if p == nil {
			continue
		}
		if p.Spec.Class != pn.Kind {
			c.errf("%s: profile %s is a %s, not a %s", where, prof, p.Spec.Class, pn.Kind)
			continue
		}
		d := c.newDevice(nil, hall, hall.Path+"/facility/power", pn.Name, p, nil)
		n := &model.PowerNode{Name: pn.Name, Kind: pn.Kind, Device: d, CapacityW: pn.CapacityW, RuntimeS: pn.RuntimeS, Charge: 1}
		byName[pn.Name], from[pn.Name] = n, pn.From
		c.m.Power = append(c.m.Power, n)
		c.m.PowerBy[d.Path] = n
	}
	// each node's input: a feed or another node, with no cycles
	for _, n := range c.m.Power {
		f := from[n.Name]
		switch {
		case c.m.Feeds[f] != nil:
			n.Feed = c.m.Feeds[f]
		case byName[f] != nil:
			n.Parent = byName[f]
		default:
			c.errf("facility.power.topology %s: from %q is neither a feed nor a node of the chain", n.Name, f)
		}
	}
	for _, n := range c.m.Power {
		seen := map[*model.PowerNode]bool{}
		for x := n; x.Parent != nil; x = x.Parent {
			if seen[x] {
				c.errf("facility.power.topology %s: its chain loops back on itself", n.Name)
				n.Parent, n.Feed = nil, nil
				break
			}
			seen[x] = true
		}
	}
	for _, n := range c.m.Power {
		if n.Parent != nil {
			n.Parent.Children = append(n.Parent.Children, n)
		}
		for x := n; x != nil && n.Feed == nil; x = x.Parent {
			n.Feed = x.Feed
		}
	}
	// parents before children, in the order the file names them
	ordered := make([]*model.PowerNode, 0, len(c.m.Power))
	var place func(n *model.PowerNode)
	done := map[*model.PowerNode]bool{}
	place = func(n *model.PowerNode) {
		if done[n] {
			return
		}
		done[n] = true
		if n.Parent != nil {
			place(n.Parent)
		}
		ordered = append(ordered, n)
	}
	for _, n := range c.m.Power {
		place(n)
	}
	c.m.Power = ordered
	// the racks each node serves
	for _, pn := range top {
		n := byName[pn.Name]
		if n == nil {
			continue
		}
		for _, s := range pn.Serves {
			sel, err := selector.Parse(s)
			if err != nil {
				c.errf("facility.power.topology %s: serves %q: %v", n.Name, s, err)
				continue
			}
			hit := false
			for _, r := range c.m.Racks {
				if sel.MatchPath(r.Path) && !containsRack(n.Racks, r) {
					n.Racks, hit = append(n.Racks, r), true
				}
			}
			if !hit {
				c.warnf("facility.power.topology %s: serves %q matches no rack", n.Name, s)
			}
		}
	}
	// each rack source: the deepest node of its feed's chain that serves the rack (an ambiguity
	// is one problem per rack, feed and pair of nodes, however many of its sources it touches)
	ambiguous := map[string]bool{}
	for _, r := range c.m.Racks {
		for _, in := range r.Inputs {
			in.SourceNode = map[string]*model.PowerNode{}
			for _, s := range in.Sources {
				feed := c.m.Feeds[in.SourceFeed[s.Path]]
				if feed == nil {
					continue
				}
				var at *model.PowerNode
				for _, n := range c.m.Power {
					if n.Feed != feed || !containsRack(n.Racks, r) {
						continue
					}
					switch {
					case at == nil || below(n, at):
						at = n
					case !below(at, n):
						if k := r.Name + "|" + feed.Name + "|" + at.Name + "|" + n.Name; !ambiguous[k] {
							ambiguous[k] = true
							c.errf("rack %s: feed %s reaches it through both %s and %s, which are on different branches of the chain: serve it from one of them", r.Name, feed.Name, at.Name, n.Name)
						}
					}
				}
				if at != nil {
					in.SourceNode[s.Path] = at
					at.Sources = append(at.Sources, s)
				}
			}
		}
	}
	// what each node carries at the racks' max draw, against its capacity
	for _, n := range c.m.Power {
		if len(n.Racks) > 0 && len(n.Sources) == 0 {
			c.warnf("facility.power.topology %s: it serves %s, but none of their power sources draws from feed %s", n.Name, rackNames(n.Racks), feedName(n.Feed))
		}
		if len(n.Racks) == 0 && len(n.Children) == 0 {
			c.warnf("facility.power.topology %s: it serves no rack", n.Name)
		}
	}
	for _, n := range c.m.Power {
		if w := c.chainMaxW(n); n.CapacityW > 0 && w > n.CapacityW {
			c.warnf("facility.power.topology %s: its racks draw up to %.0f kW at max, over its %.0f kW capacity", n.Name, w/1000, n.CapacityW/1000)
		}
	}
}

// idleFeeds warns about a feed no rack power source draws from: a rack takes the first feeds
// of the estate unless it names its own (racks[].feeds), so a feed added last feeds nothing
// until racks name it.
func (c *compiler) idleFeeds() {
	used := map[string]bool{}
	for _, r := range c.m.Racks {
		for _, in := range r.Inputs {
			for _, f := range in.SourceFeed {
				used[f] = true
			}
		}
	}
	for _, f := range c.est.Spec.Facility.Power.Feeds {
		if !used[f.Name] {
			c.warnf("feed %s: no rack draws from it (a rack takes the first feeds unless its feeds name others)", f.Name)
		}
	}
}

// chainMaxW is what a node carries when every device it feeds draws its max: for each rack
// whose sources draw through the node or a node below it, the rack's max draw shared over the
// feeds the rack draws from (the plan's impact counts it the same way).
func (c *compiler) chainMaxW(n *model.PowerNode) float64 {
	racks := map[*model.Rack]bool{}
	var walk func(x *model.PowerNode)
	walk = func(x *model.PowerNode) {
		for _, s := range x.Sources {
			if r := s.RackOf(); r != nil {
				racks[r] = true
			}
		}
		for _, ch := range x.Children {
			walk(ch)
		}
	}
	walk(n)
	w := 0.0
	for r := range racks {
		feeds := map[string]bool{}
		for _, in := range r.Inputs {
			for _, f := range in.SourceFeed {
				feeds[f] = true
			}
		}
		for _, f := range r.Feeds {
			feeds[f] = true
		}
		sum := 0.0
		for _, d := range r.Devices {
			sum += maxDraw(d)
		}
		w += sum / float64(max(1, len(feeds)))
	}
	return w
}

func maxDraw(d *model.Device) float64 {
	w := d.Profile.Spec.Power.MaxW
	for _, ch := range d.Children {
		w += maxDraw(ch)
	}
	return w
}

// below reports whether a is b or downstream of it.
func below(a, b *model.PowerNode) bool {
	for x := a; x != nil; x = x.Parent {
		if x == b {
			return true
		}
	}
	return false
}

func containsRack(rs []*model.Rack, r *model.Rack) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

func rackNames(rs []*model.Rack) string {
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func feedName(f *model.Feed) string {
	if f == nil {
		return "(none)"
	}
	return f.Name
}
