//go:build linux

package linuxnet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/crypto/ssh"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap"
	"github.com/BudEcosystem/dc-twin/internal/critic"
	"github.com/BudEcosystem/dc-twin/internal/day0"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Day 0 (18): a frontend fabric of factory switches (D72) brought up by the test playing a
// site's day-0 tooling. The tooling has only what a site has: stack-inputs/day0 (the expected
// switches by serial and MAC, the address plan), its own DHCP server (Kea with the NOSes' ZTP
// options, D74, and ONIE's default URL, D75) relayed by the twin-configured OOB fabric from each
// switch's management port (D73), its own HTTP server, and the switches' management interfaces
// (SSH, NVUE, `nv`) at the leases its DHCP server gave out. The twin's day-0 observer, holding
// every switch against the design (D77), decides when the fabric has converged.

const (
	day0Server = "10.90.1.10" // the probe: the ZTP and ONIE HTTP server
	day0Port   = 8080
	// newPassword is what the tooling gives user cumulus in place of the factory one
	newPassword = "Day0-Tool-1"
)

// day0Case is one day-0 run's setup.
type day0Case struct {
	estate  string // the estate's name, unique per test so tests can run side by side
	nos     string
	install string // preinstalled or onie
	ztp     string // enabled or disabled
	boot    string // the switches' boot time (default 60s, 3 s under the estate's timeScale)
	// reference makes the tooling stacks/reference-day0: its render.py's Kea configuration and
	// files, run and served as rendered, in place of the test's own
	reference bool
	// cumulus is the Cumulus Linux release the switches' NVUE emulates ("" the twin's default)
	cumulus string
	// plan is given (stack-inputs/day0 holds the twin's address plan) or tool (the tooling
	// allocates its own: toolPlan)
	plan string
	// images, when set, is spec.adapters.network.nos.images (YAML), whatever the install
	images string
	// dhcp, when set, is the container the site's DHCP runs in, attached to the OOB network at
	// 10.90.0.10 as dhcpIface, in place of the harness's Kea (a day-0 tool's own, NVCM's edge)
	dhcp, dhcpIface string
	// noTooling: the harness serves no HTTP at the tooling's address (the day-0 tool serves its
	// own there)
	noTooling bool
	// file is the estate under testdata ("" tap-day0.yaml)
	file string
	// bmcDHCP is the container a DSX estate's BMC DHCP runs in (tap-day0-dsx.yaml)
	bmcDHCP string
}

// day0Run is a running day-0 estate and the tooling around it.
type day0Run struct {
	t      *testing.T
	c      day0Case
	tw     *twin.Twin
	sub    *tap.Substrate
	p      *probe
	d      *nos.Docker
	kea    *nos.Container
	design day0.Design
	plan   day0.Plan
	exp    []day0.ExpectedSwitch
	// session is the critic judging the run with the day-0 policy (policies/day0/fabric.yaml)
	session *critic.Session

	mu       sync.Mutex
	requests []*http.Request // what the switches fetched from the tooling's HTTP server
	// installer is the ONIE installer the tooling serves, and its sha256
	installer []byte
	// www is the reference tooling's rendered files, served at / (reference runs)
	www string
	// mux is the tooling's HTTP server's, which a suite may add its handlers to
	mux *http.ServeMux
	// ntpFrom counts the NTP requests the tooling's NTP server answered, by source address
	ntpFrom map[string]int
}

func day0Binaries(t *testing.T) string {
	dir := os.Getenv("DCTWIN_DAY0_BINARIES")
	if dir == "" {
		t.Skip("DCTWIN_DAY0_BINARIES is not set (scripts/linux-substrate-e2e.sh builds cmd/dctwin-day0)")
	}
	if _, err := os.Stat(filepath.Join(dir, "dctwin-day0")); err != nil {
		t.Skipf("no dctwin-day0 in %s: %v", dir, err)
	}
	return dir
}

// startDay0 brings a day-0 estate up with the tooling's Kea and HTTP server around it.
func startDay0(t *testing.T, c day0Case) *day0Run {
	t.Helper()
	need(t)
	bin := day0Binaries(t)
	d, err := nosRuntime(t)
	if err != nil {
		t.Skipf("no Docker Engine for the NOS runtime: %v", err)
	}
	ctx := context.Background()
	images := []string{nos.NVUEFRRImage, keaImage}
	if c.nos == nos.SonicVS {
		images = append(images, nos.SonicVSImage, nos.SonicBGPImage, nos.SonicGNMIImage)
	}
	for _, img := range images {
		if !d.HasImage(ctx, img) {
			t.Skipf("image %s is not present (scripts/linux-substrate-e2e.sh builds it)", img)
		}
	}
	// DCTWIN_RUN tells runs of one suite apart on one host (estate, namespaces, containers)
	if run := os.Getenv("DCTWIN_RUN"); run != "" {
		c.estate += "-" + run
	}
	if c.reference && os.Getenv("DCTWIN_REFERENCE_DAY0") == "" {
		t.Skip("DCTWIN_REFERENCE_DAY0 is not set (scripts/linux-substrate-e2e.sh copies stacks/reference-day0)")
	}
	r := &day0Run{t: t, c: c, d: d, installer: []byte("#!/bin/sh\n# a NOS installer the twin recognises by name (D75)\nexit 0\n")}
	suffix := strconv.Itoa(os.Getpid())
	keaName, probeName := "dctwin-kea-"+c.estate+"-"+suffix, "dctwin-probe-"+strings.TrimPrefix(c.estate, "linuxnet-")
	r.p = newProbe(t, probeName)
	if !c.noTooling {
		r.serve(t)
	}
	r.serveNTP(t)
	keaIface := "svc0"
	if c.dhcp != "" {
		keaName, keaIface = c.dhcp, orDefault(c.dhcpIface, "eth0")
		r.kea = &nos.Container{ID: c.dhcp, Name: c.dhcp}
	} else {
		r.kea = startDay0Kea(t, d, keaName, c.reference)
	}
	onie := "[]"
	if c.install == "onie" {
		onie = fmt.Sprintf(`[{ match: { name: "^onie-installer" }, nos: %s, version: "5.13.1" }]`, c.nos)
	}
	if c.images != "" {
		onie = c.images
	}
	stacks := "[]"
	if c.reference {
		stacks = "[{ name: reference-day0@0.1.0 }]"
	}
	est := loadEstate(t, orDefault(c.file, "tap-day0.yaml"), map[string]string{"STACKS": stacks, "CUMULUS_VERSION": c.cumulus, "PLAN_MODE": orDefault(c.plan, "given"), "ESTATE_NAME": c.estate, "FABRIC_NOS": c.nos, "INSTALL_MODE": c.install,
		"ZTP_MODE": c.ztp, "BOOT_TIME": orDefault(c.boot, "60s"), "DAY0_BINARIES": bin, "ONIE_IMAGES": onie, "KEA_CONTAINER": keaName, "KEA_IFACE": keaIface, "PROBE_NETNS": probeName, "BMC_DHCP_CONTAINER": c.bmcDHCP})
	start := time.Now()
	r.tw, r.sub, _ = up(t, est)
	t.Logf("up returned after %s: the OOB fabric configured by the twin, the frontend's switches in factory state", time.Since(start).Round(time.Second))
	res, err := critic.Resolve("day0/fabric")
	if err != nil {
		t.Fatal(err)
	}
	r.session = critic.StartSession(r.tw, res, "day0tool")
	t.Cleanup(func() {
		if t.Failed() {
			r.dumpSwitches()
			hold(t) // DCTWIN_HOLD keeps a failed estate up for a look inside
		}
	})
	r.readInputs()
	if c.reference {
		r.renderReference()
	}
	return r
}

