package control

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Plan is a draft compiled without starting anything: what the twin would refuse, the diff to
// the estate file, the edited file itself, and what the change would mean — counts, power and
// cooling against capacity, fabric switches and cables, VM cost, stack capability, and the
// compiled layout of the draft for the console to draw.
type Plan struct {
	ApplyResult
	YAML    string         `json:"yaml,omitempty"`
	Summary map[string]any `json:"summary,omitempty"`
	Impact  *Impact        `json:"impact,omitempty"`
	Classes []string       `json:"classes,omitempty"` // per op: hot or cold (03 — Changing a running estate)
	Layout  map[string]any `json:"layout,omitempty"`
	Tree    any            `json:"tree,omitempty"` // the edited estate file as a JSON tree, for forms
}

type Impact struct {
	Racks   []RackImpact   `json:"racks"`
	Feeds   []FeedImpact   `json:"feeds"`
	Nodes   []NodeImpact   `json:"nodes"` // the power chain's UPS, busways and panels
	Loops   []LoopImpact   `json:"loops"`
	Fabrics []FabricImpact `json:"fabrics"`
	VM      VMImpact       `json:"vm"`
	Stacks  []StackImpact  `json:"stacks,omitempty"`
}

type RackImpact struct {
	Name      string   `json:"name"`
	Row       string   `json:"row"`
	Position  int      `json:"position"`
	Format    string   `json:"format"`
	Devices   int      `json:"devices"`
	IdleW     float64  `json:"idleW"`
	MaxW      float64  `json:"maxW"`
	CapacityW float64  `json:"capacityW"`
	Feeds     []string `json:"feeds"`
	Loop      string   `json:"loop,omitempty"`
	Liquid    bool     `json:"liquid,omitempty"`
}

// FeedImpact: the load a feed carries with both sides up (shared) and when it carries every rack
// on it alone (failover), at idle and at max draw.
type FeedImpact struct {
	Name          string  `json:"name"`
	CapacityW     float64 `json:"capacityW"`
	SharedMaxW    float64 `json:"sharedMaxW"`
	FailoverMaxW  float64 `json:"failoverMaxW"`
	SharedIdleW   float64 `json:"sharedIdleW"`
	FailoverIdleW float64 `json:"failoverIdleW"`
	Racks         int     `json:"racks"`
}

// NodeImpact is what a node of the power chain carries at the racks' max draw: its racks'
// share on its feed, and their whole draw if the other feed is lost.
type NodeImpact struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Feed         string   `json:"feed"`
	CapacityW    float64  `json:"capacityW"`
	RuntimeS     float64  `json:"runtimeS,omitempty"`
	SharedMaxW   float64  `json:"sharedMaxW"`
	FailoverMaxW float64  `json:"failoverMaxW"`
	Racks        []string `json:"racks"`
}

type LoopImpact struct {
	Name       string   `json:"name"`
	CapacityKW float64  `json:"capacityKW"`
	MaxKW      float64  `json:"maxKW"`
	Racks      []string `json:"racks"`
}

// FabricImpact: a fabric's generated switches and cables before and after the draft, the racks
// it reaches and the rack it is switched in.
type FabricImpact struct {
	Name       string   `json:"name"`
	Template   string   `json:"template,omitempty"`
	Technology string   `json:"technology,omitempty"`
	Switches   [2]int   `json:"switches"`
	Cables     [2]int   `json:"cables"`
	Racks      []string `json:"racks"`
	Hub        string   `json:"hub,omitempty"`
}

type VMImpact struct {
	Devices int     `json:"devices"`
	VCPU    float64 `json:"vcpu"`
	RAMGB   float64 `json:"ramGB"`
}

type StackImpact struct {
	Stack    string         `json:"stack"`
	Verdicts map[string]int `json:"verdicts"`
	Missing  []string       `json:"missing,omitempty"`
}

// legacyOps turns the first API's {racks, remove} body into intents: removals first, so a draft
// can replace a rack.
func legacyOps(adds []RackAdd, removes []string) []Op {
	var ops []Op
	for _, n := range removes {
		ops = append(ops, Op{Op: "rack.remove", Name: n})
	}
	for _, a := range adds {
		ops = append(ops, Op{Op: "rack.add", Hall: a.Hall, Row: a.Row, Name: a.Name, Position: a.Position, Format: a.Format, Params: a.Params})
	}
	return ops
}

