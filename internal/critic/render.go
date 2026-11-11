package critic

import (
	"encoding/xml"
	"fmt"
	"html"
	"sort"
	"strings"
)

// Text is the judgement as dctwin prints it after the fidelity report.
func (j *Judgement) Text() string {
	var b strings.Builder
	score := "n/a"
	if j.Score != nil {
		score = fmt.Sprintf("%.2f", *j.Score)
	}
	fmt.Fprintf(&b, "judgement — run %s", j.Run.ID)
	if j.Run.Scenario != "" {
		fmt.Fprintf(&b, " (scenario %s)", j.Run.Scenario)
	}
	if j.Subject.Ref != "" {
		fmt.Fprintf(&b, " subject %s", j.Subject.Ref)
	}
	b.WriteString("\n")
	var pols []string
	for _, p := range j.Policies {
		pols = append(pols, p.Name)
	}
	fmt.Fprintf(&b, "  policies: %s\n", strings.Join(pols, ", "))
	fmt.Fprintf(&b, "  verdict: %s   score %s\n", strings.ToUpper(j.Verdict), score)
	if j.Reason != "" {
		fmt.Fprintf(&b, "  reason: %s\n", j.Reason)
	}
	b.WriteString("  scorecard:")
	for _, d := range Dimensions {
		ds := j.Dimensions[d]
		if ds == nil || ds.Score == nil {
			fmt.Fprintf(&b, "  %s n/a", d)
			continue
		}
		fmt.Fprintf(&b, "  %s %.2f (%d/%d/%d)", d, *ds.Score, ds.Pass, ds.Warn, ds.Fail)
	}
	b.WriteString("   (pass/warn/fail)\n")
	if j.Gate != nil {
		fmt.Fprintf(&b, "  gate %s: %v\n", j.Gate.Criterion, j.Gate.Passed)
	}
	if len(j.Faults) > 0 {
		b.WriteString("  faults:\n")
		for _, f := range j.Faults {
			fmt.Fprintf(&b, "    %-4s %-32s %-30s %s\n", f.ID, f.Fault, shortPath(f.Target), momentsLine(f.Moments))
			if len(f.Response.Collateral) > 0 {
				fmt.Fprintf(&b, "         collateral: %s\n", strings.Join(shortAll(f.Response.Collateral), ", "))
			}
		}
	}
	if len(j.Violations) == 0 {
		b.WriteString("  violations: none\n")
	} else {
		fmt.Fprintf(&b, "  violations: %d\n", len(j.Violations))
		for i, v := range j.Violations {
			if j.Holdout {
				fmt.Fprintf(&b, "    %d. [%s] (holdout: details withheld)\n", i+1, v.Dimension)
				continue
			}
			key := ""
			if v.Key {
				key = ", key"
			}
			fmt.Fprintf(&b, "    %d. %s (%s, %s%s) at +%.1fs\n       %s\n", i+1, v.Rule, v.Dimension, v.Severity, key, v.Witness.Offset, v.Message)
			if v.Observed != "" {
				fmt.Fprintf(&b, "       observed: %s\n", v.Observed)
			}
		}
	}
	var objs []string
	for _, o := range j.Objectives {
		if o.Result == "n/a" {
			continue
		}
		v := "-"
		if o.Value != nil {
			v = fmt.Sprintf("%g", *o.Value)
		}
		objs = append(objs, fmt.Sprintf("    %-4s %-44s %s = %s (pass %s)", strings.ToUpper(o.Result), o.ID, o.Measure, v, o.Pass))
	}
	if len(objs) > 0 {
		b.WriteString("  objectives:\n" + strings.Join(objs, "\n") + "\n")
	}
	if len(j.Coverage.Uncovered) > 0 {
		fmt.Fprintf(&b, "  uncovered: %s\n", strings.Join(j.Coverage.Uncovered, ", "))
	}
	if len(j.NA) > 0 {
		fmt.Fprintf(&b, "  n/a: %d rule(s) (", len(j.NA))
		var ws []string
		for _, n := range j.NA[:min(len(j.NA), 4)] {
			ws = append(ws, n.Rule+": "+n.Why)
		}
		b.WriteString(strings.Join(ws, "; "))
		if len(j.NA) > 4 {
			b.WriteString("; …")
		}
		b.WriteString(")\n")
	}
	for _, in := range j.Incidents {
		fmt.Fprintf(&b, "  twin incident: %s\n", in)
	}
	return b.String()
}

func momentsLine(ms map[string]*Moment) string {
	var parts []string
	for _, n := range momentNames[1:] {
		if m := ms[n]; m != nil {
			parts = append(parts, fmt.Sprintf("%s +%.1fs", n, m.After))
		} else {
			parts = append(parts, n+" —")
		}
	}
	return strings.Join(parts, "  ")
}