// renderReference runs the reference tooling's generator (stacks/reference-day0/render.py) on
// stack-inputs/day0, as a site runs its own, then starts Kea on the configuration it rendered and
// serves the files it rendered.
func (r *day0Run) renderReference() {
	t := r.t
	ref := os.Getenv("DCTWIN_REFERENCE_DAY0")
	out := filepath.Join(filepath.Dir(day0Binaries(t)), "reference-"+r.c.estate+"-"+strconv.Itoa(os.Getpid()))
	_ = os.RemoveAll(out)
	t.Cleanup(func() { _ = os.RemoveAll(out) })
	b, err := exec.Command("python3", "-I", filepath.Join(ref, "render.py"), filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", "day0"), out,
		"--server", fmt.Sprintf("%s:%d", day0Server, day0Port), "--password", newPassword).CombinedOutput()
	if err != nil {
		t.Fatalf("render.py: %v\n%s", err, b)
	}
	t.Logf("render.py: %s", strings.TrimSpace(string(b)))
	cfg, err := os.ReadFile(filepath.Join(out, "kea-dhcp4.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Kea as rendered, with a log file added for the test's report
	var kea map[string]any
	if err := json.Unmarshal(cfg, &kea); err != nil {
		t.Fatalf("render.py's kea-dhcp4.json: %v", err)
	}
	kea["Dhcp4"].(map[string]any)["loggers"] = []any{map[string]any{"name": "kea-dhcp4", "severity": "INFO",
		"output-options": []any{map[string]any{"output": "/var/log/kea-dhcp4.log"}}}}
	cfg, _ = json.Marshal(kea)
	if out, code, err := r.d.Exec(context.Background(), r.kea.ID, "sh", "-c",
		"echo "+base64.StdEncoding.EncodeToString(cfg)+" | base64 -d > /etc/kea/reference.json.tmp && mv /etc/kea/reference.json.tmp /etc/kea/reference.json"); err != nil || code != 0 {
		t.Fatalf("Kea's configuration: exit %d %v: %s", code, err, out)
	}
	r.mu.Lock()
	r.www = filepath.Join(out, "www")
	r.mu.Unlock()
}

// checkCabling runs the reference tooling's cabling check (stacks/reference-day0/check-cabling.py)
// beside Kea on the OOB network, with Kea's leases, as the site's tooling has them.
func (r *day0Run) checkCabling() (string, int) {
	t := r.t
	t.Helper()
	ref := os.Getenv("DCTWIN_REFERENCE_DAY0")
	if ref == "" {
		t.Skip("DCTWIN_REFERENCE_DAY0 is not set (scripts/linux-substrate-e2e.sh copies stacks/reference-day0)")
	}
	ctx := context.Background()
	if !r.d.HasImage(ctx, ansibleImage) {
		t.Skipf("image %s is not present (it carries the tooling's python3)", ansibleImage)
	}
	dir := filepath.Join(filepath.Dir(day0Binaries(t)), "cabling-"+r.c.estate+"-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for mac, ip := range r.leases() {
		fmt.Fprintf(&b, "%s,%s\n", mac, ip)
	}
	if err := os.WriteFile(filepath.Join(dir, "leases.csv"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// the two inputs the check reads, where the container runtime can read them
	inputs := filepath.Join(dir, "inputs")
	if err := os.MkdirAll(inputs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"expected-switches.json", "cabling.csv"} {
		b, err := os.ReadFile(filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", "day0", f))
		if err == nil {
			err = os.WriteFile(filepath.Join(inputs, f), b, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	name := "dctwin-cabling-" + r.c.estate + "-" + strconv.Itoa(os.Getpid())
	c, err := r.d.Run(ctx, nos.Spec{Name: name, Image: ansibleImage, NetworkMode: "container:" + r.kea.ID,
		Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"/usr/bin/python3 -I /ref/check-cabling.py /inputs --leases /work/leases.csv --password " +
			newPassword + "; echo EXIT=$?; sleep 600"},
		Binds:  []string{ref + ":/ref:ro", inputs + ":/inputs:ro", dir + ":/work:ro"},
		Labels: map[string]string{"dc-twin.test": "linuxnet"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.d.Remove(context.Background(), c.ID)
	var out string
	waitFor(t, 5*time.Minute, "the cabling check", func() bool {
		out, _ = r.d.Logs(ctx, c.ID)
		return strings.Contains(out, "EXIT=")
	})
	code := 1
	if i := strings.LastIndex(out, "EXIT="); i >= 0 {
		code, _ = strconv.Atoi(strings.TrimSpace(strings.SplitN(out[i+5:], "\n", 2)[0]))
	}
	t.Logf("check-cabling.py (exit %d):\n%s", code, indent(out))
	return out, code
}

// judge stops the run's critic session and fails on any violation of the day-0 policy but those
// allowed, and on the policy not being judged at all for want of a stack.
func (r *day0Run) judge(allow ...string) *critic.Judgement {
	t := r.t
	t.Helper()
	j, err := r.session.Stop()
	if err != nil {
		t.Fatalf("the critic: %v", err)
	}
	t.Logf("the critic, day0/fabric: verdict %s\n%s", j.Verdict, indent(j.Text()))
	// what the tool's requests that failed were (hygiene's error ratio counts them), by protocol,
	// method, canonical request, target and status
	if evs, err := critic.ReadLog(critic.StateDir(r.tw)); err == nil {
		failed := map[string]int{}
		for _, e := range evs {
			if q := e.Request; e.Kind == "request" && e.Source.Class == "stack" && q != nil && q.Status >= 400 {
				k := fmt.Sprintf("%s %s %s %s → %d", q.Protocol, q.Method, q.Canonical, q.Target, q.Status)
				if why := q.Detail["issue"]; why != "" {
					k += ": " + strings.Join(strings.Fields(why), " ")
				}
				failed[k]++
			}
		}
		var lines []string
		for k, n := range failed {
			lines = append(lines, fmt.Sprintf("%4d  %s", n, k))
		}
		sort.Sort(sort.Reverse(sort.StringSlice(lines)))
		if len(lines) > 0 {
			t.Logf("the tool's failed requests:\n%s", indent(strings.Join(lines, "\n")))
		}
	}
	// the day-0 measures (18 — Measures): every factory switch has its time to converged
	var ms []string
	conv := map[string]bool{}
	for _, m := range j.Measures {
		if !strings.HasPrefix(m.Name, "day0.") {
			continue
		}
		ms = append(ms, fmt.Sprintf("%s %s = %v %s", m.Name, filepath.Base(m.Subject), m.Value, m.Unit))
		if m.Name == "day0.toConverged" {
			conv[m.Subject] = true
		}
	}
	t.Logf("the critic's day-0 measures:\n%s", indent(strings.Join(ms, "\n")))
	for _, e := range r.factory() {
		if !conv[e.Path] {
			t.Errorf("the critic has no day0.toConverged for %s", e.Hostname)
		}
	}
	for _, v := range j.Violations {
		if !strings.HasPrefix(v.Rule, "day0/fabric/") {
			continue
		}
		ok := false
		for _, a := range allow {
			ok = ok || v.Rule == a
		}
		if !ok {
			t.Errorf("the critic: %s violated: %s", v.Rule, v.Message)
		}
	}
	for _, na := range j.NA {
		if !strings.HasPrefix(na.Rule, "day0/fabric/") {
			continue
		}
		if r.c.nos == nos.SonicVS && !r.c.reference {
			// the test's own tooling is no declared stack, and a SONiC switch's ZTP fetches
			// everything itself: no request reaches an interface the twin logs
			t.Logf("the critic could not judge %s: %s (no stack declared, SONiC ZTP only)", na.Rule, na.Why)
			continue
		}
		t.Errorf("the critic could not judge %s: %s", na.Rule, na.Why)
	}
	return j
}

// readInputs reads stack-inputs/day0 as the tooling does.
func (r *day0Run) readInputs() {
	t := r.t
	dir := filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", "day0")
	files := []string{"expected-switches.json", "cabling.csv", "topology.dot", "netbox/devices.csv", "netbox/cables.csv", "netbox/README.md"}
	if r.c.plan != "tool" {
		files = append(files, "plan.json")
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("stack-inputs/day0/%s: %v", f, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "expected-switches.json"))
	if err := json.Unmarshal(b, &r.exp); err != nil {
		t.Fatalf("expected-switches.json: %v", err)
	}
	if r.c.plan == "tool" {
		if _, err := os.Stat(filepath.Join(dir, "plan.json")); err == nil {
			t.Fatalf("stack-inputs/day0/plan.json is there under plan: tool")
		}
		r.plan = r.toolPlan(filepath.Join(dir, "cabling.csv"))
	} else {
		b, _ = os.ReadFile(filepath.Join(dir, "plan.json"))
		if err := json.Unmarshal(b, &r.plan); err != nil {
			t.Fatalf("plan.json: %v", err)
		}
	}
	r.design = r.sub.Design()
	dot, _ := os.ReadFile(filepath.Join(dir, "topology.dot"))
	t.Logf("stack-inputs/day0: %d expected switches, %d in the address plan; topology.dot:\n%s", len(r.exp), len(r.plan.Switches), indent(string(dot)))
}

// toolPlan is the tooling's own address plan (plan: tool), made from the cabling plan as a site's
// IPAM would: loopbacks from 10.250.0.0/24, a /31 per switch-to-switch cable from 10.251.0.0/16,
// an AS for the spines and one per leaf from 64600, numbered eBGP on every cable. None of it is
// the twin's plan, so the oracle can only judge the result by its structure.
func (r *day0Run) toolPlan(cabling string) day0.Plan {
	t := r.t
	f, err := os.Open(cabling)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) == 0 {
		t.Fatalf("cabling.csv: %v", err)
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	byHost := map[string]*day0.PlanSwitch{}
	var p day0.Plan
	leaf := uint32(0)
	for i, e := range r.factory() {
		s := day0.PlanSwitch{Path: e.Path, Hostname: e.Hostname, Serial: e.Serial, Role: e.Role,
			Loopback: fmt.Sprintf("10.250.0.%d/32", i+1), RouterID: fmt.Sprintf("10.250.0.%d", i+1)}
		if e.Role == "spine" {
			s.ASN = 64600
		} else {
			leaf++
			s.ASN = 64600 + leaf
		}
		p.Switches = append(p.Switches, s)
	}
	for i := range p.Switches {
		byHost[p.Switches[i].Hostname] = &p.Switches[i]
	}
	n := 0
	for _, row := range rows[1:] {
		a, b := byHost[row[col["a_device"]]], byHost[row[col["b_device"]]]
		ai, bi := row[col["a_interface"]], row[col["b_interface"]]
		if a == nil || b == nil || ai == "" || bi == "" {
			continue
		}
		x, y := fmt.Sprintf("10.251.%d.%d", n/128, 2*(n%128)), fmt.Sprintf("10.251.%d.%d", n/128, 2*(n%128)+1)
		n++
		a.Interfaces = append(a.Interfaces, day0.Interface{Name: ai, Kind: "fabric", Address: x + "/31", Peer: b.Hostname + ":" + bi, PeerAddr: y, PeerASN: b.ASN})
		b.Interfaces = append(b.Interfaces, day0.Interface{Name: bi, Kind: "fabric", Address: y + "/31", Peer: a.Hostname + ":" + ai, PeerAddr: x, PeerASN: a.ASN})
	}
	t.Logf("the tooling's own plan (plan: tool): %d switches, %d fabric links", len(p.Switches), n)
	return p
}

// factory is the expected switches that start in factory state.
func (r *day0Run) factory() []day0.ExpectedSwitch {
	var out []day0.ExpectedSwitch
	for _, e := range r.exp {
		if e.Factory {
			out = append(out, e)
		}
	}
	return out
}

func (r *day0Run) planOf(serial string) (day0.PlanSwitch, bool) {
	for _, s := range r.plan.Switches {
		if s.Serial == serial {
			return s, true
		}
	}
	return day0.PlanSwitch{}, false
}

// ---------------------------------------------------------------- the tooling's DHCP server

// startDay0Kea is the tooling's Kea: the OOB segments, and the ZTP options per NOS, selected by
// what each client sends: Cumulus's vendor class (option 60 "cumulus-linux …") gets its
// provisioning URL in option 239, SONiC's user class SONiC-ZTP its ZTP document's URL in option
// 67, ONIE's vendor class (onie_vendor:…) its installer's URL in option 114.
func startDay0Kea(t *testing.T, d *nos.Docker, name string, reference bool) *nos.Container {
	t.Helper()
	var subnets []any
	for i := 0; i < 4; i++ {
		subnets = append(subnets, map[string]any{"id": i + 1, "subnet": fmt.Sprintf("10.30.%d.0/24", i),
			"pools":       []any{map[string]any{"pool": fmt.Sprintf("10.30.%d.10 - 10.30.%d.250", i, i)}},
			"option-data": []any{map[string]any{"name": "routers", "data": fmt.Sprintf("10.30.%d.1", i)}}})
	}
	url := func(p string) string { return fmt.Sprintf("http://%s:%d%s", day0Server, day0Port, p) }
	cfg := map[string]any{"Dhcp4": map[string]any{
		"interfaces-config": map[string]any{"interfaces": []any{"svc0"}, "dhcp-socket-type": "udp"},
		"lease-database":    map[string]any{"type": "memfile", "persist": true, "name": "/var/lib/kea/kea-leases4.csv", "lfc-interval": 0},
		"valid-lifetime":    3600,
		// Cumulus's 239 is no standard option; ONIE's default-url is code 114, which RFC 8910 gave
		// the captive portal's URL and Kea 2.6 names v4-captive-portal (a string, as ONIE reads it)
		"option-def": []any{map[string]any{"name": "cumulus-provision-url", "code": 239, "type": "string", "space": "dhcp4"}},
		"client-classes": []any{
			map[string]any{"name": "onie", "test": "substring(option[60].text,0,12) == 'onie_vendor:'",
				"option-data": []any{map[string]any{"name": "v4-captive-portal", "data": url("/onie/onie-installer"), "always-send": true}}},
			map[string]any{"name": "cumulus", "test": "substring(option[60].text,0,13) == 'cumulus-linux'",
				"option-data": []any{map[string]any{"name": "cumulus-provision-url", "data": url("/ztp/cumulus.sh"), "always-send": true}}},
			// option 77 by its raw bytes: Kea 2.6 reads it as RFC 3004's length-prefixed classes, and
			// SONiC's dhclient sends the bare string (send user-class "SONiC-ZTP")
			map[string]any{"name": "sonic", "test": "substring(option[77].hex,0,9) == 'SONiC-ZTP'",
				"option-data": []any{map[string]any{"name": "boot-file-name", "data": url("/ztp/sonic.json"), "always-send": true}}},
		},
		"subnet4": subnets,
		"loggers": []any{map[string]any{"name": "kea-dhcp4", "severity": "INFO", "output-options": []any{map[string]any{"output": "/var/log/kea-dhcp4.log"}}}},
	}}
	b, _ := json.Marshal(cfg)
	script := `mkdir -p /var/lib/kea /run/kea /var/log
printf '%s' "$KEA_CONFIG" > /etc/kea/kea-dhcp4.json
until ip -4 addr show svc0 2>/dev/null | grep -q inet; do sleep 0.2; done
until [ "$(cat /sys/class/net/svc0/carrier 2>/dev/null)" = 1 ]; do sleep 0.2; done
exec kea-dhcp4 -c /etc/kea/kea-dhcp4.json`
	if reference {
		// the reference tooling's configuration, rendered once stack-inputs exist (renderReference)
		script = `mkdir -p /var/lib/kea /run/kea /var/log
until ip -4 addr show svc0 2>/dev/null | grep -q inet; do sleep 0.2; done
until [ -s /etc/kea/reference.json ]; do sleep 0.5; done
exec kea-dhcp4 -c /etc/kea/reference.json`
	}
	ctx := context.Background()
	c, err := d.Run(ctx, nos.Spec{Name: name, Image: keaImage, Hostname: "kea", Cmd: []string{"/bin/sh", "-c", script},
		Env:    []string{"KEA_CONFIG=" + string(b), "KEA_PIDFILE_DIR=/run/kea", "KEA_LOCKFILE_DIR=/run/kea"},
		Labels: map[string]string{"dc-twin.test": "linuxnet"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _, _ := d.Exec(context.Background(), c.ID, "tail", "-n", "60", "/var/log/kea-dhcp4.log")
			t.Logf("Kea's log:\n%s", indent(out))
		}
		_ = d.Remove(context.Background(), c.ID)
	})
	return c
}

// leases reads the tooling's DHCP leases (Kea's lease file): MAC -> address.
func (r *day0Run) leases() map[string]string {
	out, _, err := r.d.Exec(context.Background(), r.kea.ID, "cat", "/var/lib/kea/kea-leases4.csv")
	if err != nil {
		return nil
	}
	res := map[string]string{}
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		f := strings.Split(l, ",")
		if len(f) > 2 && f[0] != "address" {
			res[strings.ToLower(f[1])] = f[0]
		}
	}
	return res
}

// ---------------------------------------------------------------- the tooling's HTTP server

// listen opens a TCP listener inside the probe's namespace.
func (p *probe) listen(addr string) (net.Listener, error) {
	l, _, err := p.open("tcp", addr)
	return l, err
}

// listenUDP opens a UDP socket inside the probe's namespace.
func (p *probe) listenUDP(addr string) (net.PacketConn, error) {
	_, c, err := p.open("udp", addr)
	return c, err
}

func (p *probe) open(network, addr string) (net.Listener, net.PacketConn, error) {
	type res struct {
		l   net.Listener
		c   net.PacketConn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := netns.Get()
		if err != nil {
			ch <- res{nil, nil, err}
			return
		}
		defer orig.Close()
		if err := netns.Set(p.ns); err != nil {
			ch <- res{nil, nil, err}
			return
		}
		var x res
		if network == "udp" {
			x.c, x.err = net.ListenPacket("udp", addr)
		} else {
			x.l, x.err = net.Listen("tcp", addr)
		}
		if serr := netns.Set(orig); serr == nil {
			runtime.UnlockOSThread()
		}
		ch <- x
	}()
	x := <-ch
	return x.l, x.c, x.err
}

// serve runs the tooling's HTTP server at the probe's address once the twin attaches it: the
// Cumulus ZTP script per serial (CUMULUS-SERIAL), the SONiC ZTP document and each switch's
// config_db.json (its dynamic URL carries the serial), and the ONIE installer.
func (r *day0Run) serve(t *testing.T) {
	mux := http.NewServeMux()
	r.mux = mux
	record := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			r.mu.Lock()
			r.requests = append(r.requests, req.Clone(context.Background()))
			r.mu.Unlock()
			h(w, req)
		}
	}
	mux.HandleFunc("/ztp/cumulus.sh", record(func(w http.ResponseWriter, req *http.Request) {
		s, ok := r.planOf(req.Header.Get("CUMULUS-SERIAL"))
		if !ok {
			http.Error(w, "no such serial", 404)
			return
		}
		_, _ = io.WriteString(w, cumulusScript(s))
	}))
	mux.HandleFunc("/ztp/sonic.json", record(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ztp": map[string]any{
			"01-configdb-json": map[string]any{
				"dynamic-url":               map[string]any{"source": map[string]any{"prefix": fmt.Sprintf("http://%s:%d/ztp/configdb/", day0Server, day0Port), "identifier": "serial-number", "suffix": ".json"}},
				"clear-config":              false,
				"overwrite-device-metadata": true,
			},
			"02-connectivity-check": map[string]any{"ping-hosts": []string{day0Server}, "ignore-result": true},
		}})
	}))
	mux.HandleFunc("/ztp/configdb/", record(func(w http.ResponseWriter, req *http.Request) {
		s, ok := r.planOf(strings.TrimSuffix(filepath.Base(req.URL.Path), ".json"))
		if !ok {
			http.Error(w, "no such serial", 404)
			return
		}
		_ = json.NewEncoder(w).Encode(sonicConfigDB(s, req.Header.Get("BASE-MAC-ADDRESS")))
	}))
	mux.HandleFunc("/", record(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		www := r.www
		r.mu.Unlock()
		if www == "" {
			http.NotFound(w, req)
			return
		}
		http.FileServer(http.Dir(www)).ServeHTTP(w, req)
	}))
	mux.HandleFunc("/onie/", record(func(w http.ResponseWriter, req *http.Request) {
		// ONIE's waterfall: the exact default URL first, then installer names by platform
		if !strings.HasPrefix(filepath.Base(req.URL.Path), "onie-installer") {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(r.installer)
	}))
	go func() {
		var l net.Listener
		var err error
		for i := 0; i < 3600; i++ { // the address arrives when the twin attaches the probe, after up
			if l, err = r.p.listen(fmt.Sprintf("%s:%d", day0Server, day0Port)); err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Errorf("the tooling's HTTP server: %v", err)
			return
		}
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		t.Cleanup(func() { srv.Close() })
		_ = srv.Serve(l)
	}()
}

// serveNTP is the tooling's NTP server at the probe's address: an SNTP server (RFC 4330) answering
// every client request with the host's time at stratum 1, counting requests by source address.
func (r *day0Run) serveNTP(t *testing.T) {
	r.ntpFrom = map[string]int{}
	go func() {
		var c net.PacketConn
		var err error
		for i := 0; i < 3600; i++ { // the address arrives when the twin attaches the probe, after up
			if c, err = r.p.listenUDP(fmt.Sprintf("%s:123", day0Server)); err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Errorf("the tooling's NTP server: %v", err)
			return
		}
		t.Cleanup(func() { c.Close() })
		buf := make([]byte, 512)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 || buf[0]&7 != 3 { // client mode only
				continue
			}
			host, _, _ := net.SplitHostPort(from.String())
			r.mu.Lock()
			r.ntpFrom[host]++
			r.mu.Unlock()
			out := make([]byte, 48)
			out[0] = buf[0]&0x38 | 4 // the client's version, server mode
			out[1], out[2], out[3] = 1, buf[2], 0xec
			copy(out[12:16], "GPS\x00")
			now := ntpTime(time.Now())
			copy(out[16:24], now)        // reference
			copy(out[24:32], buf[40:48]) // originate: the client's transmit
			copy(out[32:40], now)        // receive
			copy(out[40:48], ntpTime(time.Now()))
			_, _ = c.WriteTo(out, from)
		}
	}()
}

func ntpTime(t time.Time) []byte {
	b := make([]byte, 8)
	secs := uint64(t.Unix()) + 2208988800
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	for i := 0; i < 4; i++ {
		b[i] = byte(secs >> (24 - 8*i))
		b[4+i] = byte(frac >> (24 - 8*i))
	}
	return b
}

// ntpRequests is how many NTP requests the tooling's NTP server has had from an address.
func (r *day0Run) ntpRequests(addr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ntpFrom[addr]
}

func (r *day0Run) fetched(path string) []*http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*http.Request
	for _, q := range r.requests {
		if strings.HasPrefix(q.URL.Path, path) {
			out = append(out, q)
		}
	}
	return out
}

// cumulusScript is a switch's ZTP script, as a site's tooling renders it from its address plan:
// NVUE through `nv`, then a new password for the factory user.
func cumulusScript(s day0.PlanSwitch) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/bash")
	w("# CUMULUS-AUTOPROVISIONING")
	w("set -e")
	w("nv set system hostname %s", s.Hostname)
	w("nv set interface lo ip address %s", s.Loopback)
	for _, it := range s.Interfaces {
		if it.Kind != "fabric" {
			continue
		}
		w("nv set interface %s ip address %s", it.Name, it.Address)
		w("nv set interface %s link state up", it.Name)
	}
	w("nv set router bgp autonomous-system %d", s.ASN)
	w("nv set router bgp router-id %s", s.RouterID)
	w("nv set vrf default router bgp address-family ipv4-unicast redistribute connected enable on")
	for _, it := range s.Interfaces {
		if it.Kind == "fabric" {
			w("nv set vrf default router bgp neighbor %s remote-as %d", it.PeerAddr, it.PeerASN)
		}
	}
	w("nv set service ntp mgmt server %s iburst on", day0Server)
	w("nv set system aaa user cumulus password '%s'", newPassword)
	w("nv config apply -y")
	w("exit 0")
	return b.String()
}

