package critic

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/policies"
	"github.com/BudEcosystem/dc-twin/stacks"
)

// GenericPolicies apply to every run unless a policy disables a rule (D67).
var GenericPolicies = []string{"generic/safety", "generic/restraint", "generic/hygiene", "generic/liveness", "generic/allocation"}

// Dimensions in scorecard order.
var Dimensions = []string{"correctness", "safety", "timeliness", "restraint", "hygiene", "coverage"}

var defaultWeights = map[string]float64{"correctness": 4, "safety": 4, "timeliness": 2, "restraint": 1, "hygiene": 1}

// Rule is an invariant with the policy it came from.
type Rule struct {
	schema.Invariant
	ID     string // <policy>/<name>
	Policy string
}

type PlaybookRule struct {
	schema.PlaybookEntry
	Policy string
}

type ObjectiveRule struct {
	schema.Objective
	ID     string
	Policy string
}

type PolicyRef struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest"`
	Source  string `json:"source"`
}

type Disabled struct {
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
	Policy string `json:"policy"`
}

// Resolved is a policy with everything it extends, merged.
type Resolved struct {
	Name       string
	Policies   []PolicyRef
	Rules      []Rule
	Playbook   []PlaybookRule
	Objectives []ObjectiveRule
	Weights    map[string]float64
	Gate       *schema.PolicyGate
	Sampling   time.Duration
	Disabled   []Disabled
	IsNot      []string
}

type loaded struct {
	p      *schema.Policy
	source string
	digest string
}

// embedded indexes the generic policies and every stack's policy by name.
func embedded() (map[string]loaded, error) {
	out := map[string]loaded{}
	add := func(fsys fs.FS, p, shown string) error {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		pol, err := parsePolicy(data, shown)
		if err != nil {
			return err
		}
		out[pol.Metadata.Name] = loaded{pol, shown, digest(data)}
		return nil
	}
	if err := fs.WalkDir(policies.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") {
			return err
		}
		return add(policies.FS, p, "policies/"+p)
	}); err != nil {
		return nil, err
	}
	err := fs.WalkDir(stacks.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Base(p) != "policy.yaml" {
			return err
		}
		return add(stacks.FS, p, "stacks/"+p)
	})
	return out, err
}

func parsePolicy(data []byte, where string) (*schema.Policy, error) {
	var p schema.Policy
	if err := schema.Decode(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	if err := schema.CheckHeader(schema.Header{APIVersion: p.APIVersion, Kind: p.Kind, Metadata: p.Metadata}, "Policy"); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	return &p, nil
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])[:12]
}

// LoadPolicy finds a policy by name (generic/safety, reference/default), by stack (the stack's
// own policy) or as a file.
func LoadPolicy(ref string) (*schema.Policy, PolicyRef, error) {
	emb, _ := embedded()
	if l, ok := emb[ref]; ok {
		return l.p, PolicyRef{Name: l.p.Metadata.Name, Version: l.p.Metadata.Version, Digest: l.digest, Source: l.source}, nil
	}
	if data, err := fs.ReadFile(stacks.FS, ref+"/policy.yaml"); err == nil {
		p, err := parsePolicy(data, ref+"/policy.yaml")
		if err != nil {
			return nil, PolicyRef{}, err
		}
		return p, PolicyRef{Name: p.Metadata.Name, Version: p.Metadata.Version, Digest: digest(data), Source: "stacks/" + ref + "/policy.yaml"}, nil
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, PolicyRef{}, fmt.Errorf("policy %q: no embedded policy or stack of that name, and %v", ref, err)
	}
	p, err := parsePolicy(data, ref)
	if err != nil {
		return nil, PolicyRef{}, err
	}
	return p, PolicyRef{Name: p.Metadata.Name, Version: p.Metadata.Version, Digest: digest(data), Source: ref}, nil
}

// ParsePolicy reads a policy document given as bytes (a policy sent to the control API).
func ParsePolicy(data []byte) (*schema.Policy, PolicyRef, error) {
	p, err := parsePolicy(data, "policy")
	if err != nil {
		return nil, PolicyRef{}, err
	}
	return p, PolicyRef{Name: p.Metadata.Name, Version: p.Metadata.Version, Digest: digest(data), Source: "inline"}, nil
}

