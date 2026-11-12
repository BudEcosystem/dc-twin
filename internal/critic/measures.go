package critic

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// measureDefs are the built-in measures (17 — Measures): unit and what they are per.
var measureDefs = map[string]struct{ per, unit, dim string }{
	"fault.exposed":                    {"fault", "s", "timeliness"},
	"fault.acknowledged":               {"fault", "s", "timeliness"},
	"fault.firstAction":                {"fault", "s", "timeliness"},
	"fault.mitigated":                  {"fault", "s", "timeliness"},
	"fault.recovered":                  {"fault", "s", "timeliness"},
	"response.requests":                {"fault", "requests", "restraint"},
	"response.writes":                  {"fault", "requests", "restraint"},
	"response.destructive":             {"fault", "requests", "restraint"},
	"response.collateral.devices":      {"fault", "devices", "safety"},
	"response.collateral.accelerators": {"fault", "accelerators", "safety"},
	"requests.total":                   {"run", "requests", "restraint"},
	"requests.writes":                  {"run", "requests", "restraint"},
	"requests.destructive":             {"run", "requests", "restraint"},
	"requests.perMinute":               {"run", "requests/min", "restraint"},
	"requests.peakPerDeviceMinute":     {"run", "requests/min", "restraint"},
	"requests.errorRatio":              {"run", "ratio", "hygiene"},
	"sessions.leaked":                  {"run", "sessions", "hygiene"},
	"capacity.lost":                    {"run", "accelerator-s", "correctness"},
	"ingest.time":                      {"device", "s", "timeliness"},
	// day 0 (18 — Measures): per factory switch, from the day-0 observer's first look at it
	"day0.toLease":         {"device", "s", "timeliness"},
	"day0.toZTP":           {"device", "s", "timeliness"},
	"day0.toZTPDone":       {"device", "s", "timeliness"},
	"day0.toConverged":     {"device", "s", "timeliness"},
	"day0.applies":         {"device", "requests", "restraint"},
	"day0.appliesFailed":   {"device", "requests", "correctness"},
	"day0.ztpFailures":     {"device", "failures", "correctness"},
	"day0.convergedMedian": {"run", "s", "timeliness"},
	"day0.convergedMax":    {"run", "s", "timeliness"},
	"day0.refused":         {"run", "requests", "correctness"},
}

func knownMeasure(name string) bool {
	_, ok := measureDefs[name]
	return ok
}

func (jd *judge) measure(name, subject string, v float64, witness ...uint64) {
	d := measureDefs[name]
	jd.j.Measures = append(jd.j.Measures, Measure{Name: name, Per: d.per, Subject: subject, Value: round3(v), Unit: d.unit, Witness: witness})
}

// measures computes every built-in measure for the run.
func (jd *judge) measures() {
	h := jd.h
	for _, f := range h.Faults {
		ms := jd.moments[f.Seq]
		for _, n := range []string{"exposed", "acknowledged", "firstAction", "mitigated", "recovered"} {
			if m := ms[n]; m != nil {
				jd.measure("fault."+n, f.ID, m.After, m.Seq)
			}
		}
		r := jd.response(f)
		jd.measure("response.requests", f.ID, float64(r.Requests))
		jd.measure("response.writes", f.ID, float64(r.Writes))
		jd.measure("response.destructive", f.ID, float64(r.Destructive))
		jd.measure("response.collateral.devices", f.ID, float64(len(r.Collateral)))
		jd.measure("response.collateral.accelerators", f.ID, float64(r.CollateralAccelerators))
	}
	total, writes, destr, withStatus, errs := 0, 0, 0, 0, 0
	sessions := map[string]int{}
	perDev := map[string][]time.Time{}
	peak, peakSeq := 0, uint64(0)
	for _, e := range h.Events {
		if !fromStack(e) {
			continue
		}
		total++
		switch class(e) {
		case "write":
			writes++
		case "destructive":
			writes++
			destr++
		}
		if s := e.Request.Status; s > 0 {
			withStatus++
			if s >= 400 {
				errs++
			}
		}
		ok := e.Request.Status == 0 || e.Request.Status < 300
		switch e.Request.Canonical {
		case "session.create":
			if ok {
				sessions[h.Principal(e)]++
			}
		case "session.delete":
			if ok {
				sessions[h.Principal(e)]--
			}
		}
		dev := h.DevicePath(e.Path)
		t := h.time(e)
		ts := append(perDev[dev], t)
		k := 0
		for k < len(ts) && t.Sub(ts[k]) > time.Minute {
			k++
		}
		ts = ts[k:]
		perDev[dev] = ts
		if len(ts) > peak {
			peak, peakSeq = len(ts), e.Seq
		}
	}
	mins := h.Run.Ended.Sub(h.Run.Started).Minutes()
	jd.measure("requests.total", "", float64(total))
	jd.measure("requests.writes", "", float64(writes))
	jd.measure("requests.destructive", "", float64(destr))
	if mins > 0 {
		jd.measure("requests.perMinute", "", float64(total)/mins)
	}
	if total > 0 {
		jd.measure("requests.peakPerDeviceMinute", "", float64(peak), peakSeq)
	}
	if withStatus > 0 {
		jd.measure("requests.errorRatio", "", float64(errs)/float64(withStatus))
	}
	if jd.hasStk {
		leaked := 0
		for _, n := range sessions {
			if n > 0 {
				leaked += n
			}
		}
		jd.measure("sessions.leaked", "", float64(leaked))
	}
	jd.measure("capacity.lost", "", jd.capacityLost())
	jd.ingestTimes()
	jd.day0Measures()
}

