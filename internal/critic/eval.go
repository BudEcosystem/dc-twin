package critic

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/expr"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/selector"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// StandardAliases are the aliases policies and the scenario library read a stack through
// (17 — Standard aliases).
var StandardAliases = []string{"machine", "alerts", "allocation", "allocatable", "node", "workload"}

// modelFuncs are the member functions a condition may call besides the aliases.
var modelFuncs = []string{"device", "affected", "openFaults", "bySerial", "count", "state", "selects"}

// world is what a condition reads: the run as of one event (history) or the twin now (live).
type world interface {
	device(path string) map[string]any
	affected(path string) bool
	openFaults(filter map[string]any) []any
	bySerial(serial string) map[string]any
	count(filter map[string]any) int
	state(path, field string) any
	accessor(alias string, args map[string]any) (any, bool)
}

// evaluator compiles each condition once; its functions read whichever world is current, so
// evaluations are serialised.
type evaluator struct {
	mu      sync.Mutex
	env     *cel.Env
	progs   map[string]cel.Program
	cur     world
	aliases []string
}

func newEvaluator(extra []string) (*evaluator, error) {
	x := &evaluator{progs: map[string]cel.Program{}}
	seen := map[string]bool{}
	for _, a := range append(append([]string{}, StandardAliases...), extra...) {
		if !seen[a] && a != "" {
			seen[a] = true
			x.aliases = append(x.aliases, a)
		}
	}
	sort.Strings(x.aliases)
	var opts []cel.EnvOption
	for _, v := range expr.CriticVars {
		opts = append(opts, cel.Variable(v, cel.DynType))
	}
	bind := func(name string, fn func(arg any) any) cel.EnvOption {
		return cel.Function(name, cel.MemberOverload("member_"+name, []*cel.Type{cel.DynType, cel.DynType}, cel.DynType,
			cel.BinaryBinding(func(_ ref.Val, arg ref.Val) ref.Val {
				if x.cur == nil {
					return types.NewErr("no world")
				}
				return types.DefaultTypeAdapter.NativeToValue(fn(expr.Arg(arg)))
			})))
	}
	opts = append(opts,
		bind("device", func(a any) any { return x.cur.device(str(a)) }),
		bind("affected", func(a any) any { return x.cur.affected(str(a)) }),
		bind("openFaults", func(a any) any { return x.cur.openFaults(mapOf(a)) }),
		bind("bySerial", func(a any) any { return x.cur.bySerial(str(a)) }),
		bind("count", func(a any) any { return x.cur.count(mapOf(a)) }),
		bind("state", func(a any) any { m := mapOf(a); return x.cur.state(str(m["path"]), str(m["field"])) }),
		bind("selects", func(a any) any { m := mapOf(a); return selectorMatch(str(m["selector"]), str(m["path"])) }),
	)
	for _, al := range x.aliases {
		al := al
		opts = append(opts, bind(al, func(a any) any {
			args, ok := a.(map[string]any)
			if !ok {
				args = map[string]any{"id": a}
			}
			v, ok := x.cur.accessor(al, args)
			if !ok || v == nil {
				return map[string]any{}
			}
			return v
		}))
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, err
	}
	x.env = env
	return x, nil
}

func (x *evaluator) program(src string) (cel.Program, error) {
	if p := x.progs[src]; p != nil {
		return p, nil
	}
	ast, iss := x.env.Compile(src)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("CEL %q: %w", src, iss.Err())
	}
	p, err := x.env.Program(ast)
	if err != nil {
		return nil, err
	}
	x.progs[src] = p
	return p, nil
}

// check type-checks a condition.
func (x *evaluator) check(src string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	_, err := x.program(src)
	return err
}

// value evaluates a condition in a world with variables.
func (x *evaluator) value(src string, w world, vars map[string]any) (any, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	p, err := x.program(src)
	if err != nil {
		return nil, err
	}
	act := map[string]any{"model": map[string]any{}, "accessors": map[string]any{}, "events": map[string]any{}}
	for _, v := range expr.CriticVars {
		if _, ok := act[v]; !ok {
			act[v] = nil
		}
	}
	for k, v := range vars {
		act[k] = v
	}
	x.cur = w
	defer func() { x.cur = nil }()
	out, _, err := p.Eval(act)
	if err != nil {
		return nil, err
	}
	return expr.Arg(out), nil
}

// truth evaluates a boolean condition; an error (a missing field, an accessor not answered) is
// false, with the error.
func (x *evaluator) truth(src string, w world, vars map[string]any) (bool, error) {
	if strings.TrimSpace(src) == "" {
		return true, nil
	}
	v, err := x.value(src, w, vars)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("CEL %q: want bool, got %T", src, v)
	}
	return b, nil
}

// text evaluates an expression that gives a selector or a name; one that does not compile as
// CEL is taken literally (`on: "a03/**"`).
func (x *evaluator) text(src string, w world, vars map[string]any) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	v, err := x.value(src, w, vars)
	if err != nil {
		return src
	}
	return str(v)
}

// usesAccessors reports whether a condition reads the stack.
func usesAccessors(src string) bool { return strings.Contains(src, "accessors.") }

// ---------------------------------------------------------------- the history world

type histWorld struct {
	h     *History
	seq   uint64
	ahead time.Duration // how far after the event an accessor answer still counts
}

func (w histWorld) device(p string) map[string]any { return w.h.View(p, w.seq) }
func (w histWorld) affected(p string) bool         { return w.h.Affected(p, w.seq) }

