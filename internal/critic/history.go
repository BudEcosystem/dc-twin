// Package critic judges a run (docs/spec/17-critic.md): from the committed event log, the model
// it records and the stack's recorded accessor answers, it checks invariants, derives each
// fault's moments, computes measures and objectives, and returns a judgement whose every
// violation carries the events that prove it. It works on a run's events alone, so a recorded
// run is judged the same way live and offline (D64).
package critic

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/accessor"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Device is a device as the run's start marker recorded it.
type Device struct {
	Path, Class, Rack, RackName, Serial, Parent, Fidelity string
	Accelerators                                          int
}

// RunInfo is what a run's markers say about it.
type RunInfo struct {
	ID         string                           `json:"id"`
	Kind       string                           `json:"kind"`
	Scenario   string                           `json:"scenario,omitempty"`
	Subject    string                           `json:"subject,omitempty"`
	Estate     string                           `json:"estate,omitempty"`
	Clock      string                           `json:"clock"`
	TimeScale  float64                          `json:"timeScale,omitempty"`
	Seed       int64                            `json:"seed,omitempty"`
	Stacks     []string                         `json:"stacks,omitempty"`
	Aliases    map[string]string                `json:"aliases,omitempty"`
	StateMap   map[string]string                `json:"-"`
	Principals map[string]schema.StackPrincipal `json:"-"`
	StartSeq   uint64                           `json:"startSeq"`
	EndSeq     uint64                           `json:"endSeq,omitempty"`
	Started    time.Time                        `json:"started"`
	Ended      time.Time                        `json:"ended"`
	Finished   bool                             `json:"finished"`
	End        map[string]any                   `json:"-"`
	Snapshot   bool                             `json:"snapshot"`
}

// Fault is one injected fault and what it reached.
type Fault struct {
	Seq      uint64
	ID       string // the instance id (f3, tray)
	Fault    string // the catalogue id
	Target   string
	Device   string
	Rack     string
	RackName string
	Serial   string
	Class    string
	Source   string
	At       time.Time
	ClearSeq uint64
	ClearAt  time.Time
	Affected map[string]bool
}

// openAt reports whether the fault is open after event seq.
func (f *Fault) openAt(seq uint64) bool {
	return f.Seq <= seq && (f.ClearSeq == 0 || f.ClearSeq > seq)
}

// view is the fault as conditions see it (`fault`).
func (f *Fault) view() map[string]any {
	return map[string]any{"seq": f.Seq, "id": f.ID, "fault": f.Fault, "path": f.Target, "device": f.Device, "host": f.Device,
		"rack": f.Rack, "rackName": f.RackName, "serial": f.Serial, "class": f.Class, "source": f.Source,
		"catalogue": catalogueOf(f.Fault)}
}

func catalogueOf(id string) string {
	if i := strings.LastIndex(id, "/"); i > 0 {
		return id[:i]
	}
	return id
}

type point struct {
	seq uint64
	v   any
}

// History is a run rebuilt from its events: device state as of any event, the faults and what
// each reached, and what the stack's accessors answered.
type History struct {
	Events  []schema.Event
	Run     RunInfo
	Virtual bool
	Devices map[string]*Device
	Faults  []*Fault

	idx       map[uint64]int
	bySerial  map[string]string
	init      map[string]map[string]any
	timeline  map[string]map[string][]point
	faultBy   map[uint64]*Fault
	accessors map[string][]point
	evalSeqs  []uint64 // events after which a condition may change
}

