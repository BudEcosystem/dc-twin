package critic

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Judgement is the critic's output for one run (17 — The judgement).
type Judgement struct {
	APIVersion string                     `json:"apiVersion"`
	Kind       string                     `json:"kind"`
	Run        RunInfo                    `json:"run"`
	Subject    Subject                    `json:"subject"`
	Policies   []PolicyRef                `json:"policies"`
	Disabled   []Disabled                 `json:"disabled,omitempty"`
	Verdict    string                     `json:"verdict"`
	Reason     string                     `json:"reason,omitempty"`
	Score      *float64                   `json:"score"`
	Dimensions map[string]*DimensionScore `json:"dimensions"`
	Assertions []Assertion                `json:"assertions,omitempty"`
	Violations []Violation                `json:"violations"`
	Faults     []FaultJudgement           `json:"faults"`
	Measures   []Measure                  `json:"measures"`
	Objectives []ObjectiveResult          `json:"objectives"`
	Coverage   Coverage                   `json:"coverage"`
	NA         []NA                       `json:"na,omitempty"`
	Incidents  []string                   `json:"incidents,omitempty"`
	Gate       *GateResult                `json:"gate,omitempty"`
	Fidelity   any                        `json:"fidelity,omitempty"`
	IsNot      []string                   `json:"isNot,omitempty"`
	Holdout    bool                       `json:"holdout,omitempty"`
}

type Subject struct {
	Stacks []string `json:"stacks,omitempty"`
	Ref    string   `json:"ref,omitempty"`
}

type DimensionScore struct {
	Score *float64 `json:"score"`
	Pass  int      `json:"pass"`
	Warn  int      `json:"warn"`
	Fail  int      `json:"fail"`
	Items []Item   `json:"items"`
}

// Item is one scored thing: an assertion, a rule, a playbook promise or an objective.
type Item struct {
	ID     string `json:"id"`
	Result string `json:"result"` // pass, warn, fail
	Detail string `json:"detail,omitempty"`
}

type Assertion struct {
	Step   int      `json:"step"`
	Kind   string   `json:"kind"`
	Name   string   `json:"name,omitempty"`
	Status string   `json:"status"`
	Detail []string `json:"detail,omitempty"`
}

type Violation struct {
	Rule      string  `json:"rule"`
	Policy    string  `json:"policy"`
	Dimension string  `json:"dimension"`
	Severity  string  `json:"severity"`
	Key       bool    `json:"key"`
	Message   string  `json:"message"`
	Fault     string  `json:"fault,omitempty"`
	Expected  string  `json:"expected,omitempty"`
	Observed  string  `json:"observed,omitempty"`
	Witness   Witness `json:"witness"`
}

type Witness struct {
	Events     []uint64        `json:"events"`
	At         string          `json:"at,omitempty"`
	Offset     float64         `json:"offset"` // seconds since the run started
	Request    *WitnessRequest `json:"request,omitempty"`
	Principal  string          `json:"principal,omitempty"`
	CauseChain []uint64        `json:"causeChain,omitempty"`
	Before     map[string]any  `json:"before,omitempty"`
	Reads      []string        `json:"reads,omitempty"`
	OpenFaults []string        `json:"openFaults,omitempty"`
}

type WitnessRequest struct {
	Path      string `json:"path"`
	Canonical string `json:"canonical"`
	Class     string `json:"class"`
	Protocol  string `json:"protocol,omitempty"`
	Method    string `json:"method,omitempty"`
	Target    string `json:"target,omitempty"`
	Status    int    `json:"status,omitempty"`
}

type Moment struct {
	Seq   uint64  `json:"seq"`
	After float64 `json:"after"` // seconds since the fault was injected
}

type FaultJudgement struct {
	ID       string             `json:"id"`
	Fault    string             `json:"fault"`
	Target   string             `json:"target"`
	Device   string             `json:"device"`
	Seq      uint64             `json:"seq"`
	Injected float64            `json:"injected"` // seconds since the run started
	Cleared  *float64           `json:"cleared,omitempty"`
	Affected []string           `json:"affected"`
	Playbook string             `json:"playbook,omitempty"`
	Moments  map[string]*Moment `json:"moments"`
	Response Response           `json:"response"`
}

