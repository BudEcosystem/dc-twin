package nvcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// a factory leaf: four cabled ports (swp1 to swp4) and its management port
func factoryLeaf() nos.Switch {
	sw := nos.Switch{Path: "site-1/r01/leaf-1", Hostname: "r01-leaf-1", Factory: &nos.Factory{Install: "preinstalled"}, Mgmt: true}
	for i := 1; i <= 4; i++ {
		sw.Panel = append(sw.Panel, nos.PanelPort{Name: fmt.Sprintf("eth%d", i), Speed: "400G"})
		sw.Cabled = append(sw.Cabled, fmt.Sprintf("eth%d", i))
	}
	return sw
}

type fixture struct {
	srv     *nvue.Server
	ts      *httptest.Server
	mu      sync.Mutex
	applied []nvue.Files
	events  []nvue.Event
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.srv = nvue.NewServer(factoryLeaf(), nvue.FactoryPassword, func(ctx context.Context, files nvue.Files) (string, error) {
		f.mu.Lock()
		f.applied = append(f.applied, files)
		f.mu.Unlock()
		return "ok", nil
	})
	f.srv.SetRunner(func(ctx context.Context, args ...string) (string, error) {
		if len(args) == 3 && args[0] == "id" {
			switch args[2] {
			case "cumulus", "admin2":
				return args[2] + " sudo nvshow nvset nvapply\n", nil
			case "root":
				return "root\n", nil
			case "viewer":
				return "viewer nvshow\n", nil
			}
		}
		if len(args) > 0 && args[0] == "sh" { // nv config save writing startup.yaml
			return "", nil
		}
		return "", errors.New("exit 1")
	})
	f.srv.SetLocalToken("tok")
	f.srv.OnWrite(func(e nvue.Event) { f.mu.Lock(); f.events = append(f.events, e); f.mu.Unlock() })
	if err := f.srv.ApplyFactory(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.ts = httptest.NewTLSServer(f.srv)
	t.Cleanup(f.ts.Close)
	return f
}

// nv runs a command as a user and returns its exit code, output and error output.
func (f *fixture) nv(user string, args ...string) (int, string, string) {
	var out, errw bytes.Buffer
	c := &client{base: f.ts.URL + "/nvue_v1", user: user, token: "tok", http: f.ts.Client(), out: &out, errw: &errw}
	code := c.run(args)
	return code, out.String(), errw.String()
}

func (f *fixture) must(t *testing.T, user string, args ...string) string {
	t.Helper()
	code, out, errw := f.nv(user, args...)
	if code != 0 {
		t.Fatalf("nv %s: exit %d: %s%s", strings.Join(args, " "), code, out, errw)
	}
	return out
}

func showJSON(t *testing.T, f *fixture, user string, args ...string) map[string]any {
	t.Helper()
	out := f.must(t, user, append(append([]string{"show"}, args...), "-o", "json")...)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("nv show %v -o json: %v\n%s", args, err, out)
	}
	return m
}

