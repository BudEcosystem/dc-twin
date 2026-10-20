// Package e2e runs the scenario suites end to end: twin-only scenarios against the lab estate
// with every non-impersonating adapter up, the virtual-clock week on the mixed hall (twice, to
// show it replays exactly), and the stack scenarios against the reference stack, which reaches
// the twin only through relayed DHCP, Redfish through the site gateway and MQTT.
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/BudEcosystem/dc-twin/internal/adapter/bms"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/ipmi"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/metrics"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/nvue"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/snmp"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/ufm"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/scenario"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

var conformanceAdapters = []string{"redfish", "dhcp", "bms", "snmp", "ipmi", "ufm", "nvue"}

func up(t *testing.T, estate string, adapters []string, mutate func(*schema.Estate)) *twin.Twin {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(estate)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(est)
	}
	tw, err := twin.UpEstate(est, estate, reg, twin.Options{StateDir: t.TempDir(), Adapters: adapters})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	return tw
}

func load(t *testing.T, path string) *schema.Scenario {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sc schema.Scenario
	if err := schema.Decode(data, &sc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return &sc
}

func run(t *testing.T, tw *twin.Twin, path, want string) *scenario.Result {
	t.Helper()
	res := scenario.Run(context.Background(), tw, load(t, path), scenario.Options{Log: testWriter{t}})
	t.Logf("%s: %s (%s) %s", res.Scenario, strings.ToUpper(res.Verdict), res.Took, res.Reason)
	if res.Verdict != want {
		for _, s := range res.Steps {
			t.Logf("  step %d %s %s %v", s.Index, s.Kind, s.Status, s.Detail)
		}
		t.Fatalf("%s: verdict %s, want %s", res.Scenario, res.Verdict, want)
	}
	if res.Report == nil || len(res.Report.NotModelled) == 0 {
		t.Fatalf("%s: a result is never reported without its fidelity report", res.Scenario)
	}
	return res
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func TestTwinScenarios(t *testing.T) {
	tw := up(t, "../../examples/estates/lab-model.yaml", conformanceAdapters, nil)
	for _, sc := range []string{"feed-loss-redundancy", "xid-and-switch-tray", "bmc-and-firmware", "cdu-flow-loss", "amd-intel-faults"} {
		run(t, tw, "../../examples/scenarios/twin/"+sc+".yaml", "passed")
	}
}

func TestImpersonationIsLabelled(t *testing.T) {
	tw := up(t, "../../examples/estates/lab-model.yaml", []string{"redfish", "metrics"}, nil)
	run(t, tw, "../../examples/scenarios/twin/bmc-and-firmware.yaml", "impersonated")
}

func TestUnmetRequirementIsNotRun(t *testing.T) {
	tw := up(t, "../../examples/estates/lab-model.yaml", []string{"redfish"}, nil)
	run(t, tw, "../../examples/scenarios/nvl72-leak-response.yaml", "not-run")
}

func TestVirtualWeekReplaysExactly(t *testing.T) {
	if testing.Short() {
		t.Skip("a simulated week takes about a minute")
	}
	digest := func() string {
		tw := up(t, "../../examples/estates/mixed-hall.yaml", []string{"redfish"}, nil)
		run(t, tw, "../../examples/scenarios/twin/virtual-week.yaml", "passed")
		h := sha256.New()
		for _, e := range tw.Engine.Events(0, func(e schema.Event) bool { return e.Kind == "fault" || e.Kind == "state" }) {
			fmt.Fprintf(h, "%s|%s|%s|%v\n", e.T, e.Path, e.Message, e.State)
		}
		tw.Down()
		return fmt.Sprintf("%x", h.Sum(nil))[:16]
	}
	a, b := digest(), digest()
	t.Logf("replay digests: %s %s", a, b)
	if a != b {
		t.Fatal("a twin-only run on the virtual clock must replay exactly (D22)")
	}
}

// TestReferenceStack runs the stack scenarios against cmd/refstack.
func TestReferenceStack(t *testing.T) {
	if testing.Short() {
		t.Skip("the reference stack scenarios take about a minute and a half")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "refstack")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/refstack").CombinedOutput(); err != nil {
		t.Fatalf("build refstack: %v\n%s", err, out)
	}
	state := filepath.Join(dir, "state")
	reg, _ := compile.Registry()
	est, _, err := compile.LoadEstate("../../examples/estates/nvl72-refstack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Every address is chosen now, so a twin or stack already running on the estate's defaults
	// (18443, 18080, 18830, 16767) does not collide with this run.
	gateway, api, broker, dhcp := freeAddr(t, "tcp"), freeAddr(t, "tcp"), freeAddr(t, "tcp"), freeAddr(t, "udp")
	for name, n := range est.Spec.Networks {
		if n.Server != "" {
			n.Server = dhcp
			est.Spec.Networks[name] = n
		}
	}
	if est.Spec.Facility.BMS != nil && est.Spec.Facility.BMS.Broker != "" {
		est.Spec.Facility.BMS.Broker = broker
	}
	for i := range est.Spec.Stacks {
		if est.Spec.Stacks[i].Outputs != nil {
			est.Spec.Stacks[i].Outputs["refstack"] = "http://" + api
			est.Spec.Stacks[i].Outputs["dhcpServer"] = dhcp
			est.Spec.Stacks[i].Outputs["exchangeBroker"] = broker
		}
	}
	logf, _ := os.Create(filepath.Join(dir, "refstack.log"))
	cmd := exec.Command(bin, "--gateway", gateway, "--api", api, "--broker", broker, "--dhcp", dhcp,
		"--expected", filepath.Join(state, "stack-inputs", "expected-machines.json"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			if len(b) > 6000 {
				b = b[len(b)-6000:]
			}
			t.Logf("reference stack log (tail):\n%s", b)
		}
	})
	time.Sleep(time.Second)
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: state, Adapters: conformanceAdapters, GatewayAddr: gateway})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	run(t, tw, "../../examples/scenarios/bmc-discovery.yaml", "passed")
	run(t, tw, "../../examples/scenarios/nvl72-leak-response.yaml", "passed")
	run(t, tw, "../../examples/scenarios/tray-leak-isolation.yaml", "passed")
	resp, err := http.Get("http://" + api + "/v1/machines")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var ms []map[string]any
	json.Unmarshal(body, &ms)
	types := map[string]int{}
	for _, m := range ms {
		types[fmt.Sprint(m["hardware_type"])]++
	}
	t.Logf("reference stack ingested %d BMCs: %v", len(ms), types)
	if types["gb300-tray"] != 18 || types["nvswitch-tray"] != 9 || types["bluefield"] != 18 {
		t.Fatalf("the stack should identify every NVL72 BMC by its Redfish identity strings: %v", types)
	}
}

// freeAddr is a loopback address with a port the OS has just handed out and released.
func freeAddr(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().String()
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}
