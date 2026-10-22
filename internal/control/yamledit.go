package control

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Op is one edit in a draft of the estate file (09 — Drafts; 12-ui — Design mode). A generic
// op addresses a node by path and sets, adds, removes or moves it. An intent (rack.add,
// rack.remove, …) is a change that must keep the rest of the file consistent: removing a rack
// also takes it out of the cooling loops and the cabling that name it, renaming one renames it
// wherever the file refers to it.
//
// Path segments: a mapping key (string), an index (number), {"name": x} (the sequence element
// whose name is x: the merge key the spec gives for rebase) or "-" (the end of a sequence, for
// add). Missing mappings and sequences along an add or set path are created.
type Op struct {
	Op    string `json:"op"`
	Path  []any  `json:"path,omitempty"`
	Value any    `json:"value,omitempty"`
	To    []any  `json:"to,omitempty"`

	// intent arguments
	Hall       string         `json:"hall,omitempty"`
	Row        string         `json:"row,omitempty"`
	Name       string         `json:"name,omitempty"`
	Format     string         `json:"format,omitempty"`
	Position   int            `json:"position,omitempty"`
	Positions  int            `json:"positions,omitempty"`
	Params     map[string]any `json:"params,omitempty"`
	NewName    string         `json:"newName,omitempty"`
	ToHall     string         `json:"toHall,omitempty"`
	ToRow      string         `json:"toRow,omitempty"`
	ToPosition int            `json:"toPosition,omitempty"`
}

// estateDoc is the estate file as a YAML node tree, edited in place so that comments, key order
// and the author's layout survive every change but the one made.
type estateDoc struct {
	root  *yaml.Node // the document's top mapping
	doc   yaml.Node
	notes []string
}

func parseEstateDoc(src []byte) (*estateDoc, error) {
	d := &estateDoc{}
	if err := yaml.Unmarshal(src, &d.doc); err != nil {
		return nil, err
	}
	if len(d.doc.Content) == 0 || d.doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the estate file is empty or not a mapping")
	}
	d.root = d.doc.Content[0]
	return d, nil
}

