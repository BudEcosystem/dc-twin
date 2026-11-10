package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/critic"
)

// The critic's commands (docs/spec/17-critic.md — Interfaces).

// exitCode ends dctwin with a verdict's code: 0 passed, 1 failed, 2 not-run, 3 impersonated,
// 4 the policy's gate failed.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func verdictCode(js ...*critic.Judgement) exitCode {
	code := exitCode(0)
	for _, j := range js {
		c := map[string]exitCode{"passed": 0, "failed": 1, "not-run": 2, "impersonated": 3}[j.Verdict]
		if c == 0 && j.Gate != nil && !j.Gate.Passed {
			c = 4
		}
		if c == 1 || (code != 1 && c > code) {
			code = c
		}
	}
	return code
}

// outputs writes a judgement's renderings where flags ask; it returns the rest of the args.
func outputs(args []string, js []*critic.Judgement, baseline *critic.Judgement) ([]string, error) {
	write := func(flag string, render func() []byte) error {
		var p string
		p, args = flagVal(args, flag)
		if p == "" {
			return nil
		}
		return os.WriteFile(p, render(), 0o644)
	}
	errs := []error{
		write("--judgement", func() []byte {
			if len(js) == 1 {
				b, _ := json.MarshalIndent(js[0], "", "  ")
				return b
			}
			b, _ := json.MarshalIndent(js, "", "  ")
			return b
		}),
		write("--junit", func() []byte { return critic.JUnit(js...) }),
		write("--markdown", func() []byte {
			var b strings.Builder
			for _, j := range js {
				b.WriteString(j.Markdown(baseline) + "\n")
			}
			return []byte(b.String())
		}),
		write("--html", func() []byte {
			var b strings.Builder
			for _, j := range js {
				b.WriteString(j.HTML())
			}
			return []byte(b.String())
		}),
	}
	for _, e := range errs {
		if e != nil {
			return args, e
		}
	}
	return args, nil
}

func policyRefs(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func criticCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("critic judge|explain|diff|start|stop|status|list|show|rejudge|mcp")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "judge":
		return criticJudge(args)
	case "explain":
		if len(args) < 1 {
			return fmt.Errorf("critic explain <judgement.json> [violation number | rule]")
		}
		j, err := critic.Load(args[0])
		if err != nil {
			return err
		}
		if len(args) == 1 {
			for i := range j.Violations {
				fmt.Println(j.Explain(i))
			}
			if len(j.Violations) == 0 {
				fmt.Println("no violations")
			}
			return nil
		}
		if n, err := strconv.Atoi(args[1]); err == nil {
			fmt.Print(j.Explain(n - 1))
			return nil
		}
		for i, v := range j.Violations {
			if v.Rule == args[1] || strings.HasSuffix(v.Rule, "/"+args[1]) {
				fmt.Println(j.Explain(i))
			}
		}
		return nil
	case "diff":
		if len(args) != 2 {
			return fmt.Errorf("critic diff <a.json> <b.json>")
		}
		if a, b, ok := loadCampaigns(args[0], args[1]); ok {
			fmt.Print(critic.DiffCampaigns(a, b))
			return nil
		}
		a, err := critic.Load(args[0])
		if err != nil {
			return err
		}
		b, err := critic.Load(args[1])
		if err != nil {
			return err
		}
		fmt.Print(critic.Diff(a, b))
		return nil
	case "mcp":
		dir, _ := stateDir(args)
		return critic.ServeMCP(dir, os.Stdin, os.Stdout)
	}
	// the rest talk to a running estate
	dir, args := stateDir(args)
	a, err := connect(dir)
	if err != nil {
		return err
	}
	policyText := func(args []string) (string, []string, error) {
		p, rest := flagVal(args, "--policy")
		if p == "" {
			return "", rest, nil
		}
		b, err := os.ReadFile(p)
		return string(b), rest, err
	}
	show := func(b []byte, args []string) error {
		var j critic.Judgement
		if err := json.Unmarshal(b, &j); err != nil {
			return err
		}
		if _, err := outputs(args, []*critic.Judgement{&j}, nil); err != nil {
			return err
		}
		fmt.Print(j.Text())
		return verdictCode(&j)
	}
	switch sub {
	case "start":
		pol, args, err := policyText(args)
		if err != nil {
			return err
		}
		subject, _ := flagVal(args, "--subject")
		b, err := a.do("POST", "/critic/sessions", map[string]any{"policy": pol, "subject": subject}, "")
		if err != nil {
			return err
		}
		var r map[string]string
		json.Unmarshal(b, &r)
		fmt.Printf("critic session %s started; `dctwin critic stop %s` judges it\n", r["session"], r["session"])
		return nil
	case "stop", "status":
		if len(args) < 1 {
			b, err := a.do("GET", "/critic/sessions", nil, "")
			if err != nil {
				return err
			}
			os.Stdout.Write(b)
			return nil
		}
		method, path := "GET", "/critic/sessions/"+args[0]
		if sub == "stop" {
			method, path = "POST", path+"/stop"
		}
		b, err := a.do(method, path, nil, "")
		if err != nil {
			return err
		}
		return show(b, args[1:])
	case "list":
		b, err := a.do("GET", "/critic/judgements", nil, "")
		if err != nil {
			return err
		}
		var list []map[string]any
		json.Unmarshal(b, &list)
		for _, j := range list {
			fmt.Printf("%-48v %-10v score %-5v violations %-3v %v\n", j["run"], j["verdict"], j["score"], j["violations"], j["subject"])
		}
		return nil
	case "show":
		if len(args) < 1 {
			return fmt.Errorf("critic show <run id> [--format text|markdown|junit|html|json]")
		}
		f, rest := flagVal(args[1:], "--format")
		if f == "" {
			f = "text"
		}
		_ = rest
		b, err := a.do("GET", "/critic/judgements/"+args[0]+"?format="+f, nil, "")
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
		return nil
	case "rejudge":
		pol, args, err := policyText(args)
		if err != nil {
			return err
		}
		run, args := flagVal(args, "--run")
		b, err := a.do("POST", "/critic/judge", map[string]any{"policy": pol, "run": run}, "")
		if err != nil {
			return err
		}
		return show(b, args)
	}
	return fmt.Errorf("unknown critic command %q", sub)
}

