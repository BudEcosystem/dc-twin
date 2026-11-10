package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/critic"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// The critic verified against a stack with known defects (docs/spec/17-critic.md — Verifying the
// critic): every mutant of the reference stack must produce exactly its violations, and the
// unmutated stack none. Each case brings up its own twin and its own reference stack.

type mutantCase struct {
	mutant    string
	scenarios []string
	// want is the set of rules violated, exactly
	want []string
	// objectives that must fail
	failObjectives []string
	verdict        string
}

const (
	leak      = "../../examples/scenarios/nvl72-leak-response.yaml"
	isolation = "../../examples/scenarios/tray-leak-isolation.yaml"
	discovery = "../../examples/scenarios/bmc-discovery.yaml"

	safetyHealthy = "generic/safety/no-destructive-on-healthy"
	trayForbids   = "reference/default/playbook/generic/leak/tray-detector/forbids"
	rackForbids   = "reference/default/playbook/generic/leak/rack-point/forbids"
	trayMitigated = "reference/default/playbook/generic/leak/tray-detector/mitigated"
)

var mutantCases = []mutantCase{
	{mutant: "", scenarios: []string{discovery, leak, isolation}, verdict: "passed"},
	{mutant: "wrong-tray", scenarios: []string{leak}, want: []string{safetyHealthy, trayForbids}, failObjectives: []string{"reference/default/no-collateral"}, verdict: "failed"},
	{mutant: "rack-leak-poweroff", scenarios: []string{leak}, want: []string{safetyHealthy, rackForbids, trayForbids}, failObjectives: []string{"reference/default/no-collateral"}, verdict: "failed"},
	{mutant: "slow", scenarios: []string{leak}, want: []string{trayMitigated}, verdict: "failed"},
	{mutant: "ignore-leak", scenarios: []string{leak}, want: []string{trayMitigated}, verdict: "failed"},
	{mutant: "reset-storm", scenarios: []string{leak}, want: []string{"generic/restraint/power-action-storm", "generic/restraint/repeated-identical-write"}, verdict: "passed"},
	{mutant: "chatty", scenarios: []string{leak}, want: []string{"generic/restraint/request-rate-ceiling"}, failObjectives: []string{"generic/restraint/requests-per-device-minute"}, verdict: "passed"},
	{mutant: "session-leak", scenarios: []string{leak}, want: []string{"generic/restraint/repeated-identical-write"}, failObjectives: []string{"generic/hygiene/sessions-closed"}, verdict: "passed"},
	{mutant: "eager-isolation", scenarios: []string{leak}, want: []string{trayForbids}, verdict: "failed"},
	{mutant: "no-isolation", scenarios: []string{isolation}, verdict: "failed"},
}