func (d *estateDoc) bytes() ([]byte, error) {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&d.doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (d *estateDoc) note(f string, a ...any) { d.notes = append(d.notes, fmt.Sprintf(f, a...)) }

// ---------------------------------------------------------------- node helpers

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			// keep a comment written beside the old value, and the quoting of a scalar
			old := m.Content[i+1]
			if v.LineComment == "" {
				v.LineComment = old.LineComment
			}
			if v.Kind == yaml.ScalarNode && old.Kind == yaml.ScalarNode && old.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 && v.Tag != "!!int" && v.Tag != "!!float" && v.Tag != "!!bool" {
				v.Style = old.Style
			}
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

func mapDel(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

func nameOf(n *yaml.Node) string {
	if v := mapGet(n, "name"); v != nil {
		return v.Value
	}
	return ""
}

func seqIndexByName(s *yaml.Node, name string) int {
	for i, n := range nodes(s) {
		if nameOf(n) == name {
			return i
		}
	}
	return -1
}

func nodes(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

// keyOrder is the order an estate file writes keys in; others follow alphabetically.
var keyOrder = map[string]int{"name": 0, "kind": 1, "role": 2, "position": 3, "positions": 4, "format": 5, "template": 6, "technology": 7,
	"nos": 8, "manager": 9, "profile": 10, "from": 11, "select": 12, "placeIn": 13, "level": 14, "os": 15, "a": 16, "b": 17,
	"capacityW": 18, "capacityKW": 19, "runtimeS": 20, "serves": 21, "racks": 22, "feeds": 23, "params": 24, "slots": 25, "rows": 26}

func sortPairs(m *yaml.Node, order map[string]int) {
	type kv struct{ k, v *yaml.Node }
	var ps []kv
	for i := 0; i+1 < len(m.Content); i += 2 {
		ps = append(ps, kv{m.Content[i], m.Content[i+1]})
	}
	rank := func(k string) int {
		if r, ok := order[k]; ok {
			return r
		}
		return 1000
	}
	sort.SliceStable(ps, func(i, j int) bool {
		ri, rj := rank(ps[i].k.Value), rank(ps[j].k.Value)
		if ri != rj {
			return ri < rj
		}
		if ri == 1000 {
			return ps[i].k.Value < ps[j].k.Value
		}
		return false
	})
	m.Content = m.Content[:0]
	for _, p := range ps {
		m.Content = append(m.Content, p.k, p.v)
	}
}

// encodeValue turns a JSON value into YAML nodes the way the estate files are written: keys in
// estate order, whole numbers as integers, and small mappings and scalar lists on one line.
func encodeValue(v any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(normaliseJSON(v)); err != nil {
		return nil, err
	}
	var tidy func(*yaml.Node) bool // reports whether the node is "small"
	tidy = func(x *yaml.Node) bool {
		switch x.Kind {
		case yaml.MappingNode:
			sortPairs(x, keyOrder)
			small := len(x.Content) <= 12
			for i := 1; i < len(x.Content); i += 2 {
				if !tidy(x.Content[i]) {
					small = false
				}
			}
			if small {
				x.Style = yaml.FlowStyle
			}
			return small && len(x.Content) <= 8
		case yaml.SequenceNode:
			small := len(x.Content) <= 16
			for _, c := range x.Content {
				if c.Kind != yaml.ScalarNode {
					small = false
				}
				tidy(c)
			}
			if small {
				x.Style = yaml.FlowStyle
			}
			return small
		case yaml.ScalarNode:
			// a selector or pattern is quoted, as estate files write them ("a0*", "{b01,b02}"):
			// unquoted, *x would read as an alias
			if x.Tag == "!!str" && strings.ContainsAny(x.Value, "*{}[],?&!|>%@`") {
				x.Style = yaml.DoubleQuotedStyle
			}
		}
		return true
	}
	tidy(&n)
	return &n, nil
}

// normaliseJSON makes JSON numbers that are whole into ints, so YAML writes 4, not 4.0.
func normaliseJSON(v any) any {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normaliseJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normaliseJSON(e)
		}
		return out
	}
	return v
}

// ---------------------------------------------------------------- paths

func segString(s any) string {
	switch x := s.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("[%d]", int(x))
	case int:
		return fmt.Sprintf("[%d]", x)
	case map[string]any:
		return fmt.Sprintf("[%v]", x["name"])
	}
	return fmt.Sprint(s)
}

func pathString(p []any) string {
	var b strings.Builder
	for i, s := range p {
		if i > 0 {
			b.WriteString(".")
		}
		b.WriteString(segString(s))
	}
	return b.String()
}

// child steps one segment into n. create makes a missing mapping key, holding a mapping or a
// sequence depending on the segment after it.
func child(n *yaml.Node, seg, next any, create bool) (*yaml.Node, error) {
	switch s := seg.(type) {
	case string:
		if n.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%q: not a mapping", s)
		}
		c := mapGet(n, s)
		if c == nil && create {
			c = &yaml.Node{Kind: yaml.MappingNode}
			switch next.(type) {
			case float64, int, map[string]any:
				c = &yaml.Node{Kind: yaml.SequenceNode}
			case string:
				if next == "-" {
					c = &yaml.Node{Kind: yaml.SequenceNode}
				}
			}
			mapSet(n, s, c)
		}
		if c == nil {
			return nil, fmt.Errorf("no %q", s)
		}
		return c, nil
	case float64, int:
		i := toIndex(s)
		if n.Kind != yaml.SequenceNode || i < 0 || i >= len(n.Content) {
			return nil, fmt.Errorf("no element %d", i)
		}
		return n.Content[i], nil
	case map[string]any:
		name := fmt.Sprint(s["name"])
		i := seqIndexByName(n, name)
		if i < 0 {
			return nil, fmt.Errorf("no %q", name)
		}
		return n.Content[i], nil
	}
	return nil, fmt.Errorf("bad path segment %v", seg)
}

func toIndex(s any) int {
	switch x := s.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return -1
}

// walk follows path from the root; create builds missing mappings and sequences on the way.
func (d *estateDoc) walk(path []any, create bool) (*yaml.Node, error) {
	return d.walkN(path, len(path), create)
}