// day0Measures are day 0's measures (18 — Measures), from the NOS observer's state events (D72,
// policies/day0/fabric.yaml says which): per factory switch, the time from the observer's first
// day-0 observation of it (its first look after the switch powers on) to its first management
// lease, to its ZTP running, to ZTP done and to converged; the configuration applies the stack
// made to it and those that failed; and its ZTP failures. For the fabric: the median and the
// maximum time to converged. For the run: the configuration changes the switches' NVUE refused by
// name, a gap of the twin's schema reported against the twin, not the stack. A run with no
// day-0 observation has none of them.
func (jd *judge) day0Measures() {
	h := jd.h
	type sw struct {
		first                          time.Time
		lease, ztp, ztpDone, converged *Measure
		applies, failed, ztpFailures   int
	}
	switches := map[string]*sw{}
	var order []string
	at := func(s *sw, e schema.Event, name string) *Measure {
		return &Measure{Name: name, Value: h.time(e).Sub(s.first).Seconds(), Witness: []uint64{e.Seq}}
	}
	refused := 0
	for _, e := range h.Events {
		if e.Kind == "state" && e.State != nil && e.Source.Class == "observer" && (e.State.Field == "day0.phase" || e.State.Field == "mgmt.lease") {
			dev := h.DevicePath(e.Path)
			s := switches[dev]
			if s == nil {
				if e.State.Field != "day0.phase" {
					continue
				}
				s = &sw{first: h.time(e)}
				switches[dev] = s
				order = append(order, dev)
			}
			to := fmt.Sprint(e.State.To)
			switch {
			case e.State.Field == "mgmt.lease" && to != "" && to != "<nil>" && s.lease == nil:
				s.lease = at(s, e, "day0.toLease")
			case e.State.Field == "day0.phase" && to == "ztp-running" && s.ztp == nil:
				s.ztp = at(s, e, "day0.toZTP")
			case e.State.Field == "day0.phase" && to == "ztp-done" && s.ztpDone == nil:
				s.ztpDone = at(s, e, "day0.toZTPDone")
			case e.State.Field == "day0.phase" && to == "ztp-failed":
				s.ztpFailures++
			case e.State.Field == "day0.phase" && to == "converged" && s.converged == nil:
				s.converged = at(s, e, "day0.toConverged")
			}
			continue
		}
		if !fromStack(e) || e.Request == nil || !strings.HasPrefix(e.Request.Canonical, "network.config.") {
			continue
		}
		if e.Request.Method == "revision.refuse" {
			refused++
			continue
		}
		if e.Request.Canonical == "network.config.apply" {
			if s := switches[h.DevicePath(e.Path)]; s != nil {
				s.applies++
				if e.Request.Status >= 400 {
					s.failed++
				}
			}
		}
	}
	if len(switches) == 0 {
		return
	}
	var conv []float64
	for _, dev := range order {
		s := switches[dev]
		for _, m := range []*Measure{s.lease, s.ztp, s.ztpDone, s.converged} {
			if m != nil {
				jd.measure(m.Name, dev, m.Value, m.Witness...)
			}
		}
		if s.converged != nil {
			conv = append(conv, s.converged.Value)
		}
		jd.measure("day0.applies", dev, float64(s.applies))
		jd.measure("day0.appliesFailed", dev, float64(s.failed))
		jd.measure("day0.ztpFailures", dev, float64(s.ztpFailures))
	}
	if len(conv) > 0 {
		sort.Float64s(conv)
		med := conv[len(conv)/2]
		if len(conv)%2 == 0 {
			med = (conv[len(conv)/2-1] + conv[len(conv)/2]) / 2
		}
		jd.measure("day0.convergedMedian", "", med)
		jd.measure("day0.convergedMax", "", conv[len(conv)-1])
	}
	jd.measure("day0.refused", "", float64(refused))
}