// Resolve merges policies with everything they extend. The generic policies are always
// included (D67): a policy turns a generic rule off only by name, with a reason.
func Resolve(refs ...string) (*Resolved, error) {
	var docs []*schema.Policy
	var srcs []PolicyRef
	for _, r := range refs {
		p, ref, err := LoadPolicy(r)
		if err != nil {
			return nil, err
		}
		docs, srcs = append(docs, p), append(srcs, ref)
	}
	return ResolveDocs(docs, srcs)
}

// ResolveDocs merges policy documents already read.
func ResolveDocs(docs []*schema.Policy, srcs []PolicyRef) (*Resolved, error) {
	res := &Resolved{Weights: map[string]float64{}, Sampling: 2 * time.Second}
	for k, v := range defaultWeights {
		res.Weights[k] = v
	}
	seen := map[string]bool{}
	var add func(p *schema.Policy, ref PolicyRef) error
	add = func(p *schema.Policy, ref PolicyRef) error {
		name := p.Metadata.Name
		if seen[name] {
			return nil
		}
		seen[name] = true
		for _, e := range p.Spec.Extends {
			q, qref, err := LoadPolicy(e)
			if err != nil {
				return fmt.Errorf("%s extends %s: %w", name, e, err)
			}
			if err := add(q, qref); err != nil {
				return err
			}
		}
		res.Policies = append(res.Policies, ref)
		for _, inv := range p.Spec.Invariants {
			res.Rules = append(res.Rules, Rule{Invariant: inv, ID: name + "/" + inv.Name, Policy: name})
		}
		for _, pb := range p.Spec.Playbook {
			res.Playbook = append(res.Playbook, PlaybookRule{PlaybookEntry: pb, Policy: name})
		}
		for i, o := range p.Spec.Objectives {
			id := o.Name
			if id == "" {
				id = fmt.Sprintf("%s#%d", o.Measure, i+1)
			}
			res.Objectives = append(res.Objectives, ObjectiveRule{Objective: o, ID: name + "/" + id, Policy: name})
		}
		for k, v := range p.Spec.Weights {
			res.Weights[k] = v
		}
		if p.Spec.Gate != nil {
			res.Gate = p.Spec.Gate
		}
		if p.Spec.Sampling != nil && p.Spec.Sampling.Accessors != "" {
			if d, err := time.ParseDuration(p.Spec.Sampling.Accessors); err == nil && d > 0 {
				res.Sampling = d
			}
		}
		for _, d := range p.Spec.Disable {
			res.Disabled = append(res.Disabled, Disabled{Rule: d.Rule, Reason: d.Reason, Policy: name})
		}
		res.IsNot = append(res.IsNot, p.Spec.IsNot...)
		if res.Name == "" {
			res.Name = name
		}
		return nil
	}
	for i, p := range docs {
		if err := add(p, srcs[i]); err != nil {
			return nil, err
		}
	}
	for _, g := range GenericPolicies {
		if seen[g] {
			continue
		}
		p, ref, err := LoadPolicy(g)
		if err != nil {
			return nil, err
		}
		if err := add(p, ref); err != nil {
			return nil, err
		}
	}
	if len(docs) > 0 {
		res.Name = docs[0].Metadata.Name
	}
	for _, d := range res.Disabled {
		if strings.TrimSpace(d.Reason) == "" {
			return nil, fmt.Errorf("%s disables %s without a reason (D67)", d.Policy, d.Rule)
		}
	}
	return res, nil
}

// disabled reports whether a rule id is turned off, and why.
func (r *Resolved) disabled(id string) (Disabled, bool) {
	for _, d := range r.Disabled {
		if globAny(d.Rule, id) {
			return d, true
		}
	}
	return Disabled{}, false
}