// sonicConfigDB is a switch's config_db.json, as a site's tooling renders it from its address
// plan: what it adds to the factory configuration (loaded on top of it, clear-config false), in
// the unified routing mode's tables frrcfgd takes to FRR.
func sonicConfigDB(s day0.PlanSwitch, mac string) map[string]any {
	db := map[string]map[string]any{}
	put := func(table, key string, v map[string]any) {
		if db[table] == nil {
			db[table] = map[string]any{}
		}
		db[table][key] = v
	}
	asn := strconv.FormatUint(uint64(s.ASN), 10)
	meta := map[string]any{"hostname": s.Hostname, "bgp_asn": asn, "docker_routing_config_mode": "unified", "frr_mgmt_framework_config": "true"}
	if mac != "" {
		meta["mac"] = strings.ToLower(mac)
	}
	put("DEVICE_METADATA", "localhost", meta)
	put("LOOPBACK_INTERFACE", "Loopback0", map[string]any{})
	put("LOOPBACK_INTERFACE", "Loopback0|"+s.Loopback, map[string]any{})
	put("BGP_GLOBALS", "default", map[string]any{"local_asn": asn, "router_id": s.RouterID, "keepalive": "3", "holdtime": "9"})
	put("BGP_GLOBALS_AF", "default|ipv4_unicast", map[string]any{"redistribute_connected": "true"})
	put("ROUTE_REDISTRIBUTE", "default|connected|bgp|ipv4", map[string]any{})
	put("ROUTE_MAP", "DAY0_PERMIT|10", map[string]any{"route_operation": "permit"})
	for _, it := range s.Interfaces {
		if it.Kind != "fabric" {
			continue
		}
		put("INTERFACE", it.Name, map[string]any{})
		put("INTERFACE", it.Name+"|"+it.Address, map[string]any{})
		peerASN := strconv.FormatUint(uint64(it.PeerASN), 10)
		put("BGP_NEIGHBOR", "default|"+it.PeerAddr, map[string]any{"asn": peerASN, "local_addr": strings.Split(it.Address, "/")[0], "admin_status": "up",
			"keepalive": "3", "holdtime": "9", "conn_retry": "10"})
		put("BGP_NEIGHBOR_AF", "default|"+it.PeerAddr+"|ipv4_unicast", map[string]any{"admin_status": "true",
			"route_map_in": []string{"DAY0_PERMIT"}, "route_map_out": []string{"DAY0_PERMIT"}})
	}
	out := map[string]any{}
	for k, v := range db {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------- waiting and reporting

// waitConverged waits until the twin's day-0 observer holds every factory switch converged.
func (r *day0Run) waitConverged(timeout time.Duration) time.Duration {
	t := r.t
	t.Helper()
	start := time.Now()
	last := time.Time{}
	for {
		v, _ := r.sub.Day0()
		done, total := 0, 0
		var pending []string
		for _, x := range v {
			total++
			if x.Converged {
				done++
			} else {
				pending = append(pending, fmt.Sprintf("%s (%s): %s", filepath.Base(x.Path), x.Phase, strings.Join(x.Diffs, "; ")))
			}
		}
		if total > 0 && done == total {
			return time.Since(start)
		}
		if time.Since(last) > 30*time.Second {
			t.Logf("day 0 after %s: %d of %d converged\n%s", time.Since(start).Round(time.Second), done, total, indent(strings.Join(pending, "\n")))
			last = time.Now()
		}
		if time.Since(start) > timeout {
			t.Fatalf("%d of %d factory switches converged after %s:\n%s", done, total, timeout, indent(strings.Join(pending, "\n")))
		}
		time.Sleep(2 * time.Second)
	}
}

// day0Events is the committed day-0 observations of a field, by switch path, in order.
func (r *day0Run) day0Events(field string) map[string][]string {
	out := map[string][]string{}
	for _, ev := range r.tw.Engine.Events(0, nil) {
		if ev.Kind == "state" && ev.State != nil && ev.State.Field == field && ev.Source.Class == "observer" {
			out[ev.Path] = append(out[ev.Path], fmt.Sprint(ev.State.To))
		}
	}
	return out
}

func (r *day0Run) dumpSwitches() {
	for _, sp := range r.sub.Plan().Switches {
		if sp.Config.Factory == nil {
			continue
		}
		out, err := r.sub.Exec(context.Background(), sp.Device.Path, "sh", "-c",
			"ip -br addr; cat "+nos.Day0State+"/ztp/status.json "+nos.Day0State+"/onie/status.json 2>/dev/null; tail -n 30 /var/lib/cumulus/ztp/ztp_state.log /var/log/autoprovision /var/log/ztp.log /var/log/dctwin-dhcp.log 2>/dev/null; cat /run/dctwin/dhcp-eth0.env 2>/dev/null")
		r.t.Logf("%s (%v):\n%s", sp.Config.Hostname, err, indent(out))
	}
}

func (r *day0Run) checkPhases() {
	phases := r.day0Events("day0.phase")
	for _, e := range r.factory() {
		got := phases[e.Path]
		if len(got) == 0 || got[len(got)-1] != day0.PhaseConverged {
			r.t.Errorf("%s: day0.phase events %v, the last not converged", e.Hostname, got)
		}
	}
	leases := r.day0Events("mgmt.lease")
	for _, e := range r.factory() {
		if len(leases[e.Path]) == 0 {
			r.t.Errorf("%s: no mgmt.lease event", e.Hostname)
		}
	}
	r.t.Logf("day0.phase events: %v", phases)
}

// ---------------------------------------------------------------- the tests

// Cumulus ZTP (D74): each factory generic/nvue-frr switch leases on eth0 in VRF mgmt from the
// tooling's Kea through the OOB relay, is handed its provisioning URL in option 239, fetches it
// with Cumulus's identifying headers, runs the script (which needs the marker), whose `nv` calls
// configure the underlay through NVUE and change the factory password; the fabric converges.
func TestDay0Cumulus(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-cumulus", nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled"})
	took := r.waitConverged(15 * time.Minute)
	t.Logf("every factory switch converged %s after up, by ZTP alone", took.Round(time.Second))
	got := r.fetched("/ztp/cumulus.sh")
	serials := map[string]bool{}
	for _, q := range got {
		if ua := q.Header.Get("User-Agent"); !strings.HasPrefix(ua, "CumulusLinux-AutoProvision/") {
			t.Errorf("ZTP's User-Agent %q", ua)
		}
		for _, h := range []string{"CUMULUS-ARCH", "CUMULUS-SERIAL", "CUMULUS-BASE-MAC", "CUMULUS-MGMT-MAC", "CUMULUS-VERSION", "CUMULUS-PRODUCTNAME", "CUMULUS-MANUFACTURER"} {
			if q.Header.Get(h) == "" {
				t.Errorf("ZTP's request has no %s: %v", h, q.Header)
			}
		}
		serials[q.Header.Get("CUMULUS-SERIAL")] = true
	}
	for _, e := range r.factory() {
		if !serials[e.Serial] {
			t.Errorf("%s (%s) never fetched its ZTP script", e.Hostname, e.Serial)
		}
	}
	r.checkPhases()
	// the script changed the factory password: NVUE refuses the factory one and takes the new
	leases := r.leases()
	e := r.factory()[0]
	ip := leases[strings.ToLower(e.MgmtMAC)]
	if ip == "" {
		t.Fatalf("%s: no lease for %s in Kea's lease file: %v", e.Hostname, e.MgmtMAC, leases)
	}
	if st := r.nvueStatus(ip, nvue.User, nvue.FactoryPassword); st != http.StatusUnauthorized {
		t.Errorf("%s: NVUE with the factory password: %d, want 401", e.Hostname, st)
	}
	if st := r.nvueStatus(ip, nvue.User, newPassword); st != http.StatusOK {
		t.Errorf("%s: NVUE with the password ZTP set: %d, want 200", e.Hostname, st)
	}
	// ZTP is done: it reports success and does not run again
	out, err := r.sub.Exec(context.Background(), e.Path, "ztp", "-s")
	t.Logf("%s: ztp -s (%v):\n%s", e.Hostname, err, indent(out))
	if !strings.Contains(strings.ToLower(out), "success") {
		t.Errorf("%s: ztp -s does not report success", e.Hostname)
	}
	// a preinstalled switch has no ONIE to go back to
	if err := r.sub.FactoryUninstall(context.Background(), e.Path); err == nil || !strings.Contains(err.Error(), "not installed by ONIE") {
		t.Errorf("%s: uninstall of a preinstalled switch: %v, want refused", e.Hostname, err)
	}
	// a factory reset (dctwin switch reset --factory): the switch is the factory's again, and its
	// ZTP, armed again, brings it back
	fetched := len(r.fetched("/ztp/cumulus.sh"))
	if err := r.sub.FactoryReset(context.Background(), e.Path); err != nil {
		t.Fatalf("%s: factory reset: %v", e.Hostname, err)
	}
	waitFor(t, 2*time.Minute, e.Hostname+" back in its factory state or provisioned again", func() bool {
		return len(r.fetched("/ztp/cumulus.sh")) > fetched
	})
	took = r.waitConverged(5 * time.Minute)
	hosts := r.day0Events("hostname")[e.Path]
	if len(hosts) < 2 || !strings.Contains(strings.Join(hosts, " "), nvue.FactoryHostname) {
		t.Errorf("%s: hostname events %v: the reset did not return it to %s", e.Hostname, hosts, nvue.FactoryHostname)
	}
	t.Logf("%s factory-reset: ZTP ran again and the fabric converged %s later; hostnames %v", e.Hostname, took.Round(time.Second), hosts)
	r.judge()
}

