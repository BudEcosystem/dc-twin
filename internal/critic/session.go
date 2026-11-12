package critic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/accessor"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/scenario"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// DefaultPolicy is the policy a twin's runs are judged by when none is named: the installed
// stacks' own policies with the generic ones (D67).
func DefaultPolicy(tw *twin.Twin) (*Resolved, error) {
	var names []string
	for _, s := range tw.Estate.Spec.Stacks {
		names = append(names, s.Name)
	}
	return Resolve(ForStacks(names, tw.Reg)...)
}

// ---------------------------------------------------------------- the live sampler

// Sampler reads the stack while a run goes on, so the conditions that read it have answers
// recorded at the times they mattered (D68). It evaluates every condition that reads an accessor
// against the twin now; the answers it reads are committed as events, and the judgement reads
// them back from the log.
type Sampler struct {
	tw       *twin.Twin
	res      *Resolved
	startSeq uint64
	ev       *evaluator
	rec      *accessor.Recorder
	binds    map[string]*accessor.Binding
	stateMap map[string]string
	stop     chan struct{}
	done     chan struct{}
	lastSeq  uint64
	once     sync.Once
}

// StartSampler starts sampling from the events after startSeq.
func StartSampler(tw *twin.Twin, res *Resolved, startSeq uint64) *Sampler {
	s := &Sampler{tw: tw, res: res, startSeq: startSeq, lastSeq: startSeq, binds: map[string]*accessor.Binding{},
		stateMap: map[string]string{}, stop: make(chan struct{}), done: make(chan struct{}),
		rec: accessor.Shared(tw.Engine, tw.Engine.M.Site)}
	var extra []string
	for _, st := range tw.Estate.Spec.Stacks {
		name, _, _ := strings.Cut(st.Name, "@")
		def := tw.Reg.Stacks[name]
		if def == nil || def.Spec.Critic == nil {
			continue
		}
		for alias, target := range def.Spec.Critic.Aliases {
			if b, err := accessor.Resolve(tw.Estate, tw.Reg.Stacks, target); err == nil {
				s.binds[alias] = b
				extra = append(extra, alias)
			}
		}
		for k, v := range def.Spec.Critic.StateMap {
			s.stateMap[k] = v
		}
	}
	s.ev, _ = newEvaluator(extra)
	go s.loop()
	return s
}

// Stop takes a last sample and stops.
func (s *Sampler) Stop() {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
	})
}

func (s *Sampler) loop() {
	defer close(s.done)
	if len(s.binds) == 0 || s.ev == nil {
		<-s.stop
		return
	}
	period := s.res.Sampling
	if period <= 0 {
		period = 2 * time.Second
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			s.sample()
			return
		case <-t.C:
			s.sample()
		}
	}
}

// sample evaluates, against the twin now, every condition that reads the stack and could matter:
// the playbook's for the run's faults, the always rules, and the never rules' for new requests.
func (s *Sampler) sample() {
	eng := s.tw.Engine
	evs := eng.Events(s.startSeq, func(e schema.Event) bool { return e.Kind == "fault" })
	type fl struct {
		f       *Fault
		cleared time.Time
	}
	faults := map[string]*fl{}
	var order []string
	for _, e := range evs {
		id := str(e.Data["id"])
		if _, ok := e.Data["cleared"]; ok {
			if x := faults[id]; x != nil {
				x.cleared = time.Now()
				if t, err := time.Parse(time.RFC3339Nano, e.Wall); err == nil {
					x.cleared = t
				}
			}
			continue
		}
		f := &Fault{Seq: e.Seq, ID: id, Fault: str(e.Data["fault"]), Target: e.Path, Device: e.Path}
		if ds, _ := eng.SelectDevices(e.Path); len(ds) > 0 {
			d := ds[0]
			f.Device, f.Serial, f.Class = d.Path, d.Identity.Serial, d.Class
			if d.Rack != nil {
				f.Rack, f.RackName = d.Rack.Path, d.Rack.Name
			}
		} else if i := strings.LastIndex(e.Path, "/"); i > 0 {
			if ds, _ := eng.SelectDevices(e.Path[:i]); len(ds) > 0 {
				d := ds[0]
				f.Device, f.Serial, f.Class = d.Path, d.Identity.Serial, d.Class
				if d.Rack != nil {
					f.Rack, f.RackName = d.Rack.Path, d.Rack.Name
				}
			}
		}
		faults[id] = &fl{f: f}
		order = append(order, id)
	}
	w := liveWorld{s}
	for _, id := range order {
		x := faults[id]
		for i := range s.res.Playbook {
			p := &s.res.Playbook[i]
			if !globAny(p.On, x.f.Fault) {
				continue
			}
			vars := map[string]any{"fault": x.f.view()}
			open := x.cleared.IsZero()
			recent := !open && time.Since(x.cleared) < 10*time.Minute
			for _, c := range []string{p.Where, p.Acknowledged, p.Mitigated} {
				if (open || recent) && usesAccessors(c) {
					_, _ = s.ev.truth(c, w, vars)
				}
			}
			if p.Then != nil && usesAccessors(p.Then.Recovered) && recent {
				_, _ = s.ev.truth(p.Then.Recovered, w, vars)
			}
			for _, a := range append(append([]schema.ActionPattern{}, p.Permits...), p.Forbids...) {
				if usesAccessors(a.When) {
					_, _ = s.ev.truth(a.When, w, vars)
				}
			}
			break
		}
	}
	news := eng.Events(s.lastSeq, func(e schema.Event) bool { return e.Kind == "request" && e.Source.Class == "stack" })
	if n := eng.Seq(); n > s.lastSeq {
		s.lastSeq = n
	}
	for _, r := range s.res.Rules {
		switch {
		case r.Always != nil && usesAccessors(r.Always.Holds):
			_, _ = s.ev.truth(r.Always.Holds, w, map[string]any{})
		case r.Never != nil && usesAccessors(r.Never.Where):
			for _, e := range news {
				if e.Request == nil || class(e) == "read" {
					continue
				}
				_, _ = s.ev.truth(r.Never.Where, w, map[string]any{"event": map[string]any{"path": e.Path, "device": e.Path},
					"before": w.device(e.Path)})
			}
		}
	}
}

