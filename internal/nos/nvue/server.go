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
	stateApplied   = "applied"
	stateApplyFail = "apply_fail"
	stateInvalid   = "invalid"
)

type revision struct {
	tree   Tree
	state  string
	issue  string
	output string
	at     time.Time
}

// Server is one switch's NVUE: its revisions and the applied configuration.
type Server struct {
	sw       nos.Switch
	password string
	apply    Applier

	mu      sync.Mutex // one apply at a time, as NVUE serialises them
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
	defer s.mu.Unlock()
	s.startup = Clone(t)
	r := &revision{tree: t, state: statePending, at: time.Now()}
	id := s.newID()
	s.revs[id] = r
	s.applyLocked(ctx, r)
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

func (s *Server) applyLocked(ctx context.Context, r *revision) {
	files, err := Render(r.tree, s.sw)
	if err != nil {
		r.state, r.issue = stateInvalid, err.Error()
		return
	}
	out, err := s.apply(ctx, files)
	r.output = out
	if err != nil {
		r.state, r.issue = stateApplyFail, err.Error()
		return
	}
	r.state, r.issue = stateApplied, ""
	s.applied = Clone(r.tree)
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
	out, _ := Get(rv.tree, path)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) status(id string, rv *revision) map[string]any {
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
				out[id] = s.status(id, rv)
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			id := s.newID()
			rv := &revision{tree: Clone(s.applied), state: statePending, at: time.Now()}
			s.revs[id] = rv
			writeJSON(w, http.StatusOK, map[string]any{id: s.status(id, rv)})
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
		writeJSON(w, http.StatusOK, s.status(id, rv))
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
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		s.applyLocked(ctx, rv)
		writeJSON(w, http.StatusOK, s.status(id, rv))
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