func TestCriticCatchesEveryMutant(t *testing.T) {
	if testing.Short() {
		t.Skip("each mutant runs a reference stack and a twin for one to three minutes")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "refstack")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/refstack").CombinedOutput(); err != nil {
		t.Fatalf("build refstack: %v\n%s", err, out)
	}
	only := os.Getenv("MUTANTS")
	for _, mc := range mutantCases {
		mc := mc
		name := mc.mutant
		if name == "" {
			name = "none"
		}
		if only != "" && !strings.Contains(","+only+",", ","+name+",") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tw, _ := upWithRefstack(t, bin, mc.mutant)
			res, err := critic.DefaultPolicy(tw)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			failedObj := map[string]bool{}
			verdict := "passed"
			for _, path := range mc.scenarios {
				result, j, err := critic.RunScenario(context.Background(), tw, load(t, path), critic.RunOptions{Policy: res, Subject: "refstack+" + name, Log: testWriter{t}})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%s: scenario %s\n%s", name, result.Verdict, j.Text())
				for i := range j.Violations {
					got[j.Violations[i].Rule] = true
					if i < 3 {
						t.Logf("\n%s", j.Explain(i))
					}
				}
				for _, o := range j.Objectives {
					if o.Result == "fail" {
						failedObj[o.ID] = true
					}
				}
				if j.Verdict != "passed" {
					verdict = j.Verdict
				}
				if out := os.Getenv("CRITIC_OUT"); out != "" {
					critic.Save(filepath.Join(out, name), j)
				}
				// the judgement read back from the log on disk is the same judgement (D64)
				tw.Engine.FlushLog()
				evs, err := critic.ReadLog(critic.StateDir(tw))
				if err != nil {
					t.Fatal(err)
				}
				off, err := critic.Judge(critic.EventsOfRun(evs, j.Run.ID), res, critic.Options{Fidelity: result.Report, Subject: "refstack+" + name})
				if err != nil {
					t.Fatal(err)
				}
				if a, b := ruleSet(j), ruleSet(off); a != b || j.Verdict != off.Verdict {
					t.Errorf("offline re-judgement differs: live %s %s, offline %s %s", j.Verdict, a, off.Verdict, b)
				}
			}
			want := map[string]bool{}
			for _, w := range mc.want {
				want[w] = true
			}
			if keys(got) != keys(want) {
				t.Errorf("violated rules: got %s, want exactly %s", keys(got), keys(want))
			}
			for _, o := range mc.failObjectives {
				if !failedObj[o] {
					t.Errorf("objective %s should fail; failed: %s", o, keys(failedObj))
				}
			}
			if verdict != mc.verdict {
				t.Errorf("verdict %s, want %s", verdict, mc.verdict)
			}
		})
	}
}

func ruleSet(j *critic.Judgement) string {
	m := map[string]bool{}
	for _, v := range j.Violations {
		m[v.Rule] = true
	}
	return keys(m)
}

func keys(m map[string]bool) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return "[" + strings.Join(out, " ") + "]"
}

// upWithRefstack starts the reference stack (with a mutant) and the nvl72-refstack estate on
// addresses of their own.
func upWithRefstack(t *testing.T, bin, mutant string) (*twin.Twin, string) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	reg, _ := compile.Registry()
	est, _, err := compile.LoadEstate("../../examples/estates/nvl72-refstack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	gateway, api, broker, dhcp := freeAddr(t, "tcp"), freeAddr(t, "tcp"), freeAddr(t, "tcp"), freeAddr(t, "udp")
	rewireStack(est, api, broker, dhcp)
	logf, _ := os.Create(filepath.Join(dir, "refstack.log"))
	args := []string{"--gateway", gateway, "--api", api, "--broker", broker, "--dhcp", dhcp,
		"--expected", filepath.Join(state, "stack-inputs", "expected-machines.json")}
	if mutant != "" {
		args = append(args, "--mutant", mutant)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			if len(b) > 4000 {
				b = b[len(b)-4000:]
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
	return tw, api
}

func rewireStack(est *schema.Estate, api, broker, dhcp string) {
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
}

// TestCriticOnTwinScenarios judges the twin-only scenarios: with no stack installed, no rule
// blames the software under test (the critic never blames the SUT for the twin's own
// behaviour), and every judgement keeps its scenario's verdict.
func TestCriticOnTwinScenarios(t *testing.T) {
	tw := up(t, "../../examples/estates/lab-model.yaml", conformanceAdapters, nil)
	res, err := critic.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range []string{"feed-loss-redundancy", "xid-and-switch-tray", "bmc-and-firmware", "cdu-flow-loss", "amd-intel-faults"} {
		result, j, err := critic.RunScenario(context.Background(), tw, load(t, "../../examples/scenarios/twin/"+sc+".yaml"), critic.RunOptions{Policy: res})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s\n%s", sc, j.Text())
		if len(j.Violations) != 0 {
			t.Errorf("%s: no stack, no violations: %v", sc, ruleSet(j))
		}
		if j.Verdict != result.Verdict {
			t.Errorf("%s: judgement %s, scenario %s", sc, j.Verdict, result.Verdict)
		}
		if len(j.Faults) == 0 && sc != "feed-loss-redundancy" {
			t.Errorf("%s: the judgement lists the scenario's faults", sc)
		}
	}
}