// liveWorld reads the twin now; only its accessor reads matter (they are recorded).
type liveWorld struct{ s *Sampler }

func (w liveWorld) device(p string) map[string]any {
	eng := w.s.tw.Engine
	ds, _ := eng.SelectDevices(p)
	if len(ds) == 0 {
		return map[string]any{"path": p, "known": false}
	}
	d := ds[0]
	v := map[string]any{"path": d.Path, "known": true, "class": d.Class, "serial": d.Identity.Serial, "health": eng.Health(d), "affected": len(eng.Faults(d.Path)) > 0}
	eng.Read(func(*model.Model) {
		v["power"], v["ac"] = d.State.Power, d.State.AC
		if d.Rack != nil {
			v["rack"], v["rackName"], v["rackIsolated"] = d.Rack.Path, d.Rack.Name, d.Rack.Isolated["electrical"]
		}
	})
	return v
}

func (w liveWorld) affected(p string) bool { return len(w.s.tw.Engine.Faults(p)) > 0 }

func (w liveWorld) openFaults(f map[string]any) []any {
	out := []any{}
	for _, x := range w.s.tw.Engine.Faults("") {
		if g := str(f["fault"]); g != "" && !globAny(g, x.Fault) {
			continue
		}
		out = append(out, map[string]any{"id": x.ID, "fault": x.Fault, "path": x.Target})
	}
	return out
}

func (w liveWorld) bySerial(serial string) map[string]any {
	var path string
	w.s.tw.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if d.Identity.Serial == serial {
				path = d.Path
				return
			}
		}
	})
	if path == "" {
		return map[string]any{}
	}
	return w.device(path)
}

func (w liveWorld) count(map[string]any) int  { return 0 }
func (w liveWorld) state(string, string) any  { return nil }
func (w liveWorld) holders(string, any) []any { return []any{} }
func (w liveWorld) accessor(alias string, args map[string]any) (any, bool) {
	b := w.s.binds[alias]
	if b == nil {
		return nil, false
	}
	v, err := w.s.rec.Read(b, alias, args)
	if err != nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok && alias == "machine" {
		st := str(m["state"])
		if x, ok := w.s.stateMap[st]; ok {
			st = x
		}
		m["standardState"] = st
	}
	return v, true
}

// ---------------------------------------------------------------- runs

// RunOptions tune a judged scenario run.
type RunOptions struct {
	Policy   *Resolved
	Subject  string
	Log      io.Writer
	Baseline *Judgement
	Kind     string
	Holdout  bool
}

// RunScenario runs a scenario with the critic watching, and judges it.
func RunScenario(ctx context.Context, tw *twin.Twin, sc *schema.Scenario, opt RunOptions) (*scenario.Result, *Judgement, error) {
	res := opt.Policy
	if res == nil {
		var err error
		if res, err = DefaultPolicy(tw); err != nil {
			return nil, nil, err
		}
	}
	runID := fmt.Sprintf("run-%d-%s", tw.Engine.Seq()+1, sc.Metadata.Name)
	s := StartSampler(tw, res, tw.Engine.Seq())
	result := scenario.Run(ctx, tw, sc, scenario.Options{Log: opt.Log, RunID: runID, Subject: opt.Subject, Kind: opt.Kind})
	s.Stop()
	j, err := JudgeRun(tw, runID, res, Options{Fidelity: result.Report, Subject: opt.Subject, Baseline: opt.Baseline, Holdout: opt.Holdout})
	if err != nil {
		return result, nil, err
	}
	if opt.Holdout {
		j.Redact()
	}
	_ = Save(StateDir(tw), j)
	return result, j, nil
}

// StateDir is where a twin keeps its log and judgements.
func StateDir(tw *twin.Twin) string {
	if tw.Runtime != nil {
		return tw.Runtime.StateDir
	}
	return ""
}