func shortAll(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = shortPath(p)
	}
	return out
}

// Explain is one violation in full: who did what, what the device was, what was open, and what
// the principal read before it acted (17 — Explanations).
func (j *Judgement) Explain(i int) string {
	if i < 0 || i >= len(j.Violations) {
		return fmt.Sprintf("no violation %d (the judgement has %d)", i+1, len(j.Violations))
	}
	v := j.Violations[i]
	if j.Holdout {
		return "holdout scenario: the critic reports its verdict only (D70)"
	}
	var b strings.Builder
	key := ""
	if v.Key {
		key = ", key"
	}
	fmt.Fprintf(&b, "VIOLATION %s  (%s, %s%s)\n", v.Rule, v.Dimension, v.Severity, key)
	w := v.Witness
	if r := w.Request; r != nil {
		fmt.Fprintf(&b, "  %s sent %s to %s at +%.1fs (seq %d, %s %s %s", w.Principal, r.Canonical, shortPath(r.Path), w.Offset,
			w.Events[0], strings.ToUpper(r.Protocol), r.Method, r.Target)
		if r.Status != 0 {
			fmt.Fprintf(&b, " → %d", r.Status)
		}
		b.WriteString(").\n")
		if bf := w.Before; bf != nil {
			fmt.Fprintf(&b, "  Before it: %s was %v, health %v, AC %v", shortPath(str(bf["path"])), bf["power"], bf["health"], bf["ac"])
			if bf["affected"] == true {
				b.WriteString(", in the reach of an open fault")
			} else {
				b.WriteString(", in no open fault's reach")
			}
			b.WriteString(".\n")
		}
	} else if len(w.Events) > 0 {
		fmt.Fprintf(&b, "  At +%.1fs (seq %d).\n", w.Offset, w.Events[0])
	}
	fmt.Fprintf(&b, "  %s\n", v.Message)
	if v.Expected != "" {
		fmt.Fprintf(&b, "  Expected: %s\n", v.Expected)
	}
	if v.Observed != "" {
		fmt.Fprintf(&b, "  Observed: %s\n", v.Observed)
	}
	if len(w.OpenFaults) > 0 {
		fmt.Fprintf(&b, "  Open faults: %s\n", strings.Join(w.OpenFaults, "; "))
	} else {
		b.WriteString("  Open faults: none\n")
	}
	if len(w.CauseChain) > 0 {
		var cs []string
		for _, c := range w.CauseChain {
			cs = append(cs, fmt.Sprintf("#%d", c))
		}
		fmt.Fprintf(&b, "  Cause chain: %s\n", strings.Join(cs, " ← "))
	}
	if len(w.Reads) > 0 {
		fmt.Fprintf(&b, "  Reads that preceded it (same principal, 60 s):\n")
		for _, r := range w.Reads {
			fmt.Fprintf(&b, "    %s\n", r)
		}
	}
	if len(w.Events) > 1 {
		var es []string
		for _, e := range w.Events {
			es = append(es, fmt.Sprintf("#%d", e))
		}
		fmt.Fprintf(&b, "  Witness events: %s\n", strings.Join(es, " "))
	}
	return b.String()
}

// ---------------------------------------------------------------- JUnit

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

// JUnit renders judgements as JUnit XML: a suite per run, a case per assertion, rule and
// objective.
func JUnit(js ...*Judgement) []byte {
	var out junitSuites
	for _, j := range js {
		s := junitSuite{Name: j.Run.ID}
		if j.Run.Scenario != "" {
			s.Name = j.Run.Scenario + " (" + j.Run.ID + ")"
		}
		add := func(c junitCase) {
			s.Tests++
			if c.Failure != nil {
				s.Failures++
			}
			if c.Skipped != nil {
				s.Skipped++
			}
			s.Cases = append(s.Cases, c)
		}
		add(junitCase{Name: "verdict", ClassName: "critic", Failure: failureIf(j.Verdict == "failed", "verdict "+j.Verdict, j.Reason),
			Skipped: skippedIf(j.Verdict == "not-run", j.Reason)})
		byRule := map[string][]Violation{}
		for _, v := range j.Violations {
			byRule[v.Rule] = append(byRule[v.Rule], v)
		}
		for _, d := range Dimensions {
			ds := j.Dimensions[d]
			if ds == nil {
				continue
			}
			for _, it := range ds.Items {
				c := junitCase{Name: it.ID, ClassName: "critic." + d}
				if it.Result == "fail" {
					body := it.Detail
					for _, v := range byRule[strings.SplitN(it.ID, "@", 2)[0]] {
						if !j.Holdout {
							body += "\n" + v.Message + " — " + v.Observed
						}
					}
					c.Failure = &junitFailure{Message: it.ID + " failed", Type: d, Body: body}
				} else if it.Result == "warn" {
					c.SystemOut = "warning: " + it.Detail
				}
				add(c)
			}
		}
		for _, n := range j.NA {
			add(junitCase{Name: n.Rule, ClassName: "critic.na", Skipped: &junitSkipped{Message: n.Why}})
		}
		out.Suites = append(out.Suites, s)
	}
	b, _ := xml.MarshalIndent(out, "", "  ")
	return append([]byte(xml.Header), b...)
}

