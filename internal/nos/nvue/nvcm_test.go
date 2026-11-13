package nvue

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// NVIDIA Config Manager's own rendered configurations (testdata/nvcm-1.4.1: release 1.4.1's
// goldens, eight switches at Cumulus Linux 5.13.1, 5.14.0 and 5.16.1) against the emulated NVUE
// at each one's release (18 — DSX OS: NVIDIA Config Manager's ZTP, D80, Z7a). Each startup.yaml
// is what NVCM's boot script hands `nv config replace`: it must be valid in that release's
// grammar, and render, as Cumulus would apply it.
//
// What does not pass is NVCM's or the twin's, and said here:
//   - NVCM's 5.16.1 configurations put an SVI's VRR at interface <svi> vrr. Cumulus 5.15 moved it
//     to interface <svi> ipv4 vrr (NVIDIA's New and Changed NVUE Commands, 5.15), and nothing
//     NVIDIA publishes has the other place: the twin refuses it, as NVCM's.
//   - renderGaps: what the twin does not render yet, each named by its refusal.
var renderGaps = map[string]string{}

func TestNVCMGoldens(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "nvcm-1.4.1", "*", "*", "startup.yaml"))
	if err != nil || len(files) != 24 {
		t.Fatalf("NVCM's goldens: %d files (%v), want 24", len(files), err)
	}
	for _, f := range files {
		release := filepath.Base(filepath.Dir(f))
		name := filepath.Base(filepath.Dir(filepath.Dir(f))) + "@" + release
		t.Run(name, func(t *testing.T) {
			ver, err := ParseVersion(release)
			if err != nil {
				t.Fatal(err)
			}
			tree := nvcmStartup(t, f)
			err = ValidateAt(ver, nil, tree)
			if release == "5.16.1" && sub(tree, "interface") != nil && hasSVIVRR(tree) {
				if err == nil || !strings.Contains(err.Error(), `"vrr" is not part of the NVUE schema`) {
					t.Fatalf("NVCM's interface <svi> vrr on 5.16.1: want it refused by name, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("not valid at %s: %v", ver, err)
			}
			_, err = Render(tree, nvcmSwitch(tree, ver))
			switch gap := renderGaps[name]; {
			case gap == "" && err != nil:
				t.Fatalf("does not render: %v", err)
			case gap != "" && err == nil:
				t.Fatalf("renders, and renderGaps still says %q: take it out", gap)
			case gap != "" && !strings.Contains(err.Error(), gap):
				t.Fatalf("refused with %v, renderGaps says %q", err, gap)
			}
		})
	}
}

// hasSVIVRR is whether a tree has NVCM 5.16.1's interface <svi> vrr.
func hasSVIVRR(t Tree) bool {
	for _, name := range keys(sub(t, "interface")) {
		if sub(t, "interface", name, "vrr") != nil {
			return true
		}
	}
	return false
}

// nvcmStartup reads a startup.yaml's one set as NVUE's JSON has it (numbers as float64).
func nvcmStartup(t *testing.T, path string) Tree {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc []map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 1 || doc[0]["set"] == nil {
		t.Fatalf("a startup.yaml of one set, not %d entries", len(doc))
	}
	return asJSON(doc[0]["set"]).(Tree)
}

func asJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			x[k] = asJSON(c)
		}
	case []any:
		for i, c := range x {
			x[i] = asJSON(c)
		}
	case int:
		return float64(x)
	case bool:
		// YAML 1.1's on and off read as booleans by no YAML 1.2 parser; kept for one that does
		if x {
			return "on"
		}
		return "off"
	}
	return v
}

// nvcmSwitch is a factory switch at a release whose ports are every swp the configuration names
// (Native: breakout subports included), as NVCM's design gives the switch the ports it has.
func nvcmSwitch(tree Tree, ver Version) nos.Switch {
	sw := nos.Switch{Path: "site-1/hall-a/nvcm", Hostname: "nvcm", Build: Build(ver), Mgmt: true, Native: true,
		Factory: &nos.Factory{Install: "preinstalled"}}
	for _, name := range keys(sub(tree, "interface")) {
		if strings.HasPrefix(name, "swp") {
			sw.Panel = append(sw.Panel, nos.PanelPort{Name: name, Speed: "400G"})
		}
	}
	return sw
}

