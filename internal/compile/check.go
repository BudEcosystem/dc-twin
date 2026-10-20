package compile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/expr"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/selector"
	"github.com/google/cel-go/common/types/ref"
)

// Verdict is one requirement's outcome (docs/spec/06-packs-and-stacks.md — The capability
// contract): met, reduced, stub, impersonated, missing, or not-applicable when its `when` is false.
type Verdict struct {
	Component  string
	Capability string
	Verdict    string
	Gate       string
	Detail     string
}

var levelRank = map[string]int{"model": 0, "vm": 1, "hardware": 2}

// Check evaluates a stack's requirements against a compiled estate.
func Check(m *model.Model, reg *pack.Registry, stackName string) ([]Verdict, error) {
	name, _, _ := strings.Cut(stackName, "@")
	st := reg.Stacks[name]
	if st == nil {
		return nil, fmt.Errorf("stack %s not found", stackName)
	}
	var ref *struct {
		comps   []string
		options map[string]any
		imps    []string
	}
	for _, s := range m.Estate.Spec.Stacks {
		n, _, _ := strings.Cut(s.Name, "@")
		if n == name {
			ref = &struct {
				comps   []string
				options map[string]any
				imps    []string
			}{s.Components, s.Options, s.Impersonators}
		}
	}
	options := map[string]any{}
	for k, decl := range st.Spec.Options {
		options[k] = decl["default"]
	}
	enabled := map[string]bool{}
	if ref != nil {
		for k, v := range ref.options {
			options[k] = v
		}
		for _, c := range ref.comps {
			enabled[c] = true
		}
	}
	estateEnv := estateView(m)
	has := func(kind, value string) bool { return estateHas(m, kind, value) }
	var comps []string
	for c := range st.Spec.Components {
		comps = append(comps, c)
	}
	sort.Strings(comps)
	var out []Verdict
	for _, cname := range comps {
		if len(enabled) > 0 && !enabled[cname] {
			continue
		}
		comp := st.Spec.Components[cname]
		for _, rq := range comp.Requires {
			v := Verdict{Component: cname, Capability: rq.Capability, Gate: rq.Gate}
			if v.Gate == "" {
				v.Gate = "scenario"
			}
			if rq.When != "" {
				ok, err := evalWhen(rq.When, estateEnv, options, has)
				if err != nil {
					v.Verdict, v.Detail = "error", err.Error()
					out = append(out, v)
					continue
				}
				if !ok {
					v.Verdict, v.Detail = "n/a", "when: "+rq.When+" is false"
					out = append(out, v)
					continue
				}
			}
			caps := rq.AnyOf
			if len(caps) == 0 {
				caps = []string{rq.Capability}
			}
			scope := ""
			if each, ok := rq.Scope["each"].(string); ok {
				scope = each
			}
			if scope != "" {
				sel, err := selector.Parse(scope)
				if err != nil {
					v.Verdict, v.Detail = "error", err.Error()
					out = append(out, v)
					continue
				}
				total, met, reduced := 0, 0, 0
				var missing []string
				for _, d := range m.Devices {
					if !sel.MatchAttrs(d.Attr) || (sel.HasPatterns() && !sel.MatchPath(d.Path)) {
						continue
					}
					total++
					lv, ok := provides(d, caps)
					switch {
					case !ok:
						if len(missing) < 3 {
							missing = append(missing, d.Path)
						}
					case rq.Preferred != "" && levelRank[lv] < levelRank[rq.Preferred]:
						reduced++
					default:
						met++
					}
				}
				switch {
				case total == 0:
					v.Verdict, v.Detail = "n/a", "no device matches "+scope
				case met+reduced < total:
					v.Verdict, v.Detail = "missing", fmt.Sprintf("%d of %d devices provide none of %v (e.g. %s)", total-met-reduced, total, caps, strings.Join(missing, ", "))
				case reduced > 0:
					v.Verdict, v.Detail = "reduced", fmt.Sprintf("%d of %d below the preferred %s", reduced, total, rq.Preferred)
				default:
					v.Verdict, v.Detail = "met", fmt.Sprintf("%d devices", total)
				}
				out = append(out, v)
				continue
			}
			best, via := "", ""
			for _, c := range caps {
				if lv, src := estateProvides(m, c); src != "" && (best == "" || levelRank[lv] > levelRank[best]) {
					best, via = lv, src
				}
			}
			switch {
			case via == "":
				v.Verdict, v.Detail = "missing", fmt.Sprintf("nothing in the estate provides %v", caps)
			case strings.HasPrefix(via, "impersonator"):
				v.Verdict, v.Detail = "impersonated", via
			case rq.Preferred != "" && levelRank[best] < levelRank[rq.Preferred]:
				v.Verdict, v.Detail = "reduced", fmt.Sprintf("provided at %s by %s; preferred %s", best, via, rq.Preferred)
			default:
				v.Verdict, v.Detail = "met", via
			}
			out = append(out, v)
		}
	}
	return out, nil
}