// Day-0 faults (generic/day0), one on each factory switch before its ZTP: the lease without its
// ZTP option, the script cut short, the ZTP server unreachable, the switch dead on arrival. None
// of the four converges, each for its reason; cleared, a factory reset (as a site retries a
// switch's ZTP) brings the fabric up.
func TestDay0Faults(t *testing.T) {
	// a 30 s boot (600 s under timeScale 0.05): time to inject before any switch leases
	r := startDay0(t, day0Case{estate: "linuxnet-day0-faults", nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled", boot: "600s"})
	fac := r.factory()
	if len(fac) != 4 {
		t.Fatalf("%d factory switches", len(fac))
	}
	src := schema.EventSource{Class: "operator", Principal: "test"}
	faults := []string{"generic/day0/ztp-dhcp-no-option", "generic/day0/ztp-script-error", "generic/day0/ztp-server-unreachable", "generic/day0/switch-doa"}
	for i, e := range fac {
		if _, err := r.tw.Engine.Inject(e.Path, faults[i], nil, fmt.Sprintf("f%d", i), src); err != nil {
			t.Fatalf("%s: %s: %v", e.Hostname, faults[i], err)
		}
		t.Logf("%s: %s", e.Hostname, faults[i])
	}
	time.Sleep(2 * time.Minute)
	v, obs := r.sub.Day0()
	for i, x := range v {
		if x.Converged {
			t.Errorf("%s converged under %s", x.Path, faults[i])
		}
		t.Logf("%s (%s): mgmt %q; %s", filepath.Base(x.Path), x.Phase, obs[x.Path].Mgmt, strings.Join(x.Diffs, "; "))
	}
	// what each fault left: no fetch from the stripped lease's switch, from the blocked one, or
	// from the dead one (no lease at all); the corrupted script fetched and failed
	fetched := map[string]int{}
	for _, q := range r.fetched("/ztp/cumulus.sh") {
		fetched[q.Header.Get("CUMULUS-SERIAL")]++
	}
	for i, e := range fac {
		switch faults[i] {
		case "generic/day0/ztp-script-error":
			if fetched[e.Serial] == 0 {
				t.Errorf("%s fetched no script under ztp-script-error", e.Hostname)
			}
		default:
			if fetched[e.Serial] != 0 {
				t.Errorf("%s fetched its script under %s", e.Hostname, faults[i])
			}
		}
	}
	if obs[fac[3].Path].Mgmt != "" {
		t.Errorf("%s, dead on arrival, holds a lease %s", fac[3].Hostname, obs[fac[3].Path].Mgmt)
	}
	for i := range fac {
		if _, err := r.tw.Engine.Clear(fmt.Sprintf("f%d", i), "", src); err != nil {
			t.Fatal(err)
		}
	}
	// the dead switch boots once its fault clears; the others' ZTP is retried by a factory reset
	waitFor(t, 3*time.Minute, "every factory switch up", func() bool {
		_, obs := r.sub.Day0()
		for _, e := range fac {
			if obs[e.Path].Phase == day0.PhaseFactory {
				return false
			}
		}
		return true
	})
	for _, e := range fac[:3] {
		if err := r.sub.FactoryReset(context.Background(), e.Path); err != nil {
			t.Fatalf("%s: factory reset: %v", e.Hostname, err)
		}
	}
	took := r.waitConverged(10 * time.Minute)
	t.Logf("faults cleared, ZTP retried by factory reset: the fabric converged %s later", took.Round(time.Second))
	r.judge()
}

// SONiC ZTP (D74): each factory sonic-vs switch leases on eth0 through the twin's DHCP client
// (Q25), is handed its ZTP document's URL in option 67, fetches it with SONiC's headers, and its
// configdb-json section's dynamic URL (the serial) loads its config_db.json, which SONiC's
// managers and frrcfgd take to the kernel and FRR; the fabric converges.
func TestDay0Sonic(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-sonic", nos: nos.SonicVS, install: "preinstalled", ztp: "enabled"})
	took := r.waitConverged(30 * time.Minute)
	t.Logf("every factory SONiC switch converged %s after up", took.Round(time.Second))
	docs, dbs := r.fetched("/ztp/sonic.json"), r.fetched("/ztp/configdb/")
	if len(docs) < len(r.factory()) || len(dbs) < len(r.factory()) {
		t.Errorf("%d ZTP documents and %d config_db.json fetched for %d switches", len(docs), len(dbs), len(r.factory()))
	}
	for _, q := range docs {
		if ua := q.Header.Get("User-Agent"); !strings.HasPrefix(ua, "SONiC-ZTP/") || q.Header.Get("SERIAL-NUMBER") == "" {
			t.Errorf("SONiC ZTP's request: User-Agent %q, headers %v", ua, q.Header)
		}
	}
	r.checkPhases()
	e := r.factory()[0]
	out, err := r.sub.Exec(context.Background(), e.Path, "ztp", "status")
	t.Logf("%s: ztp status (%v):\n%s", e.Hostname, err, indent(out))
	r.judge()
}

// ONIE (D75): each switch has no NOS; ONIE's discovery leases with ONIE's vendor class, is
// handed the installer's URL in option 114, fetches it with ONIE's headers, the twin recognises
// it by name and the switch boots its NOS, whose ZTP then brings it up.
func TestDay0ONIE(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-onie", nos: nos.NVUEFRR, install: "onie", ztp: "enabled"})
	took := r.waitConverged(20 * time.Minute)
	t.Logf("every switch installed by ONIE and converged %s after up", took.Round(time.Second))
	got := r.fetched("/onie/")
	if len(got) < len(r.factory()) {
		t.Fatalf("%d ONIE fetches for %d switches", len(got), len(r.factory()))
	}
	for _, q := range got {
		for _, h := range []string{"ONIE-SERIAL-NUMBER", "ONIE-ETH-ADDR", "ONIE-VENDOR-ID", "ONIE-MACHINE", "ONIE-ARCH", "ONIE-OPERATION"} {
			if q.Header.Get(h) == "" {
				t.Errorf("ONIE's request has no %s: %v", h, q.Header)
			}
		}
	}
	phases := r.day0Events("day0.phase")
	for _, e := range r.factory() {
		seen := strings.Join(phases[e.Path], " ")
		if !strings.Contains(seen, day0.PhaseONIE) {
			t.Errorf("%s: phases %s, never %s", e.Hostname, seen, day0.PhaseONIE)
		}
	}
	sum := sha256.Sum256(r.installer)
	t.Logf("the installer (sha256 %s) was fetched %d times", hex.EncodeToString(sum[:8]), len(got))
	r.checkPhases()
	// ONIE's uninstall (dctwin switch reset --factory --uninstall): the switch is back in ONIE,
	// whose discovery fetches the installer again and installs; its ZTP then brings it up
	e := r.factory()[0]
	fetches := func() int {
		n := 0
		for _, q := range r.fetched("/onie/") {
			if q.Header.Get("ONIE-SERIAL-NUMBER") == e.Serial {
				n++
			}
		}
		return n
	}
	before := fetches()
	if err := r.sub.FactoryUninstall(context.Background(), e.Path); err != nil {
		t.Fatalf("%s: uninstall: %v", e.Hostname, err)
	}
	// (the reinstall takes a few seconds under the estate's timeScale, less than the observer's
	// look every 10 s, so its ONIE phase may not be seen: the installer's fetch is the evidence)
	waitFor(t, 5*time.Minute, e.Hostname+" back in ONIE, fetching its installer again", func() bool { return fetches() > before })
	took = r.waitConverged(20 * time.Minute)
	t.Logf("%s uninstalled: ONIE installed it again and the fabric converged %s later; phases %v", e.Hostname, took.Round(time.Second), r.day0Events("day0.phase")[e.Path])
	r.judge()
}

// The first push without ZTP (ztp disabled): the tooling finds each switch's address in its own
// DHCP server's leases by the MAC stack-inputs gives, logs in over SSH with the factory password,
// which must be changed first (PAM, as on Cumulus), then configures half the switches through
// NVUE's REST API (the revision workflow, as nvidia.nvue's Ansible collection does) and the other
// half through `nv` over SSH; the fabric converges.
func TestDay0API(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-api", nos: nos.NVUEFRR, install: "preinstalled", ztp: "disabled"})
	fac := r.factory()
	addr := map[string]string{}
	waitFor(t, 5*time.Minute, "every factory switch's management lease in Kea", func() bool {
		l := r.leases()
		for _, e := range fac {
			if l[strings.ToLower(e.MgmtMAC)] == "" {
				return false
			}
			addr[e.Serial] = l[strings.ToLower(e.MgmtMAC)]
		}
		return true
	})
	for i, e := range fac {
		ip := addr[e.Serial]
		s, _ := r.planOf(e.Serial)
		waitFor(t, 3*time.Minute, e.Hostname+"'s sshd", func() bool {
			c, err := r.p.dial(context.Background(), "tcp", ip+":22")
			if err == nil {
				c.Close()
			}
			return err == nil
		})
		if st := r.nvueStatus(ip, nvue.User, nvue.FactoryPassword); st == http.StatusOK {
			t.Errorf("%s: NVUE took the expired factory password", e.Hostname)
		}
		r.changePassword(ip, nvue.FactoryPassword, newPassword)
		if i%2 == 0 {
			r.pushREST(ip, s)
		} else {
			r.pushCLI(ip, s)
		}
	}
	took := r.waitConverged(10 * time.Minute)
	t.Logf("the fabric converged %s after the first push", took.Round(time.Second))
	r.checkPhases()
	applies := 0
	for _, ev := range r.tw.Engine.Events(0, nil) {
		if ev.Kind == "request" && ev.Request != nil && ev.Request.Canonical == "network.config.apply" && ev.Source.Principal == nvue.User {
			applies++
		}
	}
	if applies < len(fac) {
		t.Errorf("%d NVUE applies by user cumulus in the event log, want one per switch (%d)", applies, len(fac))
	}
	r.judge()
}

// Miscabling (generic/cabling): two leaves' uplinks to spine-1 are swapped on the floor before day
// 0. The design is unchanged, so the address plan's /31s meet the wrong peers: those sessions
// never establish and the oracle names them; LLDP on spine-1 shows the swapped neighbours. When
// the cables are put back as designed, the fabric converges.
func TestDay0Cabling(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-cabling", nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled"})
	var a, b string
	for _, c := range r.design.Cables {
		if strings.HasSuffix(c.ADevice, "spine-1") || strings.HasSuffix(c.BDevice, "spine-1") {
			leaf := c.ADevice + ":" + c.APort
			if strings.HasSuffix(c.ADevice, "spine-1") {
				leaf = c.BDevice + ":" + c.BPort
			}
			if strings.Contains(leaf, "leaf-r01") {
				a = leaf
			} else if strings.Contains(leaf, "leaf-r02") {
				b = leaf
			}
		}
	}
	if a == "" || b == "" {
		t.Fatalf("no leaf-to-spine-1 cables in the design: %+v", r.design.Cables)
	}
	ref := func(hostPort string) string {
		host, port, _ := strings.Cut(hostPort, ":")
		for _, s := range r.design.Switches {
			if s.Hostname == host {
				return s.Path + ":" + port
			}
		}
		return hostPort
	}
	src := schema.EventSource{Class: "operator", Principal: "test"}
	if _, err := r.tw.Engine.Inject(ref(a), "generic/cabling/cable-swap", map[string]any{"with": ref(b)}, "swap", src); err != nil {
		t.Fatal(err)
	}
	t.Logf("swapped the far ends of %s and %s", a, b)
	// ZTP configures every switch; the swapped sessions stay down
	time.Sleep(3 * time.Minute)
	v, obs := r.sub.Day0()
	down := 0
	for _, x := range v {
		if !x.Converged {
			down++
			t.Logf("%s (%s): %s", filepath.Base(x.Path), x.Phase, strings.Join(x.Diffs, "; "))
		}
	}
	if down == 0 {
		t.Fatalf("every switch converged over swapped cables")
	}
	var spineLLDP map[string]string
	for path, o := range obs {
		if strings.HasSuffix(path, "spine-1") {
			spineLLDP = o.LLDP
		}
	}
	t.Logf("spine-1's LLDP neighbours: %v", spineLLDP)
	if len(spineLLDP) == 0 {
		t.Errorf("spine-1 has no LLDP neighbours")
	}
	// the reference tooling's cabling check (LLDP through NVUE against cabling.csv) names both
	// swapped cables while their sessions are down (Z3)
	out, code := r.checkCabling()
	if code == 0 {
		t.Errorf("the cabling check passed over swapped cables:\n%s", indent(out))
	}
	for _, c := range r.design.Cables {
		ends := []string{c.ADevice + ":" + c.APort, c.BDevice + ":" + c.BPort}
		if (ends[0] == a || ends[1] == a || ends[0] == b || ends[1] == b) && strings.Contains(c.ADevice+c.BDevice, "spine-1") {
			if !strings.Contains(out, "MISMATCH "+c.ID+" ") {
				t.Errorf("the cabling check does not name cable %s (%s -- %s) as not as planned", c.ID, ends[0], ends[1])
			}
		}
	}
	if _, err := r.tw.Engine.Clear("swap", "", src); err != nil {
		t.Fatal(err)
	}
	took := r.waitConverged(10 * time.Minute)
	t.Logf("with the cables as designed the fabric converged %s later", took.Round(time.Second))
	// the far ends' lldpd keeps the swapped neighbour until its TTL (120 s) runs out, as a
	// switch's does: the check passes once LLDP has caught up
	if !eventually(5*time.Minute, func() bool { _, code := r.checkCabling(); return code == 0 }) {
		t.Errorf("the cabling check still fails with the cables as designed")
	}

	// a speed mismatch (no link), a cable in the wrong port of spine-1 (LLDP says which), a
	// flapping cable: each takes leaf-r01's session to spine-1 down; cleared, it comes back
	spine := ""
	for _, sw := range r.design.Switches {
		if strings.HasSuffix(sw.Hostname, "spine-1") {
			spine = sw.Path
		}
	}
	session := func() string {
		_, obs := r.sub.Day0()
		for path, o := range obs {
			if strings.HasSuffix(path, "leaf-r01-1") {
				for _, it := range r.design.Switches {
					if it.Path != path {
						continue
					}
					for _, f := range it.Interfaces {
						if strings.HasPrefix(f.Peer, "n01-spine-1:") {
							return o.Sessions[f.PeerAddr].State
						}
					}
				}
			}
		}
		return ""
	}
	for _, c := range []struct {
		fault  string
		params map[string]any
	}{
		{"generic/cabling/cable-missing", nil},
		{"generic/cabling/cable-dead", nil},
		{"generic/cabling/cable-speed-mismatch", nil},
		{"generic/cabling/cable-wrong-port", map[string]any{"port": "eth10"}},
		{"generic/cabling/cable-flap", map[string]any{"period": 20}},
	} {
		if _, err := r.tw.Engine.Inject(ref(a), c.fault, c.params, "c", src); err != nil {
			t.Fatalf("%s: %v", c.fault, err)
		}
		if !eventually(2*time.Minute, func() bool { return session() != "Established" }) {
			t.Errorf("%s: leaf-r01-1's session to spine-1 stayed Established", c.fault)
		}
		if c.fault == "generic/cabling/cable-wrong-port" {
			waitFor(t, 3*time.Minute, "spine-1's LLDP on swp10", func() bool {
				_, obs := r.sub.Day0()
				return strings.HasPrefix(obs[spine].LLDP["swp10"], "n01-leaf-r01-1")
			})
		}
		if _, err := r.tw.Engine.Clear("c", "", src); err != nil {
			t.Fatal(err)
		}
		if !eventually(3*time.Minute, func() bool { return session() == "Established" }) {
			t.Errorf("%s cleared: leaf-r01-1's session to spine-1 did not come back (%s)", c.fault, session())
		}
		t.Logf("%s: the session went down and came back when it cleared", c.fault)
	}
	r.judge()
}

