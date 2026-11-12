package nvue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// a rack's management switch: 48x1G, 4x10G uplinks, two BMCs, one uplink to the aggregation
func leaf() nos.Switch {
	sw := nos.Switch{Path: "site-1/hall-a/r01/mgmt-switch", Hostname: "r01-mgmt-switch", ASN: 65101,
		RouterID: netip.MustParseAddr("10.255.255.253"), Loopback: netip.MustParseAddr("10.255.255.253")}
	for i := 1; i <= 48; i++ {
		sw.Panel = append(sw.Panel, nos.PanelPort{Name: fmt.Sprintf("eth%d", i), Speed: "1G"})
	}
	for i := 1; i <= 4; i++ {
		sw.Panel = append(sw.Panel, nos.PanelPort{Name: fmt.Sprintf("up%d", i), Speed: "10G"})
	}
	sw.Vlans = []nos.Vlan{{Name: "Vlan1000", Network: "oob", Rack: "site-1/hall-a/r01", Gateway: netip.MustParsePrefix("10.30.0.1/24"),
		Ports: []string{"eth1", "eth4"}, Relay: []netip.Addr{netip.MustParseAddr("10.90.0.10")}}}
	sw.Routed = []nos.Routed{{Port: "up1", Addr: netip.MustParsePrefix("10.255.0.0/31"), Peer: netip.MustParseAddr("10.255.0.1"), PeerASN: 65000, PeerName: "oob-agg-1:eth1"}}
	return sw
}

func TestPortNames(t *testing.T) {
	sw := leaf()
	if Port(sw, "eth1") != "swp1" || Port(sw, "up1") != "swp49" || Port(sw, "mgmt0") != "" {
		t.Errorf("ports: eth1 %s up1 %s", Port(sw, "eth1"), Port(sw, "up1"))
	}
	e, _ := nos.Lookup(nos.NVUEFRR)
	if e.Interface(sw, "up1") != "swp49" || e.API != "nvue" || !e.Runnable || !e.RunsOn("aarch64") || !e.RunsOn("x86_64") {
		t.Errorf("registry: %+v", e)
	}
}