// Build rebuilds a run from its events: the start marker (when present) first. Events outside
// the first run in the slice are ignored.
func Build(events []schema.Event) (*History, error) {
	h := &History{Devices: map[string]*Device{}, idx: map[uint64]int{}, bySerial: map[string]string{},
		init: map[string]map[string]any{}, timeline: map[string]map[string][]point{}, faultBy: map[uint64]*Fault{},
		accessors: map[string][]point{}}
	if len(events) == 0 {
		return nil, fmt.Errorf("no events to judge")
	}
	start := 0
	for i, e := range events {
		if e.Kind == "run" && e.Data["phase"] == "start" {
			start = i
			break
		}
	}
	events = events[start:]
	h.Run = RunInfo{Kind: "session", Clock: "wall", Aliases: map[string]string{}, StateMap: map[string]string{}, Principals: map[string]schema.StackPrincipal{}}
	first := events[0]
	if first.Kind == "run" && first.Data["phase"] == "start" {
		h.readStart(first)
	} else {
		h.Run.ID = fmt.Sprintf("log-%d", first.Seq)
		h.Run.StartSeq = first.Seq
	}
	h.Virtual = h.Run.Clock == "virtual"
	h.Run.Started = h.time(first)
	for _, e := range events {
		if e.Kind == "run" && e.Data["phase"] == "end" && fmt.Sprint(e.Data["run"]) == h.Run.ID {
			h.Events = append(h.Events, e)
			h.Run.EndSeq, h.Run.Ended, h.Run.Finished, h.Run.End = e.Seq, h.time(e), true, e.Data
			break
		}
		if e.Kind == "run" && e.Data["phase"] == "start" && e.Seq != first.Seq && h.Run.Kind != "session" {
			break // the next run begins; an open session spans the runs made inside it
		}
		h.Events = append(h.Events, e)
	}
	last := h.Events[len(h.Events)-1]
	if !h.Run.Finished {
		h.Run.EndSeq, h.Run.Ended = last.Seq, h.time(last)
	}
	for i, e := range h.Events {
		h.idx[e.Seq] = i
	}
	h.index()
	return h, nil
}

func (h *History) readStart(e schema.Event) {
	d := e.Data
	r := &h.Run
	r.ID, r.Kind, r.StartSeq, r.Snapshot = str(d["run"]), str(d["runKind"]), e.Seq, d["snapshot"] != nil
	r.Scenario, r.Subject, r.Estate, r.Clock = str(d["scenario"]), str(d["subject"]), str(d["estate"]), str(d["clock"])
	if r.Clock == "" {
		r.Clock = "wall"
	}
	r.TimeScale, _ = d["timeScale"].(float64)
	if s, ok := d["seed"].(float64); ok {
		r.Seed = int64(s)
	} else if s, ok := d["seed"].(int64); ok {
		r.Seed = s
	}
	for _, s := range list(d["stacks"]) {
		r.Stacks = append(r.Stacks, str(s))
	}
	for k, v := range mapOf(d["aliases"]) {
		r.Aliases[k] = str(v)
	}
	for k, v := range mapOf(d["stateMap"]) {
		r.StateMap[k] = str(v)
	}
	for k, v := range mapOf(d["principals"]) {
		m := mapOf(v)
		var p schema.StackPrincipal
		for _, u := range list(m["users"]) {
			p.Users = append(p.Users, str(u))
		}
		for _, a := range list(m["addresses"]) {
			p.Addresses = append(p.Addresses, str(a))
		}
		r.Principals[k] = p
	}
	for _, x := range list(d["snapshot"]) {
		m := mapOf(x)
		path := str(m["path"])
		if path == "" {
			continue
		}
		if str(m["class"]) == "rack" {
			for k, v := range mapOf(m["isolated"]) {
				h.setInit(path, "isolation."+k, v)
			}
			continue
		}
		dev := &Device{Path: path, Class: str(m["class"]), Rack: str(m["rack"]), RackName: str(m["rackName"]), Serial: str(m["serial"]),
			Parent: str(m["parent"]), Fidelity: str(m["fidelity"]), Accelerators: int(num(m["accelerators"]))}
		h.Devices[path] = dev
		if dev.Serial != "" {
			h.bySerial[dev.Serial] = path
		}
		for _, f := range []string{"power", "health", "ac"} {
			if v, ok := m[f]; ok {
				h.setInit(path, f, v)
			}
		}
	}
}

func (h *History) setInit(path, field string, v any) {
	if h.init[path] == nil {
		h.init[path] = map[string]any{}
	}
	h.init[path][field] = v
}