// What NVCM's in-band leaf (its tor, the DSX leaf) and oob-spine render to at 5.14.0, the release
// the DSX day-0 estate runs: eth0 in vrf mgmt with its static address and the management default
// route; the OOB VRF's loopback; VRR on the SVI with Cumulus's fabric MAC; the L3 VNI on the VLAN
// the configuration names; the OOB VRF's relay, bound to it, upstream on the L3 VNI's SVI;
// EVPN multihoming's uplinks and timer; the oob-spine's bonds and their Ethernet segments.
func TestNVCMRenders(t *testing.T) {
	render := func(dev string) Files {
		t.Helper()
		tree := nvcmStartup(t, filepath.Join("testdata", "nvcm-1.4.1", dev, "5.14.0", "startup.yaml"))
		f, err := Render(tree, nvcmSwitch(tree, Version{5, 14, 0}))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	has := func(what, got string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s has no %q:\n%s", what, w, got)
			}
		}
	}
	tor := render("a04-u44-p01-tor-01")
	has("the tor's interfaces", tor.Interfaces,
		"auto eth0\niface eth0\n    address 192.0.2.20/23\n    vrf mgmt\n",
		"auto OOB\niface OOB\n    vrf-table auto\n    address 10.78.154.0/32\n",
		"iface vlan202\n    address 10.78.156.2/22\n    vlan-raw-device br_default\n    vlan-id 202\n    vrf OOB\n    hwaddress 00:00:5e:00:01:69\n    address 10.78.156.1/22\n",
		// the relay's upstream, the L3 VNI's SVI, holds the VRF's loopback for ISC's dhcrelay
		"iface vlan4002\n    address 10.78.154.0/32\n    vlan-raw-device br_default\n    vlan-id 4002\n    vrf OOB\n")
	if n := strings.Count(tor.Interfaces, "auto mgmt\n"); n != 1 {
		t.Errorf("vrf mgmt rendered %d times", n)
	}
	has("the tor's frr.conf", tor.FRR, "vrf mgmt\n ip route 0.0.0.0/0 192.0.2.1\nexit-vrf\n",
		"evpn mh startup-delay 30\n", "interface swp49\n evpn mh uplink\nexit\n")
	if got := strings.Join(tor.Relay, "\n"); got != "OOB -4 -d -a -m discard -id vlan202 -iu vlan4002 192.0.2.8 192.0.2.9" {
		t.Errorf("the tor's relay: %q", got)
	}
	spine := render("a08-u28-p01-oobspine-01")
	has("the oob-spine's interfaces", spine.Interfaces,
		"auto bond10\niface bond10\n    bond-slaves swp2s3\n    bond-mode 802.3ad\n    bond-lacp-rate 1\n    bond-miimon 100\n    mtu 9000\n    bridge-access 203\n")
	has("the oob-spine's frr.conf", spine.FRR, "interface bond10\n evpn mh es-id 10\n evpn mh es-sys-mac 44:38:39:ff:69:0a\n evpn mh es-df-pref 50000\nexit\n")
}