type Response struct {
	Requests               int            `json:"requests"`
	Writes                 int            `json:"writes"`
	Destructive            int            `json:"destructive"`
	ByCanonical            map[string]int `json:"byCanonical,omitempty"`
	Principals             []string       `json:"principals,omitempty"`
	Collateral             []string       `json:"collateral,omitempty"`
	CollateralAccelerators int            `json:"collateralAccelerators"`
}

type Measure struct {
	Name    string   `json:"name"`
	Per     string   `json:"per"`
	Subject string   `json:"subject,omitempty"`
	Value   float64  `json:"value"`
	Unit    string   `json:"unit"`
	Witness []uint64 `json:"witness,omitempty"`
}

type ObjectiveResult struct {
	ID        string   `json:"id"`
	Policy    string   `json:"policy"`
	Measure   string   `json:"measure"`
	Dimension string   `json:"dimension"`
	Aggregate string   `json:"aggregate"`
	Pass      string   `json:"pass"`
	Warn      string   `json:"warn,omitempty"`
	Key       bool     `json:"key"`
	Value     *float64 `json:"value"`
	Baseline  *float64 `json:"baseline,omitempty"`
	Samples   int      `json:"samples"`
	Result    string   `json:"result"` // pass, warn, fail, n/a
	Detail    string   `json:"detail,omitempty"`
}

type Coverage struct {
	Playbook     []CoverageItem `json:"playbook"`
	Sometimes    []CoverageItem `json:"sometimes"`
	FaultClasses []string       `json:"faultClasses"`
	Canonicals   map[string]int `json:"canonicals"`
	Uncovered    []string       `json:"uncovered,omitempty"`
}

type CoverageItem struct {
	ID        string `json:"id"`
	Exercised bool   `json:"exercised"`
}

type NA struct {
	Rule string `json:"rule"`
	Why  string `json:"why"`
}

type GateResult struct {
	Criterion string `json:"criterion"`
	Passed    bool   `json:"passed"`
}

// Options tune a judgement.
type Options struct {
	Baseline *Judgement
	Fidelity any
	Subject  string
	Holdout  bool
}

// judge carries one judgement in progress.
type judge struct {
	h       *History
	res     *Resolved
	ev      *evaluator
	j       *Judgement
	opt     Options
	entry   map[uint64]*PlaybookRule // fault seq -> its playbook entry
	moments map[uint64]map[string]*Moment
	items   map[string][]Item
	runView map[string]any
	hasStk  bool
}

// Judge judges a run from its events under a resolved policy.
func Judge(events []schema.Event, res *Resolved, opt Options) (*Judgement, error) {
	h, err := Build(events)
	if err != nil {
		return nil, err
	}
	return JudgeHistory(h, res, opt)
}

// JudgeHistory judges a rebuilt run.
func JudgeHistory(h *History, res *Resolved, opt Options) (*Judgement, error) {
	extra := make([]string, 0, len(h.Run.Aliases))
	for a := range h.Run.Aliases {
		extra = append(extra, a)
	}
	ev, err := newEvaluator(extra)
	if err != nil {
		return nil, err
	}
	j := &Judgement{APIVersion: "dc-twin/v1alpha1", Kind: "Judgement", Run: h.Run, Policies: res.Policies, Disabled: res.Disabled,
		Dimensions: map[string]*DimensionScore{}, Violations: []Violation{}, Faults: []FaultJudgement{}, Measures: []Measure{},
		Objectives: []ObjectiveResult{}, Fidelity: opt.Fidelity, IsNot: res.IsNot, Holdout: opt.Holdout}
	j.Subject = Subject{Stacks: h.Run.Stacks, Ref: h.Run.Subject}
	if opt.Subject != "" {
		j.Subject.Ref = opt.Subject
	}
	jd := &judge{h: h, res: res, ev: ev, j: j, opt: opt, entry: map[uint64]*PlaybookRule{}, moments: map[uint64]map[string]*Moment{},
		items: map[string][]Item{}}
	jd.runView = map[string]any{"id": h.Run.ID, "scenario": h.Run.Scenario, "stacks": list(h.Run.Stacks), "started": h.Run.Started.Format(time.RFC3339Nano)}
	jd.hasStk = len(h.Run.Stacks) > 0
	if !jd.hasStk {
		for _, e := range h.Events {
			if e.Source.Class == "stack" && e.Kind == "request" {
				jd.hasStk = true
				break
			}
		}
	}
	jd.matchPlaybook()
	jd.computeMoments()
	jd.playbookRules()
	jd.invariants()
	jd.measures()
	jd.objectives()
	jd.coverage()
	jd.assertions()
	jd.incidents()
	jd.faultJudgements()
	jd.score()
	jd.verdict()
	sort.SliceStable(j.Violations, func(a, b int) bool {
		va, vb := j.Violations[a], j.Violations[b]
		if va.Key != vb.Key {
			return va.Key
		}
		if va.Severity != vb.Severity {
			return va.Severity == "error"
		}
		return firstSeq(va) < firstSeq(vb)
	})
	return j, nil
}