// index builds the state timelines, the faults with what each reached through cause chains,
// the accessor answers and the points at which conditions are evaluated.
func (h *History) index() {
	chain := map[uint64][]uint64{} // event seq -> the faults its cause chain reaches
	for _, e := range h.Events {
		var roots []uint64
		if e.Cause != 0 {
			if f := h.faultBy[e.Cause]; f != nil {
				roots = append(roots, e.Cause)
			}
			roots = append(roots, chain[e.Cause]...)
			if len(roots) > 0 {
				chain[e.Seq] = dedupe(roots)
			}
		}
		switch e.Kind {
		case "fault":
			h.faultEvent(e)
		case "state":
			if e.State != nil {
				tl := h.timeline[e.Path]
				if tl == nil {
					tl = map[string][]point{}
					h.timeline[e.Path] = tl
				}
				tl[e.State.Field] = append(tl[e.State.Field], point{e.Seq, e.State.To})
			}
		case "observe":
			if t, ok := e.Data["accessor"].(string); ok {
				k := accessor.Key(t, mapOf(e.Data["args"]))
				v := e.Data["value"]
				if _, failed := e.Data["error"]; failed {
					v = nil
				}
				h.accessors[k] = append(h.accessors[k], point{e.Seq, v})
			}
		}
		for _, fs := range [][]uint64{chain[e.Seq]} {
			for _, fseq := range fs {
				if f := h.faultBy[fseq]; f != nil && e.Kind != "request" {
					f.Affected[h.DevicePath(e.Path)] = true
				}
			}
		}
		if h.evalPoint(e) {
			h.evalSeqs = append(h.evalSeqs, e.Seq)
		}
	}
}

func (h *History) evalPoint(e schema.Event) bool {
	switch e.Kind {
	case "state", "fault", "observe", "run", "act":
		return true
	case "request":
		return e.Request != nil && e.Request.Class != "read"
	}
	return false
}

func (h *History) faultEvent(e schema.Event) {
	id, fid := str(e.Data["id"]), str(e.Data["fault"])
	if _, cleared := e.Data["cleared"]; cleared {
		for _, f := range h.Faults {
			if f.ID == id && f.ClearSeq == 0 {
				f.ClearSeq, f.ClearAt = e.Seq, h.time(e)
			}
		}
		return
	}
	dev := h.DevicePath(e.Path)
	f := &Fault{Seq: e.Seq, ID: id, Fault: fid, Target: e.Path, Device: dev, Source: e.Source.Class, At: h.time(e),
		Affected: map[string]bool{dev: true}}
	if d := h.Devices[dev]; d != nil {
		f.Rack, f.RackName, f.Serial, f.Class = d.Rack, d.RackName, d.Serial, d.Class
	}
	h.Faults = append(h.Faults, f)
	h.faultBy[e.Seq] = f
}

// DevicePath is the device a path belongs to: a component's or a port's device, or the path.
func (h *History) DevicePath(p string) string {
	if i := strings.Index(p, ":"); i > 0 {
		p = p[:i]
	}
	if len(h.Devices) == 0 {
		return p
	}
	for q := p; ; {
		if _, ok := h.Devices[q]; ok {
			return q
		}
		i := strings.LastIndex(q, "/")
		if i <= 0 {
			return p
		}
		q = q[:i]
	}
}