// The management network's faults (generic/day0, Z4) on a converged fabric: NTP unreachable from a
// switch (its requests stop reaching the tooling's NTP server while the others' keep coming), and
// another device answering ARP for a switch's management lease (`arping -D` on the switch's eth0,
// as an operator checks a suspected duplicate, finds a second responder). Cleared, NTP from the
// switch resumes and the duplicate is gone.
func TestDay0MgmtFaults(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-mgmtfaults", nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled"})
	took := r.waitConverged(15 * time.Minute)
	t.Logf("converged %s after up", took.Round(time.Second))
	fac := r.factory()
	leases := r.leases()
	ip := func(e day0.ExpectedSwitch) string { return leases[strings.ToLower(e.MgmtMAC)] }
	waitFor(t, 3*time.Minute, "NTP requests from every switch", func() bool {
		for _, e := range fac {
			if r.ntpRequests(ip(e)) == 0 {
				return false
			}
		}
		return true
	})
	dad := func(e day0.ExpectedSwitch) (string, int) {
		out, err := r.sub.Exec(context.Background(), e.Path, "sh", "-c", "arping -D -c 3 -w 4 -I eth0 "+ip(e)+"; echo EXIT=$?")
		code := -1
		if i := strings.LastIndex(out, "EXIT="); i >= 0 {
			code, _ = strconv.Atoi(strings.TrimSpace(out[i+5:]))
		}
		if err != nil {
			t.Logf("%s: arping: %v", e.Hostname, err)
		}
		return out, code
	}
	if out, code := dad(fac[1]); code != 0 {
		t.Fatalf("%s: arping -D finds a duplicate before any fault (exit %d):\n%s", fac[1].Hostname, code, indent(out))
	}
	src := schema.EventSource{Class: "operator", Principal: "test"}
	if _, err := r.tw.Engine.Inject(fac[0].Path, "generic/day0/ntp-unreachable", nil, "ntp", src); err != nil {
		t.Fatal(err)
	}
	if _, err := r.tw.Engine.Inject(fac[1].Path, "generic/day0/mgmt-duplicate-ip", nil, "dup", src); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Second) // the substrate's fault sync
	blocked, other := r.ntpRequests(ip(fac[0])), r.ntpRequests(ip(fac[2]))
	waitFor(t, 4*time.Minute, "another NTP request from "+fac[2].Hostname, func() bool { return r.ntpRequests(ip(fac[2])) > other })
	if n := r.ntpRequests(ip(fac[0])); n != blocked {
		t.Errorf("%s: %d NTP requests reached the server under ntp-unreachable", fac[0].Hostname, n-blocked)
	}
	var out string
	code := 0
	if !eventually(2*time.Minute, func() bool { out, code = dad(fac[1]); return code != 0 }) {
		t.Errorf("%s: arping -D finds no duplicate under mgmt-duplicate-ip:\n%s", fac[1].Hostname, indent(out))
	}
	t.Logf("%s: arping -D under mgmt-duplicate-ip (exit %d):\n%s", fac[1].Hostname, code, indent(out))
	for _, id := range []string{"ntp", "dup"} {
		if _, err := r.tw.Engine.Clear(id, "", src); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 5*time.Minute, "NTP from "+fac[0].Hostname+" again", func() bool { return r.ntpRequests(ip(fac[0])) > blocked })
	if !eventually(2*time.Minute, func() bool { _, code = dad(fac[1]); return code == 0 }) {
		t.Errorf("%s: arping -D still finds a duplicate after the fault cleared", fac[1].Hostname)
	}
	r.waitConverged(5 * time.Minute)
	r.judge()
}

