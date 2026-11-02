package nvue

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// APIPort is where NVUE's REST API listens on a Cumulus switch.
const APIPort = 8765

// User is the account the twin's NVUE authenticates (Cumulus Linux's default user).
const User = "cumulus"

// Applier takes a switch to rendered files: write them, run ifreload and frr-reload.py, restart
// the relay. It returns what they printed, and an error when one of them failed.
type Applier func(ctx context.Context, f Files) (output string, err error)

// revision states, as NVUE names them
const (
	statePending   = "pending"
	stateApplying  = "applying"
	stateApplied   = "applied"
	stateApplyFail = "apply_fail"
	stateInvalid   = "invalid"
)

// change is one PATCH (v, a JSON merge) or DELETE (del) made in a revision.
type change struct {
	path []string
	v    any
	del  bool
}

// revision is the applied configuration it was created from with its changes (tree, what a GET
// at it reads) and the changes themselves, which applying replays onto the configuration applied
// then, as NVUE applies a revision's changes and not the snapshot it started from.
type revision struct {
	tree    Tree
	changes []change
	state   string
	issue   string
	output  string
}

// Server is one switch's NVUE: its revisions and the applied configuration.
type Server struct {
	sw       nos.Switch
	password string
	apply    Applier

	applyMu sync.Mutex // one apply at a time, as NVUE serialises them
	mu      sync.Mutex // the state below; never held while the switch applies
	startup Tree
	applied Tree
	revs    map[string]*revision
	next    int
}

// NewServer makes a switch's NVUE. Its applied configuration is empty until ApplyStartup.
func NewServer(sw nos.Switch, password string, apply Applier) *Server {
	return &Server{sw: sw, password: password, apply: apply, applied: Tree{}, revs: map[string]*revision{}, next: 1}
}