// time is an event's time on the clock the critic measures with: wall time, unless the run is
// on the virtual clock (D69).
func (h *History) time(e schema.Event) time.Time {
	s := e.Wall
	if h.Virtual {
		s = e.T
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// At is the time of the event with sequence seq.
func (h *History) At(seq uint64) time.Time {
	if i, ok := h.idx[seq]; ok {
		return h.time(h.Events[i])
	}
	return h.Run.Ended
}

// Event returns the event with sequence seq.
func (h *History) Event(seq uint64) (schema.Event, bool) {
	if i, ok := h.idx[seq]; ok {
		return h.Events[i], true
	}
	return schema.Event{}, false
}

// State is a field's value on a path as of event seq: the last change at or before it, else the
// value the start marker recorded.
func (h *History) State(path, field string, seq uint64) (any, bool) {
	pts := h.timeline[path][field]
	i := sort.Search(len(pts), func(i int) bool { return pts[i].seq > seq })
	if i > 0 {
		return pts[i-1].v, true
	}
	v, ok := h.init[path][field]
	return v, ok
}

// View is a device as conditions see it as of event seq (`before`, `model.device`).
func (h *History) View(path string, seq uint64) map[string]any {
	path = h.DevicePath(path)
	v := map[string]any{"path": path, "known": false}
	if d := h.Devices[path]; d != nil {
		v["known"], v["class"], v["rack"], v["rackName"], v["serial"], v["accelerators"], v["fidelity"], v["parent"] =
			true, d.Class, d.Rack, d.RackName, d.Serial, d.Accelerators, d.Fidelity, d.Parent
		iso, _ := h.State(d.Rack, "isolation.electrical", seq)
		v["rackIsolated"] = iso == true
	}
	for _, f := range []string{"power", "health", "ac", "bmc"} {
		if x, ok := h.State(path, f, seq); ok {
			v[f] = fmt.Sprint(x)
			if f == "ac" {
				v[f] = x == true
			}
		}
	}
	if _, ok := v["power"]; !ok {
		v["power"] = "unknown"
	}
	if _, ok := v["health"]; !ok {
		v["health"] = "OK"
	}
	if _, ok := v["ac"]; !ok {
		v["ac"] = true
	}
	var open []any
	for _, f := range h.Faults {
		if f.openAt(seq) && f.Affected[path] {
			open = append(open, f.Fault)
		}
	}
	v["faults"] = open
	v["affected"] = len(open) > 0
	return v
}

// Affected reports whether a device is in the reach of a fault open as of event seq.
func (h *History) Affected(path string, seq uint64) bool {
	path = h.DevicePath(path)
	for _, f := range h.Faults {
		if f.openAt(seq) && f.Affected[path] {
			return true
		}
	}
	return false
}

// Accessor is what the stack answered for a read, as of event seq: the last recorded answer at
// or before it. With ahead > 0 and no answer yet, the first answer recorded within ahead after
// the event (the sampler reads the stack just after a request it judges, D68).
func (h *History) Accessor(target string, args map[string]any, seq uint64, ahead time.Duration) (any, bool) {
	pts := h.accessors[accessor.Key(target, args)]
	if len(pts) == 0 {
		return nil, false
	}
	i := sort.Search(len(pts), func(i int) bool { return pts[i].seq > seq })
	if i > 0 {
		return pts[i-1].v, true
	}
	if ahead > 0 && h.At(pts[0].seq).Sub(h.At(seq)) <= ahead {
		return pts[0].v, true
	}
	return nil, false
}

// Principal names who made a request: the stack component whose BMC user or address it was,
// else the principal the adapter logged.
func (h *History) Principal(e schema.Event) string {
	for comp, p := range h.Run.Principals {
		for _, u := range p.Users {
			if u == e.Source.Principal {
				return comp
			}
		}
		for _, a := range p.Addresses {
			if a != "" && strings.HasPrefix(e.Source.Address, a) {
				return comp
			}
		}
	}
	if e.Source.Principal != "" {
		return e.Source.Principal
	}
	return e.Source.Class
}

// EventView is an event as conditions see it (`event`).
func (h *History) EventView(e schema.Event) map[string]any {
	v := map[string]any{"seq": e.Seq, "kind": e.Kind, "path": e.Path, "device": h.DevicePath(e.Path), "from": e.Source.Class,
		"principal": h.Principal(e), "address": e.Source.Address, "message": e.Message}
	if d := h.Devices[h.DevicePath(e.Path)]; d != nil {
		v["rack"], v["rackName"], v["class"] = d.Rack, d.RackName, d.Class
	}
	if r := e.Request; r != nil {
		cls := r.Class
		if cls == "" {
			cls = schema.RequestClass(r.Canonical)
		}
		v["canonical"], v["class"], v["protocol"], v["method"], v["target"], v["status"] = r.Canonical, cls, r.Protocol, r.Method, r.Target, r.Status
	}
	if s := e.State; s != nil {
		v["field"], v["from"], v["to"] = s.Field, fmt.Sprint(s.From), fmt.Sprint(s.To)
	}
	return v
}

// class is a request's class, from the event or the registry for logs written before it.
func class(e schema.Event) string {
	if e.Request == nil {
		return ""
	}
	if e.Request.Class != "" {
		return e.Request.Class
	}
	return schema.RequestClass(e.Request.Canonical)
}

// ---------------------------------------------------------------- helpers

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
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

func list(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	}
	return nil
}

func mapOf(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case map[string]string:
		out := map[string]any{}
		for k, s := range x {
			out[k] = s
		}
		return out
	}
	return map[string]any{}
}

func dedupe(in []uint64) []uint64 {
	seen := map[uint64]bool{}
	var out []uint64
	for _, x := range in {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
