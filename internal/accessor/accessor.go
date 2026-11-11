// Package accessor reads a stack through the accessors its definition declares (docs/spec/08 —
// What an assertion may observe) and records what it read. Every read the twin makes of a stack
// is committed as an `observe` event when its answer changed, so a recorded run can be judged
// again offline with what the stack said at the time (17 — Evidence, D68).
package accessor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Committer is the engine's commit.
type Committer interface {
	Commit(ev schema.Event) uint64
}

// Binding is one accessor of an installed stack.
type Binding struct {
	Target  string // <component>.<accessor>
	Def     schema.Accessor
	Comp    string
	Outputs map[string]string
}

// Resolve finds <component>.<accessor> among the estate's installed stacks.
func Resolve(est *schema.Estate, stacks map[string]*schema.Stack, target string) (*Binding, error) {
	comp, acc, ok := strings.Cut(target, ".")
	if !ok {
		return nil, fmt.Errorf("accessor %q is not <component>.<accessor>", target)
	}
	for _, s := range est.Spec.Stacks {
		name, _, _ := strings.Cut(s.Name, "@")
		st := stacks[name]
		if st == nil {
			continue
		}
		if c, ok := st.Spec.Components[comp]; ok {
			if a, ok := c.Accessors[acc]; ok {
				return &Binding{Target: target, Def: a, Comp: comp, Outputs: s.Outputs}, nil
			}
		}
	}
	return nil, fmt.Errorf("no installed stack declares %s", target)
}

// Call reads the accessor with its arguments.
func (b *Binding) Call(args map[string]any) (any, error) {
	a := b.Def
	if a.Kind != "http" {
		return nil, fmt.Errorf("accessor kind %s is not supported", a.Kind)
	}
	ep := a.Endpoint
	for k, v := range b.Outputs {
		ep = strings.ReplaceAll(ep, "{"+k+"}", v)
	}
	ep = strings.ReplaceAll(ep, "{"+b.Comp+"}", b.Outputs[b.Comp])
	for k, v := range args {
		ep = strings.ReplaceAll(ep, "{"+k+"}", url.QueryEscape(fmt.Sprint(v)))
	}
	if strings.Contains(ep, "{") {
		return nil, fmt.Errorf("accessor endpoint %q has unbound placeholders", ep)
	}
	req, _ := http.NewRequest("GET", ep, nil)
	if tok := b.Outputs[b.Comp+".token"]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Key is the identity of one read: the target and its arguments, canonically.
func Key(target string, args map[string]any) string {
	ks := make([]string, 0, len(args))
	for k := range args {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	b.WriteString(target)
	for _, k := range ks {
		fmt.Fprintf(&b, "|%s=%v", k, args[k])
	}
	return b.String()
}

// Recorder commits each read whose answer differs from the last one recorded for the same key.
type Recorder struct {
	C     Committer
	Path  string // the event path: the site
	Who   string // the principal: scenario or critic
	mu    sync.Mutex
	last  map[string]string
	reads int
}

func NewRecorder(c Committer, site, who string) *Recorder {
	return &Recorder{C: c, Path: site, Who: who, last: map[string]string{}}
}

// Shared returns one recorder per committer, so the scenario runner and the critic's sampler
// do not record the same unchanged answer twice.
var (
	sharedMu sync.Mutex
	shared   = map[Committer]*Recorder{}
)

func Shared(c Committer, site string) *Recorder {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if r := shared[c]; r != nil {
		return r
	}
	r := NewRecorder(c, site, "accessor")
	shared[c] = r
	return r
}

// Reset forgets the answers recorded so far, so the next read of each key is recorded again: a
// run is judged from its own events, and its first answer must be among them.
func (r *Recorder) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.last = map[string]string{}
	r.mu.Unlock()
}

// ResetShared resets a committer's shared recorder, when it has one.
func ResetShared(c Committer) {
	sharedMu.Lock()
	r := shared[c]
	sharedMu.Unlock()
	r.Reset()
}

// Forget drops a committer's shared recorder (when its engine goes down).
func Forget(c Committer) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	delete(shared, c)
}

// Read calls the binding and records the answer when it changed.
func (r *Recorder) Read(b *Binding, alias string, args map[string]any) (any, error) {
	v, err := b.Call(args)
	if r == nil || r.C == nil {
		return v, err
	}
	key := Key(b.Target, args)
	var enc string
	data := map[string]any{"accessor": b.Target, "alias": alias, "args": args}
	if err != nil {
		data["error"] = err.Error()
		enc = "error:" + err.Error()
	} else {
		data["value"] = v
		bs, _ := json.Marshal(v)
		enc = string(bs)
	}
	r.mu.Lock()
	r.reads++
	changed := r.last[key] != enc
	if changed {
		r.last[key] = enc
	}
	r.mu.Unlock()
	if changed {
		r.C.Commit(schema.Event{Kind: "observe", Source: schema.EventSource{Class: "twin", Principal: r.Who}, Path: r.Path,
			Message: "accessor " + b.Target + " answered", Data: data})
	}
	return v, err
}