// plan edits the running estate's file with ops and compiles the result, as `up` would. etag,
// when set, must be the file's current etag: a draft made against an older file is refused.
func (s *Server) plan(ops []Op, etag string) (*Plan, []byte, *schema.Estate, int) {
	t := s.T
	p := &Plan{ApplyResult: ApplyResult{Class: "restart", At: time.Now().UTC()}}
	p.Devices[0], p.Links[0] = len(t.Engine.M.Devices), len(t.Engine.M.Links)
	if t.File == "" {
		p.Problems = []string{"this estate was not started from a file, so there is nothing to change"}
		return p, nil, nil, 400
	}
	src, err := os.ReadFile(t.File)
	if err != nil {
		p.Problems = []string{err.Error()}
		return p, nil, nil, 500
	}
	if now := etagOf(src); etag != "" && etag != now {
		p.Problems = []string{fmt.Sprintf("the estate file changed since this draft was made (draft etag %s, file %s): reload and make the draft again", etag, now)}
		return p, nil, nil, 409
	}
	for _, o := range ops {
		p.Classes = append(p.Classes, classOf(o))
	}
	// no ops plans the file as it is: the designer's baseline (its impact, layout and tree)
	out, notes, err := src, []string(nil), error(nil)
	if len(ops) > 0 {
		out, notes, err = editOps(src, ops)
	}
	if err != nil {
		p.Problems = []string{err.Error()}
		return p, nil, nil, 400
	}
	p.Notes, p.Diff, p.YAML = notes, unifiedDiff(string(src), string(out), 3), string(out)
	_ = schema.DecodeAny(out, &p.Tree)
	for _, o := range ops {
		switch o.Op {
		case "rack.add":
			p.RacksAdded = append(p.RacksAdded, o.Name)
		case "rack.remove":
			p.RacksRemoved = append(p.RacksRemoved, o.Name)
		}
	}
	var est schema.Estate
	if err := schema.Decode(out, &est); err != nil {
		p.Problems = []string{err.Error()}
		return p, nil, nil, 400
	}
	cr, err := compile.Compile(&est, t.Reg, compile.Options{HostArch: compile.HostArch(), AllowTCG: t.Runtime.AllowTCG})
	if err != nil {
		if ce, ok := compile.Fail(err); ok {
			p.Problems = ce.Problems
		} else {
			p.Problems = []string{err.Error()}
		}
		return p, nil, nil, 400
	}
	p.OK = true
	p.Warnings = cr.Warnings
	p.Devices[1], p.Links[1] = len(cr.Model.Devices), len(cr.Model.Links)
	var before *model.Model
	t.Engine.Read(func(m *model.Model) { before = m })
	p.Summary = map[string]any{"devices": p.Devices, "links": p.Links, "ports": [2]int{len(before.Ports), len(cr.Model.Ports)},
		"racks": [2]int{len(before.Racks), len(cr.Model.Racks)}}
	p.Impact = impactOf(before, cr.Model, &est, s)
	p.Layout = layoutOf(cr.Model, est.Metadata.Name)
	return p, out, &est, 200
}

// ---------------------------------------------------------------- impact