func firstSeq(v Violation) uint64 {
	if len(v.Witness.Events) > 0 {
		return v.Witness.Events[0]
	}
	return 0
}

// offset is seconds since the run started.
func (jd *judge) offset(t time.Time) float64 {
	return round3(t.Sub(jd.h.Run.Started).Seconds())
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func (jd *judge) item(dim, id, result, detail string) {
	if dim == "" {
		dim = "correctness"
	}
	jd.items[dim] = append(jd.items[dim], Item{ID: id, Result: result, Detail: detail})
}

func (jd *judge) na(rule, why string) {
	jd.j.NA = append(jd.j.NA, NA{Rule: rule, Why: why})
}

// requires reports why a rule cannot be judged on this run ("" when it can).
func (jd *judge) requires(req []string) string {
	for _, r := range req {
		if r == "stack" {
			if !jd.hasStk {
				return "no stack is installed and no stack request was made"
			}
			continue
		}
		if jd.h.Run.Aliases[r] == "" {
			return "the stack does not map the alias " + r
		}
	}
	return ""
}

// violation records a violation with its witness built from an event.
func (jd *judge) violation(v Violation, seqs ...uint64) {
	if v.Severity == "" {
		v.Severity = "error"
	}
	if v.Dimension == "" {
		v.Dimension = "correctness"
	}
	if len(seqs) > 0 {
		v.Witness = jd.witness(seqs[0], v.Witness)
		v.Witness.Events = append([]uint64{}, seqs...)
	}
	jd.j.Violations = append(jd.j.Violations, v)
}

// witness gathers what proves a violation at an event: the request, who made it, its cause
// chain, the device as it was before, the faults open, and what the same principal read in the
// minute before (17 — Explanations).
func (jd *judge) witness(seq uint64, w Witness) Witness {
	h := jd.h
	e, ok := h.Event(seq)
	if !ok {
		return w
	}
	t := h.time(e)
	w.At, w.Offset = t.UTC().Format(time.RFC3339Nano), jd.offset(t)
	if r := e.Request; r != nil {
		w.Request = &WitnessRequest{Path: e.Path, Canonical: r.Canonical, Class: class(e), Protocol: r.Protocol, Method: r.Method, Target: r.Target, Status: r.Status}
		w.Principal = h.Principal(e)
		w.Before = h.View(e.Path, seq-1)
		delete(w.Before, "faults")
		seen := map[string]bool{}
		for i := h.idx[seq] - 1; i >= 0 && len(w.Reads) < 8; i-- {
			p := h.Events[i]
			if t.Sub(h.time(p)) > time.Minute {
				break
			}
			if p.Kind != "request" || p.Request == nil || class(p) != "read" || h.Principal(p) != w.Principal {
				continue
			}
			k := shortPath(h.DevicePath(p.Path)) + " " + p.Request.Target
			if !seen[k] {
				seen[k] = true
				w.Reads = append(w.Reads, k)
			}
		}
	}
	for c := e.Cause; c != 0 && len(w.CauseChain) < 12; {
		w.CauseChain = append(w.CauseChain, c)
		ce, ok := h.Event(c)
		if !ok {
			break
		}
		c = ce.Cause
	}
	for _, f := range h.Faults {
		if f.openAt(seq) {
			w.OpenFaults = append(w.OpenFaults, fmt.Sprintf("%s on %s (#%d)", f.Fault, shortPath(f.Target), f.Seq))
		}
	}
	return w
}

// shortPath drops the site and hall from a path for reading.
func shortPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) > 3 {
		return strings.Join(parts[2:], "/")
	}
	return p
}