// ApplyStartup applies the configuration the model gives the switch (Startup) as its first
// revision, the startup configuration a switch boots with.
func (s *Server) ApplyStartup(ctx context.Context) error {
	t, err := Startup(s.sw)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.startup = Clone(t)
	r := &revision{tree: Clone(t), changes: []change{{v: t}}, state: statePending}
	s.revs[s.newID()] = r
	s.mu.Unlock()
	s.applyRevision(ctx, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.state != stateApplied {
		return fmt.Errorf("the startup configuration: %s: %s", r.state, r.issue)
	}
	return nil
}

func (s *Server) newID() string {
	id := strconv.Itoa(s.next)
	s.next++
	return id
}

// Applied is a copy of the applied configuration.
func (s *Server) Applied() Tree {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Clone(s.applied)
}

// applyRevision replays a revision's changes onto the configuration applied now and takes the
// switch there, one apply at a time, without holding the state lock while the switch works, so
// reads (a client polling the revision's state) are answered meanwhile. A configuration that
// cannot render is invalid and the switch is untouched; one the switch's tools fail on is
// apply_fail, and since its files are then what the switch has, it is the applied configuration,
// so later revisions build on what the switch runs.
func (s *Server) applyRevision(ctx context.Context, r *revision) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.mu.Lock()
	t := Clone(s.applied)
	for _, c := range r.changes {
		if c.del {
			Delete(t, c.path)
		} else {
			Patch(t, c.path, c.v)
		}
	}
	r.state = stateApplying
	s.mu.Unlock()

	files, rerr := Render(t, s.sw)
	var out string
	var aerr error
	if rerr == nil {
		out, aerr = s.apply(ctx, files)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.tree, r.output = t, out
	switch {
	case rerr != nil:
		r.state, r.issue = stateInvalid, rerr.Error()
	case aerr != nil:
		r.state, r.issue = stateApplyFail, aerr.Error()
		s.applied = Clone(t)
	default:
		r.state, r.issue = stateApplied, ""
		s.applied = Clone(t)
	}
}

// ServeHTTP serves NVUE's REST API under /nvue_v1: GET of any path at a revision (`?rev=`,
// applied by default, startup, or a revision's id), PATCH (a JSON merge: null unsets) and DELETE
// of a path in a pending revision, and the revisions themselves: POST /nvue_v1/revision creates
// one from the applied configuration, PATCH /nvue_v1/revision/<id> {"state":"apply"} applies it,
// GET reads its state. Every request authenticates as User, by HTTP Basic.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || u != User || subtle.ConstantTimeCompare([]byte(p), []byte(s.password)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="nvue"`)
		problem(w, http.StatusUnauthorized, "Unauthorized", "authenticate as "+User)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/nvue_v1")
	if !ok {
		problem(w, http.StatusNotFound, "Not Found", r.URL.Path)
		return
	}
	var path []string
	for _, seg := range strings.Split(strings.Trim(rest, "/"), "/") {
		if seg == "" {
			continue
		}
		if dec, err := url.PathUnescape(seg); err == nil {
			seg = dec
		}
		path = append(path, seg)
	}
	if len(path) > 0 && path[0] == "revision" {
		s.revision(w, r, path[1:])
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.get(w, r, path)
	case http.MethodPatch, http.MethodDelete:
		s.change(w, r, path)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		problem(w, http.StatusMethodNotAllowed, "Method Not Allowed", r.Method)
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, path []string) {
	s.mu.Lock()
	var t Tree
	switch rev := r.URL.Query().Get("rev"); rev {
	case "", "applied", "operational":
		t = s.applied
	case "startup":
		t = s.startup
	default:
		rv := s.revs[rev]
		if rv == nil {
			s.mu.Unlock()
			problem(w, http.StatusNotFound, "Not Found", "no revision "+rev)
			return
		}
		t = rv.tree
	}
	v, found := Get(t, path)
	if found {
		b, _ := json.Marshal(v)
		var out any
		_ = json.Unmarshal(b, &out)
		v = out
	}
	s.mu.Unlock()
	if !found {
		if err := Validate(path, nil); err != nil {
			problem(w, http.StatusNotFound, "Not Found", err.Error())
			return
		}
		v = map[string]any{}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) change(w http.ResponseWriter, r *http.Request, path []string) {
	id := r.URL.Query().Get("rev")
	s.mu.Lock()
	defer s.mu.Unlock()
	rv := s.revs[id]
	switch {
	case id == "":
		problem(w, http.StatusBadRequest, "Bad Request", "a change names its revision (?rev=<id>, from POST /nvue_v1/revision)")
		return
	case rv == nil:
		problem(w, http.StatusNotFound, "Not Found", "no revision "+id)
		return
	case rv.state != statePending:
		problem(w, http.StatusConflict, "Conflict", "revision "+id+" is "+rv.state)
		return
	}
	if r.Method == http.MethodDelete {
		if err := Validate(path, nil); err != nil {
			problem(w, http.StatusBadRequest, "Bad Request", err.Error())
			return
		}
		Delete(rv.tree, path)
		rv.changes = append(rv.changes, change{path: path, del: true})
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		problem(w, http.StatusBadRequest, "Bad Request", err.Error())
		return
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		problem(w, http.StatusBadRequest, "Bad Request", "the body is not JSON: "+err.Error())
		return
	}
	if len(path) == 0 {
		if _, ok := v.(map[string]any); !ok {
			problem(w, http.StatusBadRequest, "Bad Request", "the root takes an object")
			return
		}
	}
	if err := Validate(path, v); err != nil {
		problem(w, http.StatusBadRequest, "Bad Request", err.Error())
		return
	}
	Patch(rv.tree, path, v)
	rv.changes = append(rv.changes, change{path: path, v: v})
	out, _ := Get(rv.tree, path)
	writeJSON(w, http.StatusOK, out)
}

func status(rv *revision) map[string]any {
	issue := map[string]any{}
	if rv.issue != "" {
		issue["0"] = map[string]any{"message": rv.issue, "severity": "error"}
	}
	st := map[string]any{"state": rv.state, "transition": map[string]any{"issue": issue, "progress": rv.output}}
	return st
}

func (s *Server) revision(w http.ResponseWriter, r *http.Request, rest []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			out := map[string]any{}
			for id, rv := range s.revs {
				out[id] = status(rv)
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			id := s.newID()
			rv := &revision{tree: Clone(s.applied), state: statePending}
			s.revs[id] = rv
			writeJSON(w, http.StatusOK, map[string]any{id: status(rv)})
		default:
			w.Header().Set("Allow", "GET, POST")
			problem(w, http.StatusMethodNotAllowed, "Method Not Allowed", r.Method)
		}
		return
	}
	id := strings.Join(rest, "/")
	rv := s.revs[id]
	if rv == nil {
		problem(w, http.StatusNotFound, "Not Found", "no revision "+id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, status(rv))
	case http.MethodDelete:
		if rv.state != statePending {
			problem(w, http.StatusConflict, "Conflict", "revision "+id+" is "+rv.state)
			return
		}
		delete(s.revs, id)
		writeJSON(w, http.StatusOK, map[string]any{})
	case http.MethodPatch:
		var body struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.State != "apply" {
			problem(w, http.StatusBadRequest, "Bad Request", `apply a revision with {"state": "apply"}`)
			return
		}
		if rv.state != statePending {
			problem(w, http.StatusConflict, "Conflict", "revision "+id+" is "+rv.state)
			return
		}
		// applying: no second apply of it, and the state lock is free while the switch works
		rv.state = stateApplying
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		s.applyRevision(ctx, rv)
		cancel()
		s.mu.Lock()
		writeJSON(w, http.StatusOK, status(rv))
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		problem(w, http.StatusMethodNotAllowed, "Method Not Allowed", r.Method)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": code, "title": title, "detail": detail})
}