// A day-0 push with nv: set, review, apply, read back.
func TestSetShowApply(t *testing.T) {
	f := newFixture(t)
	for _, c := range [][]string{
		{"set", "system", "hostname", "leaf01"},
		{"set", "interface", "lo", "ip", "address", "10.10.10.1/32"},
		{"set", "interface", "swp1-2", "ip", "address", "dhcp"}, // a range; refused at render below, then unset
		{"unset", "interface", "swp1-2"},
		{"set", "interface", "swp1,swp2", "link", "state", "up"},
		{"set", "interface", "eth0", "ip", "address", "dhcp"},
		{"set", "router", "bgp", "autonomous-system", "65101"},
		{"set", "router", "bgp", "router-id", "10.10.10.1"},
		{"set", "router", "bgp", "enable", "on"},
		{"set", "vrf", "default", "router", "bgp", "enable", "on"},
		{"set", "vrf", "default", "router", "bgp", "neighbor", "swp1", "remote-as", "external"},
		{"set", "vrf", "default", "router", "bgp", "neighbor", "swp2", "remote-as", "external"},
		{"set", "system", "aaa", "user", "admin2", "password", "secret"},
		{"set", "system", "aaa", "user", "admin2", "role", "system-admin"},
	} {
		f.must(t, "cumulus", c...)
	}
	// staged, not applied
	if m := showJSON(t, f, "cumulus", "system"); m["hostname"] != nvue.FactoryHostname {
		t.Errorf("applied before apply: %v", m)
	}
	if m := showJSON(t, f, "cumulus", "system", "--rev", "pending"); m["hostname"] != "leaf01" {
		t.Errorf("pending: %v", m)
	}
	if m := showJSON(t, f, "cumulus", "router", "bgp", "--pending"); m["autonomous-system"] != 65101.0 {
		t.Errorf("pending bgp: %v", m)
	}
	// another user's view of pending is their own
	if m := showJSON(t, f, "admin2", "system", "--rev", "pending"); m["hostname"] != nvue.FactoryHostname {
		t.Errorf("admin2 sees cumulus's pending: %v", m)
	}
	diff := f.must(t, "cumulus", "config", "diff")
	for _, want := range []string{"- set:\n", "hostname: leaf01", "autonomous-system: 65101"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	cmds := f.must(t, "cumulus", "config", "diff", "-o", "commands")
	if !strings.Contains(cmds, "nv set system hostname leaf01\n") || !strings.Contains(cmds, "nv set vrf default router bgp neighbor swp1 remote-as external\n") {
		t.Errorf("diff -o commands:\n%s", cmds)
	}
	if out := f.must(t, "cumulus", "config", "apply", "-y"); !strings.HasPrefix(out, "applied [rev_id: ") {
		t.Errorf("apply: %q", out)
	}
	if m := showJSON(t, f, "cumulus", "system"); m["hostname"] != "leaf01" {
		t.Errorf("applied: %v", m)
	}
	f.mu.Lock()
	last := f.applied[len(f.applied)-1]
	f.mu.Unlock()
	for _, want := range []string{" neighbor swp1 interface remote-as external\n", "router bgp 65101\n"} {
		if !strings.Contains(last.FRR, want) {
			t.Errorf("frr.conf lacks %q:\n%s", want, last.FRR)
		}
	}
	if !strings.Contains(last.Interfaces, "auto swp3\niface swp3\n    link-down yes\n") || strings.Contains(last.Interfaces, "iface swp1\n    link-down") {
		t.Errorf("ports:\n%s", last.Interfaces)
	}
	if !strings.Contains(last.System.Users, "admin2 $6$") || strings.Contains(last.System.Users, "secret") {
		t.Errorf("users: %q", last.System.Users)
	}
	// nothing pending any more
	if out := f.must(t, "cumulus", "config", "apply", "--assume-yes"); !strings.Contains(out, "no pending changes") {
		t.Errorf("apply with nothing pending: %q", out)
	}
	// the writes were cumulus's, through nv
	f.mu.Lock()
	for _, e := range f.events {
		if e.User != "cumulus" || e.Via != "nv" {
			t.Errorf("event %+v", e)
		}
	}
	f.mu.Unlock()
	// history lists the factory configuration and cumulus's apply
	hist := f.must(t, "cumulus", "config", "history")
	if !strings.Contains(hist, "system") || !strings.Contains(hist, "cumulus") {
		t.Errorf("history:\n%s", hist)
	}
	// yaml and commands of the applied configuration
	show := f.must(t, "cumulus", "config", "show")
	if !strings.HasPrefix(show, "- header:\n") || !strings.Contains(show, "hostname: leaf01") {
		t.Errorf("config show:\n%s", show)
	}
	if c := f.must(t, "cumulus", "config", "show", "-o", "commands"); !strings.Contains(c, "nv set interface eth0 ip address dhcp\n") || !strings.Contains(c, "nv set interface eth0 ip vrf mgmt\n") {
		t.Errorf("config show -o commands:\n%s", c)
	}
}

// What the schema does not have, or nv does not do, is refused by name.
func TestRefusals(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"set", "router", "ospf", "enable", "on"}, "not part of the NVUE schema"},
		{[]string{"set", "interface", "swp1", "link", "breakout", "3y"}, "not a breakout"},
		{[]string{"action", "generate", "system", "tech-support"}, "not emulated by the twin's nv"},
		{[]string{"config", "frobnicate"}, "not emulated"},
		{[]string{"show", "interface", "swp1", "link", "frobnicate"}, "not part of the NVUE schema"},
	} {
		code, _, errw := f.nv("cumulus", c.args...)
		if code == 0 || !strings.Contains(errw, c.want) {
			t.Errorf("nv %s: %d %q, want %q", strings.Join(c.args, " "), code, errw, c.want)
		}
	}
	// a monitor may read and not stage
	if code, _, errw := f.nv("viewer", "set", "system", "hostname", "x"); code == 0 || !strings.Contains(errw, "nvset") {
		t.Errorf("viewer set: %d %s", code, errw)
	}
	if code, _, _ := f.nv("viewer", "show", "system"); code != 0 {
		t.Errorf("viewer show: %d", code)
	}
	// a revision that cannot render fails its apply, saying why
	f.must(t, "cumulus", "set", "interface", "swp1s0", "link", "state", "up")
	code, _, errw := f.nv("cumulus", "config", "apply", "-y")
	if code == 0 || !strings.Contains(errw, "invalid") || !strings.Contains(errw, "cannot split a port") {
		t.Errorf("an invalid apply: %d %s", code, errw)
	}
}