// capacityLost integrates, over the run, the accelerators of every device that was off, without
// AC or Critical (accelerator-seconds).
func (jd *judge) capacityLost() float64 {
	h := jd.h
	lost := 0.0
	for p, d := range h.Devices {
		if d.Accelerators == 0 {
			continue
		}
		var seqs []uint64
		for _, f := range []string{"power", "ac", "health"} {
			for _, pt := range h.timeline[p][f] {
				seqs = append(seqs, pt.seq)
			}
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		down := func(seq uint64) bool {
			pw, _ := h.State(p, "power", seq)
			ac, okAC := h.State(p, "ac", seq)
			hl, _ := h.State(p, "health", seq)
			return (pw != nil && fmt.Sprint(pw) != "On") || (okAC && ac == false) || fmt.Sprint(hl) == "Critical"
		}
		at, was := h.Run.Started, down(h.Run.StartSeq)
		for _, s := range seqs {
			t := h.At(s)
			if was {
				lost += t.Sub(at).Seconds() * float64(d.Accelerators)
			}
			at, was = t, down(s)
		}
		if was {
			lost += h.Run.Ended.Sub(at).Seconds() * float64(d.Accelerators)
		}
	}
	return lost
}

// ingestTimes measures, per device, the time from the run's start to the stack's first answer
// that the machine is Ready.
func (jd *judge) ingestTimes() {
	h := jd.h
	target := h.Run.Aliases["machine"]
	if target == "" {
		return
	}
	done := map[string]bool{}
	for _, e := range h.Events {
		if e.Kind != "observe" || str(e.Data["accessor"]) != target {
			continue
		}
		args := mapOf(e.Data["args"])
		dev := h.bySerial[str(args["id"])]
		if dev == "" || done[dev] {
			continue
		}
		v, _ := h.standardise("machine", e.Data["value"]).(map[string]any)
		if str(v["standardState"]) == "Ready" {
			done[dev] = true
			jd.measure("ingest.time", dev, h.time(e).Sub(h.Run.Started).Seconds(), e.Seq)
		}
	}
}

// ---------------------------------------------------------------- objectives

type criterion struct {
	op       string
	value    float64
	relative bool // a percentage of the baseline
}

// parseCriterion reads "<=45s", "==0", ">=0.9", "<=+10%".
func parseCriterion(s string) (criterion, error) {
	s = strings.TrimSpace(s)
	var c criterion
	for _, op := range []string{"<=", ">=", "==", "!=", "<", ">"} {
		if strings.HasPrefix(s, op) {
			c.op, s = op, strings.TrimSpace(s[len(op):])
			break
		}
	}
	if c.op == "" {
		c.op = "<="
	}
	if strings.HasSuffix(s, "%") {
		c.relative = true
		v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(s, "+"), "%"), 64)
		if err != nil {
			return c, fmt.Errorf("criterion %q: %v", s, err)
		}
		c.value = v / 100
		return c, nil
	}
	if d, err := time.ParseDuration(s); err == nil && strings.IndexAny(s, "smh") > 0 {
		c.value = d.Seconds()
		return c, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return c, fmt.Errorf("criterion %q: want a number, a duration or a percentage", s)
	}
	c.value = v
	return c, nil
}

func (c criterion) holds(v float64, baseline *float64) bool {
	want := c.value
	if c.relative {
		if baseline == nil {
			return false
		}
		want = *baseline * (1 + c.value)
	}
	const eps = 1e-9
	switch c.op {
	case "<=":
		return v <= want+eps
	case "<":
		return v < want
	case ">=":
		return v >= want-eps
	case ">":
		return v > want
	case "==":
		return math.Abs(v-want) < eps
	case "!=":
		return math.Abs(v-want) >= eps
	}
	return false
}

func aggregate(how string, vs []float64) float64 {
	sort.Float64s(vs)
	switch how {
	case "min":
		return vs[0]
	case "sum":
		s := 0.0
		for _, v := range vs {
			s += v
		}
		return s
	case "mean":
		s := 0.0
		for _, v := range vs {
			s += v
		}
		return s / float64(len(vs))
	case "p50":
		return vs[(len(vs)-1)/2]
	case "p95":
		return vs[int(math.Ceil(0.95*float64(len(vs))))-1]
	}
	return vs[len(vs)-1]
}

