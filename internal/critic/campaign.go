package critic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// A campaign is a matrix of judged runs and their statistics (17 — Campaigns).

// DefaultAdapters are the adapters a campaign brings up when it names none.
var DefaultAdapters = []string{"redfish", "dhcp", "bms", "snmp", "ipmi", "ufm", "nvue"}

type CampaignResult struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Name       string         `json:"name"`
	Subject    string         `json:"subject,omitempty"`
	Started    time.Time      `json:"started"`
	Took       string         `json:"took"`
	Cells      []Cell         `json:"cells"`
	Generated  []GeneratedRun `json:"generated,omitempty"`
	Aggregate  Aggregate      `json:"aggregate"`
	Diff       string         `json:"diff,omitempty"`
}

type Cell struct {
	Estate     string             `json:"estate"`
	Scenario   string             `json:"scenario"`
	Seed       int64              `json:"seed"`
	Run        string             `json:"run"`
	Verdict    string             `json:"verdict"`
	Score      *float64           `json:"score"`
	Dimensions map[string]float64 `json:"dimensions,omitempty"`
	Violations []string           `json:"violations,omitempty"`
	Objectives map[string]float64 `json:"objectives,omitempty"`
	File       string             `json:"judgement,omitempty"`
	Holdout    bool               `json:"holdout,omitempty"`
	Error      string             `json:"error,omitempty"`
}