func (w histWorld) openFaults(f map[string]any) []any {
	out := []any{}
	for _, x := range w.h.Faults {
		if !x.openAt(w.seq) {
			continue
		}
		if g := str(f["fault"]); g != "" && !globAny(g, x.Fault) {
			continue
		}
		if r := str(f["rack"]); r != "" && r != x.Rack && r != x.RackName {
			continue
		}
		if d := str(f["device"]); d != "" && d != x.Device {
			continue
		}
		out = append(out, x.view())
	}
	return out
}

func (w histWorld) bySerial(s string) map[string]any {
	if p, ok := w.h.bySerial[s]; ok {
		return w.h.View(p, w.seq)
	}
	return map[string]any{}
}

func (w histWorld) state(p, field string) any {
	v, _ := w.h.State(p, field, w.seq)
	return v
}

// count counts the run's events before this one that match: kind, canonical, class, from, on
// (a selector), status ("2xx", "4xx", or a code), within (seconds back).
func (w histWorld) count(f map[string]any) int {
	at := w.h.At(w.seq)
	within := num(f["within"])
	n := 0
	for _, e := range w.h.Events {
		if e.Seq >= w.seq {
			break
		}
		if within > 0 && at.Sub(w.h.time(e)) > time.Duration(within*float64(time.Second)) {
			continue
		}
		flt := filterOf(f)
		if !w.h.matchEvent(flt, e) {
			continue
		}
		if st := str(f["status"]); st != "" && (e.Request == nil || !statusMatch(st, e.Request.Status)) {
			continue
		}
		n++
	}
	return n
}

func (w histWorld) accessor(alias string, args map[string]any) (any, bool) {
	target := w.h.Run.Aliases[alias]
	if target == "" {
		return nil, false
	}
	v, ok := w.h.Accessor(target, args, w.seq, w.ahead)
	return w.h.standardise(alias, v), ok
}

// standardise adds the standard vocabulary's state to a machine's answer (StateMap).
func (h *History) standardise(alias string, v any) any {
	m, ok := v.(map[string]any)
	if !ok || alias != "machine" {
		return v
	}
	st := str(m["state"])
	out := make(map[string]any, len(m)+1)
	for k, x := range m {
		out[k] = x
	}
	if s, ok := h.Run.StateMap[st]; ok {
		out["standardState"] = s
	} else {
		out["standardState"] = st
	}
	return out
}

// ---------------------------------------------------------------- matching

func filterOf(m map[string]any) eventFilter {
	return eventFilter{Kind: str(m["kind"]), Canonical: str(m["canonical"]), Class: str(m["class"]), From: str(m["from"]),
		Principal: str(m["principal"]), On: str(m["on"]), Field: str(m["field"]), To: m["to"]}
}

type eventFilter struct {
	Kind, Canonical, Class, From, Principal, On, Field string
	To                                                 any
}

// matchEvent reports whether an event passes a filter. Canonical and class take globs with |
// alternatives; On is a selector.
func (h *History) matchEvent(f eventFilter, ev schema.Event) bool {
	if f.Kind != "" && !globAny(f.Kind, ev.Kind) {
		return false
	}
	if f.Canonical != "" && (ev.Request == nil || !globAny(f.Canonical, ev.Request.Canonical)) {
		return false
	}
	if f.Class != "" && (ev.Request == nil || !globAny(f.Class, class(ev))) {
		return false
	}
	if f.From != "" && !globAny(f.From, ev.Source.Class) {
		return false
	}
	if f.Principal != "" && !globAny(f.Principal, h.Principal(ev)) {
		return false
	}
	if f.Field != "" && (ev.State == nil || !globAny(f.Field, ev.State.Field)) {
		return false
	}
	if f.To != nil && (ev.State == nil || fmt.Sprint(ev.State.To) != fmt.Sprint(f.To)) {
		return false
	}
	if f.On != "" && !h.matchSelector(f.On, h.DevicePath(ev.Path)) && !h.matchSelector(f.On, ev.Path) {
		return false
	}
	return true
}

// matchSelector matches a path against a selector, its ?class= filter included.
func (h *History) matchSelector(sel, p string) bool {
	s, err := selector.Parse(sel)
	if err != nil {
		return false
	}
	if !s.MatchPath(p) {
		return false
	}
	if len(s.Filter) == 0 {
		return true
	}
	d := h.Devices[p]
	return s.MatchAttrs(func(k string) []string {
		if d == nil {
			return nil
		}
		switch k {
		case "class":
			return []string{d.Class}
		case "fidelity":
			return []string{d.Fidelity}
		case "rack":
			return []string{d.RackName}
		}
		return nil
	})
}

func selectorMatch(sel, p string) bool {
	s, err := selector.Parse(sel)
	return err == nil && s.MatchPath(p)
}

// globAny matches a value against globs separated by |.
func globAny(pattern, v string) bool {
	for _, alt := range strings.Split(pattern, "|") {
		alt = strings.TrimSpace(alt)
		if alt == v {
			return true
		}
		if ok, _ := path.Match(alt, v); ok {
			return true
		}
		if strings.HasSuffix(alt, "*") && strings.HasPrefix(v, strings.TrimSuffix(alt, "*")) {
			return true
		}
	}
	return false
}

func statusMatch(want string, got int) bool {
	if strings.HasSuffix(want, "xx") && len(want) == 3 {
		return got/100 == int(want[0]-'0')
	}
	return fmt.Sprint(got) == want
}

func fromSchema(f schema.EventFilter) eventFilter {
	return eventFilter{Kind: f.Kind, Canonical: f.Canonical, Class: f.Class, From: f.From, Principal: f.Principal, On: f.On, Field: f.Field, To: f.To}
}