// replace and patch from files: Cumulus's startup.yaml, JSON, and nv set lines.
func TestReplaceAndPatch(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	startup := filepath.Join(dir, "startup.yaml")
	os.WriteFile(startup, []byte(`- header:
    model: VX
    nvue-api-version: nvue_v1
    rev-id: 1.0
    version: Cumulus Linux 5.6.0
- set:
    system:
      hostname: spine01
      date-time:
        timezone: US/Eastern
    interface:
      eth0:
        type: eth
        ip:
          address:
            dhcp: {}
          vrf: mgmt
      swp1:
        link:
          mtu: 9000
          state:
            up: {}
    router:
      bgp:
        autonomous-system: 65000
        enable: on
    service:
      lldp:
        tx-interval: 10
`), 0o644)
	f.must(t, "root", "config", "replace", startup)
	f.must(t, "root", "config", "apply", "--y")
	m := showJSON(t, f, "root")
	if fmt.Sprint(m["system"]) != "map[date-time:map[timezone:US/Eastern] hostname:spine01]" {
		t.Errorf("after replace: %v", m["system"])
	}
	if mtu, _ := json.Marshal(m["interface"]); !strings.Contains(string(mtu), `"mtu":9000`) {
		t.Errorf("interface: %s", mtu)
	}
	// patch: nv set lines merge into the pending revision
	cmds := filepath.Join(dir, "day0.txt")
	os.WriteFile(cmds, []byte("# day 0\nnv set system hostname 'spine01x'\nnv unset service lldp\nnv set interface swp2-3 link state up\n"), 0o644)
	f.mu.Lock()
	before := len(f.events)
	f.mu.Unlock()
	f.must(t, "root", "config", "patch", cmds)
	f.mu.Lock()
	changes := 0
	for _, e := range f.events[before:] {
		if e.Op == "revision.change" {
			changes++
		}
	}
	f.mu.Unlock()
	if changes != 1 {
		t.Errorf("a patch file made %d changes to the revision, not one", changes)
	}
	f.must(t, "root", "config", "apply", "-y")
	m = showJSON(t, f, "root")
	if fmt.Sprint(m["system"]) != "map[date-time:map[timezone:US/Eastern] hostname:spine01x]" {
		t.Errorf("after patch: %v", m["system"])
	}
	if m["service"] != nil && fmt.Sprint(m["service"]) != "map[]" {
		t.Errorf("lldp not unset: %v", m["service"])
	}
	ifs, _ := m["interface"].(map[string]any)
	if ifs["swp2"] == nil || ifs["swp3"] == nil {
		t.Errorf("patch ranges: %v", ifs)
	}
	// an unset followed by a set under it: the set's branch holds only what the set gives
	again := filepath.Join(dir, "again.yaml")
	os.WriteFile(again, []byte("- unset:\n    system:\n      date-time: {}\n- set:\n    system:\n      date-time:\n        timezone: UTC\n"), 0o644)
	f.must(t, "root", "config", "patch", again)
	f.must(t, "root", "config", "apply", "-y")
	if m := showJSON(t, f, "root", "system"); fmt.Sprint(m["date-time"]) != "map[timezone:UTC]" {
		t.Errorf("unset then set: %v", m["date-time"])
	}
	// JSON replace
	js := filepath.Join(dir, "c.json")
	os.WriteFile(js, []byte(`{"system": {"hostname": "leaf9"}, "interface": {"eth0": {"type": "eth", "ip": {"address": {"dhcp": {}}, "vrf": "mgmt"}}}}`), 0o644)
	f.must(t, "root", "config", "replace", js)
	f.must(t, "root", "config", "apply", "-y")
	if m := showJSON(t, f, "root"); fmt.Sprint(m["system"]) != "map[hostname:leaf9]" || m["router"] != nil {
		t.Errorf("after JSON replace: %v", m)
	}
	// apply startup: back to the factory configuration
	f.must(t, "root", "config", "apply", "startup", "-y")
	if m := showJSON(t, f, "root", "system"); m["hostname"] != nvue.FactoryHostname {
		t.Errorf("apply startup: %v", m)
	}
	// save makes the applied configuration the startup one
	f.must(t, "root", "set", "system", "hostname", "leaf10")
	f.must(t, "root", "config", "apply", "-y")
	f.must(t, "root", "config", "save")
	if m := showJSON(t, f, "root", "system", "--rev", "startup"); m["hostname"] != "leaf10" {
		t.Errorf("startup after save: %v", m)
	}
	// detach: the pending revision is kept under its id and is no longer the user's
	f.must(t, "root", "set", "system", "hostname", "leaf11")
	if out := f.must(t, "root", "config", "detach"); !strings.HasPrefix(out, "detached [rev_id: ") {
		t.Errorf("detach: %q", out)
	}
	if m := showJSON(t, f, "root", "system", "--pending"); m["hostname"] != "leaf10" {
		t.Errorf("pending after detach: %v", m)
	}
	if out := f.must(t, "root", "config", "revision"); !strings.Contains(out, "pending") || !strings.Contains(out, "applied") {
		t.Errorf("revisions:\n%s", out)
	}
}