// ---------------------------------------------------------------- assertions, incidents

func (jd *judge) assertions() {
	end := jd.h.Run.End
	if end == nil {
		return
	}
	for _, s := range list(end["steps"]) {
		m := mapOf(s)
		a := Assertion{Step: int(num(m["index"])), Kind: str(m["kind"]), Name: str(m["name"]), Status: str(m["status"])}
		for _, d := range list(m["detail"]) {
			a.Detail = append(a.Detail, str(d))
		}
		jd.j.Assertions = append(jd.j.Assertions, a)
		if a.Kind == "expect" || a.Kind == "wait" {
			res := "pass"
			if a.Status != "passed" {
				res = "fail"
			}
			jd.item("correctness", fmt.Sprintf("scenario step %d (%s)", a.Step, a.Kind), res, strings.Join(a.Detail, "; "))
		}
	}
}

func (jd *judge) incidents() {
	for _, e := range jd.h.Events {
		if e.Kind == "twin" && e.Severity == "incident" {
			jd.j.Incidents = append(jd.j.Incidents, fmt.Sprintf("#%d %s", e.Seq, e.Message))
		}
	}
}

// ---------------------------------------------------------------- score and verdict

func (jd *judge) score() {
	j := jd.j
	sum, wsum := 0.0, 0.0
	for _, d := range Dimensions {
		items := jd.items[d]
		ds := &DimensionScore{Items: items}
		if ds.Items == nil {
			ds.Items = []Item{}
		}
		total := 0.0
		for _, it := range items {
			switch it.Result {
			case "pass":
				ds.Pass++
				total += 1
			case "warn":
				ds.Warn++
				total += 0.5
			case "fail":
				ds.Fail++
			}
		}
		if n := ds.Pass + ds.Warn + ds.Fail; n > 0 {
			s := round3(total / float64(n))
			ds.Score = &s
			if d != "coverage" {
				w := jd.res.Weights[d]
				sum += w * s
				wsum += w
			}
		}
		j.Dimensions[d] = ds
	}
	if wsum > 0 {
		s := round3(sum / wsum)
		j.Score = &s
	}
	if g := jd.res.Gate; g != nil && g.Score != "" && j.Score != nil {
		c, err := parseCriterion(g.Score)
		j.Gate = &GateResult{Criterion: g.Score, Passed: err == nil && c.holds(*j.Score, nil)}
	}
}

// verdict follows the four results of 08 (D65, D66): the score never decides it.
func (jd *judge) verdict() {
	j := jd.j
	scen := ""
	reason := ""
	if jd.h.Run.End != nil {
		scen, reason = str(jd.h.Run.End["verdict"]), str(jd.h.Run.End["reason"])
	}
	var why []string
	for _, v := range j.Violations {
		if v.Key {
			why = append(why, "key rule "+v.Rule+" violated")
		}
	}
	for _, o := range j.Objectives {
		if o.Key && o.Result == "fail" {
			why = append(why, "key objective "+o.ID+" failed")
		}
	}
	why = uniq(why)
	switch {
	case len(j.Incidents) > 0:
		j.Verdict, j.Reason = "not-run", "twin incident during the run: "+j.Incidents[0]
	case scen == "not-run":
		j.Verdict, j.Reason = "not-run", reason
	case scen == "failed" || len(why) > 0:
		j.Verdict = "failed"
		if scen == "failed" {
			why = append([]string{"a scenario assertion failed"}, why...)
		}
		j.Reason = strings.Join(why, "; ")
	case scen == "impersonated":
		j.Verdict, j.Reason = "impersonated", reason
	default:
		j.Verdict = "passed"
		j.Reason = "no key rule violated; conditional on the fidelity report"
		if !jd.h.Run.Finished {
			j.Reason = "no key rule violated in the events judged (the run has not ended)"
		}
	}
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
