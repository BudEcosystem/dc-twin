// Package scenario runs scenario files (docs/spec/08-scenarios-and-faults.md): estate
// requirements, timed steps (wait, act, fault, expect) and assertions observed from the
// hardware side, the publisher logs, the committed event log and the stack's declared
// accessors. Every result carries its fidelity report.
package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/expr"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/report"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/selector"
	"github.com/BudEcosystem/dc-twin/internal/twin"
	"github.com/google/cel-go/common/types/ref"
)

// Options tune a run.
type Options struct {
	Log  io.Writer
	Poll time.Duration
}

type StepResult struct {
	Index  int      `json:"index"`
	Kind   string   `json:"kind"`
	Name   string   `json:"name,omitempty"`
	Status string   `json:"status"`
	Detail []string `json:"detail,omitempty"`
	Took   string   `json:"took"`
}

type Result struct {
	Scenario string         `json:"scenario"`
	Estate   string         `json:"estate"`
	Verdict  string         `json:"verdict"`
	Reason   string         `json:"reason,omitempty"`
	Steps    []StepResult   `json:"steps"`
	Started  string         `json:"started"`
	Took     string         `json:"took"`
	Report   *report.Report `json:"report"`
}

// RedfishGetter renders a device's Redfish resource as its BMC serves it, without logging a
// request (the scenario runner is outside the stack's view).
type RedfishGetter interface {
	Get(devicePath, resourcePath string) (map[string]any, bool)
}

// BMSLog is the BMS publisher's log.
type BMSLog interface {
	PublishedValues(filter string, since time.Time) []PublishedValue
}

type PublishedValue struct {
	Topic string
	Value any
	At    time.Time
}

type runner struct {
	t         *twin.Twin
	sc        *schema.Scenario
	opt       Options
	start     time.Time
	startSeq  uint64
	stepStart time.Time
	stepClock time.Time // the engine clock at the step's start (virtual or wall)
	stepSeq   uint64
	// causeSeq is where the acts and faults that an expect observes began: an expect sees the
	// events of the steps that led to it, not only those inside its own window.
	causeSeq     uint64
	lastWasCause bool
	accessors    map[string]func(args map[string]any) (any, error)
	used         *report.Used
	faultIDs     map[string]string
	log          func(f string, a ...any)
}

