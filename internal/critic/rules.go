package critic

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// maxPerRule bounds the violations listed per rule; the rest are counted in the last one.
const maxPerRule = 25

// invariants checks every rule of the resolved policy (17 — Invariants).
func (jd *judge) invariants() {
	for _, r := range jd.res.Rules {
		if d, off := jd.res.disabled(r.ID); off {
			jd.na(r.ID, "disabled by "+d.Policy+": "+d.Reason)
			continue
		}
		if why := jd.requires(r.Requires); why != "" {
			jd.na(r.ID, why)
			continue
		}
		var n, inconclusive, triggers int
		switch {
		case r.Never != nil:
			n = jd.never(r)
		case r.Always != nil:
			n = jd.always(r)
		case r.LeadsTo != nil:
			n, inconclusive, triggers = jd.leadsTo(r)
		case r.AtMost != nil:
			n = jd.atMost(r)
		case r.Sometimes != nil:
			jd.sometimes(r)
			continue
		default:
			jd.na(r.ID, "the rule has no form")
			continue
		}
		dim := r.Dimension
		if dim == "" {
			dim = "correctness"
		}
		switch {
		case n > 0 && r.Severity == "warning":
			jd.item(dim, r.ID, "warn", fmt.Sprintf("%d violation(s)", n))
		case n > 0:
			jd.item(dim, r.ID, "fail", fmt.Sprintf("%d violation(s)", n))
		case r.LeadsTo != nil && triggers > 0 && inconclusive == triggers:
			jd.na(r.ID, fmt.Sprintf("%d trigger(s), none with its window inside the run", triggers))
		default:
			jd.item(dim, r.ID, "pass", "")
		}
	}
}

func (jd *judge) ruleViolation(r Rule, msg, expected, observed string, seqs ...uint64) {
	m := r.Message
	if m == "" {
		m = r.Name
	}
	if msg != "" {
		m += ": " + msg
	}
	jd.violation(Violation{Rule: r.ID, Policy: r.Policy, Dimension: r.Dimension, Severity: r.Severity, Key: r.Key,
		Message: m, Expected: expected, Observed: observed}, seqs...)
}

// eventVars are the variables a condition about an event sees.
func (jd *judge) eventVars(e schema.Event) map[string]any {
	dev := jd.h.DevicePath(e.Path)
	return map[string]any{"event": jd.h.EventView(e), "before": jd.h.View(dev, e.Seq-1), "affected": jd.h.Affected(dev, e.Seq),
		"permitted": fromStack(e) && jd.permitted(e), "run": jd.runView}
}

// never: no event matches the filter and the condition.
func (jd *judge) never(r Rule) int {
	h := jd.h
	f := fromSchema(r.Never.Event)
	n := 0
	for _, e := range h.Events {
		if e.Kind == "run" || !h.matchEvent(f, e) {
			continue
		}
		if r.Never.Where != "" {
			ok, _ := jd.ev.truth(r.Never.Where, histWorld{h, e.Seq, 10 * time.Second}, jd.eventVars(e))
			if !ok {
				continue
			}
		}
		n++
		if n <= maxPerRule {
			jd.ruleViolation(r, "", "never: "+describeFilter(r.Never.Event)+whereText(r.Never.Where), describeEvent(h, e), e.Seq)
		}
	}
	if n > maxPerRule {
		jd.j.Violations[len(jd.j.Violations)-1].Message += fmt.Sprintf(" (and %d more)", n-maxPerRule)
	}
	return n
}

// always: the condition holds after every event that touches its scope, and at every sample.
// A violation is reported where it stops holding, once per lapse.
func (jd *judge) always(r Rule) int {
	h := jd.h
	n := 0
	var devs []string
	if r.Always.Each != "" {
		for p := range h.Devices {
			if h.matchSelector(r.Always.Each, p) {
				devs = append(devs, p)
			}
		}
		sort.Strings(devs)
	}
	failing := map[string]bool{}
	check := func(scope string, seq uint64, vars map[string]any) {
		ok, _ := jd.ev.truth(r.Always.Holds, histWorld{h: h, seq: seq}, vars)
		if ok {
			delete(failing, scope)
			return
		}
		if failing[scope] {
			return
		}
		failing[scope] = true
		n++
		if n <= maxPerRule {
			obs := "false after event #" + fmt.Sprint(seq)
			if scope != "" {
				obs += " for " + shortPath(scope)
			}
			jd.ruleViolation(r, "", "always: "+r.Always.Holds, obs, seq)
		}
	}
	for i, seq := range h.evalSeqs {
		e, _ := h.Event(seq)
		if len(devs) == 0 {
			check("", seq, map[string]any{"run": jd.runView})
			continue
		}
		touched := h.DevicePath(e.Path)
		for _, d := range devs {
			if i == 0 || d == touched || e.Kind == "observe" {
				check(d, seq, map[string]any{"device": h.View(d, seq), "run": jd.runView})
			}
		}
	}
	return n
}

type trigger struct {
	seq   uint64
	at    time.Time
	fault *Fault
	event *schema.Event
}

