package critic

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// The five moments of a fault and the playbook (17 — The five moments of a fault, The playbook).

var momentNames = []string{"injected", "exposed", "acknowledged", "firstAction", "mitigated", "recovered"}

// matchPlaybook gives each fault the first playbook entry that names it.
func (jd *judge) matchPlaybook() {
	for _, f := range jd.h.Faults {
		for i := range jd.res.Playbook {
			p := &jd.res.Playbook[i]
			if !globAny(p.On, f.Fault) {
				continue
			}
			if p.Where != "" {
				ok, _ := jd.ev.truth(p.Where, histWorld{h: jd.h, seq: f.Seq}, map[string]any{"fault": f.view(), "run": jd.runView})
				if !ok {
					continue
				}
			}
			jd.entry[f.Seq] = p
			break
		}
	}
}

// fromStack reports whether an event is a request the software under test made.
func fromStack(e schema.Event) bool {
	return e.Kind == "request" && e.Source.Class == "stack" && e.Request != nil
}

// matchAction reports whether a stack request matches an action pattern of a fault's entry.
func (jd *judge) matchAction(a schema.ActionPattern, f *Fault, e schema.Event) bool {
	if !fromStack(e) {
		return false
	}
	if a.Canonical != "" && !globAny(a.Canonical, e.Request.Canonical) {
		return false
	}
	if a.Class != "" && !globAny(a.Class, class(e)) {
		return false
	}
	w := histWorld{h: jd.h, seq: e.Seq}
	vars := map[string]any{"fault": f.view(), "event": jd.h.EventView(e), "run": jd.runView}
	dev := jd.h.DevicePath(e.Path)
	if a.On != "" {
		sel := jd.ev.text(a.On, w, vars)
		if !jd.h.matchSelector(sel, dev) && !jd.h.matchSelector(sel, e.Path) && sel != e.Path && sel != dev {
			return false
		}
	}
	if a.Except != "" {
		sel := jd.ev.text(a.Except, w, vars)
		if jd.h.matchSelector(sel, dev) || sel == dev || sel == e.Path {
			return false
		}
	}
	if a.When != "" {
		ok, _ := jd.ev.truth(a.When, w, vars)
		if !ok {
			return false
		}
	}
	return true
}

// permitted reports whether any fault open at the request permits it.
func (jd *judge) permitted(e schema.Event) bool {
	for _, f := range jd.h.Faults {
		if !f.openAt(e.Seq) {
			continue
		}
		p := jd.entry[f.Seq]
		if p == nil {
			continue
		}
		for _, a := range p.Permits {
			if jd.matchAction(a, f, e) {
				return true
			}
		}
	}
	return false
}

func (jd *judge) permittedBy(f *Fault, e schema.Event) bool {
	p := jd.entry[f.Seq]
	if p == nil {
		return false
	}
	for _, a := range p.Permits {
		if jd.matchAction(a, f, e) {
			return true
		}
	}
	return false
}

// computeMoments derives each fault's moments from the events after it.
func (jd *judge) computeMoments() {
	h := jd.h
	for _, f := range h.Faults {
		ms := map[string]*Moment{"injected": {Seq: f.Seq, After: 0}}
		jd.moments[f.Seq] = ms
		p := jd.entry[f.Seq]
		set := func(name string, seq uint64) {
			if ms[name] == nil {
				ms[name] = &Moment{Seq: seq, After: round3(h.At(seq).Sub(f.At).Seconds())}
			}
		}
		vars := map[string]any{"fault": f.view(), "run": jd.runView}
		start := h.idx[f.Seq] + 1
		for i := start; i < len(h.Events); i++ {
			e := h.Events[i]
			dev := h.DevicePath(e.Path)
			if ms["exposed"] == nil {
				if fromStack(e) && f.Affected[dev] {
					set("exposed", e.Seq)
				} else if e.Kind == "observe" && e.Data["push"] == true && (f.Affected[dev] || (f.Rack != "" && e.Path == f.Rack)) {
					set("exposed", e.Seq)
				}
			}
			if ms["firstAction"] == nil && fromStack(e) && (class(e) == "write" || class(e) == "destructive") &&
				(f.Affected[dev] || jd.permittedBy(f, e)) {
				set("firstAction", e.Seq)
			}
			if !h.evalPoint(e) {
				continue
			}
			w := histWorld{h: h, seq: e.Seq}
			if p != nil {
				if ms["acknowledged"] == nil && p.Acknowledged != "" {
					if ok, _ := jd.ev.truth(p.Acknowledged, w, vars); ok {
						set("acknowledged", e.Seq)
					}
				}
				if ms["mitigated"] == nil && p.Mitigated != "" {
					if ok, _ := jd.ev.truth(p.Mitigated, w, vars); ok {
						set("mitigated", e.Seq)
					}
				}
			}
			if f.ClearSeq != 0 && e.Seq >= f.ClearSeq && ms["recovered"] == nil {
				if p != nil && p.Then != nil && p.Then.Recovered != "" {
					if ok, _ := jd.ev.truth(p.Then.Recovered, w, vars); ok {
						set("recovered", e.Seq)
					}
				} else if jd.healthyAgain(f, e.Seq) {
					set("recovered", e.Seq)
				}
			}
		}
	}
}

