package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Restarter brings the estate down and up again from a changed file (a restart apply). It
// returns the twin that is now running: the new one, or, when the new estate does not come up,
// the previous one brought back, together with the error.
type Restarter func(old *twin.Twin, est *schema.Estate, file string) (*twin.Twin, error)

// RackAdd is a rack a draft adds to a row of a hall.
type RackAdd struct {
	Hall     string         `json:"hall,omitempty"`
	Row      string         `json:"row"`
	Name     string         `json:"name"`
	Position int            `json:"position"`
	Format   string         `json:"format"`
	Params   map[string]any `json:"params,omitempty"`
}

// ApplyResult is what an estate apply (or its dry run) reports.
type ApplyResult struct {
	OK           bool      `json:"ok"`
	DryRun       bool      `json:"dryRun"`
	Class        string    `json:"class"` // restart: the estate goes down and up again
	Problems     []string  `json:"problems,omitempty"`
	Warnings     []string  `json:"warnings,omitempty"`
	Notes        []string  `json:"notes,omitempty"`
	Diff         string    `json:"diff,omitempty"`
	Devices      [2]int    `json:"devices"` // before, after
	Links        [2]int    `json:"links"`
	File         string    `json:"file,omitempty"`
	Etag         string    `json:"etag,omitempty"`
	State        string    `json:"state,omitempty"` // restarting, up, failed, restored
	Error        string    `json:"error,omitempty"`
	At           time.Time `json:"at"`
	Took         string    `json:"took,omitempty"`
	RacksAdded   []string  `json:"racksAdded,omitempty"`
	RacksRemoved []string  `json:"racksRemoved,omitempty"`
}

func etagOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:6]) }

// apply plans a draft and, unless it is a dry run, writes the changed estate next to the state
// and restarts the estate on it. The restart runs after the response, so the caller polls
// /v1/status for the outcome. The source estate file is never written.
func (s *Server) apply(ops []Op, etag string, dry bool) (*Plan, int) {
	p, out, est, code := s.plan(ops, etag)
	p.DryRun = dry
	if !p.OK {
		return p, code
	}
	if dry {
		return p, 200
	}
	if len(ops) == 0 {
		p.OK, p.Problems = false, []string{"the draft changes nothing"}
		return p, 400
	}
	if s.Restart == nil {
		p.OK, p.Problems = false, []string{"this server cannot restart its estate (no restarter)"}
		return p, 501
	}
	file := filepath.Join(s.T.Runtime.StateDir, s.T.Estate.Metadata.Name+".applied.yaml")
	if err := os.WriteFile(file, out, 0o644); err != nil {
		p.OK, p.Problems = false, []string{err.Error()}
		return p, 500
	}
	p.File, p.Etag, p.State = file, etagOf(out), "restarting"
	s.genMu.Lock()
	if s.restarting {
		s.genMu.Unlock()
		p.OK, p.Problems = false, []string{"an apply is already restarting the estate"}
		return p, 409
	}
	s.restarting = true
	res := p.ApplyResult // /v1/status carries the outcome without the plan's diff and layout
	s.lastApply = &res
	close(s.gen)
	s.genMu.Unlock()
	go s.restart(est, file, &res)
	return p, 202
}

func (s *Server) restart(est *schema.Estate, file string, res *ApplyResult) {
	t0 := time.Now()
	s.mu.Lock() // waits for the requests in flight, the apply's own included
	nt, err := s.Restart(s.T, est, file)
	done := *res
	done.Took = time.Since(t0).Round(time.Millisecond).String()
	switch {
	case err == nil:
		done.State = "up"
	case nt != nil:
		done.State, done.Error, done.OK = "restored", err.Error(), false
	default:
		done.State, done.Error, done.OK = "failed", err.Error(), false
	}
	if nt != nil {
		s.T = nt
		s.Info.Estate, s.Info.StateDir, s.Info.Gateway = nt.Estate.Metadata.Name, nt.Runtime.StateDir, nt.Net.Gateway
		if b, err := json.MarshalIndent(s.Info, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(nt.Runtime.StateDir, "control.json"), b, 0o600)
		}
	}
	s.mu.Unlock()
	s.genMu.Lock()
	s.lastApply = &done
	s.restarting = false
	s.gen = make(chan struct{})
	s.genMu.Unlock()
}

// fileEtag is the etag of the file the running estate came from.
func fileEtag(t *twin.Twin) string {
	if t.File == "" {
		return ""
	}
	b, err := os.ReadFile(t.File)
	if err != nil {
		return ""
	}
	return etagOf(b)
}
