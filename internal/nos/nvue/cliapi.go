package nvue

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// StartupFile is where Cumulus keeps the startup configuration (vendor facts B), which `nv
// config save` writes.
const StartupFile = "/etc/nvue.d/startup.yaml"

// cliEndpoint serves what the switch's `nv` (D76, internal/day0/nvcli) needs beyond NVUE's
// documented REST API, under /nvue_v1/dctwin: real NVUE serves its CLI over a local socket, with
// state the REST API does not show (vendor facts C.2). They are the twin's own and documented as
// such:
//
//   - GET /dctwin/pending: the calling user's pending revision, {<id>: <status>}, or {} when it
//     has none; POST: the same, created from the applied configuration when it has none. NVUE
//     keeps one pending revision per user: `nv set` and `nv unset` stage changes in it and `nv
//     config apply` applies it.
//   - POST /dctwin/detach: `nv config detach`. The user's pending revision stops being theirs and
//     is kept under its id ({<id>: <status>}), as the NVUE CLI page describes it; the NVUE
//     reference says detach discards it instead (vendor facts C.2: UNCONFIRMED), and a detached
//     revision is discarded with DELETE /nvue_v1/revision/<id>.
//   - POST /dctwin/save: `nv config save`, the applied configuration made the startup one and
//     written to StartupFile in the switch. The default build (DefaultBuild, 5.6) predates
//     5.9's auto-save; a later release's is recorded (system config auto-save) and not
//     performed, so an apply does not save on any release.
//   - GET /dctwin/history: `nv config history`, the revisions applied, oldest first, with when
//     and by whom.
func (s *Server) cliEndpoint(w http.ResponseWriter, r *http.Request, rest []string) {
	user := principalOf(r).user
	if user == "" {
		user = User
	}
	what := ""
	if len(rest) == 1 {
		what = rest[0]
	}
	switch {
	case what == "pending" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		s.mu.Lock()
		defer s.mu.Unlock()
		id, rv := s.pendingOf(user)
		if rv == nil && r.Method == http.MethodPost {
			id = s.newID()
			rv = &revision{tree: Clone(s.applied), state: statePending, user: user, created: time.Now()}
			s.revs[id] = rv
			s.pending[user] = id
			s.emit(r, Event{Op: "revision.create", Revision: id, State: rv.state})
		}
		if rv == nil {
			writeJSON(w, http.StatusOK, map[string]any{})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{id: status(rv)})
	case what == "detach" && r.Method == http.MethodPost:
		s.mu.Lock()
		defer s.mu.Unlock()
		id, rv := s.pendingOf(user)
		if rv == nil {
			writeJSON(w, http.StatusOK, map[string]any{})
			return
		}
		delete(s.pending, user)
		s.emit(r, Event{Op: "revision.detach", Revision: id, State: rv.state})
		writeJSON(w, http.StatusOK, map[string]any{id: status(rv)})
	case what == "save" && r.Method == http.MethodPost:
		s.mu.Lock()
		s.startup = Clone(s.applied)
		t := Clone(s.applied)
		s.mu.Unlock()
		b, err := StartupYAML(t, s.build())
		if err == nil && s.run != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			_, err = s.run(ctx, "sh", "-c", `mkdir -p /etc/nvue.d && printf '%s' "$1" > `+StartupFile, "sh", string(b))
			cancel()
		}
		s.emit(r, Event{Op: "config.save"})
		if err != nil {
			problem(w, http.StatusInternalServerError, "Internal Server Error", "saving "+StartupFile+": "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"file": StartupFile})
	case what == "history" && r.Method == http.MethodGet:
		s.mu.Lock()
		type entry struct {
			id int
			m  map[string]any
		}
		var es []entry
		for id, rv := range s.revs {
			if rv.applied.IsZero() {
				continue
			}
			n, _ := strconv.Atoi(id)
			es = append(es, entry{n, map[string]any{"rev-id": id, "date": rv.applied.UTC().Format("2006-01-02 15:04:05"),
				"user": rv.user, "state": rv.state, "type": "apply"}})
		}
		s.mu.Unlock()
		sort.Slice(es, func(i, j int) bool { return es[i].id < es[j].id })
		out := []any{}
		for _, e := range es {
			out = append(out, e.m)
		}
		writeJSON(w, http.StatusOK, out)
	default:
		problem(w, http.StatusNotFound, "Not Found", "the twin's nv endpoints are pending, detach, save and history")
	}
}

// pendingOf is a user's pending revision, if it has one still pending; the state lock is held.
func (s *Server) pendingOf(user string) (string, *revision) {
	id, ok := s.pending[user]
	if !ok {
		return "", nil
	}
	rv := s.revs[id]
	if rv == nil || rv.state != statePending {
		delete(s.pending, user)
		return "", nil
	}
	return id, rv
}

func (s *Server) build() string {
	if s.sw.Build != "" {
		return s.sw.Build
	}
	return DefaultBuild
}

// Version is the Cumulus Linux release whose NVUE grammar the switch speaks: its build's
// (nos.Switch.Build, which spec.adapters.network.nos.cumulusVersion sets), else DefaultVersion.
func (s *Server) Version() Version { return BuildVersion(s.build()) }

// StartupYAML is a configuration in the form of Cumulus's startup.yaml and `nv config show`
// (vendor facts B, C.2): a list of a header and one set of the whole tree.
func StartupYAML(t Tree, version string) ([]byte, error) {
	doc := []any{
		map[string]any{"header": map[string]any{"model": "dctwin", "nvue-api-version": "nvue_v1", "rev-id": 1.0, "version": version}},
		map[string]any{"set": Yamlable(t)},
	}
	return yaml.Marshal(doc)
}

// Yamlable is a JSON tree with its whole numbers as integers, so YAML writes 65101 and not
// 65101.0 or 6.5101e+04.
func Yamlable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = Yamlable(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = Yamlable(c)
		}
		return out
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
	}
	return v
}
