// Package expr holds the CEL environments of docs/spec/14-expressions.md. Every `when`, `for`,
// `requires`, `formation` and `inputs` value is CEL; expressions are compiled (type-checked)
// at validation time, so an unknown function or variable is an estate or pack error.
package expr

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// Env names one environment of spec 14.
type Env string

const (
	EnvParams    Env = "params"    // format slot `when`: params
	EnvRack      Env = "rack"      // format domain `formation`: rack
	EnvBehaviour Env = "behaviour" // profile behaviours.*.requires: device, rack, estate
	EnvEstate    Env = "estate"    // stack requires[].when, inputs: estate, stack
	EnvScenario  Env = "scenario"  // scenario wait.for and model observations
)

// Funcs are the member functions bound at run time (model.select, accessors.<alias>,
// events.count …). Each takes the receiver and one argument.
type Funcs map[string]func(arg ref.Val) ref.Val

var (
	mu    sync.Mutex
	cache = map[string]cel.Program{}
)

func envFor(e Env, names []string, funcs Funcs) (*cel.Env, error) {
	var opts []cel.EnvOption
	switch e {
	case EnvParams:
		opts = append(opts, cel.Variable("params", cel.MapType(cel.StringType, cel.DynType)))
	case EnvRack:
		opts = append(opts, cel.Variable("rack", cel.DynType))
	case EnvBehaviour:
		opts = append(opts, cel.Variable("device", cel.DynType), cel.Variable("rack", cel.DynType), cel.Variable("estate", cel.DynType))
	case EnvEstate:
		opts = append(opts, cel.Variable("estate", cel.DynType), cel.Variable("stack", cel.DynType))
	case EnvScenario:
		opts = append(opts, cel.Variable("model", cel.DynType), cel.Variable("accessors", cel.DynType),
			cel.Variable("events", cel.DynType), cel.Variable("clock", cel.DynType))
	default:
		return nil, fmt.Errorf("unknown CEL environment %q", e)
	}
	for _, name := range names {
		var ov cel.FunctionOpt
		if fn, ok := funcs[name]; ok {
			fn := fn
			ov = cel.MemberOverload("member_"+name, []*cel.Type{cel.DynType, cel.DynType}, cel.DynType,
				cel.BinaryBinding(func(_ ref.Val, arg ref.Val) ref.Val { return fn(arg) }))
		} else {
			ov = cel.MemberOverload("member_"+name, []*cel.Type{cel.DynType, cel.DynType}, cel.DynType)
		}
		opts = append(opts, cel.Function(name, ov))
	}
	return cel.NewEnv(opts...)
}

// Check type-checks an expression against an environment with the named member functions.
func Check(e Env, src string, funcs ...string) error {
	env, err := envFor(e, funcs, nil)
	if err != nil {
		return err
	}
	if _, iss := env.Compile(src); iss != nil && iss.Err() != nil {
		return fmt.Errorf("CEL (%s): %q: %w", e, src, iss.Err())
	}
	return nil
}

// Eval evaluates an expression with variables and bound member functions.
func Eval(e Env, src string, vars map[string]any, funcs Funcs) (any, error) {
	names := make([]string, 0, len(funcs))
	for n := range funcs {
		names = append(names, n)
	}
	sort.Strings(names)
	key := string(e) + "\x00" + src
	if len(funcs) == 0 {
		mu.Lock()
		prg := cache[key]
		mu.Unlock()
		if prg != nil {
			return run(prg, src, vars)
		}
	}
	env, err := envFor(e, names, funcs)
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(src)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("CEL (%s): %q: %w", e, src, iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		return nil, err
	}
	if len(funcs) == 0 {
		mu.Lock()
		cache[key] = prg
		mu.Unlock()
	}
	return run(prg, src, vars)
}

func run(prg cel.Program, src string, vars map[string]any) (any, error) {
	out, _, err := prg.Eval(vars)
	if err != nil {
		return nil, fmt.Errorf("CEL %q: %w", src, err)
	}
	return toNative(out.Value()), nil
}

func toNative(v any) any {
	switch x := v.(type) {
	case ref.Val:
		return toNative(x.Value())
	case map[ref.Val]ref.Val:
		m := map[string]any{}
		for k, val := range x {
			m[fmt.Sprint(k.Value())] = toNative(val)
		}
		return m
	case []ref.Val:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = toNative(val)
		}
		return out
	}
	return v
}

// Bool evaluates an expression that must be boolean.
func Bool(e Env, src string, vars map[string]any, funcs Funcs) (bool, error) {
	v, err := Eval(e, src, vars, funcs)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("CEL %q: want bool, got %T", src, v)
	}
	return b, nil
}

// Native converts a Go value to a CEL value for function results.
func Native(v any) ref.Val { return types.DefaultTypeAdapter.NativeToValue(v) }

// Arg returns a function argument as a Go value.
func Arg(v ref.Val) any { return toNative(v.Value()) }

// IsError reports whether a CEL value is an error value.
func IsError(v ref.Val) bool { return types.IsError(v) }

// Errorf returns a CEL error value.
func Errorf(format string, args ...any) ref.Val { return types.NewErr(format, args...) }

// MemberFuncs lists the member functions a scenario expression uses (best effort, for Check).
func MemberFuncs(src string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(src, func(r rune) bool {
		return !(r == '.' || r == '_' || r == '(' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		for _, part := range strings.Split(tok, ".")[1:] {
			if i := strings.IndexByte(part, '('); i > 0 {
				out = append(out, part[:i])
			}
		}
	}
	return out
}
