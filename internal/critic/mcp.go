package critic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ServeMCP serves the critic to an agent as a Model Context Protocol server on stdio (newline-
// delimited JSON-RPC 2.0): list and read judgements, explain a violation, run a scenario, start
// and stop a session, inject and clear faults, diff two judgements. It is a client of the control
// API like the CLI, so it is unreachable from the software under test (D10); holdout judgements
// keep their details withheld (D70).
func ServeMCP(stateDir string, in io.Reader, out io.Writer) error {
	m := &mcp{dir: stateDir, out: json.NewEncoder(out)}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 32<<20)
	for sc.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		if req.ID == nil { // a notification
			continue
		}
		res, err := m.handle(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if err != nil {
			resp["error"] = map[string]any{"code": -32603, "message": err.Error()}
		} else {
			resp["result"] = res
		}
		if err := m.out.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

type mcp struct {
	dir string
	out *json.Encoder
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var strProp = map[string]any{"type": "string"}

var mcpTools = []mcpTool{
	{"list_judgements", "List the judgements of the running estate's runs: run id, scenario, verdict, score, violation count.", obj(map[string]any{})},
	{"get_judgement", "Read one run's judgement. format: text (default), json, markdown.", obj(map[string]any{"run": strProp, "format": strProp}, "run")},
	{"explain_violation", "Explain one violation of a run's judgement: who did what, the device before it, open faults, the reads before it. index starts at 1.", obj(map[string]any{"run": strProp, "index": map[string]any{"type": "integer"}}, "run", "index")},
	{"run_scenario", "Run a scenario file on the running estate and return its judgement (text). policy is an optional policy file.", obj(map[string]any{"path": strProp, "policy": strProp, "subject": strProp}, "path")},
	{"start_session", "Start a critic session that judges whatever happens until stop_session.", obj(map[string]any{"policy": strProp, "subject": strProp})},
	{"stop_session", "Stop a critic session and return its judgement (text).", obj(map[string]any{"session": strProp}, "session")},
	{"inject_fault", "Inject a catalogue fault into the twin: selector and fault id (e.g. a03/compute-tray-07, generic/leak/tray-detector).", obj(map[string]any{"selector": strProp, "fault": strProp, "id": strProp}, "selector", "fault")},
	{"clear_fault", "Clear a fault by its id.", obj(map[string]any{"id": strProp}, "id")},
	{"diff_judgements", "Diff two runs' judgements: verdict, violations new and fixed, objectives that moved, request profile.", obj(map[string]any{"a": strProp, "b": strProp}, "a", "b")},
}

func (m *mcp) handle(method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "dctwin-critic", "version": "0.1.0"}}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		text, err := m.call(p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}, nil
	}
	return nil, fmt.Errorf("method %s not found", method)
}

func (m *mcp) api(method, path string, body any) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(m.dir, "control.json"))
	if err != nil {
		return nil, fmt.Errorf("no running estate at %q: %w", m.dir, err)
	}
	var info struct{ Addr, Token string }
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, "http://"+info.Addr+"/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+info.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 2 * time.Hour}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", resp.Status, out)
	}
	return out, nil
}

func (m *mcp) judgement(run string) (*Judgement, error) {
	b, err := m.api("GET", "/critic/judgements/"+run, nil)
	if err != nil {
		return nil, err
	}
	var j Judgement
	return &j, json.Unmarshal(b, &j)
}

func (m *mcp) call(name string, a map[string]any) (string, error) {
	s := func(k string) string { return str(a[k]) }
	switch name {
	case "list_judgements":
		b, err := m.api("GET", "/critic/judgements", nil)
		return string(b), err
	case "get_judgement":
		j, err := m.judgement(s("run"))
		if err != nil {
			return "", err
		}
		switch s("format") {
		case "json":
			if j.Holdout {
				j.Redact()
			}
			b, _ := json.MarshalIndent(j, "", "  ")
			return string(b), nil
		case "markdown":
			return j.Markdown(nil), nil
		}
		return j.Text(), nil
	case "explain_violation":
		j, err := m.judgement(s("run"))
		if err != nil {
			return "", err
		}
		i, _ := strconv.Atoi(fmt.Sprint(a["index"]))
		return j.Explain(i - 1), nil
	case "run_scenario":
		sc, err := os.ReadFile(s("path"))
		if err != nil {
			return "", err
		}
		pol := ""
		if p := s("policy"); p != "" {
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			pol = string(b)
		}
		b, err := m.api("POST", "/scenarios/run", map[string]any{"scenario": string(sc), "policy": pol, "subject": s("subject")})
		if err != nil {
			return "", err
		}
		var r struct {
			Judgement *Judgement `json:"judgement"`
			Verdict   string     `json:"verdict"`
		}
		if err := json.Unmarshal(b, &r); err != nil || r.Judgement == nil {
			return string(b), err
		}
		return r.Judgement.Text(), nil
	case "start_session":
		b, err := m.api("POST", "/critic/sessions", map[string]any{"policy": s("policy"), "subject": s("subject")})
		return string(b), err
	case "stop_session":
		b, err := m.api("POST", "/critic/sessions/"+s("session")+"/stop", nil)
		if err != nil {
			return "", err
		}
		var j Judgement
		if err := json.Unmarshal(b, &j); err != nil {
			return string(b), nil
		}
		return j.Text(), nil
	case "inject_fault":
		b, err := m.api("POST", "/faults", map[string]any{"select": s("selector"), "fault": s("fault"), "id": s("id")})
		return string(b), err
	case "clear_fault":
		b, err := m.api("POST", "/faults/clear", map[string]any{"target": s("id")})
		return string(b), err
	case "diff_judgements":
		ja, err := m.judgement(s("a"))
		if err != nil {
			return "", err
		}
		jb, err := m.judgement(s("b"))
		if err != nil {
			return "", err
		}
		return Diff(ja, jb), nil
	}
	return "", fmt.Errorf("no tool %s", name)
}