// plan: tool (18 — What the SUT is given): stack-inputs/day0 holds no address plan, and the
// tooling allocates its own loopbacks, /31s and AS numbers from the cabling plan (toolPlan), none
// of them the twin's; its ZTP scripts configure them. The oracle judges the fabric by its
// structure (every cabled link with a session, unique loopbacks and AS numbers, every loopback in
// every RIB), so it converges on the tool's plan.
func TestDay0PlanTool(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-plantool", nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled", plan: "tool"})
	took := r.waitConverged(15 * time.Minute)
	t.Logf("every factory switch converged %s after up on the tooling's own plan", took.Round(time.Second))
	r.checkPhases()
	_, obs := r.sub.Day0()
	for _, s := range r.plan.Switches {
		o := obs[s.Path]
		found := false
		for _, rt := range o.Routes {
			found = found || rt == s.Loopback
		}
		if len(o.Routes) > 0 && !found {
			t.Errorf("%s: its tool-allocated loopback %s is not in its own RIB %v", s.Hostname, s.Loopback, o.Routes)
		}
	}
	r.judge()
}

// The reference day-0 tooling (stacks/reference-day0, Z2) as a site runs it: render.py reads only
// stack-inputs/day0 and writes Kea's configuration (a reservation per switch by management MAC
// with its ZTP URL) and the per-serial ZTP scripts and documents; Kea runs that configuration and
// an HTTP server serves those files, unchanged. Nothing is done by hand; the fabric converges.
func TestDay0ReferenceCumulus(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-refcl", nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled", reference: true})
	took := r.waitConverged(15 * time.Minute)
	t.Logf("every factory switch converged %s after up, from the reference tooling's files", took.Round(time.Second))
	for _, e := range r.factory() {
		if len(r.fetched("/ztp/"+e.Serial+".sh")) == 0 {
			t.Errorf("%s never fetched its rendered ZTP script /ztp/%s.sh", e.Hostname, e.Serial)
		}
	}
	r.checkPhases()
	e := r.factory()[0]
	if st := r.nvueStatus(r.leases()[strings.ToLower(e.MgmtMAC)], nvue.User, newPassword); st != http.StatusOK {
		t.Errorf("%s: NVUE with the password the rendered script set: %d", e.Hostname, st)
	}
	r.judge()
}