// Run executes a scenario against a running twin.
func Run(ctx context.Context, t *twin.Twin, sc *schema.Scenario, opt Options) *Result {
	if opt.Poll == 0 {
		opt.Poll = 200 * time.Millisecond
	}
	r := &runner{t: t, sc: sc, opt: opt, start: time.Now(), startSeq: t.Engine.Seq(), faultIDs: map[string]string{},
		used: &report.Used{Devices: map[string]bool{}, Faults: map[string]bool{}, Since: t.Engine.Seq()}}
	r.log = func(f string, a ...any) {
		if opt.Log != nil {
			fmt.Fprintf(opt.Log, f+"\n", a...)
		}
	}
	res := &Result{Scenario: sc.Metadata.Name, Estate: t.Estate.Metadata.Name, Started: r.start.UTC().Format(time.RFC3339)}
	finish := func(verdict, reason string) *Result {
		res.Verdict, res.Reason, res.Took = verdict, reason, time.Since(r.start).Round(time.Millisecond).String()
		r.markUsedFromEvents()
		res.Report = report.Build(t, r.used)
		res.Report.Scenario = sc.Metadata.Name
		return res
	}
	if why := r.requirements(); why != "" {
		return finish("not-run", why)
	}
	if err := r.bindAccessors(); err != nil {
		return finish("not-run", err.Error())
	}
	timeout, err := time.ParseDuration(sc.Spec.Timeout)
	if err != nil {
		return finish("not-run", "timeout: "+err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if sc.Spec.Chaos != nil && sc.Spec.Chaos.Enabled {
		t.Engine.SetChaos(sc.Spec.Chaos.RatePerDeviceHour, sc.Spec.Chaos.Faults)
		defer t.Engine.SetChaos(0, nil)
	}
	steps := append(append([]schema.Step{}, sc.Spec.Setup...), sc.Spec.Steps...)
	failed := false
	for i, st := range steps {
		sr := r.step(ctx, i, st)
		res.Steps = append(res.Steps, sr)
		r.log("  step %d %-6s %-7s %s %s", i+1, sr.Kind, sr.Status, sr.Took, strings.Join(sr.Detail, "; "))
		if sr.Status != "passed" {
			failed = true
			break
		}
	}
	if inc := t.Engine.Incidents(); len(inc) > 0 && inc[len(inc)-1].Seq > r.startSeq {
		return finish("not-run", "twin incident during the run: "+inc[len(inc)-1].Message)
	}
	if failed {
		return finish("failed", "")
	}
	rep := report.Build(t, nil)
	if len(rep.Impersonated) > 0 {
		return finish("impersonated", "ran with an impersonator: "+strings.Join(rep.Impersonated, "; ")+" — a scale run, not a conformance pass (D24)")
	}
	return finish("passed", "conditional on the fidelity report")
}

func (r *runner) requirements() string {
	req := r.sc.Spec.Estate.Requires
	formats := map[string]bool{}
	r.t.Engine.Read(func(m *model.Model) {
		for _, rk := range m.Racks {
			formats[rk.FormatName] = true
		}
	})
	for _, f := range req.Formats {
		if !formats[f] {
			return "estate has no rack of format " + f
		}
	}
	if req.Clock != "" && req.Clock != r.t.Engine.M.Clock {
		return "scenario needs the " + req.Clock + " clock"
	}
	provided := Provided(r.t)
	for _, c := range req.Capabilities {
		if !provided[c] {
			return "capability " + c + " is not provided by this estate"
		}
	}
	installed := map[string]bool{}
	for _, s := range r.t.Estate.Spec.Stacks {
		name, _, _ := strings.Cut(s.Name, "@")
		comps := s.Components
		if len(comps) == 0 {
			if st := r.t.Reg.Stacks[name]; st != nil {
				for c := range st.Spec.Components {
					comps = append(comps, c)
				}
			}
		}
		for _, c := range comps {
			installed[c] = true
		}
	}
	for _, c := range r.sc.Spec.Stack.Components {
		if !installed[c] {
			return "stack component " + c + " is not installed on this estate"
		}
	}
	return ""
}

// Provided lists the capabilities the running estate provides, at the fidelity each device runs.
func Provided(t *twin.Twin) map[string]bool {
	out := map[string]bool{}
	t.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			for _, p := range d.Profile.Spec.Provides {
				for _, at := range p.At {
					if at == d.Fidelity {
						out[p.Capability] = true
					}
				}
			}
		}
		for _, f := range m.Fabrics {
			for _, c := range f.Provides {
				out[c] = true
			}
		}
		if b := m.Estate.Spec.Facility.BMS; b != nil {
			out["facility/bms:"+b.Dialect] = true
			out["facility/isolation-request:"+b.Dialect] = true
		}
	})
	for _, a := range t.Adapters {
		switch a.Name() {
		case "ufm":
			out["fabric/ufm-rest"] = true
		case "kwok":
			out["kubernetes/nodes:kwok"] = true
		case "dhcp":
			out["boot/walk"] = true
		}
	}
	return out
}

// bindAccessors maps the scenario's aliases to the installed stack's accessors.
func (r *runner) bindAccessors() error {
	r.accessors = map[string]func(map[string]any) (any, error){}
	for alias, target := range r.sc.Spec.Stack.Accessors {
		comp, acc, ok := strings.Cut(target, ".")
		if !ok {
			return fmt.Errorf("accessor %s: %q is not <component>.<accessor>", alias, target)
		}
		var def *schema.Accessor
		var outputs map[string]string
		for _, s := range r.t.Estate.Spec.Stacks {
			name, _, _ := strings.Cut(s.Name, "@")
			st := r.t.Reg.Stacks[name]
			if st == nil {
				continue
			}
			if c, ok := st.Spec.Components[comp]; ok {
				if a, ok := c.Accessors[acc]; ok {
					a := a
					def, outputs = &a, s.Outputs
				}
			}
		}
		if def == nil {
			return fmt.Errorf("accessor %s: no installed stack declares %s", alias, target)
		}
		d, outs := *def, outputs
		r.accessors[alias] = func(args map[string]any) (any, error) { return callAccessor(d, comp, outs, args) }
	}
	return nil
}

