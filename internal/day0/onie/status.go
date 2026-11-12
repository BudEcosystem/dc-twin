package onie

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The phases of status.json. The TAP substrate's day-0 observer reads "phase": any phase but
// installed holds the switch in day0.PhaseONIE (internal/adapter/netsim/tap/day0_linux.go).
const (
	// PhaseDiscovering: DHCP and the waterfall, nothing being fetched.
	PhaseDiscovering = "discovering"
	// PhaseFetching: an installer is being downloaded from URL.
	PhaseFetching = "fetching"
	// PhaseInstalling: the fetched file matched the switch's NOS; installTime runs.
	PhaseInstalling = "installing"
	// PhaseInstalled: the NOS is installed and the program has exited 0.
	PhaseInstalled = "installed"
	// PhaseFailed: the last installer fetched failed (Reason says why); the waterfall goes on.
	PhaseFailed = "failed"
)

// Status is <State>/status.json (nos.Day0State/onie/status.json in the switch). URL, NOS and
// Version are the installer last fetched and what it matched; Reason why the last attempt failed,
// kept through later phases until a NOS installs; Attempts the waterfall passes begun.
type Status struct {
	Phase    string `json:"phase"`
	URL      string `json:"url,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	NOS      string `json:"nos,omitempty"`
	Version  string `json:"version,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Attempts int    `json:"attempts"`
	Updated  string `json:"updated"`
}

// setPhase moves to a phase and writes the status. Discovering forgets the last installer
// fetched (not why it failed).
func (e *Env) setPhase(phase string) {
	e.st.Phase = phase
	if phase == PhaseDiscovering {
		e.st.URL, e.st.SHA256, e.st.NOS, e.st.Version = "", "", "", ""
	}
	e.write()
}

// fail records a failure reason without leaving the current phase.
func (e *Env) fail(format string, a ...any) {
	e.st.Reason = fmt.Sprintf(format, a...)
	e.write()
	e.logf("%s", e.st.Reason)
}

// write replaces status.json atomically, so the observer never reads half a file.
func (e *Env) write() {
	if e.State == "" {
		return
	}
	e.st.Updated = e.Now().UTC().Format(time.RFC3339)
	if e.onWrite != nil {
		e.onWrite(e.st)
	}
	b, _ := json.Marshal(e.st)
	if err := os.MkdirAll(e.State, 0o755); err != nil {
		e.logf("status: %v", err)
		return
	}
	tmp := filepath.Join(e.State, ".status.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		e.logf("status: %v", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(e.State, "status.json")); err != nil {
		e.logf("status: %v", err)
	}
}

// Status is the current status, for tests.
func (e *Env) Status() Status { return e.st }