func TestDay0ReferenceSonic(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-refsn", nos: nos.SonicVS, install: "preinstalled", ztp: "enabled", reference: true})
	took := r.waitConverged(30 * time.Minute)
	t.Logf("every factory SONiC switch converged %s after up, from the reference tooling's files", took.Round(time.Second))
	for _, e := range r.factory() {
		if len(r.fetched("/ztp/"+e.Serial+".json")) == 0 || len(r.fetched("/configdb/"+e.Serial+".json")) == 0 {
			t.Errorf("%s did not fetch its rendered ZTP document and config_db.json", e.Hostname)
		}
	}
	r.checkPhases()
	r.judge()
}

// ONIE's faults (generic/day0, Z4), injected before any switch boots: power lost during the
// install brings the switch back to ONIE's discovery, again for as long as it holds; an installer
// ONIE rejects is never installed; a slow boot starts later than the rest. Cleared, every switch
// installs and converges through ZTP.
func TestDay0ONIEFaults(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-oniefaults", nos: nos.NVUEFRR, install: "onie", ztp: "enabled", boot: "600s"})
	fac := r.factory()
	src := schema.EventSource{Class: "operator", Principal: "test"}
	faults := map[string]string{fac[0].Path: "generic/day0/onie-install-power-loss", fac[1].Path: "generic/day0/onie-image-mismatch",
		fac[2].Path: "generic/day0/switch-slow-boot"}
	for i, e := range fac[:3] {
		if _, err := r.tw.Engine.Inject(e.Path, faults[e.Path], nil, fmt.Sprintf("o%d", i), src); err != nil {
			t.Fatalf("%s: %v", faults[e.Path], err)
		}
	}
	// the unfaulted switch installs and boots; the power-lost one starts its install, loses
	// power and is back in ONIE's discovery, fetching the installer again; the rejected one
	// fetches but never installs
	fetchedBy := func() map[string]int {
		m := map[string]int{}
		for _, q := range r.fetched("/onie/") {
			if strings.HasPrefix(filepath.Base(q.URL.Path), "onie-installer") {
				m[q.Header.Get("ONIE-SERIAL-NUMBER")]++
			}
		}
		return m
	}
	// (the unfaulted switch cannot converge yet: its peers include the faulted ones)
	waitFor(t, 10*time.Minute, fac[3].Hostname+" installed and booted and "+fac[0].Hostname+" back in ONIE's discovery after a lost install", func() bool {
		ph := r.day0Events("day0.phase")[fac[3].Path]
		ok := len(ph) > 0 && ph[len(ph)-1] != day0.PhaseFactory && ph[len(ph)-1] != day0.PhaseONIE
		f := fetchedBy()
		return ok && f[fac[0].Serial] >= 2 && f[fac[1].Serial] >= 1
	})
	phases := r.day0Events("day0.phase")
	t.Logf("phases under the faults: %v; installer fetches by serial: %v", phases, fetchedBy())
	for _, e := range fac[:2] {
		if ph := phases[e.Path]; len(ph) > 0 && ph[len(ph)-1] != day0.PhaseONIE {
			t.Errorf("%s booted its NOS under %s", e.Hostname, faults[e.Path])
		}
	}
	// the slow switch has not yet got past ONIE when the unfaulted one has
	for _, ph := range phases[fac[2].Path] {
		if ph != day0.PhaseFactory && ph != day0.PhaseONIE {
			t.Errorf("%s, slow to boot, already %s when %s had installed", fac[2].Hostname, ph, fac[3].Hostname)
		}
	}
	for i := range fac[:3] {
		if _, err := r.tw.Engine.Clear(fmt.Sprintf("o%d", i), "", src); err != nil {
			t.Fatal(err)
		}
	}
	took := r.waitConverged(20 * time.Minute)
	t.Logf("ONIE faults cleared: every switch installed and converged %s later", took.Round(time.Second))
	r.checkPhases()
	r.judge()
}