// criticJudge judges a recorded log offline: no running twin, no stack (17 — principle 6).
func criticJudge(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("critic judge <state dir | events.jsonl> [--policy p[,p…]] [--run id | --all] [--subject ref] [--baseline j.json] [--judgement f] [--junit f] [--markdown f] [--html f]")
	}
	src, args := args[0], args[1:]
	pol, args := flagVal(args, "--policy")
	runID, args := flagVal(args, "--run")
	all, args := hasFlag(args, "--all")
	subject, args := flagVal(args, "--subject")
	basePath, args := flagVal(args, "--baseline")
	quiet, args := hasFlag(args, "--quiet")
	evs, err := critic.ReadLog(src)
	if err != nil {
		return err
	}
	if len(evs) == 0 {
		return fmt.Errorf("%s holds no events", src)
	}
	var baseline *critic.Judgement
	if basePath != "" {
		if baseline, err = critic.Load(basePath); err != nil {
			return err
		}
	}
	runs := critic.RunsIn(evs)
	var pick []string
	switch {
	case runID != "":
		pick = []string{runID}
	case all:
		pick = runs
	case len(runs) > 0:
		pick = runs[len(runs)-1:]
	}
	reg, _ := compile.Registry()
	var js []*critic.Judgement
	if len(pick) == 0 {
		res, err := resolveFor(pol, nil, reg)
		if err != nil {
			return err
		}
		j, err := critic.Judge(evs, res, critic.Options{Subject: subject, Baseline: baseline})
		if err != nil {
			return err
		}
		js = append(js, j)
	}
	for _, id := range pick {
		part := critic.EventsOfRun(evs, id)
		if part == nil {
			return fmt.Errorf("no run %s in %s (runs: %s)", id, src, strings.Join(runs, ", "))
		}
		h, err := critic.Build(part)
		if err != nil {
			return err
		}
		res, err := resolveFor(pol, h.Run.Stacks, reg)
		if err != nil {
			return err
		}
		j, err := critic.JudgeHistory(h, res, critic.Options{Subject: subject, Baseline: baseline})
		if err != nil {
			return err
		}
		js = append(js, j)
	}
	if _, err := outputs(args, js, baseline); err != nil {
		return err
	}
	if !quiet {
		for _, j := range js {
			fmt.Print(j.Text())
		}
	}
	return verdictCode(js...)
}

