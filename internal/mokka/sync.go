package mokka

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// Source is where a Syncer reads committed model state: the engine in process (EngineSource), or
// a running twin's control API (ControlSource) for a process outside the twin.
type Source interface {
	// GPUs returns a host's GPUs' state now.
	GPUs(ctx context.Context, host Host) ([]GPUState, error)
	// Changes delivers the model path of everything a committed event touches, until ctx ends.
	// A Source that cannot follow events may return nil; the Syncer then polls.
	Changes(ctx context.Context) <-chan string
}

// Sink applies a node's override document.
type Sink interface {
	Apply(ctx context.Context, node string, doc []byte) error
}

// Syncer keeps every host's override document equal to the model: it renders each host's
// document whenever an event touches the host (and on every resync), and applies it to the
// host's node when it differs from the last one applied.
type Syncer struct {
	Source Source
	Sink   Sink
	Hosts  []Host
	// Resync is how often every host's document is rendered and applied again regardless of
	// events (default 10 s), so that a missed event converges, and so does a node whose document
	// was replaced behind the twin's back: Mokka's node agent clears the override document every
	// time it starts (a node pod restart, a host reboot), and a Sink cannot say what a node holds,
	// so the resync applies every document, changed or not. Applying only what changed since the
	// Syncer's own last write left a GPU the model holds lost answering in NVML after a node agent
	// restart, until the model next changed (found by compat/fleet-intelligence).
	Resync time.Duration
	// Applied, when set, is told every document that changed when it is applied (the first of
	// each host's included): the harness's evidence. A resync's unchanged document is not told.
	Applied func(node string, doc []byte)

	mu   sync.Mutex
	last map[string][]byte
}

// SyncOnce renders and applies every host's document that changed. force applies them all, as
// the resync does.
func (s *Syncer) SyncOnce(ctx context.Context, force bool) error {
	var errs []string
	for _, h := range s.Hosts {
		if err := s.syncHost(ctx, h, force); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (s *Syncer) syncHost(ctx context.Context, h Host, force bool) error {
	gs, err := s.Source.GPUs(ctx, h)
	if err != nil {
		return fmt.Errorf("%s: %w", h.Path, err)
	}
	doc := Overrides(gs)
	s.mu.Lock()
	if s.last == nil {
		s.last = map[string][]byte{}
	}
	same := bytes.Equal(s.last[h.Node], doc)
	s.mu.Unlock()
	if same && !force {
		return nil
	}
	if err := s.Sink.Apply(ctx, h.Node, doc); err != nil {
		return fmt.Errorf("%s: apply to node %s: %w", h.Path, h.Node, err)
	}
	s.mu.Lock()
	s.last[h.Node] = doc
	s.mu.Unlock()
	if s.Applied != nil && !same {
		s.Applied(h.Node, doc)
	}
	return nil
}

// Run applies every host's document, then follows the source until ctx ends, and applies every
// document again at each resync (Resync). Errors are reported to logf and retried at the next
// change or resync.
func (s *Syncer) Run(ctx context.Context, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := s.SyncOnce(ctx, true); err != nil {
		logf("mokka: %v", err)
	}
	every := s.Resync
	if every <= 0 {
		every = 10 * time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	changes := s.Source.Changes(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.SyncOnce(ctx, true); err != nil {
				logf("mokka: %v", err)
			}
		case p, ok := <-changes:
			if !ok {
				changes = nil // the stream ended: resync alone from here
				continue
			}
			for _, h := range s.Hosts {
				if p == h.Path || strings.HasPrefix(p, h.Path+"/") {
					if err := s.syncHost(ctx, h, false); err != nil {
						logf("mokka: %v", err)
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------- the engine, in process

// EngineSource reads an engine in the same process.
type EngineSource struct{ E *engine.Engine }

// GPUs reads the host's GPU components' health, state and faults through the engine's
// accessors, and each fault's shim effects from its catalogue entry.
func (s EngineSource) GPUs(_ context.Context, h Host) ([]GPUState, error) {
	ds, err := s.E.SelectDevices(h.Path)
	if err != nil || len(ds) != 1 {
		return nil, fmt.Errorf("no device %s", h.Path)
	}
	d := ds[0]
	faults := s.E.Faults(d.Path)
	var out []GPUState
	for _, g := range h.GPUs {
		c := componentAt(d, g.Path)
		if c == nil {
			return nil, fmt.Errorf("no component %s", g.Path)
		}
		health, state := s.E.ComponentHealth(c)
		gs := GPUState{Index: g.Index, Path: g.Path, Health: health, State: state}
		for _, f := range faults {
			if f.Target != g.Path {
				continue
			}
			fl := Fault{ID: f.ID, Fault: f.Fault, Title: f.Title}
			if ref := s.E.Catalogue(f.Fault); ref != nil && ref.Spec != nil {
				fl.Shims = ShimsOf(ref.Spec.Effects)
			}
			gs.Faults = append(gs.Faults, fl)
		}
		out = append(out, gs)
	}
	return out, nil
}

// Changes follows the engine's committed events.
func (s EngineSource) Changes(ctx context.Context) <-chan string {
	id, ch := s.E.Subscribe(1024)
	out := make(chan string, 64)
	go func() {
		defer close(out)
		defer s.E.Unsubscribe(id)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- ev.Path:
				default: // the resync covers a burst the consumer did not keep up with
				}
			}
		}
	}()
	return out
}

func componentAt(d *model.Device, path string) *model.Component {
	for _, c := range d.Components {
		if c.Path == path {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------- sinks

// DirSink writes each node's document to <Dir>/<node>/overrides.yaml, atomically (a temporary
// file renamed over the old), as nvml-mock-ctl replaces the file on a node.
type DirSink struct{ Dir string }

func (s DirSink) Apply(_ context.Context, node string, doc []byte) error {
	dir := filepath.Join(s.Dir, node)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".overrides.yaml.tmp")
	if err := os.WriteFile(tmp, doc, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "overrides.yaml"))
}

// ExecSink runs a command per document: its arguments with {node} replaced by the node name, the
// document on its standard input. The command places the document on the node (for a Kind node,
// `kubectl exec` into the nvml-mock pod there; compat/gpu-operator/put-override.sh).
type ExecSink struct {
	Argv    []string
	Timeout time.Duration
}

func (s ExecSink) Apply(ctx context.Context, node string, doc []byte) error {
	if len(s.Argv) == 0 {
		return fmt.Errorf("exec sink: no command")
	}
	t := s.Timeout
	if t <= 0 {
		t = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	args := make([]string, len(s.Argv))
	for i, a := range s.Argv {
		args[i] = strings.ReplaceAll(a, "{node}", node)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader(doc)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}