// healthyAgain reports whether every device a fault reached is healthy as of an event.
func (jd *judge) healthyAgain(f *Fault, seq uint64) bool {
	for d := range f.Affected {
		if v, ok := jd.h.State(d, "health", seq); ok && fmt.Sprint(v) != "OK" {
			return false
		}
	}
	return true
}

// playbookRules turns each fault's entry into judgements: what it forbids, whether it was
// acknowledged and mitigated in time, and whether it recovered.
func (jd *judge) playbookRules() {
	h := jd.h
	for _, f := range h.Faults {
		p := jd.entry[f.Seq]
		if p == nil {
			continue
		}
		key := p.Key == nil || *p.Key
		base := p.Policy + "/playbook/" + p.On
		ms := jd.moments[f.Seq]
		// forbids: while the fault is open, from injection to its clear (or the run's end)
		if len(p.Forbids) > 0 {
			bad := 0
			for i := h.idx[f.Seq] + 1; i < len(h.Events); i++ {
				e := h.Events[i]
				if f.ClearSeq != 0 && e.Seq > f.ClearSeq {
					break
				}
				if !fromStack(e) {
					continue
				}
				for _, a := range p.Forbids {
					if !jd.matchAction(a, f, e) || jd.permitted(e) {
						continue
					}
					bad++
					if bad <= 25 {
						pat := jd.resolvePattern(a, f, e)
						jd.violation(Violation{Rule: base + "/forbids", Policy: p.Policy, Dimension: "safety", Key: true, Fault: f.ID,
							Message:  fmt.Sprintf("the playbook for %s forbids %s while it is open", f.Fault, describePattern(pat)),
							Expected: "no " + describePattern(pat) + " while " + f.Fault + " is open on " + shortPath(f.Target),
							Observed: describeRequest(e)}, e.Seq)
					}
					break
				}
			}
			jd.item("safety", base+"/forbids@"+f.ID, map[bool]string{true: "pass", false: "fail"}[bad == 0], "")
		}
		within := 0.0
		if d, err := time.ParseDuration(p.Within); err == nil {
			within = d.Seconds()
		}
		check := func(name, cond string) {
			if cond == "" {
				return
			}
			id := base + "/" + name
			m := ms[name]
			switch {
			case m != nil && (within == 0 || m.After <= within):
				jd.item("correctness", id+"@"+f.ID, "pass", fmt.Sprintf("%s after %.1fs", name, m.After))
			case m == nil && within > 0 && h.Run.Ended.Sub(f.At).Seconds() < within:
				jd.na(id+"@"+f.ID, fmt.Sprintf("the run ended %.0fs after the fault, before its %s window of %s", h.Run.Ended.Sub(f.At).Seconds(), name, p.Within))
			case m == nil && within > 0 && f.ClearSeq != 0 && f.ClearAt.Sub(f.At).Seconds() < within:
				jd.na(id+"@"+f.ID, fmt.Sprintf("the fault was cleared %.0fs after injection, before its %s window of %s", f.ClearAt.Sub(f.At).Seconds(), name, p.Within))
			default:
				obs := "never, in the " + fmt.Sprintf("%.0fs", h.Run.Ended.Sub(f.At).Seconds()) + " the run lasted after the fault"
				seq := f.Seq
				if m != nil {
					obs, seq = fmt.Sprintf("after %.1fs", m.After), m.Seq
				}
				jd.violation(Violation{Rule: id, Policy: p.Policy, Dimension: "correctness", Key: key, Fault: f.ID,
					Message:  fmt.Sprintf("%s on %s was not %s within %s", f.Fault, shortPath(f.Target), name, p.Within),
					Expected: name + " (" + cond + ") within " + p.Within, Observed: obs}, seq)
				jd.item("correctness", id+"@"+f.ID, "fail", obs)
			}
		}
		check("acknowledged", p.Acknowledged)
		check("mitigated", p.Mitigated)
		if p.Then != nil && p.Then.Recovered != "" && f.ClearSeq != 0 {
			id := base + "/recovered"
			lim, _ := time.ParseDuration(p.Then.Within)
			m := ms["recovered"]
			sinceClear := 0.0
			if m != nil {
				sinceClear = h.At(m.Seq).Sub(f.ClearAt).Seconds()
			}
			switch {
			case m != nil && (lim == 0 || sinceClear <= lim.Seconds()):
				jd.item("correctness", id+"@"+f.ID, "pass", fmt.Sprintf("recovered %.1fs after the clear", sinceClear))
			case m == nil && lim > 0 && h.Run.Ended.Sub(f.ClearAt) < lim:
				jd.na(id+"@"+f.ID, "the run ended before the recovery window closed")
			default:
				jd.violation(Violation{Rule: id, Policy: p.Policy, Dimension: "correctness", Key: false, Fault: f.ID,
					Message:  fmt.Sprintf("%s on %s did not recover within %s of its clear", f.Fault, shortPath(f.Target), p.Then.Within),
					Expected: p.Then.Recovered}, f.ClearSeq)
				jd.item("correctness", id+"@"+f.ID, "fail", "")
			}
		}
	}
}

