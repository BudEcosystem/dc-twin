// Package engine is the single writer of the model (docs/spec/02-architecture.md — The engine):
// the clock, the committed event log, behaviours, the fault engine and derived state. Adapters
// translate protocol requests into engine requests and project committed state; they never
// change state themselves.
package engine

import (
	"bufio"
	"container/heap"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Engine owns the model. Every mutation holds mu for writing; adapters read under Read.
type Engine struct {
	mu        sync.RWMutex
	M         *model.Model
	Reg       *pack.Registry
	virtual   bool
	now       time.Time // virtual time; wall time when not virtual
	scale     float64
	timers    timerHeap
	timerSeq  uint64
	cancelled map[uint64]bool

	seq     uint64
	events  []schema.Event // retained window
	retain  int
	logFile *bufio.Writer
	logFH   *os.File
	subs    map[int]chan schema.Event
	subSeq  int
	subMu   sync.Mutex

	faults   []*ActiveFault
	faultSeq int
	health   map[string]string // committed health per device and component path
	sensorSt map[string]string // committed threshold state per sensor
	anchors  map[string]anchor // first-order lag anchors (derived, never logged)
	domains  map[string]string
	flowLost map[string]time.Time

	rng        *rand.Rand
	chaosRate  float64
	chaosFault []string
	incidents  []schema.Event
	hooks      Hooks
	stop       chan struct{}
	started    time.Time
	transition map[string]uint64 // pending power transition timer per device
	// substrates carry out power requests for devices whose power is observed (D19).
	substrates map[string]Substrate
	// pendingHooks run after the write lock is released (adapter callbacks must not re-enter).
	pendingHooks []func()
	// fwTasks are the firmware update tasks each device's BMC is running; a BMC that goes down
	// aborts them (abortFirmwareTasks, D53).
	fwTasks map[string][]fwTask
}

// Hooks let adapters react to behaviour without the engine knowing protocols.
type Hooks struct {
	// OnBoot is called (outside the lock) when a model host completes power-on with a boot
	// override or a network boot first in its order: the boot walker runs.
	OnBoot []func(d *model.Device, target string)
	// OnBMCUp is called when a BMC comes up (after AC returns or a BMC reset): DHCP restarts.
	OnBMCUp []func(d *model.Device)
}

type anchor struct {
	v0, target float64
	t0         time.Time
}

// New creates an engine over a compiled model. logPath may be empty.
func New(m *model.Model, reg *pack.Registry, logPath string) (*Engine, error) {
	e := &Engine{M: m, Reg: reg, virtual: m.Clock == "virtual", scale: m.Estate.Spec.TimeScale,
		cancelled: map[uint64]bool{}, retain: 200000, subs: map[int]chan schema.Event{},
		health: map[string]string{}, sensorSt: map[string]string{}, anchors: map[string]anchor{},
		domains: map[string]string{}, flowLost: map[string]time.Time{}, rng: rand.New(rand.NewSource(m.Seed)),
		stop: make(chan struct{}), transition: map[string]uint64{}, fwTasks: map[string][]fwTask{}}
	if e.scale <= 0 {
		e.scale = 1
	}
	if e.virtual {
		e.now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		e.now = time.Now().UTC()
	}
	e.started = e.now
	if logPath != "" {
		fh, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		e.logFH, e.logFile = fh, bufio.NewWriter(fh)
	}
	e.mu.Lock()
	e.commit(schema.Event{Kind: "twin", Source: schema.EventSource{Class: "twin", Principal: "engine"}, Path: m.Site,
		Message: fmt.Sprintf("estate %s up: %d devices, %d links, clock %s", m.Name, len(m.Devices), len(m.Links), m.Clock)})
	e.recomputePower(0)
	for _, d := range m.Devices {
		e.health[d.Path] = model.OK
		for _, c := range d.Components {
			e.health[c.Path] = model.OK
		}
	}
	e.evalDomains(0)
	e.mu.Unlock()
	return e, nil
}

// Start runs the wall clock loop. Virtual estates advance only by Advance.
func (e *Engine) Start() {
	if e.virtual {
		return
	}
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		lastTick := time.Now()
		for {
			select {
			case <-e.stop:
				return
			case now := <-t.C:
				e.mu.Lock()
				e.now = now.UTC()
				e.runDue()
				if now.Sub(lastTick) >= 500*time.Millisecond {
					e.tick()
					lastTick = now
				}
				hooks := e.pendingHooks
				e.pendingHooks = nil
				e.mu.Unlock()
				for _, h := range hooks {
					go h()
				}
			}
		}
	}()
}