func provides(d *model.Device, caps []string) (string, bool) {
	for _, p := range d.Profile.Spec.Provides {
		for _, c := range caps {
			if p.Capability == c || (strings.HasSuffix(c, "*") && strings.HasPrefix(p.Capability, strings.TrimSuffix(c, "*"))) {
				for _, at := range p.At {
					if at == d.Fidelity {
						return at, true
					}
				}
			}
		}
	}
	return "", false
}

func estateProvides(m *model.Model, c string) (string, string) {
	for _, d := range m.Devices {
		if lv, ok := provides(d, []string{c}); ok {
			return lv, d.ProfileName
		}
	}
	for _, f := range m.Fabrics {
		for _, p := range f.Provides {
			if p == c {
				lv := "model"
				if f.NOS != "none" {
					lv = "vm"
				}
				return lv, "fabric " + f.Name + " (" + f.Template + ", nos " + f.NOS + ")"
			}
		}
	}
	if b := m.Estate.Spec.Facility.BMS; b != nil && (c == "facility/bms:"+b.Dialect || c == "facility/isolation-request:"+b.Dialect) {
		return "model", "facility BMS publisher " + b.Dialect
	}
	if c == "kubernetes/nodes:kwok" {
		return "model", "impersonator: kwok projection"
	}
	return "", ""
}

func estateView(m *model.Model) map[string]any {
	nets := []any{}
	for n, x := range m.Networks {
		nets = append(nets, map[string]any{"name": n, "role": x.Role, "cidr": x.CIDR, "dhcp": x.DHCP})
	}
	fabs := []any{}
	for _, f := range m.Fabrics {
		fabs = append(fabs, map[string]any{"name": f.Name, "technology": f.Technology, "nos": f.NOS, "manager": f.Manager})
	}
	racks := []any{}
	for _, r := range m.Racks {
		racks = append(racks, map[string]any{"name": r.Name, "format": r.FormatName, "archetype": r.Format.Spec.Archetype})
	}
	return map[string]any{"name": m.Name, "networks": nets, "fabrics": fabs, "racks": racks}
}

func estateHas(m *model.Model, kind, value string) bool {
	switch kind {
	case "fabric.technology":
		for _, f := range m.Fabrics {
			if f.Technology == value {
				return true
			}
		}
	case "domain":
		for _, d := range m.Domains {
			if d.Kind+"/"+d.Technology == value {
				return true
			}
		}
	case "facility.bms":
		return m.Estate.Spec.Facility.BMS != nil && m.Estate.Spec.Facility.BMS.Dialect == value
	case "format":
		for _, r := range m.Racks {
			if r.FormatName == value {
				return true
			}
		}
	}
	return false
}

// evalWhen evaluates a requirement's `when` with estate.has(kind, value) bound.
func evalWhen(src string, estate map[string]any, options map[string]any, has func(kind, value string) bool) (bool, error) {
	// estate.has(k, v) is a two-argument member call; rewrite it to hasKV("k|v") so the
	// one-argument member-function environment can bind it.
	rewritten := rewriteHas(src)
	funcs := expr.Funcs{"hasKV": func(arg ref.Val) ref.Val {
		k, v, _ := strings.Cut(fmt.Sprint(expr.Arg(arg)), "|")
		return expr.Native(has(k, v))
	}}
	return expr.Bool(expr.EnvEstate, rewritten, map[string]any{"estate": estate, "stack": map[string]any{"options": options}}, funcs)
}

func rewriteHas(src string) string {
	out := src
	for {
		i := strings.Index(out, "estate.has(")
		if i < 0 {
			return out
		}
		j := strings.Index(out[i:], ")")
		if j < 0 {
			return out
		}
		inner := out[i+len("estate.has(") : i+j]
		parts := strings.SplitN(inner, ",", 2)
		if len(parts) != 2 {
			return out
		}
		k := strings.Trim(strings.TrimSpace(parts[0]), `"'`)
		v := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		out = out[:i] + fmt.Sprintf(`estate.hasKV("%s|%s")`, k, v) + out[i+j+1:]
	}
}