// RunEvents are a run's events, from the engine's memory when it still holds them, else from
// the committed log on disk.
func RunEvents(tw *twin.Twin, runID string) ([]schema.Event, error) {
	eng := tw.Engine
	starts := eng.Events(0, func(e schema.Event) bool {
		return e.Kind == "run" && str(e.Data["run"]) == runID && e.Data["phase"] == "start"
	})
	if len(starts) > 0 {
		evs := eng.Events(starts[0].Seq-1, nil)
		return evs, nil
	}
	eng.FlushLog()
	dir := StateDir(tw)
	if dir == "" {
		return nil, fmt.Errorf("run %s: not in memory and no state directory", runID)
	}
	all, err := ReadLog(dir)
	if err != nil {
		return nil, err
	}
	for i, e := range all {
		if e.Kind == "run" && str(e.Data["run"]) == runID && e.Data["phase"] == "start" {
			return all[i:], nil
		}
	}
	return nil, fmt.Errorf("run %s: no start marker in the log", runID)
}

// JudgeRun judges a run of a live twin.
func JudgeRun(tw *twin.Twin, runID string, res *Resolved, opt Options) (*Judgement, error) {
	evs, err := RunEvents(tw, runID)
	if err != nil {
		return nil, err
	}
	return Judge(evs, res, opt)
}

// ---------------------------------------------------------------- open sessions

// Session judges whatever happens between its start and stop (17 — Interfaces: critic start).
type Session struct {
	ID      string
	tw      *twin.Twin
	res     *Resolved
	sampler *Sampler
	subject string
	stopped *Judgement
	mu      sync.Mutex
}

// StartSession marks a run's start and samples the stack until Stop.
func StartSession(tw *twin.Twin, res *Resolved, subject string) *Session {
	id := fmt.Sprintf("session-%d", tw.Engine.Seq()+1)
	seq := scenario.MarkStart(tw, id, "session", "", subject)
	return &Session{ID: id, tw: tw, res: res, subject: subject, sampler: StartSampler(tw, res, seq-1)}
}

// Current is the judgement of the session so far.
func (s *Session) Current() (*Judgement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped != nil {
		return s.stopped, nil
	}
	return JudgeRun(s.tw, s.ID, s.res, Options{Subject: s.subject})
}

// Stop ends the session and judges it.
func (s *Session) Stop() (*Judgement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped != nil {
		return s.stopped, nil
	}
	s.sampler.Stop()
	scenario.MarkEnd(s.tw, s.ID, map[string]any{"verdict": "", "reason": "session stopped"})
	j, err := JudgeRun(s.tw, s.ID, s.res, Options{Subject: s.subject})
	if err != nil {
		return nil, err
	}
	s.stopped = j
	_ = Save(StateDir(s.tw), j)
	return j, nil
}

// ---------------------------------------------------------------- the log on disk

// ReadLog reads a committed log: a state directory (its closed segments in order, then the
// active file) or one JSONL file.
func ReadLog(p string) ([]schema.Event, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	var files []string
	if st.IsDir() {
		segs, _ := filepath.Glob(filepath.Join(p, "events.*-*.jsonl"))
		sort.Strings(segs)
		files = append(segs, filepath.Join(p, "events.jsonl"))
	} else {
		files = []string{p}
	}
	var out []schema.Event
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var e schema.Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				out = append(out, e)
			}
		}
		fh.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// RunsIn lists the runs a log holds, by their start markers.
func RunsIn(evs []schema.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Kind == "run" && e.Data["phase"] == "start" {
			out = append(out, str(e.Data["run"]))
		}
	}
	return out
}

// EventsOfRun is the slice of a log from a run's start marker on.
func EventsOfRun(evs []schema.Event, runID string) []schema.Event {
	for i, e := range evs {
		if e.Kind == "run" && e.Data["phase"] == "start" && str(e.Data["run"]) == runID {
			return evs[i:]
		}
	}
	return nil
}

// ---------------------------------------------------------------- stored judgements

// Save writes a judgement under <state>/judgements.
func Save(stateDir string, j *Judgement) error {
	if stateDir == "" {
		return nil
	}
	dir := filepath.Join(stateDir, "judgements")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, safeName(j.Run.ID)+".json"), b, 0o600)
}

// Load reads a judgement file.
func Load(p string) (*Judgement, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var j Judgement
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &j, nil
}

// List lists the stored judgements' summaries, newest first.
func List(stateDir string) []map[string]any {
	files, _ := filepath.Glob(filepath.Join(stateDir, "judgements", "*.json"))
	var out []map[string]any
	for _, f := range files {
		j, err := Load(f)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{"run": j.Run.ID, "scenario": j.Run.Scenario, "verdict": j.Verdict, "score": j.Score,
			"violations": len(j.Violations), "started": j.Run.Started, "subject": j.Subject.Ref})
	}
	sort.Slice(out, func(a, b int) bool { return fmt.Sprint(out[a]["started"]) > fmt.Sprint(out[b]["started"]) })
	return out
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ' ' || r == ':' {
			return '_'
		}
		return r
	}, s)
}