func TestExpandAndWords(t *testing.T) {
	got := expand(strings.Fields("interface swp1s0-3,swp9 link state up"))
	var names []string
	for _, w := range got {
		names = append(names, w[1])
	}
	if strings.Join(names, " ") != "swp1s0 swp1s1 swp1s2 swp1s3 swp9" {
		t.Errorf("expand: %v", names)
	}
	w, err := shellWords(`nv set interface swp1 description "to spine's \"1\""`)
	if err != nil || len(w) != 6 || w[5] != `to spine's "1"` {
		t.Errorf("shellWords: %q %v", w, err)
	}
}

// `nv show` with no -o is NVUE's table: a row per attribute, the operational and applied columns.
func TestShowTable(t *testing.T) {
	f := newFixture(t)
	f.must(t, "cumulus", "set", "system", "hostname", "leaf09")
	f.must(t, "cumulus", "config", "apply", "-y")
	out := f.must(t, "cumulus", "show", "system")
	lines := strings.Split(out, "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], "operational") || !strings.Contains(lines[0], "applied") || !strings.HasPrefix(strings.TrimSpace(lines[1]), "--") {
		t.Fatalf("nv show system's header:\n%s", out)
	}
	found := false
	for _, l := range lines[2:] {
		fs := strings.Fields(l)
		found = found || len(fs) >= 2 && fs[0] == "hostname" && fs[len(fs)-1] == "leaf09"
	}
	if !found {
		t.Errorf("nv show system has no hostname row with leaf09:\n%s", out)
	}
}