// parentOf is the node that holds path's last segment, built if missing: a sequence when the
// last segment is "-" or an index, a mapping otherwise.
func (d *estateDoc) parentOf(path []any) (*yaml.Node, error) {
	return d.walkN(path, len(path)-1, true)
}

func (d *estateDoc) walkN(path []any, n int, create bool) (*yaml.Node, error) {
	at := d.root
	for i, seg := range path[:n] {
		var next any
		if i+1 < len(path) {
			next = path[i+1]
		}
		c, err := child(at, seg, next, create)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", pathString(path[:i+1]), err)
		}
		at = c
	}
	return at, nil
}

// ---------------------------------------------------------------- generic ops

func (d *estateDoc) set(path []any, v any) error {
	if len(path) == 0 {
		return errors.New("set: empty path")
	}
	parent, err := d.parentOf(path)
	if err != nil {
		return err
	}
	val, err := encodeValue(v)
	if err != nil {
		return err
	}
	switch s := path[len(path)-1].(type) {
	case string:
		if parent.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: not a mapping", pathString(path))
		}
		mapSet(parent, s, val)
	case float64, int, map[string]any:
		i := -1
		if m, ok := s.(map[string]any); ok {
			i = seqIndexByName(parent, fmt.Sprint(m["name"]))
		} else {
			i = toIndex(s)
		}
		if parent.Kind != yaml.SequenceNode || i < 0 || i >= len(parent.Content) {
			return fmt.Errorf("%s: no such element", pathString(path))
		}
		parent.Content[i] = val
	default:
		return fmt.Errorf("%s: bad last segment", pathString(path))
	}
	return nil
}

// add appends to (path ends in "-") or inserts into (path ends in an index) a sequence, or adds
// a mapping key that is not there yet.
func (d *estateDoc) add(path []any, v any) error {
	if len(path) == 0 {
		return errors.New("add: empty path")
	}
	last := path[len(path)-1]
	parent, err := d.parentOf(path)
	if err != nil {
		return err
	}
	val, err := encodeValue(v)
	if err != nil {
		return err
	}
	switch s := last.(type) {
	case string:
		if s == "-" {
			if parent.Kind != yaml.SequenceNode {
				return fmt.Errorf("%s: not a sequence", pathString(path[:len(path)-1]))
			}
			if name := nameOf(val); name != "" && seqIndexByName(parent, name) >= 0 {
				return fmt.Errorf("%s: %q is already there", pathString(path[:len(path)-1]), name)
			}
			parent.Content = append(parent.Content, val)
			return nil
		}
		if parent.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: not a mapping", pathString(path))
		}
		if mapGet(parent, s) != nil {
			return fmt.Errorf("%s: already set", pathString(path))
		}
		mapSet(parent, s, val)
	case float64, int:
		i := toIndex(s)
		if parent.Kind != yaml.SequenceNode || i < 0 || i > len(parent.Content) {
			return fmt.Errorf("%s: no such position", pathString(path))
		}
		parent.Content = append(parent.Content[:i], append([]*yaml.Node{val}, parent.Content[i:]...)...)
	default:
		return fmt.Errorf("%s: bad last segment", pathString(path))
	}
	return nil
}

// remove deletes a mapping key or a sequence element, and returns what it took out.
func (d *estateDoc) remove(path []any) (*yaml.Node, error) {
	if len(path) == 0 {
		return nil, errors.New("remove: empty path")
	}
	parent, err := d.walk(path[:len(path)-1], false)
	if err != nil {
		return nil, err
	}
	switch s := path[len(path)-1].(type) {
	case string:
		v := mapGet(parent, s)
		if v == nil || !mapDel(parent, s) {
			return nil, fmt.Errorf("%s: not set", pathString(path))
		}
		return v, nil
	case float64, int, map[string]any:
		i := -1
		if m, ok := s.(map[string]any); ok {
			i = seqIndexByName(parent, fmt.Sprint(m["name"]))
		} else {
			i = toIndex(s)
		}
		if parent.Kind != yaml.SequenceNode || i < 0 || i >= len(parent.Content) {
			return nil, fmt.Errorf("%s: no such element", pathString(path))
		}
		v := parent.Content[i]
		parent.Content = append(parent.Content[:i:i], parent.Content[i+1:]...)
		return v, nil
	}
	return nil, fmt.Errorf("%s: bad last segment", pathString(path))
}

