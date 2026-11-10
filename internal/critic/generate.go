package critic

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
	"sigs.k8s.io/yaml"
)

// Generated runs: fault sequences drawn from the catalogues, with no assertions. The invariants
// and the playbook are the oracle; a run that violates one is shrunk to a minimal scenario,
// which is written out to keep (17 — Campaigns).

type GenFault struct {
	Select string  `json:"select"`
	Fault  string  `json:"fault"`
	At     float64 `json:"at"`    // seconds after the setup
	Clear  float64 `json:"clear"` // seconds after the setup
}

type GenPlan struct {
	Seed   int64      `json:"seed"`
	Faults []GenFault `json:"faults"`
	Settle float64    `json:"settle"`
}

type GeneratedRun struct {
	Seed          int64      `json:"seed"`
	Plan          GenPlan    `json:"plan"`
	Verdict       string     `json:"verdict"`
	Violations    []string   `json:"violations,omitempty"`
	File          string     `json:"judgement,omitempty"`
	Minimal       string     `json:"minimal,omitempty"`
	MinimalFaults []GenFault `json:"minimalFaults,omitempty"`
	Attempts      int        `json:"shrinkAttempts,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// Plan draws a fault sequence for an estate from a seed.
func Plan(tw *twin.Twin, g *schema.CampaignGenerate, seed int64) (GenPlan, error) {
	rng := rand.New(rand.NewSource(seed))
	sel := g.Select
	if sel == "" {
		sel = "**"
	}
	var ids []string
	for id := range tw.Reg.Faults {
		for _, want := range g.Faults {
			if id == want || strings.HasPrefix(id, strings.TrimSuffix(want, "/*")+"/") {
				ids = append(ids, id)
				break
			}
		}
	}
	sort.Strings(ids)
	type cand struct{ target, fault string }
	var cands []cand
	for _, id := range ids {
		ts, err := tw.Engine.FaultTargets(sel, id)
		if err != nil {
			continue
		}
		for _, t := range ts {
			if strings.Contains(t, ":") {
				continue // port faults are drawn by their device's selector only
			}
			cands = append(cands, cand{t, id})
		}
	}
	if len(cands) == 0 {
		return GenPlan{}, fmt.Errorf("no target in %q admits any of %v", sel, g.Faults)
	}
	n := g.Count
	if n <= 0 {
		n = 3
	}
	within := 60.0
	if d, err := time.ParseDuration(g.Within); err == nil {
		within = d.Seconds()
	}
	settle := 30.0
	if d, err := time.ParseDuration(g.Settle); err == nil {
		settle = d.Seconds()
	}
	maxc := g.MaxConcurrent
	if maxc <= 0 {
		maxc = 2
	}
	p := GenPlan{Seed: seed, Settle: settle}
	used := map[string]bool{}
	for len(p.Faults) < n && len(used) < len(cands) {
		c := cands[rng.Intn(len(cands))]
		if used[c.target+" "+c.fault] {
			continue
		}
		used[c.target+" "+c.fault] = true
		at := float64(int(rng.Float64()*within*10)) / 10
		hold := float64(int((10+rng.Float64()*30)*10)) / 10
		p.Faults = append(p.Faults, GenFault{Select: c.target, Fault: c.fault, At: at, Clear: at + hold})
	}
	sort.Slice(p.Faults, func(i, j int) bool { return p.Faults[i].At < p.Faults[j].At })
	// no more than maxc open at once: push a fault after the earliest clear before it
	for i := range p.Faults {
		for {
			open := 0
			first := 1e18
			for j := 0; j < i; j++ {
				if p.Faults[j].Clear > p.Faults[i].At {
					open++
					if p.Faults[j].Clear < first {
						first = p.Faults[j].Clear
					}
				}
			}
			if open < maxc {
				break
			}
			hold := p.Faults[i].Clear - p.Faults[i].At
			p.Faults[i].At = first + 0.5
			p.Faults[i].Clear = p.Faults[i].At + hold
		}
	}
	return p, nil
}

// Scenario turns a plan into a scenario: the setup, then each fault and clear at its time,
// then a settling wait. It asserts nothing.
func (p GenPlan) Scenario(g *schema.CampaignGenerate, name string) *schema.Scenario {
	type ev struct {
		t     float64
		i     int
		clear bool
	}
	var evs []ev
	end := 0.0
	for i, f := range p.Faults {
		evs = append(evs, ev{f.At, i, false}, ev{f.Clear, i, true})
		if f.Clear > end {
			end = f.Clear
		}
	}
	sort.SliceStable(evs, func(a, b int) bool { return evs[a].t < evs[b].t })
	sc := &schema.Scenario{APIVersion: "dc-twin/v1alpha1", Kind: "Scenario", Metadata: schema.Metadata{Name: name}}
	sc.Spec.Stack = g.Stack
	sc.Spec.Setup = append([]schema.Step{}, g.Setup...)
	sc.Spec.Timeout = time.Duration((end + p.Settle + 600) * float64(time.Second)).Round(time.Second).String()
	now := 0.0
	for _, e := range evs {
		if dt := e.t - now; dt > 0.05 {
			sc.Spec.Steps = append(sc.Spec.Steps, schema.Step{Act: &schema.ActStep{Seconds: round3(dt)}})
			now = e.t
		}
		f := p.Faults[e.i]
		id := fmt.Sprintf("g%d", e.i+1)
		if e.clear {
			sc.Spec.Steps = append(sc.Spec.Steps, schema.Step{Fault: &schema.FaultStep{Clear: id}})
		} else {
			sc.Spec.Steps = append(sc.Spec.Steps, schema.Step{Fault: &schema.FaultStep{ID: id, Select: f.Select, Inject: f.Fault}})
		}
	}
	if p.Settle > 0 {
		sc.Spec.Steps = append(sc.Spec.Steps, schema.Step{Act: &schema.ActStep{Seconds: p.Settle}})
	}
	sc.Spec.Report = "fidelity"
	return sc
}

// targetRules are the rules a generated run's violations name, the errors first.
func targetRules(j *Judgement) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range j.Violations {
		if v.Severity == "error" && !seen[v.Rule] {
			seen[v.Rule] = true
			out = append(out, v.Rule)
		}
	}
	return out
}

func violates(j *Judgement, rule string) bool {
	for _, v := range j.Violations {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

// generatedRun draws one plan, runs it on a fresh cell, and shrinks it when it violates a rule.
func generatedRun(ctx context.Context, c *schema.Campaign, g *schema.CampaignGenerate, estPath string, seed int64, pol *Resolved,
	opt CampaignOptions, logf func(string, ...any)) GeneratedRun {
	gr := GeneratedRun{Seed: seed}
	run := func(p GenPlan, tag string) (*Judgement, error) {
		env, err := upCell(c, estPath, seed, opt.Out, fmt.Sprintf("%s-gen-%d-%s", base(estPath), seed, tag))
		if err != nil {
			return nil, err
		}
		defer env.stop()
		res := pol
		if res == nil {
			if res, err = DefaultPolicy(env.tw); err != nil {
				return nil, err
			}
		}
		if len(p.Faults) == 0 {
			if p, err = Plan(env.tw, g, seed); err != nil {
				return nil, err
			}
			gr.Plan = p
		}
		sc := p.Scenario(g, fmt.Sprintf("generated-%d-%s", seed, tag))
		_, j, err := RunScenario(ctx, env.tw, sc, RunOptions{Policy: res, Subject: c.Spec.Subject, Kind: "generated"})
		return j, err
	}
	j, err := run(GenPlan{}, "full")
	if err != nil {
		gr.Error, gr.Verdict = err.Error(), "not-run"
		logf("generated seed %d: %v", seed, err)
		return gr
	}
	gr.Verdict = j.Verdict
	gr.File = filepath.Join("generated", fmt.Sprintf("seed-%d.json", seed))
	writeJSONFile(filepath.Join(opt.Out, gr.File), j)
	for _, v := range targetRules(j) {
		gr.Violations = append(gr.Violations, v)
	}
	logf("generated seed %d: %d faults → %s %s", seed, len(gr.Plan.Faults), strings.ToUpper(j.Verdict), strings.Join(gr.Violations, " "))
	if len(gr.Violations) == 0 || !g.Shrink {
		return gr
	}
	rule := gr.Violations[0]
	best := gr.Plan
	try := func(p GenPlan, tag string) bool {
		if ctx.Err() != nil {
			return false
		}
		gr.Attempts++
		j, err := run(p, tag)
		ok := err == nil && violates(j, rule)
		logf("  shrink %s: %d fault(s) → %v", tag, len(p.Faults), ok)
		return ok
	}
	// drop faults one at a time while the rule is still violated
	for changed := true; changed && len(best.Faults) > 1; {
		changed = false
		for i := range best.Faults {
			cand := best
			cand.Faults = append(append([]GenFault{}, best.Faults[:i]...), best.Faults[i+1:]...)
			if try(cand, fmt.Sprintf("drop%d", gr.Attempts+1)) {
				best, changed = cand, true
				break
			}
		}
	}
	// shorten: bring the faults forward, and the settle down
	cand := best
	cand.Faults = append([]GenFault{}, best.Faults...)
	shift := cand.Faults[0].At
	for i := range cand.Faults {
		cand.Faults[i].At -= shift
		cand.Faults[i].Clear -= shift
	}
	cand.Settle = best.Settle / 2
	if shift > 0 || cand.Settle < best.Settle {
		if try(cand, fmt.Sprintf("shorten%d", gr.Attempts+1)) {
			best = cand
		}
	}
	gr.MinimalFaults = best.Faults
	sc := best.Scenario(g, fmt.Sprintf("minimal-%s-seed-%d", strings.ReplaceAll(rule, "/", "-"), seed))
	b, _ := yaml.Marshal(sc)
	gr.Minimal = filepath.Join(opt.Out, "generated", fmt.Sprintf("minimal-seed-%d.yaml", seed))
	header := fmt.Sprintf("# Shrunk by the critic from generated seed %d: the smallest sequence found that still violates\n# %s (%d attempts).\n", seed, rule, gr.Attempts)
	_ = os.MkdirAll(filepath.Dir(gr.Minimal), 0o755)
	_ = os.WriteFile(gr.Minimal, append([]byte(header), b...), 0o644)
	return gr
}