func callAccessor(a schema.Accessor, comp string, outputs map[string]string, args map[string]any) (any, error) {
	if a.Kind != "http" {
		return nil, fmt.Errorf("accessor kind %s is not supported", a.Kind)
	}
	ep := a.Endpoint
	for k, v := range outputs {
		ep = strings.ReplaceAll(ep, "{"+k+"}", v)
	}
	ep = strings.ReplaceAll(ep, "{"+comp+"}", outputs[comp])
	for k, v := range args {
		ep = strings.ReplaceAll(ep, "{"+k+"}", url.QueryEscape(fmt.Sprint(v)))
	}
	if strings.Contains(ep, "{") {
		return nil, fmt.Errorf("accessor endpoint %q has unbound placeholders", ep)
	}
	req, _ := http.NewRequest("GET", ep, nil)
	if tok := outputs[comp+".token"]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func (r *runner) src() schema.EventSource {
	return schema.EventSource{Class: "scenario", Principal: r.sc.Metadata.Name}
}

func (r *runner) step(ctx context.Context, i int, st schema.Step) StepResult {
	r.stepStart, r.stepSeq, r.stepClock = time.Now(), r.t.Engine.Seq(), r.t.Engine.Now()
	isCause := st.Act != nil || st.Fault != nil
	if isCause && !r.lastWasCause {
		r.causeSeq = r.stepSeq
	}
	if !isCause && !r.lastWasCause {
		r.causeSeq = r.stepSeq
	}
	r.lastWasCause = isCause
	sr := StepResult{Index: i + 1, Name: st.Name}
	defer func() { sr.Took = time.Since(r.stepStart).Round(time.Millisecond).String() }()
	switch {
	case st.Wait != nil:
		sr.Kind = "wait"
		within, err := time.ParseDuration(st.Wait.Within)
		if err != nil {
			sr.Status, sr.Detail = "failed", []string{err.Error()}
			return sr
		}
		ok, detail := r.until(ctx, within, func() (bool, string) {
			v, err := r.cel(st.Wait.For)
			if err != nil {
				return false, err.Error()
			}
			return v, ""
		})
		sr.Status = map[bool]string{true: "passed", false: "failed"}[ok]
		if !ok {
			sr.Detail = []string{"not true within " + st.Wait.Within + ": " + st.Wait.For, detail}
		}
	case st.Act != nil:
		sr.Kind = "act"
		if err := r.act(st.Act); err != nil {
			sr.Status, sr.Detail = "failed", []string{err.Error()}
			return sr
		}
		sr.Status = "passed"
	case st.Fault != nil:
		sr.Kind = "fault"
		if err := r.fault(st.Fault); err != nil {
			sr.Status, sr.Detail = "failed", []string{err.Error()}
			return sr
		}
		sr.Status = "passed"
	case st.Expect != nil:
		sr.Kind = "expect"
		sr.Status, sr.Detail = r.expect(ctx, st.Expect)
	default:
		sr.Kind, sr.Status, sr.Detail = "?", "failed", []string{"empty step"}
	}
	return sr
}

// until polls cond (advancing a virtual clock) until it holds or the window closes.
func (r *runner) until(ctx context.Context, within time.Duration, cond func() (bool, string)) (bool, string) {
	eng := r.t.Engine
	deadline := time.Now().Add(within)
	vstart := eng.Now()
	last := ""
	for {
		ok, detail := cond()
		if ok {
			return true, ""
		}
		last = detail
		if eng.Virtual() {
			if eng.Now().Sub(vstart) >= within {
				return false, last
			}
			_ = eng.Advance(time.Second)
			continue
		}
		if time.Now().After(deadline) {
			return false, last
		}
		select {
		case <-ctx.Done():
			return false, "scenario timeout"
		case <-time.After(r.opt.Poll):
		}
	}
}

func (r *runner) act(a *schema.ActStep) error {
	eng := r.t.Engine
	switch {
	case a.Power != "":
		ds, err := eng.SelectDevices(a.Select)
		if err != nil || len(ds) == 0 {
			return fmt.Errorf("act power: select %q matches no device", a.Select)
		}
		canonical := map[string]string{"On": "power.on", "Off": "power.off.force", "ForceOff": "power.off.force", "GracefulShutdown": "power.off.graceful",
			"ForceRestart": "power.restart", "PowerCycle": "power.cycle", "ACCycle": "ac.cycle"}[a.Power]
		if canonical == "" {
			return fmt.Errorf("act power %q", a.Power)
		}
		for _, d := range ds {
			r.used.Devices[d.Path] = true
			if err := eng.Power(engine.Request{Path: d.Path, Canonical: canonical, Source: r.src(), Protocol: "scenario"}); err != nil {
				return fmt.Errorf("%s: %w", d.Path, err)
			}
		}
	case a.Feed != "":
		up := a.Up != nil && *a.Up
		return eng.SetFeed(a.Feed, up, r.src())
	case a.Utilisation != nil:
		ds, _ := eng.SelectDevices(a.Select)
		for _, d := range ds {
			if err := eng.SetUtilisation(d.Path, *a.Utilisation, r.src()); err != nil {
				return err
			}
		}
	case a.Cable != "":
		return eng.SetLink(a.Link, a.Cable == "replace", r.src())
	case a.Reseat:
		ds, _ := eng.SelectDevices(a.Select)
		for _, d := range ds {
			r.used.Devices[d.Path] = true
			if err := eng.Reseat(d.Path, r.src()); err != nil {
				return err
			}
		}
	case a.Seconds > 0:
		d := time.Duration(a.Seconds * float64(time.Second))
		if eng.Virtual() {
			return eng.Advance(d)
		}
		time.Sleep(d)
	case a.Clock != "":
		d, err := time.ParseDuration(a.Clock)
		if err != nil {
			return err
		}
		return eng.Advance(d)
	default:
		return fmt.Errorf("empty act")
	}
	return nil
}

func (r *runner) fault(f *schema.FaultStep) error {
	eng := r.t.Engine
	if f.Clear != nil {
		switch c := f.Clear.(type) {
		case string:
			_, err := eng.Clear(c, "", r.src())
			return err
		case map[string]any:
			_, err := eng.Clear(fmt.Sprint(c["select"]), fmt.Sprint(c["inject"]), r.src())
			return err
		}
		return fmt.Errorf("fault clear: want an id or {select, inject}")
	}
	fs, err := eng.Inject(f.Select, f.Inject, f.Params, f.ID, r.src())
	if err != nil {
		return err
	}
	r.used.Faults[f.Inject] = true
	for _, x := range fs {
		ds, _ := eng.SelectDevices(x.Target)
		for _, d := range ds {
			r.used.Devices[d.Path] = true
		}
		if p := strings.Index(x.Target, ":"); p > 0 {
			r.used.Devices[x.Target[:p]] = true
		} else {
			r.used.Devices[x.Target] = true
		}
	}
	return nil
}

func (r *runner) expect(ctx context.Context, ex *schema.ExpectStep) (string, []string) {
	within, err := time.ParseDuration(ex.Within)
	if err != nil {
		return "failed", []string{err.Error()}
	}
	hasNot := false
	for _, o := range ex.All {
		if o.Not != nil {
			hasNot = true
		}
	}
	positive := map[int]bool{}
	var violated []string
	eng := r.t.Engine
	deadline := time.Now().Add(within)
	vstart := eng.Now()
	for {
		for i, o := range ex.All {
			if o.Not != nil {
				ok, detail := r.observe(*o.Not)
				if ok {
					violated = append(violated, fmt.Sprintf("not #%d became true: %s", i+1, detail))
				}
				continue
			}
			if positive[i] {
				continue
			}
			if ok, _ := r.observe(o); ok {
				positive[i] = true
			}
		}
		if len(violated) > 0 {
			return "failed", violated
		}
		done := len(positive) == countPositive(ex.All)
		over := time.Now().After(deadline)
		if eng.Virtual() {
			over = eng.Now().Sub(vstart) >= within
		}
		if done && !hasNot {
			return "passed", nil
		}
		if over {
			if done {
				return "passed", []string{"every positive observation held; every not held for the whole window"}
			}
			var missing []string
			for i, o := range ex.All {
				if o.Not == nil && !positive[i] {
					_, detail := r.observe(o)
					missing = append(missing, fmt.Sprintf("#%d never became true: %s", i+1, detail))
				}
			}
			return "failed", missing
		}
		if eng.Virtual() {
			_ = eng.Advance(time.Second)
			continue
		}
		select {
		case <-ctx.Done():
			return "failed", []string{"scenario timeout"}
		case <-time.After(r.opt.Poll):
		}
	}
}

func countPositive(all []schema.Observation) int {
	n := 0
	for _, o := range all {
		if o.Not == nil {
			n++
		}
	}
	return n
}

// observe evaluates one observation now; detail explains the value seen.
func (r *runner) observe(o schema.Observation) (bool, string) {
	eng := r.t.Engine
	switch {
	case o.Redfish != nil:
		return r.observeRedfish(o.Redfish)
	case o.Event != nil:
		ev := o.Event
		var sel *selector.Selector
		if ev.On != "" {
			sel, _ = selector.Parse(ev.On)
		}
		hits := eng.Events(r.causeSeq, func(e schema.Event) bool {
			if ev.Kind != "" && e.Kind != ev.Kind {
				return false
			}
			if ev.Canonical != "" && (e.Request == nil || e.Request.Canonical != ev.Canonical) {
				return false
			}
			if ev.From != "" && e.Source.Class != ev.From {
				return false
			}
			if ev.Field != "" && (e.State == nil || e.State.Field != ev.Field || (ev.To != nil && fmt.Sprint(e.State.To) != fmt.Sprint(ev.To))) {
				return false
			}
			if sel != nil && !sel.MatchPath(e.Path) {
				return false
			}
			return true
		})
		for _, h := range hits {
			r.used.Devices[h.Path] = true
		}
		return len(hits) > 0, fmt.Sprintf("%d matching events", len(hits))
	case o.BMS != nil:
		for _, a := range r.t.Adapters {
			if bl, ok := a.(BMSLog); ok {
				vals := bl.PublishedValues(o.BMS.Topic, r.stepClock.Add(-time.Second))
				for _, v := range vals {
					if looseEqual(v.Value, o.BMS.Value) {
						return true, "published " + v.Topic
					}
				}
				return false, fmt.Sprintf("%d publications on %s, none with value %v", len(vals), o.BMS.Topic, o.BMS.Value)
			}
		}
		return false, "no BMS publisher is running"
	case o.Domain != nil:
		for _, d := range eng.Domains() {
			if strings.HasSuffix(d["path"].(string), o.Domain.Path) {
				return d["state"] == o.Domain.State, fmt.Sprintf("domain %s is %v", d["path"], d["state"])
			}
		}
		return false, "no domain " + o.Domain.Path
	case o.Faults != nil:
		n := len(eng.Faults(o.Faults.Select))
		return n == o.Faults.Count, fmt.Sprintf("%d active faults", n)
	case o.Accessor != nil:
		fn := r.accessors[o.Accessor.Alias]
		if fn == nil {
			return false, "no accessor " + o.Accessor.Alias
		}
		v, err := fn(r.resolveArgs(o.Accessor.Args))
		if err != nil {
			return false, err.Error()
		}
		return subset(o.Accessor.Equals, v), fmt.Sprintf("accessor returned %v", truncate(v))
	case o.Model != nil:
		ok, err := r.cel(o.Model.Expr)
		if err != nil {
			return false, err.Error()
		}
		return ok, ""
	case o.DHCP != nil:
		ds, _ := eng.SelectDevices(o.DHCP.Select)
		leased := 0
		eng.Read(func(*model.Model) {
			for _, d := range ds {
				if d.State.Lease != nil && d.State.Lease.IP != "" {
					leased++
				}
			}
		})
		return (leased == len(ds) && len(ds) > 0) == o.DHCP.Leased, fmt.Sprintf("%d of %d leased", leased, len(ds))
	}
	return false, "observation kind not supported by this runner"
}

func (r *runner) observeRedfish(o *schema.RedfishObs) (bool, string) {
	var getter RedfishGetter
	for _, a := range r.t.Adapters {
		if g, ok := a.(RedfishGetter); ok {
			getter = g
		}
	}
	if getter == nil {
		return false, "no Redfish adapter is running"
	}
	ds, err := r.t.Engine.SelectDevices(o.Select)
	if err != nil || len(ds) == 0 {
		return false, "select matches no device"
	}
	segs := strings.Split(strings.Trim(o.Property, "/"), "/")
	prop := segs[len(segs)-1]
	var seen []string
	for _, d := range ds {
		r.used.Devices[d.Path] = true
		paths := []string{"/redfish/v1"}
		for _, s := range segs[:len(segs)-1] {
			var next []string
			for _, p := range paths {
				if s == "*" {
					doc, ok := getter.Get(d.Path, p)
					if !ok {
						continue
					}
					members, _ := doc["Members"].([]any)
					for _, m := range members {
						if id, ok := m.(map[string]any)["@odata.id"].(string); ok {
							next = append(next, strings.TrimRight(id, "/"))
						}
					}
				} else {
					next = append(next, p+"/"+s)
				}
			}
			paths = next
		}
		match := false
		for _, p := range paths {
			doc, ok := getter.Get(d.Path, p)
			if !ok {
				continue
			}
			v := getDotted(doc, prop)
			if v == nil {
				continue
			}
			seen = append(seen, fmt.Sprintf("%s%s=%v", strings.TrimPrefix(p, "/redfish/v1"), "#"+prop, v))
			if looseEqual(v, o.Equals) {
				match = true
			}
		}
		if !match {
			return false, fmt.Sprintf("%s: %s", d.Path, strings.Join(firstN(seen, 4), ", "))
		}
	}
	return true, ""
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func getDotted(m map[string]any, prop string) any {
	cur := any(m)
	for _, p := range strings.Split(prop, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func looseEqual(a, b any) bool {
	norm := func(v any) string {
		switch x := v.(type) {
		case bool:
			if x {
				return "1"
			}
			return "0"
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		case int:
			return strconv.Itoa(x)
		}
		return fmt.Sprint(v)
	}
	return norm(a) == norm(b)
}

// subset reports whether want is contained in got (maps by key, lists by any element).
func subset(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		if gl, ok := got.([]any); ok {
			for _, g := range gl {
				if subset(want, g) {
					return true
				}
			}
			return false
		}
		gm, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if !subset(v, gm[k]) {
				return false
			}
		}
		return true
	case []any:
		for _, x := range w {
			if !subset(x, got) {
				return false
			}
		}
		return true
	}
	return looseEqual(want, got) || reflect.DeepEqual(want, got)
}

func truncate(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// resolveArgs turns "{serial:<selector>}" into the selected device's serial, so a scenario can
// name a device and the accessor receives the identity the stack joins on.
func (r *runner) resolveArgs(args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "{serial:") && strings.HasSuffix(s, "}") {
			if ds, _ := r.t.Engine.SelectDevices(s[len("{serial:") : len(s)-1]); len(ds) > 0 {
				v = ds[0].Identity.Serial
			}
		}
		out[k] = v
	}
	return out
}

// cel evaluates a scenario expression over the model, accessors, events and the clock.
func (r *runner) cel(src string) (bool, error) {
	eng := r.t.Engine
	funcs := expr.Funcs{
		"select": func(arg ref.Val) ref.Val {
			views, err := eng.Select(fmt.Sprint(expr.Arg(arg)))
			if err != nil {
				return expr.Errorf("%v", err)
			}
			out := make([]any, len(views))
			for i, v := range views {
				out[i] = v
			}
			return expr.Native(out)
		},
		"device": func(arg ref.Val) ref.Val {
			ds, _ := eng.SelectDevices(fmt.Sprint(expr.Arg(arg)))
			if len(ds) == 0 {
				return expr.Errorf("no device %v", expr.Arg(arg))
			}
			return expr.Native(eng.DeviceView(ds[0]))
		},
		"count": func(arg ref.Val) ref.Val {
			f, _ := expr.Arg(arg).(map[string]any)
			n := 0
			for _, e := range eng.Events(r.startSeq, nil) {
				if eventMatches(e, f) {
					n++
				}
			}
			return expr.Native(n)
		},
	}
	for alias, fn := range r.accessors {
		fn := fn
		funcs[alias] = func(arg ref.Val) ref.Val {
			a := expr.Arg(arg)
			args, ok := a.(map[string]any)
			if !ok {
				args = map[string]any{"id": a}
			}
			v, err := fn(args)
			if err != nil {
				return expr.Native(map[string]any{"error": err.Error()})
			}
			return expr.Native(v)
		}
	}
	vars := map[string]any{"model": map[string]any{}, "accessors": map[string]any{}, "events": map[string]any{},
		"clock": map[string]any{"elapsed": time.Since(r.stepStart).Seconds(), "now": eng.Now().Format(time.RFC3339)}}
	return expr.Bool(expr.EnvScenario, src, vars, funcs)
}

func eventMatches(e schema.Event, f map[string]any) bool {
	if v, ok := f["kind"].(string); ok && e.Kind != v {
		return false
	}
	if v, ok := f["canonical"].(string); ok && (e.Request == nil || e.Request.Canonical != v) {
		return false
	}
	if v, ok := f["from"].(string); ok && e.Source.Class != v {
		return false
	}
	if v, ok := f["on"].(string); ok {
		sel, err := selector.Parse(v)
		if err != nil || !sel.MatchPath(e.Path) {
			return false
		}
	}
	return true
}

// markUsedFromEvents adds every device the stack or the run touched.
func (r *runner) markUsedFromEvents() {
	for _, e := range r.t.Engine.Events(r.startSeq, func(e schema.Event) bool {
		return e.Kind == "request" || e.Kind == "act" || e.Kind == "fault"
	}) {
		p := e.Path
		if i := strings.Index(p, ":"); i > 0 {
			p = p[:i]
		}
		r.used.Devices[p] = true
	}
}