// The model's configuration, as `nv set` would make it, renders to Cumulus's files.
func TestStartupRenders(t *testing.T) {
	sw := leaf()
	tree, err := Startup(sw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(tree, "", "  ")
	t.Logf("startup:\n%s", b)
	for path, want := range map[string]string{
		"system/hostname": "r01-mgmt-switch",
		"interface/swp1/bridge/domain/br_default/access":       "1000",
		"interface/swp49/description":                          "oob-agg-1:eth1",
		"router/bgp/autonomous-system":                         "65101",
		"vrf/default/router/bgp/neighbor/10.255.0.1/remote-as": "65000",
	} {
		v, ok := Get(tree, strings.Split(path, "/"))
		if !ok || str(v) != want {
			t.Errorf("%s = %v, want %s", path, v, want)
		}
	}
	// address keys hold a slash: these paths are given as segments
	for _, path := range [][]string{{"interface", "lo", "ip", "address", "10.255.255.253/32"}, {"interface", "vlan1000", "ip", "address", "10.30.0.1/24"},
		{"interface", "swp49", "ip", "address", "10.255.0.0/31"}, {"bridge", "domain", "br_default", "vlan", "1000"}, {"service", "dhcp-relay", "default", "server", "10.90.0.10"},
		{"service", "dhcp-relay", "default", "interface", "vlan1000"}, {"service", "dhcp-relay", "default", "interface", "swp49"}} {
		if _, ok := Get(tree, path); !ok {
			t.Errorf("startup lacks %s", strings.Join(path, " "))
		}
	}
	f, err := Render(tree, sw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("interfaces:\n%s\nfrr.conf:\n%s\nrelay: %v", f.Interfaces, f.FRR, f.Relay)
	for _, want := range []string{
		"auto lo\niface lo inet loopback\n    address 10.255.255.253/32\n",
		"auto swp1\niface swp1\n    bridge-access 1000\n",
		"auto swp49\niface swp49\n    alias oob-agg-1:eth1\n    address 10.255.0.0/31\n",
		"auto br_default\niface br_default\n    bridge-ports swp1 swp4\n    bridge-vlan-aware yes\n    bridge-vids 1000\n",
		"auto vlan1000\niface vlan1000\n    address 10.30.0.1/24\n    vlan-raw-device br_default\n    vlan-id 1000\n",
	} {
		if !strings.Contains(f.Interfaces, want) {
			t.Errorf("interfaces lack %q", want)
		}
	}
	for _, want := range []string{"hostname r01-mgmt-switch\n", "router bgp 65101\n bgp router-id 10.255.255.253\n", " neighbor 10.255.0.1 remote-as 65000\n",
		"  redistribute connected\n  neighbor 10.255.0.1 activate\n"} {
		if !strings.Contains(f.FRR, want) {
			t.Errorf("frr.conf lacks %q", want)
		}
	}
	if strings.Contains(f.FRR, "l2vpn evpn") {
		t.Errorf("EVPN without evpn enable")
	}
	if got := strings.Join(f.Relay, " "); got != "-4 -d -a -m discard -id vlan1000 -iu swp49 10.90.0.10" {
		t.Errorf("relay %q", got)
	}
}

// A tenant: an L2 VNI on a VLAN with its SVI in a VRF, the VRF's L3 VNI, EVPN to the spine.
func TestTenantRenders(t *testing.T) {
	sw := leaf()
	tree, _ := Startup(sw)
	patch := map[string]any{
		"vrf": map[string]any{
			"tenant-a": map[string]any{"evpn": map[string]any{"vni": map[string]any{"104001": map[string]any{}}},
				"router": map[string]any{"bgp": map[string]any{"enable": "on", "address-family": map[string]any{"ipv4-unicast": map[string]any{"redistribute": map[string]any{"connected": map[string]any{"enable": "on"}}}}}}},
			"default": map[string]any{"router": map[string]any{"bgp": map[string]any{"neighbor": map[string]any{"10.255.0.1": map[string]any{"address-family": map[string]any{"l2vpn-evpn": map[string]any{"enable": "on"}}}}}}},
		},
		"bridge":    map[string]any{"domain": map[string]any{"br_default": map[string]any{"vlan": map[string]any{"2001": map[string]any{"vni": map[string]any{"10001": map[string]any{}}}}}}},
		"interface": map[string]any{"vlan2001": map[string]any{"ip": map[string]any{"address": map[string]any{"10.40.1.1/24": map[string]any{}}, "vrf": "tenant-a"}}, "swp4": map[string]any{"bridge": map[string]any{"domain": map[string]any{"br_default": map[string]any{"access": 2001}}}}},
		"nve":       map[string]any{"vxlan": map[string]any{"enable": "on", "source": map[string]any{"address": "10.255.255.253"}, "arp-nd-suppress": "on"}},
		"evpn":      map[string]any{"enable": "on"},
	}
	if err := Validate(nil, patch); err != nil {
		t.Fatal(err)
	}
	Patch(tree, nil, patch)
	f, err := Render(tree, sw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("interfaces:\n%s\nfrr.conf:\n%s", f.Interfaces, f.FRR)
	for _, want := range []string{
		"auto vni10001\niface vni10001\n    vxlan-id 10001\n    vxlan-local-tunnelip 10.255.255.253\n    bridge-access 2001\n    bridge-learning off\n    bridge-arp-nd-suppress on\n",
		"auto vni104001\niface vni104001\n    vxlan-id 104001\n    vxlan-local-tunnelip 10.255.255.253\n    bridge-access 4024\n",
		"auto tenant-a\niface tenant-a\n    vrf-table auto\n",
		"    bridge-ports swp1 swp4 vni10001 vni104001\n    bridge-vlan-aware yes\n    bridge-vids 1000 2001 4024\n",
		"auto swp4\niface swp4\n    bridge-access 2001\n",
		"auto vlan2001\niface vlan2001\n    address 10.40.1.1/24\n    vlan-raw-device br_default\n    vlan-id 2001\n    vrf tenant-a\n",
		"auto vlan4024\niface vlan4024\n    vlan-raw-device br_default\n    vlan-id 4024\n    vrf tenant-a\n",
	} {
		if !strings.Contains(f.Interfaces, want) {
			t.Errorf("interfaces lack %q", want)
		}
	}
	for _, want := range []string{"vrf tenant-a\n vni 104001\nexit-vrf\n",
		" address-family l2vpn evpn\n  neighbor 10.255.0.1 activate\n  advertise-all-vni\n",
		"router bgp 65101 vrf tenant-a\n", "  advertise ipv4 unicast\n"} {
		if !strings.Contains(f.FRR, want) {
			t.Errorf("frr.conf lacks %q", want)
		}
	}
	// the same configuration without the NVE's source address is refused, as NVUE refuses it
	bad := Clone(tree)
	Delete(bad, []string{"nve", "vxlan", "source"})
	if _, err := Render(bad, sw); err == nil || !strings.Contains(err.Error(), "source address") {
		t.Errorf("a VNI without a source address: %v", err)
	}
	bad = Clone(tree)
	Patch(bad, []string{"interface", "vlan2001", "ip", "vrf"}, "tenant-b")
	if _, err := Render(bad, sw); err == nil || !strings.Contains(err.Error(), "vrf tenant-b does not exist") {
		t.Errorf("an SVI in a missing VRF: %v", err)
	}
	bad = Clone(tree)
	Patch(bad, []string{"interface", "swp30", "ip", "address", "10.9.9.1/24"}, map[string]any{})
	if _, err := Render(bad, sw); err == nil || !strings.Contains(err.Error(), "no such port") {
		t.Errorf("an uncabled port: %v", err)
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		path []string
		v    any
		ok   bool
	}{
		{[]string{"interface", "swp1", "ip", "address"}, map[string]any{"10.0.0.1/24": map[string]any{}}, true},
		{[]string{"interface", "swp1", "ip", "address"}, map[string]any{"10.0.0.1": map[string]any{}}, false},
		{[]string{"evpn", "enable"}, "maybe", false},
		{[]string{"router", "ospf"}, map[string]any{"enable": "on"}, false},
		{[]string{"vrf", "red", "router", "bgp", "neighbor", "10.0.0.2", "remote-as"}, "external", true},
		{[]string{"vrf", "red", "router", "bgp", "neighbor", "swp51"}, map[string]any{"type": "unnumbered"}, true},
		{[]string{"vrf", "red", "router", "bgp", "neighbor", "not/a-peer"}, map[string]any{}, false},
		{nil, map[string]any{"mlag": map[string]any{}}, false}, // qos is emulated since day 0 (qos roce)
		{[]string{"bridge", "domain", "br_default", "vlan", "10", "vni"}, map[string]any{"10": nil}, true},
	} {
		err := Validate(c.path, c.v)
		if (err == nil) != c.ok {
			t.Errorf("%v %v: %v", c.path, c.v, err)
		}
	}
}

// ApplyScript is shell the switch's /bin/sh parses.
func TestApplyScriptParses(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "-c", ApplyScript).CombinedOutput(); err != nil {
		t.Errorf("%v %s", err, out)
	}
	e, _ := nos.Lookup(nos.NVUEFRR)
	s, err := e.StartScript(leaf())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil || !strings.Contains(s, `want="swp1 swp4 swp49"`) {
		t.Errorf("start script: %v %s\n%s", err, out, s)
	}
}

// The REST flow a client runs: create a revision, patch it, apply it, read the result.
func TestServer(t *testing.T) {
	var applied []Files
	fail := false
	srv := NewServer(leaf(), "secret", func(ctx context.Context, f Files) (string, error) {
		if fail {
			return "error: tenant-a: create failed", errors.New("exit 1")
		}
		applied = append(applied, f)
		return "ok", nil
	})
	if err := srv.ApplyStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	do := func(method, path string, body any, auth bool) (int, map[string]any) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		if auth {
			req.SetBasicAuth(User, "secret")
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
	if st, _ := do("GET", "/nvue_v1/system", nil, false); st != http.StatusUnauthorized {
		t.Errorf("no credentials: %d", st)
	}
	if st, out := do("GET", "/nvue_v1/system", nil, true); st != 200 || out["hostname"] != "r01-mgmt-switch" {
		t.Errorf("GET system: %d %v", st, out)
	}
	st, out := do("POST", "/nvue_v1/revision", nil, true)
	var rev string
	for id := range out {
		rev = id
	}
	if st != 200 || rev == "" {
		t.Fatalf("POST revision: %d %v", st, out)
	}
	if st, out := do("PATCH", "/nvue_v1/bridge/domain/br_default/vlan/2001?rev="+rev, map[string]any{"vni": map[string]any{"10001": map[string]any{}}}, true); st != 200 {
		t.Errorf("PATCH vlan: %d %v", st, out)
	}
	if st, _ := do("PATCH", "/nvue_v1/nve/vxlan/source?rev="+rev, map[string]any{"address": "10.255.255.253"}, true); st != 200 {
		t.Errorf("PATCH nve: %d", st)
	}
	if st, out := do("PATCH", "/nvue_v1/router/ospf?rev="+rev, map[string]any{"enable": "on"}, true); st != 400 || !strings.Contains(fmt.Sprint(out["detail"]), "not part of the NVUE schema") {
		t.Errorf("PATCH an unemulated path: %d %v", st, out)
	}
	if st, _ := do("PATCH", "/nvue_v1/evpn?rev=99", map[string]any{"enable": "on"}, true); st != 404 {
		t.Errorf("PATCH an unknown revision: %d", st)
	}
	// the change is in the revision, not applied
	if _, out := do("GET", "/nvue_v1/bridge/domain/br_default/vlan?rev="+rev, nil, true); out["2001"] == nil {
		t.Errorf("the revision lacks vlan 2001: %v", out)
	}
	if _, out := do("GET", "/nvue_v1/bridge/domain/br_default/vlan", nil, true); out["2001"] != nil {
		t.Errorf("applied before apply: %v", out)
	}
	if st, out := do("PATCH", "/nvue_v1/revision/"+rev, map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes"}}, true); st != 200 || out["state"] != "applied" {
		t.Fatalf("apply: %d %v", st, out)
	}
	if len(applied) != 2 || !strings.Contains(applied[1].Interfaces, "vxlan-id 10001") {
		t.Errorf("applied %d times; last %v", len(applied), applied)
	}
	if _, out := do("GET", "/nvue_v1/bridge/domain/br_default/vlan/2001/vni", nil, true); out["10001"] == nil {
		t.Errorf("not applied: %v", out)
	}
	if st, _ := do("PATCH", "/nvue_v1/evpn?rev="+rev, map[string]any{"enable": "on"}, true); st != http.StatusConflict {
		t.Errorf("PATCH an applied revision: %d", st)
	}
	// an invalid revision is refused before the switch is touched; a failed apply says why
	_, out = do("POST", "/nvue_v1/revision", nil, true)
	for id := range out {
		rev = id
	}
	do("DELETE", "/nvue_v1/nve/vxlan/source?rev="+rev, nil, true)
	if _, out := do("PATCH", "/nvue_v1/revision/"+rev, map[string]any{"state": "apply"}, true); out["state"] != "invalid" || len(applied) != 2 {
		t.Errorf("invalid revision: %v", out)
	}
	_, out = do("POST", "/nvue_v1/revision", nil, true)
	for id := range out {
		rev = id
	}
	do("PATCH", "/nvue_v1/vrf/tenant-a?rev="+rev, map[string]any{"evpn": map[string]any{"vni": map[string]any{"104001": map[string]any{}}}}, true)
	fail = true
	_, out = do("PATCH", "/nvue_v1/revision/"+rev, map[string]any{"state": "apply"}, true)
	if out["state"] != "apply_fail" || !strings.Contains(fmt.Sprint(out["transition"]), "exit 1") {
		t.Errorf("failed apply: %v", out)
	}
	// its files are what the switch has: the applied configuration, which later revisions build on
	if _, out := do("GET", "/nvue_v1/vrf", nil, true); out["tenant-a"] == nil {
		t.Errorf("a failed apply is not the applied configuration: %v", out)
	}
	fail = false

	// two revisions open at once: each applies its own changes onto what is applied then, so the
	// second does not undo the first
	newRev := func() string {
		_, out := do("POST", "/nvue_v1/revision", nil, true)
		for id := range out {
			return id
		}
		return ""
	}
	ra, rb := newRev(), newRev()
	do("PATCH", "/nvue_v1/bridge/domain/br_default/vlan/2002?rev="+ra, map[string]any{"vni": map[string]any{"10002": map[string]any{}}}, true)
	do("PATCH", "/nvue_v1/bridge/domain/br_default/vlan/2003?rev="+rb, map[string]any{"vni": map[string]any{"10003": map[string]any{}}}, true)
	for _, id := range []string{ra, rb} {
		if _, out := do("PATCH", "/nvue_v1/revision/"+id, map[string]any{"state": "apply"}, true); out["state"] != "applied" {
			t.Fatalf("apply %s: %v", id, out)
		}
	}
	if _, out := do("GET", "/nvue_v1/bridge/domain/br_default/vlan", nil, true); out["2002"] == nil || out["2003"] == nil {
		t.Errorf("the second revision undid the first: %v", out)
	}
	if _, out := do("GET", "/nvue_v1/system?rev=startup", nil, true); out["hostname"] != "r01-mgmt-switch" {
		t.Errorf("startup: %v", out)
	}
}

// An apply's frr-reload output keeps every error it printed, however early, ahead of its last
// lines: the first error is what failed the batch, and frr-reload's line-by-line retry after it
// prints far more than the tail keeps.
func TestReloadSummaryKeepsTheFirstError(t *testing.T) {
	var in strings.Builder
	in.WriteString("Lines to add:\n% Unknown command: vni 104001\nline 3: Failure to communicate[13] to zebra, line: vni 104001\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&in, "retry line %d\n", i)
	}
	cmd := exec.Command("sh", "-c", "printf '%s' \"$1\" | "+ReloadSummary, "sh", in.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	s := string(out)
	for _, want := range []string{"% Unknown command: vni 104001", "Failure to communicate[13] to zebra", "retry line 29"} {
		if !strings.Contains(s, want) {
			t.Errorf("the summary lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "retry line 5\n") {
		t.Errorf("the summary keeps more than the tail:\n%s", s)
	}
}

// The VRFs' stanzas (each VRF's L3 VNI) are what an apply gives vtysh before frr-reload: only
// the vrf ... exit-vrf blocks of the rendered frr.conf, whole, and nothing of router bgp.
func TestVRFStanzas(t *testing.T) {
	sw := leaf()
	tree, _ := Startup(sw)
	Patch(tree, nil, map[string]any{
		"vrf": map[string]any{
			"tenant-a": map[string]any{"evpn": map[string]any{"vni": map[string]any{"104001": map[string]any{}}},
				"router": map[string]any{"bgp": map[string]any{"enable": "on"}}},
			"tenant-b": map[string]any{"evpn": map[string]any{"vni": map[string]any{"104002": map[string]any{}}}},
		},
		"nve":  map[string]any{"vxlan": map[string]any{"enable": "on", "source": map[string]any{"address": "10.255.255.253"}}},
		"evpn": map[string]any{"enable": "on"},
	})
	f, err := Render(tree, sw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", "printf '%s' \"$1\" | "+VRFStanzas, "sh", f.FRR).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if want := "vrf tenant-a\n vni 104001\nexit-vrf\nvrf tenant-b\n vni 104002\nexit-vrf\n"; string(out) != want {
		t.Errorf("stanzas:\n%s\nwant:\n%s", out, want)
	}
	if !strings.Contains(ApplyScript, VRFStanzas) || strings.Index(ApplyScript, "vtysh -f") > strings.Index(ApplyScript, "frr-reload.py --reload") {
		t.Error("the apply does not give vtysh the VRFs' stanzas before frr-reload")
	}
}

// A VLAN that carries another VNI after a revision (a VRF given another L3 VNI on the same
// reserved VLAN) has its SVI and VXLAN devices made again.
func TestRecreateOnVNIChange(t *testing.T) {
	old := Files{Interfaces: "iface vni5002\n    vxlan-id 5002\n    bridge-access 4024\n\niface vlan4024\n    vlan-id 4024\n"}
	new := Files{Interfaces: "iface vni5001\n    vxlan-id 5001\n    bridge-access 4024\n\niface vlan4024\n    vlan-id 4024\n"}
	if got := strings.Join(recreate(old, new), " "); got != "vlan4024 vni5001 vni5002" {
		t.Errorf("recreate: %q", got)
	}
	if got := recreate(new, new); len(got) != 0 {
		t.Errorf("recreate with no change: %v", got)
	}
}