// What the emulation refuses in a relay, by name.
func TestRelayRefusals(t *testing.T) {
	base := func() Tree {
		return Tree{
			"vrf":       Tree{"OOB": Tree{}},
			"interface": Tree{"vlan10": Tree{"type": "svi", "ip": Tree{"address": Tree{"10.0.0.1/24": Tree{}}, "vrf": "OOB"}}},
			"bridge":    Tree{"domain": Tree{Bridge: Tree{"vlan": Tree{"10": Tree{}}}}},
		}
	}
	sw := nos.Switch{Path: "site-1/hall-a/leaf", Hostname: "leaf", Build: Build(Version{5, 14, 0}), Native: true, Panel: []nos.PanelPort{{Name: "swp1"}}}
	for _, c := range []struct {
		name  string
		relay Tree
		want  string
	}{
		{"two groups", Tree{"OOB": Tree{"server-group": Tree{"a": Tree{"server": Tree{"10.9.0.1": Tree{}}}, "b": Tree{"server": Tree{"10.9.0.2": Tree{}}}}}}, "2 server groups"},
		{"group not defined", Tree{"OOB": Tree{"server-group": Tree{"a": Tree{"server": Tree{"10.9.0.1": Tree{}}}}, "downstream-interface": Tree{"vlan10": Tree{"server-group-name": "b"}}}}, `server-group-name "b" is not a group`},
		{"no VRF", Tree{"INBAND": Tree{"server": Tree{"10.9.0.1": Tree{}}, "interface": Tree{"vlan10": Tree{}}}}, "vrf INBAND does not exist"},
		{"other VRF's SVI", Tree{"default": Tree{"server": Tree{"10.9.0.1": Tree{}}, "interface": Tree{"vlan10": Tree{}}}}, "in vrf OOB, not default"},
		{"no L3 VNI", Tree{"OOB": Tree{"server-group": Tree{"a": Tree{"server": Tree{"10.9.0.1": Tree{}}, "upstream-interface": Tree{"vlan4002_l3": Tree{}}}}, "downstream-interface": Tree{"vlan10": Tree{"server-group-name": "a"}}}}, "vlan4002_l3: no such SVI"},
		{"no server", Tree{"OOB": Tree{"interface": Tree{"vlan10": Tree{}}}}, "no server"},
	} {
		tree := base()
		tree["service"] = Tree{"dhcp-relay": c.relay}
		if err := ValidateAt(Version{5, 14, 0}, nil, tree); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if _, err := Render(tree, sw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// NVIDIA Config Manager's deploy over REST, as its Cumulus client does it (cumulus.py at 1.4.1):
// a revision, its root deleted and the whole startup.yaml PATCHed in, the diff read both ways,
// applied with commit-confirm, awaited until its state has "ays", confirmed, then saved through
// revision/applied; and an unconfirmed apply rolled back when its window ends. Then its backup's
// read, its ZTP status, its running image and uptime, and its reboot and factory reset actions.
func TestNVCMDeployREST(t *testing.T) {
	var applied []Files
	srv := NewServer(nvcmSwitch(Tree{}, Version{5, 14, 0}), FactoryPassword, func(ctx context.Context, f Files) (string, error) {
		applied = append(applied, f)
		return "ok", nil
	})
	sw := &fakeSwitch{shadow: map[string]string{"cumulus": "cumulus:" + sha512Crypt("cumulus", "s", 5000, false) + ":19000:0:99999:7:::"},
		groups: map[string]string{"cumulus": "cumulus sudo nvshow nvset nvapply"}}
	srv.SetRunner(sw.run)
	var reboots, resets atomic.Int32
	srv.SetActions(Actions{
		Reboot:       func(context.Context) error { reboots.Add(1); return nil },
		FactoryReset: func(context.Context) error { resets.Add(1); return nil },
		ZTPStatus:    func(context.Context) (string, error) { return "success", nil },
	})
	if err := srv.ApplyFactory(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := caller{t, ts}
	auth := basic("cumulus", "cumulus")
	cfg := Tree{"system": Tree{"hostname": "spine-1"}, "interface": Tree{"lo": Tree{"type": "loopback", "ip": Tree{"address": Tree{"10.254.254.16/32": Tree{}}}}}}

	revision := func() string {
		t.Helper()
		st, out := c.do("POST", "/nvue_v1/revision", nil, auth)
		if st != 200 || len(out) != 1 {
			t.Fatalf("POST revision: %d %v", st, out)
		}
		for k := range out {
			return k
		}
		return ""
	}
	load := func(rev string) {
		t.Helper()
		if st, out := c.do("DELETE", "/nvue_v1/?rev="+rev, nil, auth); st != 200 {
			t.Fatalf("DELETE root: %d %v", st, out)
		}
		if st, out := c.do("PATCH", "/nvue_v1/?rev="+rev, cfg, auth); st != 200 {
			t.Fatalf("PATCH root: %d %v", st, out)
		}
	}
	rev := revision()
	load(rev)
	if st, out := c.do("GET", "/nvue_v1/?rev="+rev+"&diff=applied&filled=false", nil, auth); st != 200 || out["system"] == nil {
		t.Fatalf("diff: %d %v", st, out)
	}
	confirm := map[string]any{"state": "apply", "auto-prompt": map[string]any{"confirm": "confirm_yes", "ays": "ays_yes"}, "state-controls": map[string]any{"confirm": 300}}
	if st, out := c.do("PATCH", "/nvue_v1/revision/"+rev, confirm, auth); st != 200 || !strings.Contains(fmt.Sprint(out["state"]), "ays") {
		t.Fatalf("apply with confirm: %d %v", st, out)
	}
	if st, out := c.do("PATCH", "/nvue_v1/revision/"+rev, map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes"}}, auth); st != 200 || out["state"] != "applied" {
		t.Fatalf("confirm: %d %v", st, out)
	}
	if st, out := c.do("GET", "/nvue_v1/revision/applied", nil, auth); st != 200 || out["state"] != "applied" {
		t.Fatalf("applied before a save: %d %v", st, out)
	}
	if st, out := c.do("PATCH", "/nvue_v1/revision/applied", map[string]any{"state": "save", "auto-prompt": map[string]any{"ays": "ays_yes"}}, auth); st != 200 {
		t.Fatalf("save: %d %v", st, out)
	}
	if st, out := c.do("GET", "/nvue_v1/revision/applied", nil, auth); st != 200 || out["state"] != "applied_and_saved" {
		t.Fatalf("applied after a save: %d %v", st, out)
	}
	if st, out := c.do("GET", "/nvue_v1/?rev=applied&filled=false", nil, auth); st != 200 || str(out, "system", "hostname") != "spine-1" {
		t.Fatalf("the backup's read: %d %v", st, out)
	}

	// an unconfirmed apply: back to spine-1 when its window ends
	rev = revision()
	if st, out := c.do("PATCH", "/nvue_v1/?rev="+rev, Tree{"system": Tree{"hostname": "spine-9"}}, auth); st != 200 {
		t.Fatalf("PATCH: %d %v", st, out)
	}
	short := map[string]any{"state": "apply", "state-controls": map[string]any{"confirm": 1}}
	if st, out := c.do("PATCH", "/nvue_v1/revision/"+rev, short, auth); st != 200 || out["state"] != "ays" {
		t.Fatalf("apply with a 1 s window: %d %v", st, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, out := c.do("GET", "/nvue_v1/revision/"+rev, nil, auth)
		if st == 200 && out["state"] == "confirm_fail" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rollback: %v", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st, out := c.do("GET", "/nvue_v1/system", nil, auth); st != 200 || out["hostname"] != "spine-1" || out["product-release"] != "5.14.0" || out["uptime"] == nil {
		t.Fatalf("after the rollback: %d %v", st, out)
	}
	if st, out := c.do("PATCH", "/nvue_v1/revision/"+rev, map[string]any{"state": "apply", "state-controls": map[string]any{"confirm": "x"}}, auth); st != 400 && st != 409 {
		t.Errorf("a bad window: %d %v", st, out)
	}

	if st, out := c.do("GET", "/nvue_v1/system/ztp", nil, auth); st != 200 || out["status"] != "success" {
		t.Errorf("system/ztp: %d %v", st, out)
	}
	if st, out := c.do("POST", "/nvue_v1/system/factory-default", map[string]any{"@reset": map[string]any{"state": "start", "parameters": map[string]any{"force": true}}}, auth); st != 200 {
		t.Errorf("@reset: %d %v", st, out)
	}
	if st, out := c.do("POST", "/nvue_v1/system", map[string]any{"@reboot": map[string]any{"state": "start", "parameters": map[string]any{"no-confirm": true}}}, auth); st != 200 {
		t.Errorf("@reboot: %d %v", st, out)
	}
	if st, _ := c.do("POST", "/nvue_v1/system", map[string]any{"@shutdown": map[string]any{}}, auth); st != 400 {
		t.Errorf("an action not emulated: %d", st)
	}
	time.Sleep(500 * time.Millisecond)
	if reboots.Load() != 1 || resets.Load() != 1 {
		t.Errorf("actions carried out: %d reboots, %d resets", reboots.Load(), resets.Load())
	}
}

// A change's event says how it spelt its switches and how the switch holds them (Q30), so a
// day-0 tool's test can assert it handles both.
func TestChangeSpelling(t *testing.T) {
	for _, c := range []struct {
		ver  Version
		body Tree
		want string
	}{
		{Version{5, 16, 1}, Tree{"enable": "on", "autonomous-system": 65000.0}, "enable->state"},
		{Version{5, 16, 1}, Tree{"state": "enabled"}, "state"},
		{Version{5, 14, 0}, Tree{"state": "enabled"}, "state->enable"},
		{Version{5, 14, 0}, Tree{"enable": "on"}, "enable"},
		{Version{5, 14, 0}, Tree{"autonomous-system": 65000.0}, ""},
	} {
		srv := NewServer(nvcmSwitch(Tree{}, c.ver), FactoryPassword, func(context.Context, Files) (string, error) { return "ok", nil })
		sw := &fakeSwitch{shadow: map[string]string{"cumulus": "cumulus:" + sha512Crypt("cumulus", "s", 5000, false) + ":19000:0:99999:7:::"},
			groups: map[string]string{"cumulus": "cumulus sudo nvshow nvset nvapply"}}
		srv.SetRunner(sw.run)
		var got []Event
		srv.OnWrite(func(e Event) { got = append(got, e) })
		if err := srv.ApplyFactory(context.Background()); err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(srv)
		cl := caller{t, ts}
		auth := basic("cumulus", "cumulus")
		_, out := cl.do("POST", "/nvue_v1/revision", nil, auth)
		var rev string
		for k := range out {
			rev = k
		}
		if st, out := cl.do("PATCH", "/nvue_v1/router/bgp?rev="+rev, c.body, auth); st != 200 {
			t.Fatalf("%v %v: %d %v", c.ver, c.body, st, out)
		}
		ts.Close()
		last := got[len(got)-1]
		if last.Op != "revision.change" || last.Spelling != c.want {
			t.Errorf("%v %v: %s %q, want %q", c.ver, c.body, last.Op, last.Spelling, c.want)
		}
	}
}

// A relay in the plain form whose interfaces are an SVI and the VRF's L3 VNI SVI (Foundry's BMC
// relay, 18 — Z7d): the L3 VNI SVI is its upstream, and with no VRF loopback it holds the switch's
// own loopback for ISC's dhcrelay.
func TestRelayPlainFormL3VNI(t *testing.T) {
	tree := nvcmStartup(t, filepath.Join("testdata", "nvcm-1.4.1", "a04-u44-p01-tor-01", "5.14.0", "startup.yaml"))
	Delete(tree, []string{"service", "dhcp-relay"})
	Delete(tree, []string{"vrf", "OOB", "loopback"})
	Patch(tree, []string{"service", "dhcp-relay", "OOB"}, map[string]any{
		"server":    map[string]any{"10.90.2.10": map[string]any{}},
		"interface": map[string]any{"vlan202": map[string]any{}, "vlan4002": map[string]any{}}})
	f, err := Render(tree, nvcmSwitch(tree, Version{5, 14, 0}))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Relay) != 1 || !strings.Contains(f.Relay[0], "-id vlan202") || !strings.Contains(f.Relay[0], "-iu vlan4002") {
		t.Errorf("relay: %q", f.Relay)
	}
	lo := keys(sub(tree, "interface", "lo", "ip", "address"))
	if len(lo) == 0 || !strings.Contains(f.Interfaces, "iface vlan4002\n    address "+lo[0]+"\n") {
		t.Errorf("vlan4002 without the switch's loopback %v:\n%s", lo, f.Interfaces)
	}
}
