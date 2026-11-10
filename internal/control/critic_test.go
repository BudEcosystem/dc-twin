package control

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func post(t *testing.T, url, token string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// TestCriticRoutes: a scenario run comes back judged and stored; a session judges a fault
// injected through the API between its start and stop; a stored judgement renders as text.
func TestCriticRoutes(t *testing.T) {
	tw := upMixedHall(t)
	srv, err := Serve(tw, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	base, tok := "http://"+srv.Info.Addr+"/v1", srv.Info.Token
	sc := `apiVersion: dc-twin/v1alpha1
kind: Scenario
metadata: { name: api-judged }
spec:
  estate: {}
  stack: {}
  timeout: 1m
  steps:
    - act: { seconds: 0.2 }
`
	code, b := post(t, base+"/scenarios/run", tok, map[string]any{"scenario": sc, "subject": "test"})
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var r struct {
		Run       string `json:"run"`
		Verdict   string `json:"verdict"`
		Judgement *struct {
			Verdict string               `json:"verdict"`
			Subject struct{ Ref string } `json:"subject"`
		} `json:"judgement"`
	}
	json.Unmarshal(b, &r)
	if r.Judgement == nil || r.Judgement.Verdict != "passed" || r.Judgement.Subject.Ref != "test" {
		t.Fatalf("a scenario run comes back judged: %s", b)
	}
	resp := get(t, base+"/critic/judgements", tok)
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0]["run"] != r.Run {
		t.Fatalf("stored: %v", list)
	}
	resp = get(t, base+"/critic/judgements/"+r.Run+"?format=text", tok)
	txt, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(txt, []byte("verdict: PASSED")) {
		t.Fatalf("text: %s", txt)
	}
	code, b = post(t, base+"/critic/sessions", tok, map[string]any{"subject": "s"})
	var s map[string]string
	json.Unmarshal(b, &s)
	if code != 200 || s["session"] == "" {
		t.Fatalf("%d %s", code, b)
	}
	// a holdout run inside the session: its stored judgement says only its verdict (D70)
	code, b = post(t, base+"/scenarios/run", tok, map[string]any{"scenario": strings.Replace(sc, "api-judged", "api-holdout", 1), "holdout": true})
	var h struct{ Run string }
	json.Unmarshal(b, &h)
	if code != 200 || h.Run == "" {
		t.Fatalf("%d %s", code, b)
	}
	stored, _ := io.ReadAll(get(t, base+"/critic/judgements/"+h.Run, tok).Body)
	if !bytes.Contains(stored, []byte(`"holdout": true`)) && !bytes.Contains(stored, []byte(`"holdout":true`)) || bytes.Contains(stored, []byte(`"assertions"`)) {
		t.Fatalf("the stored holdout judgement is redacted: %s", stored)
	}
	// the session goes on past the run made inside it
	if code, b := post(t, base+"/faults", tok, map[string]any{"select": "a01/compute-tray-07/gpu-2", "fault": "nvidia/xid/xid-79", "id": "x"}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	code, b = post(t, base+"/critic/sessions/"+s["session"]+"/stop", tok, nil)
	var j struct {
		Faults []struct{ Fault string } `json:"faults"`
	}
	json.Unmarshal(b, &j)
	if code != 200 || len(j.Faults) != 1 || j.Faults[0].Fault != "nvidia/xid/xid-79" {
		t.Fatalf("the session judges the fault injected during it: %d %s", code, b)
	}
	if code, _ := post(t, base+"/critic/judge", tok, map[string]any{}); code != 200 {
		t.Fatalf("re-judge the last run: %d", code)
	}
}