// leadsTo: every trigger is followed by its response within the window. It reports violations,
// triggers whose window outlived the run, and triggers.
func (jd *judge) leadsTo(r Rule) (int, int, int) {
	h := jd.h
	lt := r.LeadsTo
	within, _ := time.ParseDuration(lt.Within)
	var ts []trigger
	switch {
	case lt.When.Fault != "":
		for _, f := range h.Faults {
			if !globAny(lt.When.Fault, f.Fault) {
				continue
			}
			if lt.When.Where != "" {
				if ok, _ := jd.ev.truth(lt.When.Where, histWorld{h: h, seq: f.Seq}, map[string]any{"fault": f.view(), "run": jd.runView}); !ok {
					continue
				}
			}
			ts = append(ts, trigger{seq: f.Seq, at: f.At, fault: f})
		}
	case lt.When.Event != nil:
		flt := fromSchema(*lt.When.Event)
		for i := range h.Events {
			e := h.Events[i]
			if e.Kind == "run" || !h.matchEvent(flt, e) {
				continue
			}
			if lt.When.Where != "" {
				if ok, _ := jd.ev.truth(lt.When.Where, histWorld{h: h, seq: e.Seq}, jd.eventVars(e)); !ok {
					continue
				}
			}
			ts = append(ts, trigger{seq: e.Seq, at: h.time(e), event: &h.Events[i]})
		}
	case lt.When.Holds != "":
		was := false
		for _, seq := range h.evalSeqs {
			ok, _ := jd.ev.truth(lt.When.Holds, histWorld{h: h, seq: seq}, map[string]any{"run": jd.runView})
			if ok && !was {
				ts = append(ts, trigger{seq: seq, at: h.At(seq)})
			}
			was = ok
		}
	}
	n, inconclusive := 0, 0
	for _, t := range ts {
		vars := map[string]any{"run": jd.runView}
		if t.fault != nil {
			vars["fault"] = t.fault.view()
		}
		if t.event != nil {
			for k, v := range jd.eventVars(*t.event) {
				vars[k] = v
			}
		}
		if lt.Unless != "" {
			if ok, _ := jd.ev.truth(lt.Unless, histWorld{h: h, seq: t.seq}, vars); ok {
				continue
			}
		}
		if jd.satisfied(lt.Then, t, within, vars) {
			continue
		}
		deadline := t.at.Add(within)
		if t.fault != nil && t.fault.ClearSeq != 0 && t.fault.ClearAt.Before(deadline) {
			inconclusive++ // the fault went away before the stack had to answer it
			continue
		}
		if h.Run.Ended.Before(deadline) {
			inconclusive++
			continue
		}
		n++
		if n <= maxPerRule {
			what := ""
			if t.fault != nil {
				what = t.fault.Fault + " on " + shortPath(t.fault.Target)
			} else if t.event != nil {
				what = describeEvent(h, *t.event)
			}
			jd.ruleViolation(r, what, "then "+describeTrigger(lt.Then)+" within "+lt.Within, "not within "+lt.Within, t.seq)
			if t.fault != nil {
				jd.j.Violations[len(jd.j.Violations)-1].Fault = t.fault.ID
			}
		}
	}
	return n, inconclusive, len(ts)
}

// satisfied reports whether a trigger's response happened within its window.
func (jd *judge) satisfied(then schema.Trigger, t trigger, within time.Duration, vars map[string]any) bool {
	h := jd.h
	deadline := t.at.Add(within)
	if then.Moment != "" && t.fault != nil {
		for _, name := range strings.Split(then.Moment, "|") {
			if m := jd.moments[t.fault.Seq][strings.TrimSpace(name)]; m != nil && !h.At(m.Seq).After(deadline) {
				return true
			}
		}
		return false
	}
	if then.Event != nil {
		flt := fromSchema(*then.Event)
		flt.On = jd.placeholders(flt.On, t)
		for i := h.idx[t.seq] + 1; i < len(h.Events); i++ {
			e := h.Events[i]
			if h.time(e).After(deadline) {
				break
			}
			if h.matchEvent(flt, e) {
				if then.Where == "" {
					return true
				}
				if ok, _ := jd.ev.truth(then.Where, histWorld{h: h, seq: e.Seq}, jd.eventVars(e)); ok {
					return true
				}
			}
		}
		return false
	}
	if then.Holds != "" {
		for _, seq := range h.evalSeqs {
			if seq <= t.seq {
				continue
			}
			if h.At(seq).After(deadline) {
				break
			}
			if ok, _ := jd.ev.truth(then.Holds, histWorld{h: h, seq: seq}, vars); ok {
				return true
			}
		}
	}
	return false
}

// placeholders fills {fault.device}, {fault.rack}, {event.device} in a selector.
func (jd *judge) placeholders(s string, t trigger) string {
	if t.fault != nil {
		s = strings.NewReplacer("{fault.device}", t.fault.Device, "{fault.rack}", t.fault.Rack, "{fault.path}", t.fault.Target).Replace(s)
	}
	if t.event != nil {
		s = strings.ReplaceAll(s, "{event.device}", jd.h.DevicePath(t.event.Path))
	}
	return s
}