// move takes the node at path out and adds it at to (a sequence path ending in "-" or an index).
func (d *estateDoc) move(path, to []any) error {
	n, err := d.remove(path)
	if err != nil {
		return err
	}
	if len(to) == 0 {
		return errors.New("move: no destination")
	}
	parent, err := d.parentOf(to)
	if err != nil {
		return err
	}
	if parent.Kind != yaml.SequenceNode {
		return fmt.Errorf("%s: not a sequence", pathString(to[:len(to)-1]))
	}
	switch s := to[len(to)-1].(type) {
	case string:
		if s != "-" {
			return fmt.Errorf("%s: move into a sequence, ending in - or an index", pathString(to))
		}
		parent.Content = append(parent.Content, n)
	default:
		i := toIndex(s)
		if i < 0 || i > len(parent.Content) {
			return fmt.Errorf("%s: no such position", pathString(to))
		}
		parent.Content = append(parent.Content[:i], append([]*yaml.Node{n}, parent.Content[i:]...)...)
	}
	return nil
}

// ---------------------------------------------------------------- the estate's places

func (d *estateDoc) sites() *yaml.Node { return mapGet(mapGet(d.root, "spec"), "sites") }

// hall finds a hall by name ("" = the first hall of the first site).
func (d *estateDoc) hall(name string) (*yaml.Node, error) {
	for _, site := range nodes(d.sites()) {
		for _, h := range nodes(mapGet(site, "halls")) {
			if name == "" || nameOf(h) == name {
				return h, nil
			}
		}
	}
	if name == "" {
		return nil, errors.New("the estate has no hall")
	}
	return nil, fmt.Errorf("no hall %q", name)
}

func (d *estateDoc) row(hall, name string) (*yaml.Node, *yaml.Node, error) {
	h, err := d.hall(hall)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range nodes(mapGet(h, "rows")) {
		if nameOf(r) == name {
			return h, r, nil
		}
	}
	return nil, nil, fmt.Errorf("hall %s has no row %q", nameOf(h), name)
}

type rackAt struct {
	hall, row, rack *yaml.Node
	racks           *yaml.Node // the row's racks sequence
	index           int
}

