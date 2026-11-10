package control

import (
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/critic"
	"github.com/BudEcosystem/dc-twin/internal/scenario"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// The critic's routes (docs/spec/17-critic.md — Interfaces): scenario runs come back judged,
// open sessions judge whatever happens between their start and stop, and stored judgements are
// listed and read. All of it is control plane, unreachable from the software under test (D10).

type criticState struct {
	mu       sync.Mutex
	sessions map[string]*critic.Session
}

// JudgedResult is a scenario's result with its judgement.
type JudgedResult struct {
	*scenario.Result
	Judgement *critic.Judgement `json:"judgement,omitempty"`
	CriticErr string            `json:"criticError,omitempty"`
}

// policyFrom reads a policy sent inline (YAML or JSON text), else the twin's default.
func (s *Server) policyFrom(raw string) (*critic.Resolved, error) {
	if strings.TrimSpace(raw) == "" {
		return critic.DefaultPolicy(s.T)
	}
	p, ref, err := critic.ParsePolicy([]byte(raw))
	if err != nil {
		return nil, err
	}
	return critic.ResolveDocs([]*schema.Policy{p}, []critic.PolicyRef{ref})
}

func (s *Server) runScenario(w http.ResponseWriter, r *http.Request, body map[string]any) {
	raw, _ := body["_raw"].(string)
	if sc, ok := body["scenario"].(string); ok {
		raw = sc
	}
	var sc schema.Scenario
	if err := schema.Decode([]byte(raw), &sc); err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	pol, _ := body["policy"].(string)
	res, err := s.policyFrom(pol)
	if err != nil {
		jsonErr(w, 400, "policy: "+err.Error())
		return
	}
	subject, _ := body["subject"].(string)
	holdout, _ := body["holdout"].(bool)
	result, j, err := critic.RunScenario(r.Context(), s.T, &sc, critic.RunOptions{Policy: res, Subject: subject, Holdout: holdout})
	out := JudgedResult{Result: result, Judgement: j}
	if err != nil {
		out.CriticErr = err.Error()
	}
	if holdout && j != nil {
		// a holdout reports its verdict and scores only (D70)
		// (RunScenario has already redacted the judgement it stored)
		out.Result = &scenario.Result{Run: result.Run, Scenario: result.Scenario, Estate: result.Estate, Verdict: result.Verdict, Took: result.Took}
	}
	writeJSON(w, out)
}

// criticRoute serves /v1/critic/…; it reports whether it handled the request.
func (s *Server) criticRoute(w http.ResponseWriter, r *http.Request, p string, body map[string]any) bool {
	if !strings.HasPrefix(p, "/critic/") {
		return false
	}
	s.crit.mu.Lock()
	if s.crit.sessions == nil {
		s.crit.sessions = map[string]*critic.Session{}
	}
	s.crit.mu.Unlock()
	dir := critic.StateDir(s.T)
	rest := strings.TrimPrefix(p, "/critic/")
	if dir == "" && strings.HasPrefix(rest, "judgements") {
		jsonErr(w, 404, "this twin keeps no state directory, so no judgement is stored")
		return true
	}
	switch {
	case rest == "judgements" && r.Method == http.MethodGet:
		writeJSON(w, critic.List(dir))
	case strings.HasPrefix(rest, "judgements/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(rest, "judgements/")
		if strings.ContainsAny(id, "/\\") || strings.HasPrefix(id, ".") {
			jsonErr(w, 400, "bad judgement id")
			return true
		}
		j, err := critic.Load(filepath.Join(dir, "judgements", id+".json"))
		if err != nil {
			jsonErr(w, 404, err.Error())
			return true
		}
		if f := r.URL.Query().Get("format"); f != "" {
			s.renderJudgement(w, j, f)
			return true
		}
		writeJSON(w, j)
	case rest == "sessions" && r.Method == http.MethodPost:
		pol, _ := body["policy"].(string)
		res, err := s.policyFrom(pol)
		if err != nil {
			jsonErr(w, 400, "policy: "+err.Error())
			return true
		}
		subject, _ := body["subject"].(string)
		sess := critic.StartSession(s.T, res, subject)
		s.crit.mu.Lock()
		s.crit.sessions[sess.ID] = sess
		s.crit.mu.Unlock()
		writeJSON(w, map[string]any{"session": sess.ID})
	case rest == "sessions" && r.Method == http.MethodGet:
		s.crit.mu.Lock()
		var ids []string
		for id := range s.crit.sessions {
			ids = append(ids, id)
		}
		s.crit.mu.Unlock()
		writeJSON(w, map[string]any{"sessions": ids})
	case strings.HasPrefix(rest, "sessions/"):
		id, action, _ := strings.Cut(strings.TrimPrefix(rest, "sessions/"), "/")
		s.crit.mu.Lock()
		sess := s.crit.sessions[id]
		s.crit.mu.Unlock()
		if sess == nil {
			jsonErr(w, 404, "no session "+id)
			return true
		}
		var j *critic.Judgement
		var err error
		switch {
		case action == "stop" && r.Method == http.MethodPost:
			j, err = sess.Stop()
			s.crit.mu.Lock()
			delete(s.crit.sessions, id)
			s.crit.mu.Unlock()
		case action == "" && r.Method == http.MethodGet:
			j, err = sess.Current()
		default:
			jsonErr(w, 405, "GET a session, or POST …/stop")
			return true
		}
		if err != nil {
			jsonErr(w, 500, err.Error())
			return true
		}
		writeJSON(w, j)
	case rest == "judge" && r.Method == http.MethodPost:
		// judge a run of this twin again, under a policy given now (offline re-judgement)
		pol, _ := body["policy"].(string)
		res, err := s.policyFrom(pol)
		if err != nil {
			jsonErr(w, 400, "policy: "+err.Error())
			return true
		}
		run, _ := body["run"].(string)
		if run == "" {
			// the latest run: in memory when the engine still holds its start, else in the log
			runs := critic.RunsIn(s.T.Engine.Events(0, func(e schema.Event) bool { return e.Kind == "run" }))
			if len(runs) == 0 && dir != "" {
				s.T.Engine.FlushLog()
				evs, err := critic.ReadLog(dir)
				if err != nil {
					jsonErr(w, 500, err.Error())
					return true
				}
				runs = critic.RunsIn(evs)
			}
			if len(runs) == 0 {
				jsonErr(w, 404, "no run in the log")
				return true
			}
			run = runs[len(runs)-1]
		}
		j, err := critic.JudgeRun(s.T, run, res, critic.Options{})
		if err != nil {
			jsonErr(w, 404, err.Error())
			return true
		}
		writeJSON(w, j)
	default:
		jsonErr(w, 404, "no critic route "+p)
	}
	return true
}

func (s *Server) renderJudgement(w http.ResponseWriter, j *critic.Judgement, format string) {
	switch format {
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(j.Text()))
	case "markdown":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write([]byte(j.Markdown(nil)))
	case "junit":
		w.Header().Set("Content-Type", "application/xml")
		w.Write(critic.JUnit(j))
	case "html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(j.HTML()))
	default:
		writeJSON(w, j)
	}
}
