//go:build linux

package linuxnet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/day0"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// GPU Foundry as the day-0 tool (its M15, G2 to G4): `foundry-site day0`, Foundry's own binary,
// unchanged, run as a site runs it on its management network: in the tooling's namespace at
// 10.90.1.10 (oob-agg-1:eth127), serving its ZTP there, writing Kea's configuration, which the site's
// Kea (the tooling's, at 10.90.0.10) is reloaded with on every change, reading only
// stack-inputs/day0 and the switches. The twin's OOB fabric is the estate's (bringUp: twin), which
// Foundry observes. Each run is judged by the critic's day0/fabric.
//
// DCTWIN_FOUNDRY names the binary (scripts/linux-substrate-e2e.sh: FOUNDRY=…).
type foundryRun struct {
	r    *day0Run
	work string
	out  *lockedBuf
	cmd  *exec.Cmd
	done chan error
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func foundryCase(name, nosName, release string) day0Case {
	// Foundry serves ZTP where the harness's own HTTP server would: the harness's is off
	return day0Case{estate: "linuxnet-day0-foundry-" + name, nos: nosName, install: "preinstalled", ztp: "enabled",
		cumulus: release, boot: "40m", noTooling: true}
}

// startFoundry runs foundry-site day0 against the running estate. keep: it keeps cycling after
// convergence (a factory reset's day 0 again, a fault's recovery); extra are flags of the case's.
func startFoundry(t *testing.T, r *day0Run, keep bool, extra ...string) *foundryRun {
	t.Helper()
	bin := os.Getenv("DCTWIN_FOUNDRY")
	if bin == "" {
		t.Skip("DCTWIN_FOUNDRY is not set (scripts/linux-substrate-e2e.sh: FOUNDRY=<foundry-site>)")
	}
	f := &foundryRun{r: r, out: &lockedBuf{}, done: make(chan error, 1)}
	f.work = filepath.Join(filepath.Dir(day0Binaries(t)), "foundry-"+r.c.estate+"-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(filepath.Join(f.work, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	inputs := filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", "day0")
	kea := filepath.Join(f.work, "kea-dhcp4.json")
	leases := filepath.Join(f.work, "kea-leases4.csv")
	args := []string{"netns", "exec", r.p.name, bin, "day0", "--inputs=" + inputs, "--listen=" + day0Server + ":8080",
		"--base=http://" + day0Server + ":8080", "--kea-config=" + kea, "--kea-leases=" + leases, "--kea-interface=svc0", "--observe=oob",
		"--management-dhcp=10.90.0.10", "--state=" + filepath.Join(f.work, "state"), "--interval=10s", "--timeout=25m"}
	if keep {
		args = append(args, "--keep-running")
	}
	for _, x := range extra {
		if strings.HasPrefix(x, "--observe=") {
			// the case names what Foundry observes (none, where the OOB fabric is the factory one)
			for i, a := range args {
				if strings.HasPrefix(a, "--observe=") {
					args = append(args[:i], args[i+1:]...)
					break
				}
			}
			if x == "--observe=" {
				continue
			}
		}
		args = append(args, x)
	}
	f.cmd = exec.Command("ip", args...)
	f.cmd.Stdout, f.cmd.Stderr = f.out, f.out
	if err := f.cmd.Start(); err != nil {
		t.Fatalf("foundry-site: %v", err)
	}
	go func() { f.done <- f.cmd.Wait() }()
	ctx, cancel := context.WithCancel(context.Background())
	go f.syncKea(ctx, kea)
	go f.mirrorLeases(ctx, kea, leases)
	t.Cleanup(func() {
		cancel()
		_ = f.cmd.Process.Kill()
		if t.Failed() {
			t.Logf("foundry-site's output (tail):\n%s", indent(lastLines(f.out.String(), 60)))
			if b, err := os.ReadFile(filepath.Join(f.work, "state", "switches.json")); err == nil {
				t.Logf("switches.json:\n%s", indent(string(b)))
			}
		}
	})
	return f
}

// syncKea reloads the site's Kea with Foundry's configuration whenever Foundry rewrites it, with
// the site's logger added (Foundry writes none): the file into Kea's container, then SIGHUP.
func (f *foundryRun) syncKea(ctx context.Context, path string) {
	var last [32]byte
	for ctx.Err() == nil {
		time.Sleep(time.Second)
		b, err := os.ReadFile(path)
		if err != nil || len(b) == 0 {
			continue
		}
		sum := sha256.Sum256(b)
		if sum == last {
			continue
		}
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) != nil {
			continue // mid-write
		}
		if d, ok := cfg["Dhcp4"].(map[string]any); ok && d["loggers"] == nil {
			d["loggers"] = []any{map[string]any{"name": "kea-dhcp4", "severity": "INFO", "output-options": []any{map[string]any{"output": "/var/log/kea-dhcp4.log"}}}}
		}
		out, _ := json.Marshal(cfg)
		if _, code, err := f.r.d.Exec(context.Background(), f.r.kea.ID, "sh", "-c",
			"mkdir -p /var/lib/kea && printf '%s' \"$1\" > /etc/kea/kea-dhcp4.json && kill -HUP 1", "sh", string(out)); err == nil && code == 0 {
			last = sum
		}
	}
}