func resolveFor(pol string, stacks []string, reg *registryT) (*critic.Resolved, error) {
	if pol != "" {
		return critic.Resolve(policyRefs(pol)...)
	}
	return critic.Resolve(critic.ForStacks(stacks, reg)...)
}

// policyCmd validates a policy against the catalogues and, with --stack, the stack's aliases.
func policyCmd(args []string) error {
	if len(args) < 2 || args[0] != "validate" {
		return fmt.Errorf("policy validate <file | name> [--stack name]")
	}
	stack, rest := flagVal(args[1:], "--stack")
	res, err := critic.Resolve(rest...)
	if err != nil {
		return err
	}
	reg, err := compile.Registry()
	if err != nil {
		return err
	}
	var aliases map[string]string
	if stack != "" {
		st := reg.Stacks[stack]
		if st == nil {
			return fmt.Errorf("no stack %s", stack)
		}
		aliases = map[string]string{}
		if st.Spec.Critic != nil {
			aliases = st.Spec.Critic.Aliases
		}
	}
	probs := critic.Validate(res, reg, aliases)
	var names []string
	for _, p := range res.Policies {
		names = append(names, p.Name+"@"+p.Version)
	}
	fmt.Printf("policy %s: %d rules, %d playbook entries, %d objectives (%s)\n", res.Name, len(res.Rules), len(res.Playbook), len(res.Objectives), strings.Join(names, ", "))
	hard := 0
	for _, p := range probs {
		fmt.Println("  " + p)
		if !strings.Contains(p, "will be n/a") {
			hard++
		}
	}
	if hard > 0 {
		return fmt.Errorf("%d problem(s)", hard)
	}
	fmt.Println("  valid")
	return nil
}

// loadCampaigns reads two campaign results, when both files are campaign results.
func loadCampaigns(a, b string) (*critic.CampaignResult, *critic.CampaignResult, bool) {
	read := func(p string) *critic.CampaignResult {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var r critic.CampaignResult
		if json.Unmarshal(data, &r) != nil || r.Kind != "CampaignResult" {
			return nil
		}
		return &r
	}
	ra, rb := read(a), read(b)
	return ra, rb, ra != nil && rb != nil
}

// campaignCmd runs a campaign in this process: each cell brings its estate (and the campaign's
// stack) up on addresses of its own and down again.
func campaignCmd(args []string) error {
	if len(args) < 2 || args[0] != "run" {
		return fmt.Errorf("campaign run <campaign.yaml> [--out dir] [--junit f] [--markdown f]")
	}
	out, rest := flagVal(args[2:], "--out")
	junit, rest := flagVal(rest, "--junit")
	md, _ := flagVal(rest, "--markdown")
	c, err := critic.LoadCampaign(args[1])
	if err != nil {
		return err
	}
	if out == "" {
		out = "campaign-" + c.Metadata.Name
	}
	res, err := critic.RunCampaign(context.Background(), c, critic.CampaignOptions{Dir: filepath.Dir(args[1]), Out: out, Log: os.Stdout})
	if err != nil {
		return err
	}
	fmt.Print(res.Text())
	fmt.Printf("results in %s\n", out)
	var js []*critic.Judgement
	for _, cell := range res.Cells {
		if cell.File == "" {
			continue
		}
		if j, err := critic.Load(filepath.Join(out, cell.File)); err == nil {
			js = append(js, j)
		}
	}
	if junit != "" {
		if err := os.WriteFile(junit, critic.JUnit(js...), 0o644); err != nil {
			return err
		}
	}
	if md != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "## dc-twin campaign %s\n\n```\n%s```\n\n", c.Metadata.Name, res.Text())
		for _, j := range js {
			b.WriteString(j.Markdown(nil) + "\n")
		}
		if err := os.WriteFile(md, []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	code := exitCode(0)
	for _, cell := range res.Cells {
		if cell.Verdict == "failed" {
			code = 1
		}
	}
	for _, g := range res.Generated {
		if len(g.Violations) > 0 {
			code = 1
		}
	}
	return code
}