// Stop ends the loop and flushes the log.
func (e *Engine) Stop() {
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.logFile != nil {
		e.logFile.Flush()
		e.logFH.Close()
		e.logFile = nil
	}
}

// AddHooks registers adapter hooks.
func (e *Engine) AddHooks(h Hooks) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hooks.OnBoot = append(e.hooks.OnBoot, h.OnBoot...)
	e.hooks.OnBMCUp = append(e.hooks.OnBMCUp, h.OnBMCUp...)
}

// Read runs f under the read lock.
func (e *Engine) Read(f func(m *model.Model)) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	f(e.M)
}

// Now is the engine clock.
func (e *Engine) Now() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.now
}

func (e *Engine) nowLocked() time.Time { return e.now }

// Virtual reports whether the estate runs on the virtual clock.
func (e *Engine) Virtual() bool { return e.virtual }

// Scale is the behaviour time scale (spec.timeScale).
func (e *Engine) Scale() float64 { return e.scale }

// Started is when the engine came up.
func (e *Engine) Started() time.Time { return e.started }

// ---------------------------------------------------------------- timers

type timer struct {
	at  time.Time
	id  uint64
	fn  func()
	tag string
}

type timerHeap []*timer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].id < h[j].id
	}
	return h[i].at.Before(h[j].at)
}
func (h timerHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *timerHeap) Push(x any)   { *h = append(*h, x.(*timer)) }
func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// after schedules fn (run under the write lock) after a behaviour duration, scaled.
func (e *Engine) after(d time.Duration, tag string, fn func()) uint64 {
	e.timerSeq++
	scaled := time.Duration(float64(d) * e.scale)
	heap.Push(&e.timers, &timer{at: e.now.Add(scaled), id: e.timerSeq, fn: fn, tag: tag})
	return e.timerSeq
}

func (e *Engine) cancel(id uint64) {
	if id != 0 {
		e.cancelled[id] = true
	}
}

func (e *Engine) runDue() {
	for e.timers.Len() > 0 && !e.timers[0].at.After(e.now) {
		t := heap.Pop(&e.timers).(*timer)
		if e.cancelled[t.id] {
			delete(e.cancelled, t.id)
			continue
		}
		t.fn()
	}
}

// Advance moves the virtual clock forward, running behaviours as they fall due. On a wall
// clock estate it is refused (D22).
func (e *Engine) Advance(d time.Duration) error {
	if !e.virtual {
		return fmt.Errorf("clock advance: the estate runs on the wall clock (D22)")
	}
	e.mu.Lock()
	target := e.now.Add(d)
	step := time.Second
	if d > 10*time.Minute {
		step = 5 * time.Second
	}
	for e.now.Before(target) {
		next := e.now.Add(step)
		if e.timers.Len() > 0 && e.timers[0].at.Before(next) {
			next = e.timers[0].at
		}
		if next.After(target) {
			next = target
		}
		if !next.After(e.now) {
			next = e.now.Add(time.Nanosecond)
		}
		e.now = next
		e.runDue()
		e.tick()
	}
	hooks := e.pendingHooks
	e.pendingHooks = nil
	e.mu.Unlock()
	for _, h := range hooks {
		h()
	}
	return nil
}

// ---------------------------------------------------------------- events

// commit appends an event to the committed log; callers hold the write lock.
func (e *Engine) commit(ev schema.Event) uint64 {
	e.seq++
	ev.Seq = e.seq
	ev.T = e.now.Format(time.RFC3339Nano)
	ev.Wall = time.Now().UTC().Format(time.RFC3339Nano)
	ev.Data = redact(ev.Data)
	e.events = append(e.events, ev)
	if len(e.events) > e.retain {
		e.events = append([]schema.Event{}, e.events[len(e.events)-e.retain/2:]...)
	}
	if e.logFile != nil {
		b, _ := json.Marshal(ev)
		e.logFile.Write(b)
		e.logFile.WriteByte('\n')
		if ev.Kind != "request" {
			e.logFile.Flush()
		}
	}
	if ev.Kind == "twin" && ev.Severity == "incident" {
		e.incidents = append(e.incidents, ev)
	}
	e.subMu.Lock()
	for id, ch := range e.subs {
		select {
		case ch <- ev:
		default:
			// a subscriber that cannot keep up is dropped rather than slowing the engine
			close(ch)
			delete(e.subs, id)
		}
	}
	e.subMu.Unlock()
	return ev.Seq
}