// ForStacks is the policy a run of these stacks is judged by when none is named: each stack's
// own policy, else the generic policies alone.
func ForStacks(stacksInstalled []string, reg *pack.Registry) []string {
	var out []string
	for _, s := range stacksInstalled {
		name, _, _ := strings.Cut(s, "@")
		if reg != nil {
			if st := reg.Stacks[name]; st != nil && st.Spec.Critic != nil && st.Spec.Critic.Policy != "" {
				out = append(out, name)
				continue
			}
		}
		if _, err := fs.ReadFile(stacks.FS, name+"/policy.yaml"); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// ---------------------------------------------------------------- validation

// Validate checks a resolved policy: every condition compiles, every rule has one form, every
// playbook entry names faults the catalogues hold, every objective names a measure and parses,
// and every alias it reads is one the stack maps (when a stack is given).
func Validate(res *Resolved, reg *pack.Registry, aliases map[string]string) []string {
	var probs []string
	extra := make([]string, 0, len(aliases))
	for a := range aliases {
		extra = append(extra, a)
	}
	ev, err := newEvaluator(extra)
	if err != nil {
		return []string{err.Error()}
	}
	check := func(where, src string) {
		if strings.TrimSpace(src) == "" {
			return
		}
		if err := ev.check(src); err != nil {
			probs = append(probs, fmt.Sprintf("%s: %v", where, err))
		}
		if aliases != nil {
			for _, a := range StandardAliases {
				if strings.Contains(src, "accessors."+a+"(") {
					if _, ok := aliases[a]; !ok {
						probs = append(probs, fmt.Sprintf("%s: reads alias %s, which the stack does not map (the rule will be n/a)", where, a))
					}
				}
			}
		}
	}
	for _, r := range res.Rules {
		forms := 0
		w := "rule " + r.ID
		if r.Never != nil {
			forms++
			check(w+" where", r.Never.Where)
		}
		if r.Always != nil {
			forms++
			check(w+" holds", r.Always.Holds)
		}
		if r.LeadsTo != nil {
			forms++
			check(w+" when.where", r.LeadsTo.When.Where)
			check(w+" when.holds", r.LeadsTo.When.Holds)
			check(w+" then.holds", r.LeadsTo.Then.Holds)
			check(w+" unless", r.LeadsTo.Unless)
			if _, err := time.ParseDuration(r.LeadsTo.Within); err != nil {
				probs = append(probs, w+": within: "+errText(err))
			}
		}
		if r.AtMost != nil {
			forms++
			check(w+" where", r.AtMost.Where)
			if r.AtMost.Window != "" {
				if _, err := time.ParseDuration(r.AtMost.Window); err != nil {
					probs = append(probs, w+": window: "+errText(err))
				}
			}
		}
		if r.Sometimes != nil {
			forms++
			check(w+" holds", r.Sometimes.Holds)
		}
		if forms != 1 {
			probs = append(probs, fmt.Sprintf("%s: has %d forms, want exactly one of never, always, leadsTo, atMost, sometimes", w, forms))
		}
		if r.Dimension != "" && !contains(Dimensions, r.Dimension) {
			probs = append(probs, fmt.Sprintf("%s: unknown dimension %q", w, r.Dimension))
		}
	}
	for i, p := range res.Playbook {
		w := fmt.Sprintf("playbook %s #%d (%s)", p.Policy, i+1, p.On)
		if reg != nil {
			n := 0
			for id := range reg.Faults {
				if globAny(p.On, id) {
					n++
				}
			}
			if n == 0 {
				probs = append(probs, w+": names no fault in the pinned catalogues")
			}
		}
		for _, c := range []struct{ k, v string }{{"where", p.Where}, {"acknowledged", p.Acknowledged}, {"mitigated", p.Mitigated}} {
			check(w+" "+c.k, c.v)
		}
		if p.Then != nil {
			check(w+" then.recovered", p.Then.Recovered)
		}
		for _, a := range append(append([]schema.ActionPattern{}, p.Permits...), p.Forbids...) {
			check(w+" when", a.When)
		}
		if p.Within != "" {
			if _, err := time.ParseDuration(p.Within); err != nil {
				probs = append(probs, w+": within: "+errText(err))
			}
		}
	}
	for _, o := range res.Objectives {
		w := "objective " + o.ID
		if !knownMeasure(o.Measure) {
			probs = append(probs, fmt.Sprintf("%s: unknown measure %q", w, o.Measure))
		}
		check(w+" where", o.Where)
		for _, c := range []string{o.Pass, o.Warn} {
			if c == "" {
				continue
			}
			if _, err := parseCriterion(c); err != nil {
				probs = append(probs, w+": "+err.Error())
			}
		}
	}
	sort.Strings(probs)
	return probs
}

func errText(err error) string { return err.Error() }

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