// mirrorLeases keeps a copy of Kea's lease file where Foundry reads it (--kea-leases): Kea runs in
// its container, Foundry beside it. The file is the one Foundry's configuration names (Kea's own
// default otherwise), replaced whole so Foundry never reads half of it.
func (f *foundryRun) mirrorLeases(ctx context.Context, config, to string) {
	for ctx.Err() == nil {
		time.Sleep(3 * time.Second) // one exec per tick: podman is shared (Foundry cycles every 10s)
		src := "/var/lib/kea/kea-leases4.csv"
		var cfg struct {
			Dhcp4 struct {
				LeaseDatabase struct {
					Name string `json:"name"`
				} `json:"lease-database"`
			}
		}
		if b, err := os.ReadFile(config); err == nil && json.Unmarshal(b, &cfg) == nil && cfg.Dhcp4.LeaseDatabase.Name != "" {
			src = cfg.Dhcp4.LeaseDatabase.Name
		}
		out, code, err := f.r.d.Exec(context.Background(), f.r.kea.ID, "cat", src)
		if err != nil || code != 0 {
			continue // no lease yet
		}
		if os.WriteFile(to+".tmp", []byte(out), 0o644) == nil {
			_ = os.Rename(to+".tmp", to)
		}
	}
}

// wait is foundry-site's exit (0: every frontend switch converged), or the deadline's.
func (f *foundryRun) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-f.done:
		return err
	case <-time.After(d):
		return fmt.Errorf("still running after %s", d)
	}
}

// reports is what Foundry says of each switch: switches.json.
func (f *foundryRun) reports() string {
	b, _ := os.ReadFile(filepath.Join(f.work, "state", "switches.json"))
	return string(b)
}

// Cumulus Linux 5.6.0 from the factory: Foundry's ZTP, its first push over SSH, convergence; then
// a leaf factory-reset (dctwin switch reset --factory), whose day 0 Foundry runs again unasked.
func TestDay0FoundryCumulus(t *testing.T) {
	r := startDay0(t, foundryCase("cumulus", nos.NVUEFRR, "5.6.0"))
	f := startFoundry(t, r, true)
	took := r.waitConverged(25 * time.Minute)
	t.Logf("Foundry: every factory 5.6.0 switch converged %s after it started", took.Round(time.Second))
	var leaf string
	for _, e := range r.factory() {
		if e.Role == "leaf" {
			leaf = e.Path
			break
		}
	}
	if err := r.sub.FactoryReset(context.Background(), leaf); err != nil {
		t.Fatalf("factory reset of %s: %v", leaf, err)
	}
	waitFor(t, 3*time.Minute, leaf+" back in factory state", func() bool {
		v, _ := r.sub.Day0()
		for _, x := range v {
			if x.Path == leaf && !x.Converged {
				return true
			}
		}
		return false
	})
	took = r.waitConverged(20 * time.Minute)
	t.Logf("Foundry ran %s's day 0 again unasked: converged %s after the reset", filepath.Base(leaf), took.Round(time.Second))
	_ = f
	r.judge()
}

// Cumulus Linux 5.18.3: 5.15+'s grammar.
func TestDay0FoundryCumulus518(t *testing.T) {
	r := startDay0(t, foundryCase("cumulus518", nos.NVUEFRR, "5.18.3"))
	f := startFoundry(t, r, false)
	if err := f.wait(t, 26*time.Minute); err != nil {
		t.Fatalf("foundry-site: %v", err)
	}
	took := r.waitConverged(2 * time.Minute)
	t.Logf("Foundry: every factory 5.18.3 switch converged (oracle %s after Foundry exited 0)", took.Round(time.Second))
	r.judge()
}