// resolvePattern evaluates a pattern's selectors for a fault, for reading.
func (jd *judge) resolvePattern(a schema.ActionPattern, f *Fault, e schema.Event) schema.ActionPattern {
	w := histWorld{h: jd.h, seq: e.Seq}
	vars := map[string]any{"fault": f.view(), "event": jd.h.EventView(e), "run": jd.runView}
	if a.On != "" {
		a.On = shortPath(jd.ev.text(a.On, w, vars))
	}
	if a.Except != "" {
		a.Except = shortPath(jd.ev.text(a.Except, w, vars))
	}
	return a
}

func describePattern(a schema.ActionPattern) string {
	var parts []string
	if a.Class != "" {
		parts = append(parts, a.Class)
	}
	if a.Canonical != "" {
		parts = append(parts, a.Canonical)
	}
	if len(parts) == 0 {
		parts = append(parts, "any request")
	}
	s := strings.Join(parts, " ")
	if a.On != "" {
		s += " on " + a.On
	}
	if a.Except != "" {
		s += " except " + a.Except
	}
	return s
}

func describeRequest(e schema.Event) string {
	if e.Request == nil {
		return e.Message
	}
	return fmt.Sprintf("%s (%s %s %s) on %s from %s", e.Request.Canonical, e.Request.Protocol, e.Request.Method, e.Request.Target,
		shortPath(e.Path), e.Source.Principal)
}

// faultJudgements summarises each fault: its moments and the stack's response to it.
func (jd *judge) faultJudgements() {
	h := jd.h
	for _, f := range h.Faults {
		fj := FaultJudgement{ID: f.ID, Fault: f.Fault, Target: f.Target, Device: f.Device, Seq: f.Seq, Injected: jd.offset(f.At),
			Moments: jd.moments[f.Seq], Response: jd.response(f)}
		if f.ClearSeq != 0 {
			c := jd.offset(f.ClearAt)
			fj.Cleared = &c
		}
		for d := range f.Affected {
			fj.Affected = append(fj.Affected, d)
		}
		sort.Strings(fj.Affected)
		if p := jd.entry[f.Seq]; p != nil {
			fj.Playbook = p.Policy + ": " + p.On
		}
		jd.j.Faults = append(jd.j.Faults, fj)
	}
}

// window is the span a fault's response is counted in: from its exposure (or injection) to its
// recovery (or clear, or the run's end).
func (jd *judge) window(f *Fault) (uint64, uint64) {
	ms := jd.moments[f.Seq]
	from := f.Seq
	if m := ms["exposed"]; m != nil {
		from = m.Seq
	}
	to := jd.h.Run.EndSeq
	if m := ms["recovered"]; m != nil {
		to = m.Seq
	} else if f.ClearSeq != 0 {
		to = f.ClearSeq
	}
	return from, to
}

// response is what the stack did while a fault was its business, and what it touched that the
// fault did not reach and no playbook permitted (collateral).
func (jd *judge) response(f *Fault) Response {
	h := jd.h
	r := Response{ByCanonical: map[string]int{}}
	from, to := jd.window(f)
	princ := map[string]bool{}
	coll := map[string]bool{}
	for i := h.idx[from]; i < len(h.Events); i++ {
		e := h.Events[i]
		if e.Seq > to {
			break
		}
		if !fromStack(e) {
			continue
		}
		r.Requests++
		cls := class(e)
		if cls == "write" || cls == "destructive" {
			r.Writes++
			r.ByCanonical[e.Request.Canonical]++
			princ[h.Principal(e)] = true
			dev := h.DevicePath(e.Path)
			if cls == "destructive" {
				r.Destructive++
			}
			// collateral is damage: a destructive request outside every open fault's reach that no
			// playbook permitted (a session opened is a write, not damage)
			if cls == "destructive" && !f.Affected[dev] && !jd.permitted(e) && !h.Affected(dev, e.Seq) {
				coll[dev] = true
			}
		}
	}
	for p := range princ {
		r.Principals = append(r.Principals, p)
	}
	sort.Strings(r.Principals)
	for d := range coll {
		r.Collateral = append(r.Collateral, d)
		if dv := h.Devices[d]; dv != nil {
			r.CollateralAccelerators += dv.Accelerators
		}
	}
	sort.Strings(r.Collateral)
	return r
}