// atMost: matching events per key stay within a budget, over a sliding window when one is set.
// A breach is reported once, and again only after the count has fallen back within the budget.
func (jd *judge) atMost(r Rule) int {
	h := jd.h
	am := r.AtMost
	f := fromSchema(am.Event)
	var window time.Duration
	if am.Window != "" {
		window, _ = time.ParseDuration(am.Window)
	}
	type hit struct {
		seq uint64
		at  time.Time
	}
	recent := map[string][]hit{}
	breached := map[string]bool{}
	n := 0
	for _, e := range h.Events {
		if e.Kind == "run" || !h.matchEvent(f, e) {
			continue
		}
		if am.Where != "" {
			if ok, _ := jd.ev.truth(am.Where, histWorld{h: h, seq: e.Seq}, jd.eventVars(e)); !ok {
				continue
			}
		}
		key := jd.perKey(am.Per, e)
		at := h.time(e)
		hs := append(recent[key], hit{e.Seq, at})
		if window > 0 {
			k := 0
			for k < len(hs) && at.Sub(hs[k].at) > window {
				k++
			}
			hs = hs[k:]
		}
		recent[key] = hs
		if len(hs) <= am.Max {
			breached[key] = false
			continue
		}
		if breached[key] {
			continue
		}
		breached[key] = true
		n++
		if n <= maxPerRule {
			var seqs []uint64
			for i := len(hs) - 1; i >= 0 && len(seqs) < 10; i-- {
				seqs = append([]uint64{hs[i].seq}, seqs...)
			}
			seqs = append([]uint64{e.Seq}, seqs[:len(seqs)-1]...)
			span := ""
			if window > 0 {
				span = " within " + am.Window
			}
			scope := key
			if scope == "" {
				scope = "the run"
			}
			jd.ruleViolation(r, "", fmt.Sprintf("at most %d %s per %s%s", am.Max, describeFilter(am.Event), orRun(am.Per), span),
				fmt.Sprintf("%d for %s%s", len(hs), shortPath(scope), span), seqs...)
		}
	}
	return n
}

func orRun(per string) string {
	if per == "" {
		return "run"
	}
	return per
}

func (jd *judge) perKey(per string, e schema.Event) string {
	dev := jd.h.DevicePath(e.Path)
	can := ""
	if e.Request != nil {
		can = e.Request.Canonical
	}
	switch per {
	case "device":
		return dev
	case "device+canonical":
		return dev + " " + can
	case "principal":
		return jd.h.Principal(e)
	case "rack":
		if d := jd.h.Devices[dev]; d != nil {
			return d.Rack
		}
		return dev
	}
	return ""
}

// sometimes: it happened at least once; unmet, it is uncovered, never a failure of the SUT.
func (jd *judge) sometimes(r Rule) {
	h := jd.h
	hit := false
	if r.Sometimes.Event != nil {
		f := fromSchema(*r.Sometimes.Event)
		for _, e := range h.Events {
			if e.Kind != "run" && h.matchEvent(f, e) {
				hit = true
				break
			}
		}
	}
	if !hit && r.Sometimes.Holds != "" {
		for _, seq := range h.evalSeqs {
			if ok, _ := jd.ev.truth(r.Sometimes.Holds, histWorld{h: h, seq: seq}, map[string]any{"run": jd.runView}); ok {
				hit = true
				break
			}
		}
	}
	jd.j.Coverage.Sometimes = append(jd.j.Coverage.Sometimes, CoverageItem{ID: r.ID, Exercised: hit})
	jd.item("coverage", r.ID, map[bool]string{true: "pass", false: "fail"}[hit], "")
}

// ---------------------------------------------------------------- descriptions

func describeFilter(f schema.EventFilter) string {
	var parts []string
	if f.From != "" {
		parts = append(parts, f.From)
	}
	if f.Class != "" {
		parts = append(parts, f.Class)
	}
	if f.Canonical != "" {
		parts = append(parts, f.Canonical)
	}
	if f.Kind != "" {
		parts = append(parts, f.Kind+"s")
	}
	if f.Field != "" {
		parts = append(parts, f.Field)
	}
	s := strings.Join(parts, " ")
	if f.On != "" {
		s += " on " + f.On
	}
	if s == "" {
		s = "events"
	}
	return s
}

func whereText(w string) string {
	if w == "" {
		return ""
	}
	return " where " + w
}

func describeTrigger(t schema.Trigger) string {
	switch {
	case t.Moment != "":
		return "the fault's " + t.Moment
	case t.Event != nil:
		return describeFilter(*t.Event)
	case t.Holds != "":
		return t.Holds
	}
	return "?"
}

func describeEvent(h *History, e schema.Event) string {
	if e.Request != nil {
		return fmt.Sprintf("%s by %s on %s (#%d)", e.Request.Canonical, h.Principal(e), shortPath(e.Path), e.Seq)
	}
	if e.State != nil {
		return fmt.Sprintf("%s %s: %v → %v (#%d)", shortPath(e.Path), e.State.Field, e.State.From, e.State.To, e.Seq)
	}
	return fmt.Sprintf("%s %s (#%d)", e.Kind, e.Message, e.Seq)
}