func failureIf(cond bool, msg, body string) *junitFailure {
	if !cond {
		return nil
	}
	return &junitFailure{Message: msg, Type: "verdict", Body: body}
}

func skippedIf(cond bool, msg string) *junitSkipped {
	if !cond {
		return nil
	}
	return &junitSkipped{Message: msg}
}

// ---------------------------------------------------------------- Markdown, HTML

// Markdown renders a judgement for a pull-request comment, with the baseline's diff when given.
func (j *Judgement) Markdown(baseline *Judgement) string {
	var b strings.Builder
	icon := map[string]string{"passed": "✅", "failed": "❌", "not-run": "⏭️", "impersonated": "🎭"}[j.Verdict]
	score := "n/a"
	if j.Score != nil {
		score = fmt.Sprintf("%.2f", *j.Score)
	}
	name := j.Run.Scenario
	if name == "" {
		name = j.Run.ID
	}
	fmt.Fprintf(&b, "### %s dc-twin critic: %s — **%s** (score %s)\n\n", icon, name, strings.ToUpper(j.Verdict), score)
	if j.Reason != "" {
		fmt.Fprintf(&b, "%s\n\n", j.Reason)
	}
	b.WriteString("| Dimension | Score | Pass | Warn | Fail |\n|---|---|---|---|---|\n")
	for _, d := range Dimensions {
		ds := j.Dimensions[d]
		if ds == nil {
			continue
		}
		s := "n/a"
		if ds.Score != nil {
			s = fmt.Sprintf("%.2f", *ds.Score)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d |\n", d, s, ds.Pass, ds.Warn, ds.Fail)
	}
	if len(j.Violations) > 0 && !j.Holdout {
		b.WriteString("\n**Violations**\n\n")
		for _, v := range j.Violations {
			key := ""
			if v.Key {
				key = " **key**"
			}
			fmt.Fprintf(&b, "- `%s` (%s%s) at +%.1fs: %s", v.Rule, v.Dimension, key, v.Witness.Offset, v.Message)
			if v.Observed != "" {
				fmt.Fprintf(&b, " — observed %s", v.Observed)
			}
			b.WriteString("\n")
		}
	}
	if len(j.Faults) > 0 {
		b.WriteString("\n| Fault | Target | Exposed | Acknowledged | First action | Mitigated | Recovered |\n|---|---|---|---|---|---|---|\n")
		for _, f := range j.Faults {
			cell := func(n string) string {
				if m := f.Moments[n]; m != nil {
					return fmt.Sprintf("%.1fs", m.After)
				}
				return "—"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", f.Fault, shortPath(f.Target), cell("exposed"), cell("acknowledged"),
				cell("firstAction"), cell("mitigated"), cell("recovered"))
		}
	}
	if baseline != nil {
		b.WriteString("\n**Against the baseline**\n\n```\n" + Diff(baseline, j) + "```\n")
	}
	return b.String()
}

// HTML renders a judgement as a standalone page with the fault timeline.
func (j *Judgement) HTML() string {
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><title>dc-twin judgement</title><style>
:root{--bg:#fff;--fg:#1d1d1f;--mute:#6b6b70;--line:#e3e3e6;--ok:#1a7f37;--warn:#9a6700;--bad:#cf222e}
@media (prefers-color-scheme:dark){:root{--bg:#111214;--fg:#e8e8ea;--mute:#9a9aa1;--line:#2a2b30;--ok:#3fb950;--warn:#d29922;--bad:#f85149}}
body{background:var(--bg);color:var(--fg);font:14px/1.5 system-ui,sans-serif;max-width:1100px;margin:24px auto;padding:0 16px}
table{border-collapse:collapse;width:100%;margin:12px 0}td,th{border-bottom:1px solid var(--line);padding:6px 8px;text-align:left;vertical-align:top}
.pass{color:var(--ok)}.warn{color:var(--warn)}.fail,.failed{color:var(--bad)}.passed{color:var(--ok)}code{font-size:12px}
.bar{position:relative;height:14px;background:var(--line);border-radius:3px}.tick{position:absolute;top:-2px;width:2px;height:18px}
</style>`)
	score := "n/a"
	if j.Score != nil {
		score = fmt.Sprintf("%.2f", *j.Score)
	}
	fmt.Fprintf(&b, "<h1>%s <span class=%q>%s</span></h1><p>score %s · run <code>%s</code> · %s</p>",
		html.EscapeString(j.Run.Scenario), j.Verdict, strings.ToUpper(j.Verdict), score, html.EscapeString(j.Run.ID), html.EscapeString(j.Reason))
	b.WriteString("<h2>Scorecard</h2><table><tr><th>Dimension</th><th>Score</th><th>Items</th></tr>")
	for _, d := range Dimensions {
		ds := j.Dimensions[d]
		if ds == nil {
			continue
		}
		s := "n/a"
		if ds.Score != nil {
			s = fmt.Sprintf("%.2f", *ds.Score)
		}
		var its []string
		for _, it := range ds.Items {
			its = append(its, fmt.Sprintf("<span class=%q>%s</span>", it.Result, html.EscapeString(it.ID)))
		}
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>", d, s, strings.Join(its, "<br>"))
	}
	b.WriteString("</table>")
	if len(j.Faults) > 0 {
		b.WriteString("<h2>Faults</h2><table><tr><th>Fault</th><th>Moments</th><th style='width:45%'>Timeline</th></tr>")
		span := 1.0
		for _, f := range j.Faults {
			for _, m := range f.Moments {
				if m.After > span {
					span = m.After
				}
			}
		}
		colors := map[string]string{"exposed": "#8250df", "acknowledged": "#0969da", "firstAction": "#bf8700", "mitigated": "#1a7f37", "recovered": "#57606a"}
		for _, f := range j.Faults {
			var ticks []string
			names := make([]string, 0)
			for n := range f.Moments {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if c, ok := colors[n]; ok {
					ticks = append(ticks, fmt.Sprintf("<span class=tick title='%s +%.1fs' style='left:%.1f%%;background:%s'></span>", n, f.Moments[n].After, 100*f.Moments[n].After/span, c))
				}
			}
			fmt.Fprintf(&b, "<tr><td>%s<br><code>%s</code></td><td>%s</td><td><div class=bar>%s</div></td></tr>", html.EscapeString(f.Fault),
				html.EscapeString(shortPath(f.Target)), html.EscapeString(momentsLine(f.Moments)), strings.Join(ticks, ""))
		}
		b.WriteString("</table>")
	}
	b.WriteString("<h2>Violations</h2>")
	if len(j.Violations) == 0 {
		b.WriteString("<p>None.</p>")
	}
	for i, v := range j.Violations {
		fmt.Fprintf(&b, "<pre>%s</pre>", html.EscapeString(j.Explain(i)))
		_ = v
	}
	return b.String()
}

// ---------------------------------------------------------------- diff

// Diff lists what changed from judgement a to b: verdict, violations new and fixed, objectives
// that moved, and the stack's request profile.
func Diff(a, b *Judgement) string {
	var out strings.Builder
	if a.Verdict != b.Verdict {
		fmt.Fprintf(&out, "verdict: %s → %s\n", a.Verdict, b.Verdict)
	}
	if a.Score != nil && b.Score != nil && *a.Score != *b.Score {
		fmt.Fprintf(&out, "score: %.2f → %.2f\n", *a.Score, *b.Score)
	}
	ka, kb := violationKeys(a), violationKeys(b)
	for k, n := range kb {
		if ka[k] == 0 {
			fmt.Fprintf(&out, "new violation: %s (%d)\n", k, n)
		}
	}
	for k, n := range ka {
		if kb[k] == 0 {
			fmt.Fprintf(&out, "fixed: %s (%d)\n", k, n)
		}
	}
	oa := map[string]ObjectiveResult{}
	for _, o := range a.Objectives {
		oa[o.ID] = o
	}
	for _, o := range b.Objectives {
		p, ok := oa[o.ID]
		if !ok {
			continue
		}
		if p.Result != o.Result || (p.Value != nil && o.Value != nil && *p.Value != *o.Value) {
			fmt.Fprintf(&out, "objective %s: %s %s → %s %s\n", o.ID, p.Result, fmtPtr(p.Value), o.Result, fmtPtr(o.Value))
		}
	}
	for c, n := range b.Coverage.Canonicals {
		if a.Coverage.Canonicals[c] == 0 {
			fmt.Fprintf(&out, "started using %s (%d requests)\n", c, n)
		}
	}
	for c, n := range a.Coverage.Canonicals {
		if b.Coverage.Canonicals[c] == 0 {
			fmt.Fprintf(&out, "stopped using %s (was %d requests)\n", c, n)
		}
	}
	s := out.String()
	if s == "" {
		return "no change\n"
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

func violationKeys(j *Judgement) map[string]int {
	m := map[string]int{}
	for _, v := range j.Violations {
		m[v.Rule]++
	}
	return m
}

func fmtPtr(f *float64) string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%g", *f)
}
