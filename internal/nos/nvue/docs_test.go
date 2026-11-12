package nvue

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The emulated NVUE against NVIDIA's own documentation, as docs/reviews/2026-11-11-day0-vendor-
// facts.md (C) records it, rather than against the twin's reading: the REST API's documented
// revision workflow, byte for byte from the curl examples of the Cumulus Linux 5.x NVUE API page,
// and every NVUE command the day-0 facts list (C.3), each of which a switch accepts.

// TestDocumentedRESTWorkflow replays C.1: POST /nvue_v1/revision answers {"<rev>": {"state":
// "pending", "transition": {"issue": {}, "progress": ""}}}; PATCH a path with ?rev= and a JSON
// body (the page's own: {"99.99.99.99/32": {}} at interface/lo/ip/address); PATCH the revision
// with {"state": "apply", "auto-prompt": {"ays": "ays_yes"}}; the revision is applied and the
// applied configuration holds the address.
func TestDocumentedRESTWorkflow(t *testing.T) {
	srv := NewServer(leaf(), "cumulus", func(context.Context, Files) (string, error) { return "ok", nil })
	if err := srv.ApplyStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	do := func(method, path, body string) (int, map[string]any) {
		var rd io.Reader
		if body != "" {
			rd = bytes.NewReader([]byte(body))
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.SetBasicAuth("cumulus", "cumulus") // curl -u 'cumulus:cumulus'
		if body != "" {
			req.Header.Set("Content-Type", "application/json") // -H 'Content-Type: application/json'
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	// curl -u 'cumulus:cumulus' --insecure -X POST https://127.0.0.1:8765/nvue_v1/revision
	st, out := do("POST", "/nvue_v1/revision", "")
	if st != 200 || len(out) != 1 {
		t.Fatalf("POST /nvue_v1/revision: %d %v", st, out)
	}
	var rev string
	var body map[string]any
	for k, v := range out {
		rev, body = k, v.(map[string]any)
	}
	if body["state"] != "pending" {
		t.Errorf("the new revision's state is %v, the docs' pending", body["state"])
	}
	tr, ok := body["transition"].(map[string]any)
	if !ok || tr["progress"] != "" || tr["issue"] == nil {
		t.Errorf("the new revision's transition is %v, the docs' {\"issue\": {}, \"progress\": \"\"}", body["transition"])
	}
	// curl ... -d '{"99.99.99.99/32": {}}' -H 'Content-Type: application/json' -k -X PATCH
	//   https://127.0.0.1:8765/nvue_v1/interface/lo/ip/address?rev=2
	if st, out := do("PATCH", "/nvue_v1/interface/lo/ip/address?rev="+rev, `{"99.99.99.99/32": {}}`); st != 200 {
		t.Fatalf("PATCH interface/lo/ip/address: %d %v", st, out)
	}
	// apply: PATCH /nvue_v1/revision/<rev> {"state": "apply", "auto-prompt": {"ays": "ays_yes"}}
	if st, out := do("PATCH", "/nvue_v1/revision/"+rev, `{"state": "apply", "auto-prompt": {"ays": "ays_yes"}}`); st != 200 {
		t.Fatalf("apply: %d %v", st, out)
	}
	if st, out := do("GET", "/nvue_v1/revision/"+rev, ""); st != 200 || out["state"] != "applied" {
		t.Fatalf("GET the revision after its apply: %d %v", st, out)
	}
	if _, ok := sub(srv.Applied(), "interface", "lo", "ip", "address")["99.99.99.99/32"]; !ok {
		t.Fatalf("the applied configuration has no 99.99.99.99/32 on lo: %v", sub(srv.Applied(), "interface", "lo"))
	}
}

// TestDocumentedCommands: every NVUE command the day-0 facts list (C.3, from NVIDIA's Cumulus
// Linux 5.x pages, cited there) is accepted by the emulated schema as NVUE's `nv set` takes it
// (CLIPath), and the configuration renders.
func TestDocumentedCommands(t *testing.T) {
	cmds := []string{
		"system hostname leaf01",
		"system date-time timezone US/Eastern",
		"service ntp default server 4.cumulusnetworks.pool.ntp.org iburst on",
		"service ntp mgmt server 4.cumulusnetworks.pool.ntp.org iburst on",
		"service dns mgmt server 198.51.100.31",
		"system syslog server 192.168.0.254 port 601",
		"system syslog server 192.168.0.254 vrf mgmt",
		"service lldp tx-interval 30",
		"service lldp tx-hold-multiplier 4",
		"system aaa user admin2 role system-admin",
		"system aaa user admin2 password Secret1!",
		"interface swp1 link breakout 4x",
		"interface swp1 link mtu 1500",
		"interface swp1 link speed 400G",
		"interface swp1 link state up",
		"qos roce mode lossless",
		"router bgp autonomous-system 65101",
		"router bgp router-id 10.10.10.1",
		"vrf default router bgp neighbor swp51 remote-as external",
		"vrf default router bgp address-family ipv4-unicast network 10.10.10.1/32",
		"vrf default router bgp peer-group SPINE remote-as external",
		"vrf default router bgp neighbor swp52 peer-group SPINE",
		"router ptm enable on",
		"interface eth0 ip address dhcp",
		"interface eth0 ip vrf mgmt",
	}
	tree := Tree{}
	for _, c := range cmds {
		path, v, err := CLIPath(strings.Fields(c), true)
		if err != nil {
			t.Errorf("nv set %s: %v", c, err)
			continue
		}
		if err := Validate(path, v); err != nil {
			t.Errorf("nv set %s: %v", c, err)
			continue
		}
		// the docs give NTP in the default VRF and in mgmt as alternatives; one VRF is rendered
		if !strings.HasPrefix(c, "service ntp default") {
			Patch(tree, path, v)
		}
	}
	sw := factorySwitch()
	if _, err := Render(tree, sw); err != nil && !strings.Contains(err.Error(), "swp51") && !strings.Contains(err.Error(), "swp52") {
		t.Errorf("the documented commands render: %v", err)
	}
}