// What NVIDIA Config Manager's boot script reads and runs with nv (18, Z7a): the serial from `nv
// show platform | grep serial-number | awk '{print $2}'`, and its startup.yaml fetched with `nv
// action fetch system` into place, in the management VRF, with its permissions.
func TestPlatformAndFetch(t *testing.T) {
	f := newFixture(t)
	sw := factoryLeaf()
	sw.Factory.Identity = nos.Identity{Serial: "MT2245X03839", BaseMAC: "44:38:39:00:01:01", Model: "SN5600"}
	srv := nvue.NewServer(sw, nvue.FactoryPassword, func(context.Context, nvue.Files) (string, error) { return "ok", nil })
	srv.SetLocalToken("tok")
	srv.SetRunner(func(ctx context.Context, args ...string) (string, error) { return "root\n", nil })
	if err := srv.ApplyFactory(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(srv)
	defer ts.Close()
	var out, errw bytes.Buffer
	c := &client{base: ts.URL + "/nvue_v1", user: "root", token: "tok", http: ts.Client(), out: &out, errw: &errw}
	if rc := c.run([]string{"show", "platform"}); rc != 0 {
		t.Fatalf("nv show platform: %d %s", rc, errw.String())
	}
	var serial string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "serial-number") {
			if fs := strings.Fields(l); len(fs) > 1 {
				serial = fs[1]
			}
		}
	}
	if serial != "MT2245X03839" {
		t.Errorf("the serial as the script reads it: %q from\n%s", serial, out.String())
	}

	d := t.TempDir()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/startup.yaml" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "- set:\n    system:\n      hostname: leaf01\n")
	}))
	defer web.Close()
	ipLog := filepath.Join(d, "ip.log")
	ip := filepath.Join(d, "ip")
	if err := os.WriteFile(ip, []byte("#!/bin/sh\necho \"$*\" >> "+ipLog+"\nshift 3\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(d, "startup.yaml")
	fc := &client{base: f.ts.URL + "/nvue_v1", user: "root", token: "tok", http: f.ts.Client(), out: &out, errw: &errw, ipCmd: ip}
	if rc := fc.run([]string{"action", "fetch", "system", "file-path", dst, "uri", web.URL + "/startup.yaml", "file-permissions", "600", "vrf", "mgmt"}); rc != 0 {
		t.Fatalf("nv action fetch: %d %s", rc, errw.String())
	}
	if st, err := os.Stat(dst); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the fetched file: %v %v", st, err)
	}
	if b, _ := os.ReadFile(ipLog); !strings.HasPrefix(string(b), "vrf exec mgmt curl ") {
		t.Errorf("the fetch's VRF: %q", b)
	}
	errw.Reset()
	if rc := fc.run([]string{"action", "fetch", "system", "file-path", dst + "2", "uri", web.URL + "/missing"}); rc == 0 {
		t.Error("a 404 fetched")
	}
	if _, err := os.Stat(dst + "2"); err == nil {
		t.Error("a failed fetch left its file")
	}
	for _, args := range [][]string{
		{"action", "reboot", "system", "now"},
		{"action", "fetch", "system", "file-path", "rel", "uri", web.URL},
		{"action", "fetch", "system", "file-path", dst, "uri", "ftp://x/y"},
		{"action", "fetch", "system", "file-path", dst, "uri", web.URL, "file-permissions", "999"},
	} {
		if rc := fc.run(args); rc == 0 {
			t.Errorf("nv %s succeeded", strings.Join(args, " "))
		}
	}
}

// nv config apply --confirm: the apply waits for its confirmation (nv config confirm) and is rolled
// back when none comes in time; nv config apply --confirm-yes confirms as nv config confirm does.
func TestConfirm(t *testing.T) {
	f := newFixture(t)
	f.must(t, "root", "set", "system", "hostname", "leaf01")
	if out := f.must(t, "root", "config", "apply", "--confirm", "60", "-y"); !strings.Contains(out, "confirm within 60 s") {
		t.Fatalf("apply --confirm: %q", out)
	}
	if out := f.must(t, "root", "config", "confirm"); !strings.Contains(out, "confirmed") {
		t.Fatalf("confirm: %q", out)
	}
	if code, _, errw := f.nv("root", "config", "confirm"); code == 0 || !strings.Contains(errw, "no apply is waiting") {
		t.Errorf("a second confirm: %d %q", code, errw)
	}
	f.must(t, "root", "set", "system", "hostname", "leaf02")
	f.must(t, "root", "config", "apply", "--confirm", "1s", "-y")
	deadline := time.Now().Add(10 * time.Second)
	for showJSON(t, f, "root", "system")["hostname"] != "leaf01" {
		if time.Now().After(deadline) {
			t.Fatalf("not rolled back: %v", showJSON(t, f, "root", "system")["hostname"])
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.must(t, "root", "set", "system", "hostname", "leaf03")
	f.must(t, "root", "config", "apply", "--confirm", "-y")
	if out := f.must(t, "root", "config", "apply", "--confirm-yes"); !strings.Contains(out, "confirmed") {
		t.Errorf("--confirm-yes: %q", out)
	}
	if h := showJSON(t, f, "root", "system")["hostname"]; h != "leaf03" {
		t.Errorf("after --confirm-yes: %v", h)
	}
}
