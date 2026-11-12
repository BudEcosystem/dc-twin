package ztp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The twin's ZTP engine against sonic-ztp's own documents and rules, not the twin's reading of the
// docs: testdata/sonic-ztp-reference.json holds every ZTP JSON document in sonic-ztp's tests
// (tests/*.py at the commit it names) and the documents the day-0 tooling writes
// (testdata/tooling/), collected by scripts/sonic-ztp-reference.py. For each, the engine must
// agree with sonic-ztp's code at that commit, which the expectations below transcribe:
//
//   - sections: ZTPJson.__init__ (ZTPSections.py): sorted(ztpDict.keys()) less every key whose
//     value is not a dict; ztp-engine.py runs them in that order.
//   - plugin: ZTPJson.plugin: a string names it; a dict with a truthy dynamic-url or url is a
//     download; a dict's name names it; else re.split("^[0-9]+-", section) names it; any other
//     type is an error.
//   - fields: ConfigSection.__buildDefaults with getField/getValue (ZTPLib.py): the four flags
//     are True/False or a string "true"/"false" in any case, else their default False; status is
//     BOOT when absent, itself when one of the six states, else DISABLED; suspend-exit-code is
//     int() of it, dropped when that fails or is negative.

type refDoc struct {
	Name string         `json:"name"`
	Doc  map[string]any `json:"doc"`
}

func loadReference(t *testing.T) (string, []refDoc) {
	t.Helper()
	b, err := os.ReadFile("testdata/sonic-ztp-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Commit string   `json:"commit"`
		Docs   []refDoc `json:"docs"`
	}
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatal(err)
	}
	return ref.Commit, ref.Docs
}

var upstreamLeadingNum = regexp.MustCompile(`^[0-9]+-`)

// upstreamPlugin is ZTPJson.plugin's resolution: a name, "download", or "" for an error.
func upstreamPlugin(section string, sec map[string]any) string {
	var name *string
	switch p := sec["plugin"].(type) {
	case map[string]any:
		if truthy(p["dynamic-url"]) || truthy(p["url"]) {
			return "download"
		}
		if n, ok := p["name"]; ok && n != nil {
			s := fmt.Sprint(n)
			name = &s
		}
	case string:
		name = &p
	case nil:
	default:
		return ""
	}
	if name == nil {
		res := upstreamLeadingNum.Split(section, 2)
		s := res[len(res)-1]
		name = &s
	}
	return *name
}

// upstreamBool is getValue(val, bool, False).
func upstreamBool(v any) bool {
	var s string
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		s = v
	default:
		s = fmt.Sprint(v)
	}
	return strings.ToLower(s) == "true"
}

// upstreamStatus is __buildDefaults' status.
func upstreamStatus(v any) string {
	if v == nil {
		return stBoot
	}
	if s, ok := v.(string); ok && slices.Contains([]string{"BOOT", "IN-PROGRESS", "SUSPEND", "DISABLED", "FAILED", "SUCCESS"}, s) {
		return s
	}
	return stDisabled
}

// upstreamSuspend is the suspend-exit-code __buildDefaults keeps, or 0 when it drops it.
func upstreamSuspend(v any) int {
	switch v := v.(type) {
	case float64:
		if v >= 0 {
			return int(v)
		}
	case bool:
		if v {
			return 1
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

func TestSonicZTPReference(t *testing.T) {
	commit, docs := loadReference(t)
	t.Logf("sonic-ztp %s: %d documents", commit, len(docs))
	tooling := 0
	for _, rd := range docs {
		if strings.HasPrefix(rd.Name, "tooling/") {
			tooling++
		}
		b, _ := json.Marshal(rd.Doc)
		d, err := parseDoc(b)
		if err != nil {
			t.Errorf("%s: the engine refuses a document sonic-ztp reads: %v", rd.Name, err)
			continue
		}
		top := d.top()
		var want []string
		for k, v := range top {
			if _, ok := v.(map[string]any); ok {
				want = append(want, k)
			}
		}
		sort.Strings(want)
		if got := sections(top); !slices.Equal(got, want) {
			t.Errorf("%s: sections %v, sonic-ztp's %v", rd.Name, got, want)
		}
		for _, n := range want {
			sec := top[n].(map[string]any)
			wantP := upstreamPlugin(n, sec)
			builtin, script, err := pluginOf(n, sec)
			gotP := builtin
			switch {
			case err != nil:
				gotP = ""
			case script != nil:
				gotP = "download"
			}
			if gotP != wantP {
				t.Errorf("%s: %s: plugin %q, sonic-ztp's %q", rd.Name, n, gotP, wantP)
			}
			for _, k := range []string{"ignore-result", "halt-on-failure", "reboot-on-success", "reboot-on-failure"} {
				if got, want := boolOf(sec, k, false), upstreamBool(sec[k]); got != want {
					t.Errorf("%s: %s: %s %v, sonic-ztp's %v (from %#v)", rd.Name, n, k, got, want, sec[k])
				}
			}
			if got, want := statusOf(sec), upstreamStatus(sec["status"]); got != want {
				t.Errorf("%s: %s: status %s, sonic-ztp's %s (from %#v)", rd.Name, n, got, want, sec["status"])
			}
			if got, want := max(intOf(sec, "suspend-exit-code", 0), 0), upstreamSuspend(sec["suspend-exit-code"]); got != want {
				t.Errorf("%s: %s: suspend-exit-code %d, sonic-ztp's %d", rd.Name, n, got, want)
			}
		}
	}
	if tooling == 0 {
		t.Error("no document of the day-0 tooling's in the reference set")
	}
}

// A ZTP JSON whose ztp object has a url is a pointer to the real document (ZTPJson.__init__):
// the engine runs the document downloaded there, not the pointer's (empty) sections.
func TestSonicTopLevelURL(t *testing.T) {
	c, d := testConfig(t)
	order := filepath.Join(d, "order")
	srv, _ := sonicServer(t, map[string]string{
		"/a.sh":      fmt.Sprintf("#!/bin/sh\necho a >> %s\n", order),
		"/real.json": "",
	})
	srv2, _ := sonicServer(t, map[string]string{
		"/real.json": fmt.Sprintf(`{"ztp": {"01-a": {"plugin": {"url": %q, "ignore-section-data": true}}}}`, srv.URL+"/a.sh"),
	})
	writeFile(t, filepath.Join(c.ztpDir, "ztp_data_local.json"), fmt.Sprintf(`{"ztp": {"url": {"source": %q}}}`, srv2.URL+"/real.json"), 0o644)
	if rc := newSonic(c, false).daemon(ctxT(t, 30*time.Second)); rc != 0 {
		t.Fatalf("daemon = %d; log:\n%s", rc, readFile(t, c.ztpLog))
	}
	if got := strings.TrimSpace(readFile(t, order)); got != "a" {
		t.Errorf("ran %q, want the downloaded document's 01-a", got)
	}
	if secStatus(loadDoc(t, c), "01-a") != stSuccess {
		t.Errorf("01-a is not SUCCESS in the saved document")
	}
}