var secretKeys = []string{"password", "token", "secret", "credential", "key"}

func redact(d map[string]any) map[string]any {
	if d == nil {
		return nil
	}
	out := make(map[string]any, len(d))
	for k, v := range d {
		lk := strings.ToLower(k)
		red := false
		for _, s := range secretKeys {
			if strings.Contains(lk, s) {
				red = true
			}
		}
		if red {
			out[k] = "[redacted]"
			continue
		}
		if m, ok := v.(map[string]any); ok {
			v = redact(m)
		}
		out[k] = v
	}
	return out
}

// Commit is the adapters' way to log an event they observed (a request, a lease); it runs
// under the write lock.
func (e *Engine) Commit(ev schema.Event) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commit(ev)
}

// Subscribe returns a channel of committed events from now on.
func (e *Engine) Subscribe(buf int) (int, <-chan schema.Event) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	e.subSeq++
	ch := make(chan schema.Event, buf)
	e.subs[e.subSeq] = ch
	return e.subSeq, ch
}

func (e *Engine) Unsubscribe(id int) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	if ch, ok := e.subs[id]; ok {
		close(ch)
		delete(e.subs, id)
	}
}

// Events returns retained events with seq > since that pass the filter.
func (e *Engine) Events(since uint64, filter func(schema.Event) bool) []schema.Event {
	e.mu.RLock()
	defer e.mu.RUnlock()
	i := sort.Search(len(e.events), func(i int) bool { return e.events[i].Seq > since })
	var out []schema.Event
	for _, ev := range e.events[i:] {
		if filter == nil || filter(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// Seq is the last committed sequence number.
func (e *Engine) Seq() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.seq
}

// Incidents are the twin incidents committed so far.
func (e *Engine) Incidents() []schema.Event {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]schema.Event{}, e.incidents...)
}

// Incident commits a twin incident (an adapter crash, a lagging projection).
func (e *Engine) Incident(source, msg string) {
	e.Commit(schema.Event{Kind: "twin", Severity: "incident", Source: schema.EventSource{Class: "twin", Principal: source}, Path: e.M.Site, Message: msg})
}

func (e *Engine) stateEvent(path, field string, from, to any, src schema.EventSource, cause uint64, sev string) uint64 {
	return e.commit(schema.Event{Kind: "state", Source: src, Path: path, State: &schema.EventState{Field: field, From: from, To: to}, Cause: cause, Severity: sev})
}

var behaviourSrc = schema.EventSource{Class: "behaviour", Principal: "engine"}

// ---------------------------------------------------------------- tick

func (e *Engine) tick() {
	e.updateAnchors()
	e.evalThresholds()
	e.rollup(0)
	e.evalDomains(0)
	e.chaos()
}

// SetChaos sets the random fault rate per device per hour (seeded, 08).
func (e *Engine) SetChaos(rate float64, faults []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.chaosRate, e.chaosFault = rate, faults
	e.commit(schema.Event{Kind: "act", Source: schema.EventSource{Class: "user", Principal: "chaos"}, Path: e.M.Site,
		Message: fmt.Sprintf("chaos rate %.3g per device-hour", rate), Data: map[string]any{"rate": rate, "faults": faults}})
}

func (e *Engine) chaos() {
	if e.chaosRate <= 0 {
		return
	}
	// probability per tick interval of 0.5 s for each eligible device
	p := e.chaosRate * 0.5 / 3600
	for _, d := range e.M.Devices {
		if e.rng.Float64() >= p {
			continue
		}
		cands := e.chaosFault
		if len(cands) == 0 {
			continue
		}
		f := cands[e.rng.Intn(len(cands))]
		_, _ = e.injectLocked(d.Path, f, nil, "", schema.EventSource{Class: "twin", Principal: "chaos"}, 0)
	}
}