// ---------------------------------------------------------------- the tooling's clients

func (r *day0Run) httpClient() *http.Client {
	return &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DialContext: r.p.dial, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

// nvueStatus is NVUE's answer to a read with a user's credentials.
func (r *day0Run) nvueStatus(ip, user, pw string) int {
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://%s:%d/nvue_v1/system", ip, nvue.APIPort), nil)
	req.SetBasicAuth(user, pw)
	res, err := r.httpClient().Do(req)
	if err != nil {
		r.t.Logf("NVUE at %s: %v", ip, err)
		return 0
	}
	res.Body.Close()
	return res.StatusCode
}

// pushREST configures a switch through NVUE's REST API: a revision, the tree patched at its
// root, applied (nvidia.nvue's path).
func (r *day0Run) pushREST(ip string, s day0.PlanSwitch) {
	t := r.t
	cl := r.httpClient()
	do := func(method, path string, body any) map[string]any {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, fmt.Sprintf("https://%s:%d/nvue_v1/%s", ip, nvue.APIPort, path), rd)
		req.SetBasicAuth(nvue.User, newPassword)
		req.Header.Set("Content-Type", "application/json")
		res, err := cl.Do(req)
		if err != nil {
			t.Fatalf("%s %s at %s: %v", method, path, ip, err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		if res.StatusCode != 200 {
			t.Fatalf("%s %s at %s: %d %v", method, path, ip, res.StatusCode, out)
		}
		return out
	}
	var rev string
	for id := range do("POST", "revision", nil) {
		rev = id
	}
	ifs := map[string]any{"lo": map[string]any{"ip": map[string]any{"address": map[string]any{s.Loopback: map[string]any{}}}}}
	nbr := map[string]any{}
	for _, it := range s.Interfaces {
		if it.Kind != "fabric" {
			continue
		}
		ifs[it.Name] = map[string]any{"type": "swp", "link": map[string]any{"state": map[string]any{"up": map[string]any{}}},
			"ip": map[string]any{"address": map[string]any{it.Address: map[string]any{}}}}
		nbr[it.PeerAddr] = map[string]any{"remote-as": it.PeerASN, "type": "numbered"}
	}
	tree := map[string]any{
		"system":    map[string]any{"hostname": s.Hostname},
		"interface": ifs,
		"router":    map[string]any{"bgp": map[string]any{"enable": "on", "autonomous-system": s.ASN, "router-id": s.RouterID}},
		"vrf": map[string]any{"default": map[string]any{"router": map[string]any{"bgp": map[string]any{"enable": "on", "neighbor": nbr,
			"address-family": map[string]any{"ipv4-unicast": map[string]any{"enable": "on", "redistribute": map[string]any{"connected": map[string]any{"enable": "on"}}}}}}}},
	}
	do("PATCH", "?rev="+rev, tree)
	out := do("PATCH", "revision/"+rev, map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes"}})
	t.Logf("%s (%s): REST apply: %v", s.Hostname, ip, out["state"])
}

func (r *day0Run) sshClient(ip, pw string) (*ssh.Client, error) { return r.sshAs(ip, nvue.User, pw) }

// sshAs logs in to a switch's management address over SSH as a user, with a password.
func (r *day0Run) sshAs(ip, user, pw string) (*ssh.Client, error) {
	conn, err := r.p.dial(context.Background(), "tcp", ip+":22")
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(pw),
		ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			out := make([]string, len(qs))
			for i := range qs {
				out[i] = pw
			}
			return out, nil
		})},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Minute}
	c, ch, reqs, err := ssh.NewClientConn(conn, ip+":22", cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, ch, reqs), nil
}

// changePassword logs in with an expired password and answers PAM's forced change (current,
// new, retype), as a first login on a factory Cumulus switch does.
func (r *day0Run) changePassword(ip, old, pw string) {
	t := r.t
	cl, err := r.sshClient(ip, old)
	if err != nil {
		t.Fatalf("ssh %s@%s with the factory password: %v", nvue.User, ip, err)
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.RequestPty("xterm", 40, 120, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	in, _ := sess.StdinPipe()
	outp, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	var seen bytes.Buffer
	buf := make([]byte, 4096)
	answered := map[string]bool{}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		n, err := outp.Read(buf)
		seen.Write(buf[:n])
		s := strings.ToLower(seen.String())
		for _, step := range []struct{ prompt, answer string }{{"current password:", old}, {"new password:", pw}, {"retype new password:", pw}} {
			if strings.Count(s, step.prompt) > 0 && !answered[step.prompt] && (step.prompt != "new password:" || answered["current password:"]) &&
				(step.prompt != "retype new password:" || answered["new password:"]) {
				answered[step.prompt] = true
				fmt.Fprintln(in, step.answer)
			}
		}
		if answered["retype new password:"] && (strings.Contains(s, "updated successfully") || err != nil) {
			break
		}
		if err != nil {
			break
		}
	}
	if !answered["retype new password:"] {
		t.Fatalf("%s: no forced password change at first login:\n%s", ip, indent(seen.String()))
	}
	t.Logf("%s: first login changed the factory password", ip)
}

// pushCLI configures a switch with `nv` over SSH.
func (r *day0Run) pushCLI(ip string, s day0.PlanSwitch) {
	t := r.t
	cl, err := r.sshClient(ip, newPassword)
	if err != nil {
		t.Fatalf("ssh %s@%s with the new password: %v", nvue.User, ip, err)
	}
	defer cl.Close()
	script := strings.Split(cumulusScript(s), "\n")
	var cmds []string
	for _, l := range script {
		if strings.HasPrefix(l, "nv ") && !strings.Contains(l, "aaa user") {
			cmds = append(cmds, l)
		}
	}
	sess, err := cl.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput("set -e; " + strings.Join(cmds, "; ") + "; nv show system -o json")
	if err != nil {
		t.Fatalf("%s: nv over SSH: %v\n%s", s.Hostname, err, indent(string(out)))
	}
	t.Logf("%s (%s): nv over SSH applied; nv show system:\n%s", s.Hostname, ip, indent(lastLines(string(out), 12)))
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

var _ = sort.Strings

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