// sonic-vs from the factory: Foundry's SONiC driver (option 67, CONFIG_DB).
func TestDay0FoundrySonic(t *testing.T) {
	r := startDay0(t, foundryCase("sonic", nos.SonicVS, ""))
	f := startFoundry(t, r, false)
	if err := f.wait(t, 26*time.Minute); err != nil {
		t.Fatalf("foundry-site: %v", err)
	}
	took := r.waitConverged(2 * time.Minute)
	t.Logf("Foundry: every factory sonic-vs switch converged (oracle %s after Foundry exited 0)", took.Round(time.Second))
	r.judge()
}

// A cable swap before day 0 (generic/cabling cable-swap between two leaf-to-spine-1 cables):
// Foundry must report both swapped links before a session comes up on either, and not converge
// until they are put back.
func TestDay0FoundryCableSwap(t *testing.T) {
	r := startDay0(t, foundryCase("swap", nos.NVUEFRR, "5.6.0"))
	a, b := leafSpineCables(t, r)
	src := schema.EventSource{Class: "operator", Principal: "test"}
	if _, err := r.tw.Engine.Inject(a, "generic/cabling/cable-swap", map[string]any{"with": b}, "swap", src); err != nil {
		t.Fatal(err)
	}
	f := startFoundry(t, r, true)
	time.Sleep(8 * time.Minute)
	rep := f.reports()
	t.Logf("Foundry's report under the swap:\n%s", indent(lastLines(rep, 40)))
	for _, p := range []string{a, b} {
		host := filepath.Base(strings.SplitN(p, ":", 2)[0])
		if !strings.Contains(rep, host) {
			t.Errorf("Foundry's report names no swapped link of %s", host)
		}
	}
	if _, err := r.tw.Engine.Clear("swap", "", src); err != nil {
		t.Fatal(err)
	}
	took := r.waitConverged(15 * time.Minute)
	t.Logf("the cables put back: converged %s later", took.Round(time.Second))
	r.judge()
}

// The other generic/cabling faults on one leaf-to-spine cable, one after another on a converged
// fabric, each cleared before the next: the session goes down, Foundry reports it, and the fabric
// converges again once it clears.
func TestDay0FoundryCablingFaults(t *testing.T) {
	r := startDay0(t, foundryCase("cabling", nos.NVUEFRR, "5.6.0"))
	f := startFoundry(t, r, true)
	r.waitConverged(25 * time.Minute)
	a, _ := leafSpineCables(t, r)
	src := schema.EventSource{Class: "operator", Principal: "test"}
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
		if _, err := r.tw.Engine.Inject(a, c.fault, c.params, "c", src); err != nil {
			t.Fatalf("%s: %v", c.fault, err)
		}
		time.Sleep(90 * time.Second)
		t.Logf("%s: Foundry's report:\n%s", c.fault, indent(lastLines(f.reports(), 12)))
		if _, err := r.tw.Engine.Clear("c", "", src); err != nil {
			t.Fatal(err)
		}
		took := r.waitConverged(10 * time.Minute)
		t.Logf("%s cleared: converged %s later", c.fault, took.Round(time.Second))
	}
	r.judge()
}

// generic/day0's faults, one per switch before it boots: the lease without its ZTP option, the
// ZTP server unreachable, the script cut short, NTP unreachable. None of the first three can
// finish its day 0 while its fault holds; cleared, Foundry brings every switch up.
func TestDay0FoundryDay0Faults(t *testing.T) {
	r := startDay0(t, foundryCase("faults", nos.NVUEFRR, "5.6.0"))
	fac := r.factory()
	src := schema.EventSource{Class: "operator", Principal: "test"}
	faults := []string{"generic/day0/ztp-dhcp-no-option", "generic/day0/ztp-server-unreachable", "generic/day0/ztp-script-error", "generic/day0/ntp-unreachable"}
	for i, e := range fac {
		if i >= len(faults) {
			break
		}
		if _, err := r.tw.Engine.Inject(e.Path, faults[i], nil, fmt.Sprintf("f%d", i), src); err != nil {
			t.Fatalf("%s: %s: %v", e.Hostname, faults[i], err)
		}
		t.Logf("%s: %s", e.Hostname, faults[i])
	}
	f := startFoundry(t, r, true)
	time.Sleep(8 * time.Minute)
	v, _ := r.sub.Day0()
	for _, x := range v {
		t.Logf("%s converged=%v (%s)", filepath.Base(x.Path), x.Converged, strings.Join(x.Diffs, "; "))
	}
	t.Logf("Foundry's report under the faults:\n%s", indent(lastLines(f.reports(), 40)))
	for i := range faults {
		if _, err := r.tw.Engine.Clear(fmt.Sprintf("f%d", i), "", src); err != nil {
			t.Fatal(err)
		}
	}
	took := r.waitConverged(20 * time.Minute)
	t.Logf("faults cleared: converged %s later, with no reset from the test", took.Round(time.Second))
	r.judge()
}

