package nvue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// What NVUE's REST API does beyond reads, changes and a plain apply, as NVIDIA Config Manager's
// workflows use it (packages/workflows/…/clients/device/cumulus.py at 1.4.1; 18 — Z7a):
//
//   - commit-confirm: PATCH revision/<id> {"state": "apply", "state-controls": {"confirm": <s>}}
//     applies the revision and waits for a second PATCH with {"state": "apply"} that keeps it. Until
//     then the revision's state is "ays" (awaiting the yes; NVCM waits for a state containing it,
//     and which name NVUE gives that state is UNCONFIRMED); unconfirmed after <s> seconds, the
//     configuration applied before it is applied again and the revision is "confirm_fail".
//   - revision/applied: GET is the applied configuration's state, "applied_and_saved" when it is
//     the startup one, else "applied"; PATCH {"state": "save"} saves it (nv config save).
//   - the actions NVCM posts: system {"@reboot": …}, and system/factory-default {"@reset": …},
//     carried out by the twin through the switch's Actions (SetActions), as on a switch they
//     restart it.
//   - system/ztp: ZTP's state as `nv show system ztp` gives it ({"status": …}).

// Actions are what the switch does for NVUE's reboot and factory reset: the twin's substrate.
type Actions struct {
	// Reboot restarts the switch, keeping its saved configuration
	Reboot func(ctx context.Context) error
	// FactoryReset returns it to its factory state, its ZTP armed
	FactoryReset func(ctx context.Context) error
	// ZTPStatus is ZTP's status ("success", "failed", "in-progress", "disabled", "not-started")
	ZTPStatus func(ctx context.Context) (string, error)
}

// SetActions gives the server the switch's actions.
func (s *Server) SetActions(a Actions) { s.actions = a }

// stateAwaiting is a revision applied with commit-confirm and not confirmed yet.
const (
	stateAwaiting   = "ays"
	stateConfirmOut = "confirm_fail"
	stateSaved      = "applied_and_saved"
)

// revisionPatch is a revision PATCH's body.
type revisionPatch struct {
	State      string         `json:"state"`
	AutoPrompt map[string]any `json:"auto-prompt"`
	Controls   struct {
		Confirm json.Number `json:"confirm"`
	} `json:"state-controls"`
}

// appliedRevision serves revision/applied: its state, and a save.
func (s *Server) appliedRevision(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		st := stateApplied
		if reflectEqual(s.applied, s.startup) {
			st = stateSaved
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": st, "transition": map[string]any{"issue": map[string]any{}, "progress": ""}})
	case http.MethodPatch:
		var body revisionPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.State != "save" {
			problem(w, http.StatusBadRequest, "Bad Request", `revision applied takes {"state": "save"}`)
			return
		}
		s.mu.Unlock()
		err := s.save(r.Context())
		s.mu.Lock()
		s.emit(r, Event{Op: "config.save", Revision: "applied"})
		if err != nil {
			problem(w, http.StatusInternalServerError, "Internal Server Error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": stateSaved})
	default:
		w.Header().Set("Allow", "GET, PATCH")
		problem(w, http.StatusMethodNotAllowed, "Method Not Allowed", r.Method)
	}
}

// save makes the applied configuration the startup one, written to the switch's startup.yaml.
func (s *Server) save(ctx context.Context) error {
	s.mu.Lock()
	s.startup = Clone(s.applied)
	t := Clone(s.applied)
	s.mu.Unlock()
	b, err := StartupYAML(t, s.build())
	if err == nil && s.run != nil {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err = s.run(ctx, "sh", "-c", `mkdir -p /etc/nvue.d && printf '%s' "$1" > `+StartupFile, "sh", string(b))
		cancel()
	}
	if err != nil {
		return fmt.Errorf("saving %s: %w", StartupFile, err)
	}
	return nil
}

// confirmWindow starts commit-confirm's window after a revision applied: unconfirmed when it
// ends, the configuration before it is applied again. Called with s.mu held.
func (s *Server) confirmWindow(rv *revision, prev Tree, d time.Duration) {
	rv.state = stateAwaiting
	rv.confirm = time.AfterFunc(d, func() {
		s.mu.Lock()
		if rv.state != stateAwaiting {
			s.mu.Unlock()
			return
		}
		back := &revision{changes: []change{{del: true}, {v: Clone(prev)}}, state: statePending, user: "system", created: time.Now()}
		s.revs[s.newID()] = back
		s.mu.Unlock()
		s.applyRevision(context.Background(), back)
		s.mu.Lock()
		rv.state, rv.issue = stateConfirmOut, fmt.Sprintf("not confirmed within %s: the configuration before it was applied again", d)
		s.mu.Unlock()
		s.emit(nil, Event{Op: "revision.rollback", State: rv.state, Issue: rv.issue})
	})
}

// action serves POST on system (@reboot) and system/factory-default (@reset).
func (s *Server) action(w http.ResponseWriter, r *http.Request, path []string) {
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		problem(w, http.StatusBadRequest, "Bad Request", "the body is not JSON: "+err.Error())
		return
	}
	var name string
	var do func(context.Context) error
	switch {
	case len(path) == 1 && path[0] == "system" && body["@reboot"] != nil:
		name, do = "reboot", s.actions.Reboot
	case len(path) == 2 && path[0] == "system" && path[1] == "factory-default" && body["@reset"] != nil:
		name, do = "factory-reset", s.actions.FactoryReset
	default:
		problem(w, http.StatusBadRequest, "Bad Request", "the twin's NVUE carries out @reboot on system and @reset on system/factory-default")
		return
	}
	if do == nil {
		problem(w, http.StatusNotImplemented, "Not Implemented", "this switch has no "+name+" (a switch the twin configures itself)")
		return
	}
	s.emit(r, Event{Op: "action." + name})
	// the switch restarts as it answers: the twin acts once the response is written
	go func() {
		time.Sleep(200 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := do(ctx); err != nil {
			s.emit(nil, Event{Op: "action." + name, Issue: err.Error()})
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"state": "start", "status": "action started"})
}

// ztp is GET system/ztp.
func (s *Server) ztp(ctx context.Context) (map[string]any, error) {
	if s.actions.ZTPStatus == nil {
		return map[string]any{"status": "disabled"}, nil
	}
	st, err := s.actions.ZTPStatus(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": st}, nil
}

func reflectEqual(a, b Tree) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