// objectives judges each objective against its measure, aggregated over its subjects.
func (jd *judge) objectives() {
	faultByID := map[string]*Fault{}
	for _, f := range jd.h.Faults {
		faultByID[f.ID] = f
	}
	for _, o := range jd.res.Objectives {
		if d, off := jd.res.disabled(o.ID); off {
			jd.na(o.ID, "disabled by "+d.Policy+": "+d.Reason)
			continue
		}
		def := measureDefs[o.Measure]
		dim := o.Dimension
		if dim == "" {
			dim = def.dim
		}
		agg := o.Aggregate
		if agg == "" {
			agg = "max"
		}
		r := ObjectiveResult{ID: o.ID, Policy: o.Policy, Measure: o.Measure, Dimension: dim, Aggregate: agg, Pass: o.Pass, Warn: o.Warn, Key: o.Key}
		var vs []float64
		for _, m := range jd.j.Measures {
			if m.Name != o.Measure {
				continue
			}
			if o.Where != "" && m.Per == "fault" {
				f := faultByID[m.Subject]
				if f == nil {
					continue
				}
				if ok, _ := jd.ev.truth(o.Where, histWorld{h: jd.h, seq: f.Seq}, map[string]any{"fault": f.view(), "run": jd.runView}); !ok {
					continue
				}
			}
			vs = append(vs, m.Value)
		}
		r.Samples = len(vs)
		if len(vs) == 0 {
			r.Result, r.Detail = "n/a", "no value of "+o.Measure+" in this run"
			jd.j.Objectives = append(jd.j.Objectives, r)
			jd.na(o.ID, r.Detail)
			continue
		}
		v := round3(aggregate(agg, vs))
		r.Value = &v
		if b := jd.opt.Baseline; b != nil {
			for _, bo := range b.Objectives {
				if bo.ID == o.ID && bo.Value != nil {
					bv := *bo.Value
					r.Baseline = &bv
				}
			}
		}
		pass, err := parseCriterion(o.Pass)
		if err != nil {
			r.Result, r.Detail = "n/a", err.Error()
			jd.j.Objectives = append(jd.j.Objectives, r)
			jd.na(o.ID, r.Detail)
			continue
		}
		if pass.relative && r.Baseline == nil {
			r.Result, r.Detail = "n/a", "relative to a baseline, and no baseline was given"
			jd.j.Objectives = append(jd.j.Objectives, r)
			jd.na(o.ID, r.Detail)
			continue
		}
		if jd.h.Run.TimeScale != 0 && jd.h.Run.TimeScale != 1 && strings.HasPrefix(o.Measure, "fault.") && pass.relative {
			r.Result, r.Detail = "n/a", "scaled: timeliness against a baseline is void under timeScale ≠ 1 (D69)"
			jd.j.Objectives = append(jd.j.Objectives, r)
			jd.na(o.ID, r.Detail)
			continue
		}
		switch {
		case pass.holds(v, r.Baseline):
			r.Result = "pass"
		case o.Warn != "":
			if w, err := parseCriterion(o.Warn); err == nil && w.holds(v, r.Baseline) {
				r.Result = "warn"
			} else {
				r.Result = "fail"
			}
		default:
			r.Result = "fail"
		}
		r.Detail = fmt.Sprintf("%s %s = %g (pass %s)", agg, o.Measure, v, o.Pass)
		jd.j.Objectives = append(jd.j.Objectives, r)
		jd.item(dim, "objective "+o.ID, r.Result, r.Detail)
	}
}

// ---------------------------------------------------------------- coverage

func (jd *judge) coverage() {
	h := jd.h
	c := &jd.j.Coverage
	c.Canonicals = map[string]int{}
	for i, p := range jd.res.Playbook {
		hit := false
		for _, e := range jd.entry {
			if e == &jd.res.Playbook[i] {
				hit = true
			}
		}
		id := p.Policy + "/playbook/" + p.On
		c.Playbook = append(c.Playbook, CoverageItem{ID: id, Exercised: hit})
		if !hit {
			c.Uncovered = append(c.Uncovered, id)
		}
	}
	classes := map[string]bool{}
	for _, f := range h.Faults {
		classes[f.Fault] = true
	}
	for k := range classes {
		c.FaultClasses = append(c.FaultClasses, k)
	}
	sort.Strings(c.FaultClasses)
	for _, e := range h.Events {
		if fromStack(e) {
			c.Canonicals[e.Request.Canonical]++
		}
	}
	for _, s := range c.Sometimes {
		if !s.Exercised {
			c.Uncovered = append(c.Uncovered, s.ID)
		}
	}
	if c.Playbook == nil {
		c.Playbook = []CoverageItem{}
	}
	if c.Sometimes == nil {
		c.Sometimes = []CoverageItem{}
	}
	if c.FaultClasses == nil {
		c.FaultClasses = []string{}
	}
}