// leafSpineCables are two leaves' cables to spine-1, as fault targets (<path>:<port>).
func leafSpineCables(t *testing.T, r *day0Run) (string, string) {
	t.Helper()
	var a, b string
	for _, c := range r.design.Cables {
		if !strings.HasSuffix(c.ADevice, "spine-1") && !strings.HasSuffix(c.BDevice, "spine-1") {
			continue
		}
		leaf := c.ADevice + ":" + c.APort
		if strings.HasSuffix(c.ADevice, "spine-1") {
			leaf = c.BDevice + ":" + c.BPort
		}
		host, port, _ := strings.Cut(leaf, ":")
		for _, s := range r.design.Switches {
			if s.Hostname == host {
				leaf = s.Path + ":" + port
			}
		}
		if strings.Contains(leaf, "leaf-r01") {
			a = leaf
		} else if strings.Contains(leaf, "leaf-r02") {
			b = leaf
		}
	}
	if a == "" || b == "" {
		t.Fatalf("no two leaves' cables to spine-1 in the design")
	}
	return a, b
}

// A DSX site's day 0 with Foundry as the tool (G5's network half, on 18 — Z7d's estate,
// tap-day0-dsx.yaml): the OOB fabric's server-rack management switches leave the factory and
// Foundry brings them up through its own DHCP and ZTP, leased through the aggregation switch the
// twin configures; it renders each one's BMC relay to the site's BMC DHCP (--relay-target) beside
// its switch-management relay. Every server BMC then leases from the BMC DHCP (a Kea standing in
// for NICo's forge-dhcp-server, one subnet per rack, answering the relay's giaddr) through the
// relays Foundry rendered, with nothing configured by hand.
func TestDay0FoundryDSX(t *testing.T) {
	d, err := nosRuntime(t)
	if err != nil {
		t.Skipf("no Docker Engine: %v", err)
	}
	bmcName := "dctwin-bmcdhcp-" + strconv.Itoa(os.Getpid())
	bmc := startKeaContainer(t, d, bmcName)
	c := foundryCase("dsx", nos.NVUEFRR, "5.14.0")
	c.file, c.bmcDHCP = "tap-day0-dsx.yaml", bmcName
	r := startDay0(t, c)
	var leaves []day0.Switch
	var aggLo string
	for _, s := range r.design.Switches {
		switch {
		case s.Factory && s.Role == "oob":
			leaves = append(leaves, s)
		case strings.Contains(s.Hostname, "oob-agg"):
			aggLo = strings.TrimSuffix(s.Loopback, "/32")
		}
	}
	// the aggregation switch carries VRF OOB, the BMC DHCP in it, EVPN on its underlay sessions
	// with the leaves, as Foundry renders the leaves' side (--relay-target …@OOB:2000:4002)
	aggregationOOB(t, r, leaves, aggLo, false)
	// the OOB fabric is the factory one: Foundry observes none (its aggregation switch, the
	// twin's, is factory: false in the design)
	f := startFoundry(t, r, false, fmt.Sprintf("--relay-target=bmc=10.90.2.10@%s:%d:%d", day0.NVCMOOBVRF, day0.NVCMOOBL3VNI, day0.NVCMOOBL3VLAN), "--observe=")
	if err := f.wait(t, 26*time.Minute); err != nil {
		t.Fatalf("foundry-site: %v", err)
	}
	took := r.waitConverged(2 * time.Minute)
	t.Logf("Foundry: both factory OOB leaves converged (oracle %s after Foundry exited 0)", took.Round(time.Second))
	var macs []string
	for _, ep := range r.sub.Plan().Endpoints {
		// the BMCs behind the factory leaves (rack n01's are on the twin's management switches,
		// leased by the site's management DHCP)
		if ep.Switch != nil && ep.Switch.Config.Factory != nil {
			macs = append(macs, strings.ToLower(ep.MAC.String()))
		}
	}
	if len(macs) == 0 {
		t.Fatal("no BMC behind a factory leaf")
	}
	var leases string
	waitFor(t, 5*time.Minute, "every BMC leased through Foundry's relays", func() bool {
		leases, _, _ = d.Exec(context.Background(), bmc.ID, "cat", "/var/lib/kea/kea-leases4.csv")
		for _, m := range macs {
			if !strings.Contains(strings.ToLower(leases), m) {
				return false
			}
		}
		return true
	})
	t.Logf("the BMC DHCP's leases (%d BMCs):\n%s", len(macs), indent(leases))
	_ = f
	r.judge()
}