type Stats struct {
	N   int     `json:"n"`
	Min float64 `json:"min"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

type Aggregate struct {
	Verdicts   map[string]int   `json:"verdicts"`
	Score      Stats            `json:"score"`
	Dimensions map[string]Stats `json:"dimensions"`
	Objectives map[string]Stats `json:"objectives"`
	Rules      map[string]int   `json:"rules"`
	Flaky      []string         `json:"flaky,omitempty"`
	Uncovered  []string         `json:"uncovered,omitempty"`
}

func stats(vs []float64) Stats {
	if len(vs) == 0 {
		return Stats{}
	}
	return Stats{N: len(vs), Min: round3(aggregate("min", append([]float64{}, vs...))), P50: round3(aggregate("p50", append([]float64{}, vs...))),
		P95: round3(aggregate("p95", append([]float64{}, vs...))), Max: round3(aggregate("max", append([]float64{}, vs...)))}
}

// CampaignOptions tune a campaign run.
type CampaignOptions struct {
	Dir string // the campaign file's directory, for relative paths
	Out string // where judgements and the result go
	Log io.Writer
}

// LoadCampaign reads a campaign file.
func LoadCampaign(p string) (*schema.Campaign, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c schema.Campaign
	if err := schema.Decode(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if err := schema.CheckHeader(schema.Header{APIVersion: c.APIVersion, Kind: c.Kind, Metadata: c.Metadata}, "Campaign"); err != nil {
		return nil, err
	}
	return &c, nil
}

// cellEnv is one estate with its stack, up for one cell.
type cellEnv struct {
	tw      *twin.Twin
	stop    func()
	state   string
	stackLg string
}

// RunCampaign runs every cell and generated run of a campaign and aggregates them.
func RunCampaign(ctx context.Context, c *schema.Campaign, opt CampaignOptions) (*CampaignResult, error) {
	logf := func(f string, a ...any) {
		if opt.Log != nil {
			fmt.Fprintf(opt.Log, f+"\n", a...)
		}
	}
	if b := c.Spec.Budget; b != nil && b.Wall != "" {
		if d, err := time.ParseDuration(b.Wall); err == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}
	rel := func(p string) string {
		if filepath.IsAbs(p) || opt.Dir == "" {
			return p
		}
		return filepath.Join(opt.Dir, p)
	}
	if err := os.MkdirAll(filepath.Join(opt.Out, "cells"), 0o755); err != nil {
		return nil, err
	}
	res := &CampaignResult{APIVersion: "dc-twin/v1alpha1", Kind: "CampaignResult", Name: c.Metadata.Name, Subject: c.Spec.Subject, Started: time.Now().UTC()}
	var pol *Resolved
	if c.Spec.Policy != "" {
		var err error
		if pol, err = Resolve(policyPath(c.Spec.Policy, rel)); err != nil {
			return nil, err
		}
	}
	var baseline *CampaignResult
	if b := c.Spec.Baseline; b != nil && b.File != "" {
		if data, err := os.ReadFile(rel(b.File)); err == nil {
			baseline = &CampaignResult{}
			if json.Unmarshal(data, baseline) != nil {
				baseline = nil
			}
		}
	}
	seeds := []int64{0}
	if s := c.Spec.Seeds; s != nil && s.Count > 0 {
		seeds = nil
		for i := 0; i < s.Count; i++ {
			seeds = append(seeds, s.From+int64(i))
		}
	}
	type scen struct {
		path    string
		holdout bool
	}
	var scens []scen
	for _, f := range c.Spec.Scenarios.Files {
		matches, _ := filepath.Glob(rel(f))
		for _, m := range matches {
			scens = append(scens, scen{m, false})
		}
	}
	if h := c.Spec.Holdout; h != nil {
		for _, f := range h.Files {
			matches, _ := filepath.Glob(rel(f))
			for _, m := range matches {
				scens = append(scens, scen{m, true})
			}
		}
	}
	for _, estPath := range c.Spec.Estates {
		estPath = rel(estPath)
		for _, seed := range seeds {
			if len(scens) == 0 {
				break
			}
			if ctx.Err() != nil {
				break
			}
			env, err := upCell(c, estPath, seed, opt.Out, fmt.Sprintf("%s-%d", base(estPath), seed))
			if err != nil {
				res.Cells = append(res.Cells, Cell{Estate: base(estPath), Seed: seed, Error: err.Error(), Verdict: "not-run"})
				logf("cell %s seed %d: %v", base(estPath), seed, err)
				continue
			}
			p := pol
			if p == nil {
				if p, err = DefaultPolicy(env.tw); err != nil {
					env.stop()
					return nil, err
				}
			}
			for _, sc := range scens {
				if ctx.Err() != nil {
					break
				}
				s, err := loadScenario(sc.path)
				if err != nil {
					return nil, err
				}
				cell := Cell{Estate: base(estPath), Scenario: s.Metadata.Name, Seed: seed, Holdout: sc.holdout}
				result, j, err := RunScenario(ctx, env.tw, s, RunOptions{Policy: p, Subject: c.Spec.Subject, Kind: "campaign-cell", Holdout: sc.holdout})
				if err != nil {
					cell.Error, cell.Verdict = err.Error(), "not-run"
					if result != nil {
						cell.Verdict = result.Verdict
					}
				} else {
					fillCell(&cell, j)
					cell.File = filepath.Join("cells", safeName(fmt.Sprintf("%s-%d-%s", base(estPath), seed, s.Metadata.Name))+".json")
					writeJSONFile(filepath.Join(opt.Out, cell.File), j)
				}
				logf("cell %-24s seed %-3d %-28s %-10s score %s %s", cell.Estate, seed, cell.Scenario, strings.ToUpper(cell.Verdict), fmtPtr(cell.Score), strings.Join(cell.Violations, " "))
				res.Cells = append(res.Cells, cell)
			}
			env.stop()
		}
		if g := c.Spec.Generate; g != nil && g.Runs > 0 {
			for i := 0; i < g.Runs && ctx.Err() == nil; i++ {
				seed := int64(i)
				if s := c.Spec.Seeds; s != nil {
					seed += s.From
				}
				gr := generatedRun(ctx, c, g, estPath, seed, pol, opt, logf)
				res.Generated = append(res.Generated, gr)
			}
		}
	}
	res.Aggregate = aggregateCells(res.Cells, res.Generated)
	res.Took = time.Since(res.Started).Round(time.Second).String()
	if baseline != nil {
		res.Diff = DiffCampaigns(baseline, res)
	}
	writeJSONFile(filepath.Join(opt.Out, "campaign.json"), res)
	return res, nil
}

func policyPath(p string, rel func(string) string) string {
	if _, err := os.Stat(rel(p)); err == nil {
		return rel(p)
	}
	return p
}

func base(p string) string { return strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)) }

func loadScenario(p string) (*schema.Scenario, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var s schema.Scenario
	if err := schema.Decode(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &s, nil
}

func fillCell(cell *Cell, j *Judgement) {
	cell.Run, cell.Verdict, cell.Score = j.Run.ID, j.Verdict, j.Score
	cell.Dimensions = map[string]float64{}
	for d, ds := range j.Dimensions {
		if ds.Score != nil {
			cell.Dimensions[d] = *ds.Score
		}
	}
	cell.Objectives = map[string]float64{}
	for _, o := range j.Objectives {
		if o.Value != nil {
			cell.Objectives[o.ID] = *o.Value
		}
	}
	seen := map[string]bool{}
	for _, v := range j.Violations {
		if !seen[v.Rule] {
			seen[v.Rule] = true
			cell.Violations = append(cell.Violations, v.Rule)
		}
	}
	sort.Strings(cell.Violations)
}

func writeJSONFile(p string, v any) {
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.MarshalIndent(v, "", "  ")
	_ = os.WriteFile(p, b, 0o644)
}

func aggregateCells(cells []Cell, gen []GeneratedRun) Aggregate {
	a := Aggregate{Verdicts: map[string]int{}, Dimensions: map[string]Stats{}, Objectives: map[string]Stats{}, Rules: map[string]int{}}
	var scores []float64
	dims := map[string][]float64{}
	objs := map[string][]float64{}
	verdicts := map[string]map[string]bool{}
	for _, c := range cells {
		a.Verdicts[c.Verdict]++
		if c.Score != nil {
			scores = append(scores, *c.Score)
		}
		for d, v := range c.Dimensions {
			dims[d] = append(dims[d], v)
		}
		for o, v := range c.Objectives {
			objs[o] = append(objs[o], v)
		}
		for _, r := range c.Violations {
			a.Rules[r]++
		}
		k := c.Estate + " " + c.Scenario
		if verdicts[k] == nil {
			verdicts[k] = map[string]bool{}
		}
		verdicts[k][c.Verdict] = true
	}
	for _, g := range gen {
		a.Verdicts["generated-"+g.Verdict]++
		for _, r := range g.Violations {
			a.Rules[r]++
		}
	}
	a.Score = stats(scores)
	for d, vs := range dims {
		a.Dimensions[d] = stats(vs)
	}
	for o, vs := range objs {
		a.Objectives[o] = stats(vs)
	}
	for k, vs := range verdicts {
		if vs["passed"] && vs["failed"] {
			a.Flaky = append(a.Flaky, k)
		}
	}
	sort.Strings(a.Flaky)
	return a
}

// DiffCampaigns lists what changed between two campaigns: cells whose verdict flipped, rules
// that started or stopped being violated, and objectives that moved beyond the spread seen
// across seeds.
func DiffCampaigns(a, b *CampaignResult) string {
	var out []string
	key := func(c Cell) string { return fmt.Sprintf("%s %s seed %d", c.Estate, c.Scenario, c.Seed) }
	av := map[string]Cell{}
	for _, c := range a.Cells {
		av[key(c)] = c
	}
	for _, c := range b.Cells {
		if p, ok := av[key(c)]; ok && p.Verdict != c.Verdict {
			out = append(out, fmt.Sprintf("verdict %s: %s → %s", key(c), p.Verdict, c.Verdict))
		}
	}
	for r, n := range b.Aggregate.Rules {
		if a.Aggregate.Rules[r] == 0 {
			out = append(out, fmt.Sprintf("new violation: %s (%d cells)", r, n))
		}
	}
	for r, n := range a.Aggregate.Rules {
		if b.Aggregate.Rules[r] == 0 {
			out = append(out, fmt.Sprintf("fixed: %s (was %d cells)", r, n))
		}
	}
	for o, s := range b.Aggregate.Objectives {
		p, ok := a.Aggregate.Objectives[o]
		if !ok || p.N == 0 {
			continue
		}
		noise := (p.Max - p.Min) + (s.Max - s.Min)
		if d := s.P50 - p.P50; d > noise && d != 0 || -d > noise && d != 0 {
			out = append(out, fmt.Sprintf("objective %s moved: p50 %g → %g (spread %g)", o, p.P50, s.P50, noise))
		}
	}
	if len(out) == 0 {
		return "no change\n"
	}
	sort.Strings(out)
	return strings.Join(out, "\n") + "\n"
}

// Text summarises a campaign.
func (r *CampaignResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "campaign %s: %d cells, %d generated runs, took %s\n", r.Name, len(r.Cells), len(r.Generated), r.Took)
	var vs []string
	for v, n := range r.Aggregate.Verdicts {
		vs = append(vs, fmt.Sprintf("%s %d", v, n))
	}
	sort.Strings(vs)
	fmt.Fprintf(&b, "  verdicts: %s\n", strings.Join(vs, ", "))
	if r.Aggregate.Score.N > 0 {
		s := r.Aggregate.Score
		fmt.Fprintf(&b, "  score: min %.2f p50 %.2f max %.2f\n", s.Min, s.P50, s.Max)
	}
	var rules []string
	for k, n := range r.Aggregate.Rules {
		rules = append(rules, fmt.Sprintf("%s ×%d", k, n))
	}
	sort.Strings(rules)
	if len(rules) > 0 {
		fmt.Fprintf(&b, "  violated rules: %s\n", strings.Join(rules, ", "))
	}
	if len(r.Aggregate.Flaky) > 0 {
		fmt.Fprintf(&b, "  flaky: %s\n", strings.Join(r.Aggregate.Flaky, "; "))
	}
	for _, g := range r.Generated {
		fmt.Fprintf(&b, "  generated seed %d: %s %s", g.Seed, g.Verdict, strings.Join(g.Violations, " "))
		if g.Minimal != "" {
			fmt.Fprintf(&b, " → shrunk to %d fault(s): %s", len(g.MinimalFaults), g.Minimal)
		}
		b.WriteString("\n")
	}
	if r.Diff != "" {
		b.WriteString("  against the baseline:\n")
		for _, l := range strings.Split(strings.TrimRight(r.Diff, "\n"), "\n") {
			b.WriteString("    " + l + "\n")
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- cells

var placeholderRE = regexp.MustCompile(`\{(tcp|udp):([a-zA-Z0-9_-]+)\}`)

// upCell brings an estate up for one cell, with its seed and its stack, on addresses of its own.
func upCell(c *schema.Campaign, estPath string, seed int64, out, name string) (*cellEnv, error) {
	reg, err := compile.Registry()
	if err != nil {
		return nil, err
	}
	est, _, err := compile.LoadEstate(estPath)
	if err != nil {
		return nil, err
	}
	if seed != 0 {
		est.Spec.Seed = seed
	}
	state, err := os.MkdirTemp("", "dctwin-cell-")
	if err != nil {
		return nil, err
	}
	adapters := c.Spec.Adapters
	if len(adapters) == 0 {
		adapters = DefaultAdapters
	}
	env := &cellEnv{state: state}
	gateway := ""
	var cmd *exec.Cmd
	if st := c.Spec.Stack; st != nil && st.Command != "" {
		addrs := map[string]string{}
		fill := func(s string) string {
			s = placeholderRE.ReplaceAllStringFunc(s, func(m string) string {
				if a, ok := addrs[m]; ok {
					return a
				}
				p := placeholderRE.FindStringSubmatch(m)
				a := freeAddr(p[1])
				addrs[m] = a
				return a
			})
			return strings.ReplaceAll(s, "{state}", state)
		}
		command := fill(st.Command)
		gateway = addrs["{tcp:gateway}"]
		if st.Server != "" {
			srv := fill(st.Server)
			for k, n := range est.Spec.Networks {
				if n.Server != "" {
					n.Server = srv
					est.Spec.Networks[k] = n
				}
			}
		}
		if st.Broker != "" && est.Spec.Facility.BMS != nil {
			est.Spec.Facility.BMS.Broker = fill(st.Broker)
		}
		for i := range est.Spec.Stacks {
			if est.Spec.Stacks[i].Outputs == nil {
				est.Spec.Stacks[i].Outputs = map[string]string{}
			}
			for k, v := range st.Outputs {
				est.Spec.Stacks[i].Outputs[k] = fill(v)
			}
		}
		env.stackLg = filepath.Join(out, "cells", name+".stack.log")
		lf, _ := os.Create(env.stackLg)
		// exec: the shell becomes the stack, so stopping the cell stops the stack and leaves no orphan
		cmd = exec.Command("sh", "-c", "exec "+command)
		cmd.Stdout, cmd.Stderr = lf, lf
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("stack command: %w", err)
		}
		if st.Ready != "" {
			waitReady(fill(st.Ready), 30*time.Second)
		} else {
			time.Sleep(time.Second)
		}
	}
	tw, err := twin.UpEstate(est, estPath, reg, twin.Options{StateDir: state, Adapters: adapters, GatewayAddr: gateway})
	if err != nil {
		if cmd != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		return nil, err
	}
	env.tw = tw
	env.stop = func() {
		tw.Down()
		if cmd != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
		os.RemoveAll(state)
	}
	return env, nil
}

func freeAddr(network string) string {
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			return "127.0.0.1:0"
		}
		defer c.Close()
		return c.LocalAddr().String()
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "127.0.0.1:0"
	}
	defer l.Close()
	return l.Addr().String()
}

func waitReady(url string, within time.Duration) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}
