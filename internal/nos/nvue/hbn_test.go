package nvue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// nicoConfig reads one of NICo's expected NVUE configurations (testdata/nico) as the JSON NICo's
// agent sends: the "set" object, with YAML 1.2's scalars (on and off are strings).
func nicoConfig(t *testing.T, file string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc []map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	for _, e := range doc {
		if set, ok := e["set"]; ok {
			j, err := json.Marshal(jsonable(set))
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			_ = json.Unmarshal(j, &out)
			return out
		}
	}
	t.Fatalf("%s: no set", file)
	return nil
}

func jsonable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			x[k] = jsonable(c)
		}
		return x
	case map[any]any:
		out := map[string]any{}
		for k, c := range x {
			out[fmt.Sprint(k)] = jsonable(c)
		}
		return out
	case []any:
		for i := range x {
			x[i] = jsonable(x[i])
		}
	}
	return v
}

// nicoPush is NICo's agent's push_config (nvue-client): create a revision, empty it, patch the
// whole configuration in, ask the diff from applied, apply when there is one, poll its state.
// It returns the revision's final state, "" when the diff was empty.
func nicoPush(t *testing.T, url string, cfg map[string]any) (string, map[string]any) {
	t.Helper()
	do := func(method, path string, body any) (int, map[string]any) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url+path, rd)
		req.SetBasicAuth(User, "secret")
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	st, rev := do("POST", "/nvue_v1/revision", nil)
	if st != 200 || len(rev) != 1 {
		t.Fatalf("POST revision: %d %v", st, rev)
	}
	var id string
	for k := range rev {
		id = k
	}
	if st, out := do("DELETE", "/nvue_v1/?rev="+id, map[string]any{}); st != 200 {
		t.Fatalf("DELETE /: %d %v", st, out)
	}
	// NICo's NvueConfig: every top-level object, null when the template has none
	body := map[string]any{}
	for _, k := range []string{"bridge", "evpn", "interface", "nve", "router", "system", "vrf", "acl"} {
		body[k] = cfg[k]
	}
	if st, out := do("PATCH", "/nvue_v1/?rev="+id, body); st != 200 {
		t.Fatalf("PATCH /: %d %v", st, out)
	}
	if st, d := do("GET", "/nvue_v1/?diff=applied&rev="+id+"&filled=false", nil); st != 200 {
		t.Fatalf("diff: %d %v", st, d)
	} else if len(d) == 0 {
		return "", nil
	}
	st, out := do("PATCH", "/nvue_v1/revision/"+id, map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes", "ignore_fail": "ignore_fail_no"}})
	if st != 200 {
		t.Fatalf("apply: %d %v", st, out)
	}
	_, out = do("GET", "/nvue_v1/revision/"+id, nil)
	return fmt.Sprint(out["state"]), out
}

// refused are NICo's fixtures that NVUE refuses, and why: data a site cannot have, made for the
// template's own test (two VPCs with one L3 VNI).
var refused = map[string]string{
	"nvue_startup_fnn_classic_with_empty_nsg_default_deny.yaml.expected": "VNI 3109 is used twice",
}

// Every one of NICo's expected NVUE configurations applies through the REST API as NICo's agent
// applies it, and renders to files FRR, ifupdown2 and nftables take; the same configuration
// pushed again is no change.
func TestNICoConfigsApply(t *testing.T) {
	files, _ := filepath.Glob("testdata/nico/*.yaml.expected")
	if len(files) < 15 {
		t.Fatalf("%d of NICo's configurations in testdata/nico", len(files))
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			var got Files
			srv := NewServer(HBNSwitch("site-1/hall-a/r01/tray-01/bf3", "tray-01-dpu"), "secret", func(ctx context.Context, fl Files) (string, error) {
				got = fl
				return "ok", nil
			})
			hs := httptest.NewServer(srv)
			defer hs.Close()
			cfg := nicoConfig(t, f)
			state, out := nicoPush(t, hs.URL, cfg)
			if why, bad := refused[filepath.Base(f)]; bad {
				if state != "invalid" || !strings.Contains(fmt.Sprint(out["transition"]), why) {
					t.Fatalf("state %s, want invalid (%s): %v", state, why, out["transition"])
				}
				return
			}
			if state != "applied" {
				t.Fatalf("state %s: %v", state, out["transition"])
			}
			if !strings.Contains(got.Interfaces, "iface p0_if") || !strings.Contains(got.FRR, "router bgp") {
				t.Errorf("rendered:\n%s\n%s", got.Interfaces, got.FRR)
			}
			if strings.Contains(fmt.Sprint(cfg["interface"]), "acl:") && !strings.Contains(got.ACL, "jump acl_") {
				t.Errorf("ACLs not bound:\n%s", got.ACL)
			}
			if again, _ := nicoPush(t, hs.URL, cfg); again != "" {
				t.Errorf("the same configuration again: %s, want no change", again)
			}
			if testing.Verbose() && strings.Contains(f, "fnn_l3") && !strings.Contains(f, "full") {
				t.Logf("interfaces:\n%s\nfrr.conf:\n%s\nnft:\n%s", got.Interfaces, got.FRR, got.ACL)
			}
		})
	}
}