func impactOf(before, after *model.Model, est *schema.Estate, s *Server) *Impact {
	im := &Impact{}
	var draw func(d *model.Device) (idle, max float64)
	draw = func(d *model.Device) (idle, max float64) {
		p := d.Profile.Spec.Power
		idle, max = p.IdleW, p.MaxW
		for _, c := range d.Children {
			ci, cm := draw(c)
			idle, max = idle+ci, max+cm
		}
		return
	}
	feedW := map[string]*FeedImpact{}
	for _, f := range est.Spec.Facility.Power.Feeds {
		feedW[f.Name] = &FeedImpact{Name: f.Name, CapacityW: f.CapacityW}
	}
	for _, r := range after.Racks {
		ri := RackImpact{Name: r.Name, Position: r.Position, Format: r.FormatName, Devices: len(r.Devices)}
		if r.Row != nil {
			ri.Row = r.Row.Name
		}
		for _, d := range r.Devices {
			i, m := draw(d)
			ri.IdleW += i
			ri.MaxW += m
		}
		feeds := map[string]bool{}
		for _, in := range r.Inputs {
			ri.CapacityW += in.CapacityW
			for _, f := range in.SourceFeed {
				feeds[f] = true
			}
		}
		for _, f := range r.Feeds {
			feeds[f] = true
		}
		for f := range feeds {
			ri.Feeds = append(ri.Feeds, f)
		}
		sort.Strings(ri.Feeds)
		if r.Loop != nil {
			ri.Loop = r.Loop.Name
		}
		if k, _ := r.Format.Spec.Cooling["kind"].(string); k == "liquid" {
			ri.Liquid = true
		}
		n := float64(len(ri.Feeds))
		for _, f := range ri.Feeds {
			if fi := feedW[f]; fi != nil {
				fi.Racks++
				fi.SharedMaxW += ri.MaxW / n
				fi.SharedIdleW += ri.IdleW / n
				fi.FailoverMaxW += ri.MaxW
				fi.FailoverIdleW += ri.IdleW
			}
		}
		im.Racks = append(im.Racks, ri)
	}
	for _, f := range est.Spec.Facility.Power.Feeds {
		im.Feeds = append(im.Feeds, *feedW[f.Name])
	}
	byRack := map[string]RackImpact{}
	for _, r := range im.Racks {
		byRack[r.Name] = r
	}
	for _, n := range after.Power {
		ni := NodeImpact{Name: n.Name, Kind: n.Kind, CapacityW: n.CapacityW, RuntimeS: n.RuntimeS, Racks: []string{}}
		if n.Feed != nil {
			ni.Feed = n.Feed.Name
		}
		// the racks whose sources draw through the node or a node below it
		reached := map[string]bool{}
		var walk func(x *model.PowerNode)
		walk = func(x *model.PowerNode) {
			for _, s := range x.Sources {
				if r := s.RackOf(); r != nil {
					reached[r.Name] = true
				}
			}
			for _, ch := range x.Children {
				walk(ch)
			}
		}
		walk(n)
		for name := range reached {
			r := byRack[name]
			ni.Racks = append(ni.Racks, name)
			ni.SharedMaxW += r.MaxW / math.Max(1, float64(len(r.Feeds)))
			ni.FailoverMaxW += r.MaxW
		}
		sort.Strings(ni.Racks)
		im.Nodes = append(im.Nodes, ni)
	}
	for _, lp := range est.Spec.Facility.Cooling.Loops {
		li := LoopImpact{Name: lp.Name, CapacityKW: lp.CapacityKW, Racks: lp.Racks}
		for _, n := range lp.Racks {
			li.MaxKW += byRack[n].MaxW / 1000
		}
		im.Loops = append(im.Loops, li)
	}
	// fabrics: switches and cables before and after, the racks they reach and the hub
	count := func(m *model.Model) (sw, cab map[string]int) {
		sw, cab = map[string]int{}, map[string]int{}
		for _, d := range m.Devices {
			if d.Generated != "" {
				sw[d.Generated]++
			}
		}
		for _, l := range m.Links {
			if l.Kind == "cable" && l.Fabric != "" {
				cab[l.Fabric]++
			}
		}
		return
	}
	sw0, cab0 := count(before)
	sw1, cab1 := count(after)
	names := map[string]bool{}
	for n := range sw0 {
		names[n] = true
	}
	for n := range cab0 {
		names[n] = true
	}
	for n := range after.Fabrics {
		names[n] = true
	}
	racksOf, hubOf := fabricReach(after)
	for n := range names {
		fi := FabricImpact{Name: n, Switches: [2]int{sw0[n], sw1[n]}, Cables: [2]int{cab0[n], cab1[n]}, Racks: racksOf[n], Hub: hubOf[n]}
		if f := after.Fabrics[n]; f != nil {
			fi.Template, fi.Technology = f.Template, f.Technology
		}
		im.Fabrics = append(im.Fabrics, fi)
	}
	sort.Slice(im.Fabrics, func(i, j int) bool { return im.Fabrics[i].Name < im.Fabrics[j].Name })
	// VM cost of every device the draft runs as a VM
	for _, d := range after.Devices {
		if d.Fidelity != "vm" {
			continue
		}
		im.VM.Devices++
		if c, ok := d.Profile.Spec.Fidelity["vm"]["cost"].(map[string]any); ok {
			im.VM.VCPU += num(c["vcpu"])
			im.VM.RAMGB += num(c["ramGB"])
		}
	}
	// stack capability against the draft
	for _, st := range est.Spec.Stacks {
		name := strings.SplitN(st.Name, "@", 2)[0]
		vs, err := compile.Check(after, s.T.Reg, name)
		si := StackImpact{Stack: name, Verdicts: map[string]int{}}
		if err != nil {
			si.Missing = []string{err.Error()}
		}
		for _, v := range vs {
			si.Verdicts[v.Verdict]++
			if v.Verdict == "missing" {
				si.Missing = append(si.Missing, fmt.Sprintf("%s: %s", v.Component, v.Capability))
			}
		}
		im.Stacks = append(im.Stacks, si)
	}
	return im
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

// fabricReach: per cabled fabric, the racks with a port on it, and its hub — the rack whose
// switches end the most cables from other racks, where the fabric is switched.
func fabricReach(m *model.Model) (map[string][]string, map[string]string) {
	racks, sw := map[string]map[string]bool{}, map[string]map[string]int{}
	rackOf := func(p *model.Port) string {
		if p == nil || p.Device == nil {
			return ""
		}
		if r := p.Device.RackOf(); r != nil {
			return r.Name
		}
		return ""
	}
	for _, l := range m.Links {
		if l.Kind != "cable" || l.Fabric == "" {
			continue
		}
		if racks[l.Fabric] == nil {
			racks[l.Fabric], sw[l.Fabric] = map[string]bool{}, map[string]int{}
		}
		a, b := rackOf(l.A), rackOf(l.B)
		for _, r := range []string{a, b} {
			if r != "" {
				racks[l.Fabric][r] = true
			}
		}
		if a != "" && b != "" && a != b {
			for _, e := range []*model.Port{l.A, l.B} {
				if e.Device.BaseClass == "switch" || e.Device.Class == "switch" {
					sw[l.Fabric][rackOf(e)]++
				}
			}
		}
	}
	outR, outH := map[string][]string{}, map[string]string{}
	for f, rs := range racks {
		for r := range rs {
			outR[f] = append(outR[f], r)
		}
		sort.Strings(outR[f])
		best, n := "", 0
		for r, c := range sw[f] {
			if c > n || (c == n && r < best) {
				best, n = r, c
			}
		}
		outH[f] = best
	}
	return outR, outH
}

// layoutOf is what the console draws from (12, D18): halls, rows with their positions, racks
// with their format's height, and every device's slot, U and state.
func layoutOf(m *model.Model, estate string) map[string]any {
	var halls []any
	for _, h := range m.Halls {
		var rows []any
		for _, row := range h.Rows {
			var racks []any
			for _, r := range row.Racks {
				var devs []any
				for _, d := range r.Devices {
					devs = append(devs, map[string]any{"path": d.Path, "name": d.Name, "slot": d.Slot, "u": d.U, "units": d.Units,
						"side": d.Side, "class": d.Class, "profile": d.ProfileName, "fidelity": d.Fidelity, "generated": d.Generated})
				}
				racks = append(racks, map[string]any{"name": r.Name, "path": r.Path, "position": r.Position, "format": r.FormatName,
					"archetype": r.Format.Spec.Archetype, "height": r.Format.Spec.Layout.Height, "params": r.Params, "feeds": r.Feeds, "devices": devs})
			}
			rows = append(rows, map[string]any{"name": row.Name, "positions": row.Positions, "racks": racks})
		}
		halls = append(halls, map[string]any{"name": h.Name, "path": h.Path, "rows": rows})
	}
	return map[string]any{"estate": estate, "site": m.Site, "halls": halls}
}

// ---------------------------------------------------------------- diff

// unifiedDiff is a line diff of two texts in unified form, with ctx lines of context.
func unifiedDiff(a, b string, ctx int) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	n, m := len(x), len(y)
	mx := matchLines(n, m, func(i, j int) bool { return x[i] == y[j] })
	type line struct {
		op   byte // ' ', '-', '+'
		text string
		ai   int // line number in a (1-based) for ' ' and '-'
		bi   int
	}
	var ls []line
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && mx[i] == j:
			ls = append(ls, line{' ', x[i], i + 1, j + 1})
			i++
			j++
		case i < n && mx[i] < 0:
			ls = append(ls, line{'-', x[i], i + 1, j + 1}) // removals before additions, as diff(1) writes them
			i++
		default:
			ls = append(ls, line{'+', y[j], i + 1, j + 1})
			j++
		}
	}
	var out strings.Builder
	for k := 0; k < len(ls); {
		if ls[k].op == ' ' {
			k++
			continue
		}
		start := k - ctx
		if start < 0 {
			start = 0
		}
		end := k
		for end < len(ls) {
			if ls[end].op != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ls) && ls[run].op == ' ' {
				run++
			}
			if run-end > 2*ctx || run == len(ls) {
				end += ctx
				if end > len(ls) {
					end = len(ls)
				}
				break
			}
			end = run
		}
		na, nb := 0, 0
		for _, l := range ls[start:end] {
			if l.op != '+' {
				na++
			}
			if l.op != '-' {
				nb++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", ls[start].ai, na, ls[start].bi, nb)
		for _, l := range ls[start:end] {
			out.WriteByte(l.op)
			out.WriteString(l.text)
			out.WriteByte('\n')
		}
		k = end
	}
	return out.String()
}