func (d *estateDoc) findRack(name string) (*rackAt, error) {
	for _, site := range nodes(d.sites()) {
		for _, h := range nodes(mapGet(site, "halls")) {
			for _, r := range nodes(mapGet(h, "rows")) {
				rs := mapGet(r, "racks")
				for i, k := range nodes(rs) {
					if nameOf(k) == name {
						return &rackAt{hall: h, row: r, rack: k, racks: rs, index: i}, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("no rack %q in the estate", name)
}

func (d *estateDoc) allRacks() []*yaml.Node {
	var out []*yaml.Node
	for _, site := range nodes(d.sites()) {
		for _, h := range nodes(mapGet(site, "halls")) {
			for _, r := range nodes(mapGet(h, "rows")) {
				out = append(out, nodes(mapGet(r, "racks"))...)
			}
		}
	}
	return out
}

func rowRacks(row *yaml.Node) *yaml.Node {
	rs := mapGet(row, "racks")
	if rs == nil {
		rs = &yaml.Node{Kind: yaml.SequenceNode}
		mapSet(row, "racks", rs)
	}
	return rs
}

// ---------------------------------------------------------------- intents

func (d *estateDoc) rackAdd(o Op) error {
	if o.Name == "" || o.Row == "" || o.Format == "" || o.Position < 1 {
		return errors.New("rack.add: name, row, position and format are required")
	}
	if _, err := d.findRack(o.Name); err == nil {
		return fmt.Errorf("rack.add: a rack named %s exists", o.Name)
	}
	_, row, err := d.row(o.Hall, o.Row)
	if err != nil {
		return err
	}
	m := map[string]any{"name": o.Name, "position": o.Position, "format": o.Format}
	if len(o.Params) > 0 {
		m["params"] = o.Params
	}
	n, err := encodeValue(m)
	if err != nil {
		return err
	}
	if len(o.Params) == 0 {
		// rack another of what the hall already has: the params of a rack of the same format
		for _, k := range d.allRacks() {
			if f := mapGet(k, "format"); f != nil && f.Value == o.Format {
				if p := mapGet(k, "params"); p != nil {
					var c yaml.Node
					if err := c.Encode(nodeValue(p)); err == nil {
						c.Style = p.Style
						mapSet(n, "params", &c)
						d.note("%s: params copied from %s", o.Name, nameOf(k))
					}
					break
				}
			}
		}
	}
	n.Style = yaml.FlowStyle
	rs := rowRacks(row)
	rs.Content = append(rs.Content, n)
	return nil
}

// nodeValue decodes a node back to plain values (to copy it without sharing nodes).
func nodeValue(n *yaml.Node) any {
	var v any
	_ = n.Decode(&v)
	return v
}

// segRef matches a name as a whole path segment inside a selector or a port reference.
func segRef(name string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[/\s{,~])` + regexp.QuoteMeta(name) + `($|[/\s},:~])`)
}

func (d *estateDoc) rackRemove(name string) error {
	at, err := d.findRack(name)
	if err != nil {
		return err
	}
	at.racks.Content = append(at.racks.Content[:at.index:at.index], at.racks.Content[at.index+1:]...)
	d.dropRefs(name)
	return nil
}

// dropRefs takes a removed rack out of the places that list it by name: cooling loops and
// hand cabling. Selectors and peers that name it are left for the compiler to report.
func (d *estateDoc) dropRefs(name string) {
	spec := mapGet(d.root, "spec")
	for _, lp := range nodes(mapGet(mapGet(mapGet(spec, "facility"), "cooling"), "loops")) {
		rs := mapGet(lp, "racks")
		for i, r := range nodes(rs) {
			if r.Value == name {
				rs.Content = append(rs.Content[:i:i], rs.Content[i+1:]...)
				d.note("%s: removed from cooling loop %s", name, nameOf(lp))
				break
			}
		}
	}
	re := segRef(name)
	cab := mapGet(spec, "cabling")
	if add := mapGet(cab, "add"); add != nil {
		keep := add.Content[:0]
		for _, c := range add.Content {
			a, b := mapGet(c, "a"), mapGet(c, "b")
			if (a != nil && re.MatchString(a.Value)) || (b != nil && re.MatchString(b.Value)) {
				d.note("%s: hand cable %s ↔ %s removed", name, a.Value, b.Value)
				continue
			}
			keep = append(keep, c)
		}
		add.Content = keep
	}
	if rm := mapGet(cab, "remove"); rm != nil {
		keep := rm.Content[:0]
		for _, c := range rm.Content {
			if re.MatchString(c.Value) {
				d.note("%s: cabling removal %s dropped", name, c.Value)
				continue
			}
			keep = append(keep, c)
		}
		rm.Content = keep
	}
	for _, f := range nodes(mapGet(spec, "fabrics")) {
		if p := mapGet(f, "placeIn"); p != nil && re.MatchString(p.Value) {
			d.note("fabric %s places its switches in %s, which this draft removes", nameOf(f), name)
		}
	}
}

func (d *estateDoc) rackMove(o Op) error {
	at, err := d.findRack(o.Name)
	if err != nil {
		return err
	}
	hall := o.ToHall
	if hall == "" {
		hall = nameOf(at.hall)
	}
	toRow := o.ToRow
	if toRow == "" {
		toRow = nameOf(at.row)
	}
	_, row, err := d.row(hall, toRow)
	if err != nil {
		return err
	}
	if o.ToPosition > 0 {
		pos, _ := encodeValue(o.ToPosition)
		mapSet(at.rack, "position", pos)
	}
	if row != at.row {
		at.racks.Content = append(at.racks.Content[:at.index:at.index], at.racks.Content[at.index+1:]...)
		rs := rowRacks(row)
		rs.Content = append(rs.Content, at.rack)
	}
	return nil
}

func (d *estateDoc) rackRename(o Op) error {
	if o.NewName == "" || o.NewName == o.Name {
		return errors.New("rack.rename: newName is required and must differ")
	}
	if _, err := d.findRack(o.NewName); err == nil {
		return fmt.Errorf("rack.rename: a rack named %s exists", o.NewName)
	}
	at, err := d.findRack(o.Name)
	if err != nil {
		return err
	}
	mapSet(at.rack, "name", &yaml.Node{Kind: yaml.ScalarNode, Value: o.NewName})
	spec := mapGet(d.root, "spec")
	re := segRef(o.Name)
	repl := func(n *yaml.Node, where string) {
		if n != nil && n.Kind == yaml.ScalarNode && re.MatchString(n.Value) {
			old := n.Value
			n.Value = re.ReplaceAllString(n.Value, "${1}"+o.NewName+"${2}")
			d.note("%s: %s %q → %q", where, "renamed in", old, n.Value)
		}
	}
	for _, lp := range nodes(mapGet(mapGet(mapGet(spec, "facility"), "cooling"), "loops")) {
		for _, r := range nodes(mapGet(lp, "racks")) {
			if r.Value == o.Name {
				r.Value = o.NewName
				d.note("cooling loop %s: %s → %s", nameOf(lp), o.Name, o.NewName)
			}
		}
	}
	for _, f := range nodes(mapGet(spec, "fabrics")) {
		repl(mapGet(f, "select"), "fabric "+nameOf(f)+" select")
		repl(mapGet(f, "placeIn"), "fabric "+nameOf(f)+" placeIn")
	}
	for _, ov := range nodes(mapGet(mapGet(spec, "fidelity"), "overrides")) {
		repl(mapGet(ov, "select"), "fidelity override")
	}
	cab := mapGet(spec, "cabling")
	for _, c := range nodes(mapGet(cab, "add")) {
		repl(mapGet(c, "a"), "hand cable")
		repl(mapGet(c, "b"), "hand cable")
	}
	for _, c := range nodes(mapGet(cab, "remove")) {
		repl(c, "cabling removal")
	}
	for _, k := range d.allRacks() {
		if p := mapGet(mapGet(k, "params"), "peer"); p != nil && p.Value == o.Name {
			p.Value = o.NewName
			d.note("rack %s: peer %s → %s", nameOf(k), o.Name, o.NewName)
		}
	}
	return nil
}

func (d *estateDoc) rowAdd(o Op) error {
	if o.Name == "" || o.Positions < 1 {
		return errors.New("row.add: name and positions (≥ 1) are required")
	}
	h, err := d.hall(o.Hall)
	if err != nil {
		return err
	}
	rows := mapGet(h, "rows")
	if rows == nil {
		rows = &yaml.Node{Kind: yaml.SequenceNode}
		mapSet(h, "rows", rows)
	}
	if seqIndexByName(rows, o.Name) >= 0 {
		return fmt.Errorf("row.add: hall %s has a row %s", nameOf(h), o.Name)
	}
	n, err := encodeValue(map[string]any{"name": o.Name, "positions": o.Positions})
	if err != nil {
		return err
	}
	n.Style = 0 // block style, ready for a racks list
	mapSet(n, "racks", &yaml.Node{Kind: yaml.SequenceNode})
	rows.Content = append(rows.Content, n)
	return nil
}

func (d *estateDoc) rowRemove(o Op) error {
	h, row, err := d.row(o.Hall, o.Name)
	if err != nil {
		return err
	}
	for _, k := range nodes(mapGet(row, "racks")) {
		d.note("row %s: rack %s removed with it", o.Name, nameOf(k))
		d.dropRefs(nameOf(k))
	}
	rows := mapGet(h, "rows")
	i := seqIndexByName(rows, o.Name)
	rows.Content = append(rows.Content[:i:i], rows.Content[i+1:]...)
	return nil
}

func (d *estateDoc) hallAdd(o Op) error {
	if o.Name == "" {
		return errors.New("hall.add: name is required")
	}
	site := nodes(d.sites())
	if len(site) == 0 {
		return errors.New("the estate has no site")
	}
	halls := mapGet(site[0], "halls")
	if halls == nil {
		halls = &yaml.Node{Kind: yaml.SequenceNode}
		mapSet(site[0], "halls", halls)
	}
	if seqIndexByName(halls, o.Name) >= 0 {
		return fmt.Errorf("hall.add: a hall named %s exists", o.Name)
	}
	n := &yaml.Node{Kind: yaml.MappingNode}
	mapSet(n, "name", &yaml.Node{Kind: yaml.ScalarNode, Value: o.Name})
	mapSet(n, "rows", &yaml.Node{Kind: yaml.SequenceNode})
	halls.Content = append(halls.Content, n)
	return nil
}

func (d *estateDoc) hallRemove(o Op) error {
	for _, site := range nodes(d.sites()) {
		halls := mapGet(site, "halls")
		if i := seqIndexByName(halls, o.Name); i >= 0 {
			for _, r := range nodes(mapGet(halls.Content[i], "rows")) {
				for _, k := range nodes(mapGet(r, "racks")) {
					d.dropRefs(nameOf(k))
				}
			}
			halls.Content = append(halls.Content[:i:i], halls.Content[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("no hall %q", o.Name)
}

// apply runs one op.
func (d *estateDoc) apply(o Op) error {
	switch o.Op {
	case "set":
		return d.set(o.Path, o.Value)
	case "add":
		return d.add(o.Path, o.Value)
	case "remove":
		_, err := d.remove(o.Path)
		return err
	case "move":
		return d.move(o.Path, o.To)
	case "rack.add":
		return d.rackAdd(o)
	case "rack.remove":
		return d.rackRemove(o.Name)
	case "rack.move":
		return d.rackMove(o)
	case "rack.rename":
		return d.rackRename(o)
	case "row.add":
		return d.rowAdd(o)
	case "row.remove":
		return d.rowRemove(o)
	case "hall.add":
		return d.hallAdd(o)
	case "hall.remove":
		return d.hallRemove(o)
	}
	return fmt.Errorf("unknown op %q", o.Op)
}

// editOps applies a draft's ops to the estate file in order. The first op that cannot apply
// stops the edit; its index is in the error.
func editOps(src []byte, ops []Op) ([]byte, []string, error) {
	d, err := parseEstateDoc(src)
	if err != nil {
		return nil, nil, err
	}
	for i, o := range ops {
		if err := d.apply(o); err != nil {
			return nil, nil, fmt.Errorf("change %d (%s): %v", i+1, o.Op, err)
		}
	}
	out, err := d.bytes()
	if err != nil {
		return nil, nil, err
	}
	// the file as the encoder writes it unedited, to keep the author's layout where the edit
	// did not reach
	base, err := parseEstateDoc(src)
	if err != nil {
		return nil, nil, err
	}
	norm, err := base.bytes()
	if err != nil {
		return nil, nil, err
	}
	return keepLayout(src, norm, out), d.notes, nil
}

// classOf is how an op would apply to a running estate under the hot/cold rules of 03 —
// Changing a running estate. Every apply restarts the estate today; the class says what a hot
// apply would do with the change.
func classOf(o Op) string {
	if strings.HasPrefix(o.Op, "rack.") || strings.HasPrefix(o.Op, "row.") || strings.HasPrefix(o.Op, "hall.") {
		return "hot"
	}
	if len(o.Path) < 2 || o.Path[0] != "spec" {
		return "cold"
	}
	switch o.Path[1] {
	case "sites", "fidelity", "cabling", "facility", "networks":
		return "hot"
	case "fabrics":
		// a fabric's template, params or NOS regenerate its switches; its select re-cables racks
		if len(o.Path) >= 4 {
			if k, ok := o.Path[3].(string); ok && (k == "select" || k == "placeIn") {
				return "hot"
			}
		}
		return "cold"
	}
	return "cold" // clock, seed, packs, stacks, adapters: the engine restarts
}
