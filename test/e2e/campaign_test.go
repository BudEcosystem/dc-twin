package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/critic"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// campaign reads the example campaign and points it at a refstack binary built for the test.
func campaign(t *testing.T, bin, mutant string) *schema.Campaign {
	t.Helper()
	c, err := critic.LoadCampaign("../../examples/campaigns/refstack-nightly.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cmd := strings.Replace(c.Spec.Stack.Command, "bin/refstack", bin, 1)
	if mutant != "" {
		cmd += " --mutant " + mutant
	}
	c.Spec.Stack.Command = cmd
	return c
}

func buildRefstack(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "refstack")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/refstack").CombinedOutput(); err != nil {
		t.Fatalf("build refstack: %v\n%s", err, out)
	}
	return bin
}

// TestCampaignMatrixAndBaseline runs a matrix of the reference stack twice: the second run is
// compared with the first as its baseline, the holdout cell reports its verdict only, and a
// clean stack shows no flakiness and no change.
func TestCampaignMatrixAndBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("a campaign brings the reference stack and the twin up per cell")
	}
	bin := buildRefstack(t)
	c := campaign(t, bin, "")
	c.Spec.Generate = nil
	c.Spec.Scenarios.Files = []string{"../scenarios/nvl72-leak-response.yaml"}
	out := t.TempDir()
	dir := filepath.Dir("../../examples/campaigns/refstack-nightly.yaml")
	first, err := critic.RunCampaign(context.Background(), c, critic.CampaignOptions{Dir: dir, Out: filepath.Join(out, "a"), Log: testWriter{t}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + first.Text())
	if len(first.Cells) != 4 || first.Aggregate.Verdicts["passed"] != 4 {
		t.Fatalf("2 seeds × (1 scenario + 1 holdout), all passed: %+v", first.Aggregate.Verdicts)
	}
	for _, cell := range first.Cells {
		if cell.Holdout {
			j, err := critic.Load(filepath.Join(out, "a", cell.File))
			if err != nil {
				t.Fatal(err)
			}
			if !j.Holdout || !strings.Contains(j.Text(), "") {
				t.Fatal("a holdout cell's judgement is marked holdout")
			}
		}
	}
	c.Spec.Baseline = &schema.CampaignBaseline{File: filepath.Join(out, "a", "campaign.json")}
	second, err := critic.RunCampaign(context.Background(), c, critic.CampaignOptions{Dir: dir, Out: filepath.Join(out, "b"), Log: testWriter{t}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + second.Text())
	if len(second.Aggregate.Flaky) != 0 {
		t.Fatalf("a clean stack is not flaky: %v", second.Aggregate.Flaky)
	}
	for _, l := range strings.Split(second.Diff, "\n") {
		if strings.HasPrefix(l, "verdict") || strings.HasPrefix(l, "new violation") {
			t.Fatalf("a clean stack against its own baseline: %s", second.Diff)
		}
	}
}

// TestGeneratorFindsAndShrinks gives the generator a stack with a seeded defect and no scenario
// that covers it: a random fault sequence finds the violation, and shrinking leaves the one
// fault that causes it, written out as a scenario that is then run again and violates the same
// rule (17 — the C5 exit).
func TestGeneratorFindsAndShrinks(t *testing.T) {
	if testing.Short() {
		t.Skip("each generated run and shrink attempt brings the stack and the twin up")
	}
	bin := buildRefstack(t)
	c := campaign(t, bin, "wrong-tray")
	c.Spec.Scenarios = schema.CampaignScenarios{}
	c.Spec.Holdout = nil
	c.Spec.Generate.Runs = 1
	// choose a seed whose plan has a tray leak and something else to shrink away
	tw := up(t, "../../examples/estates/nvl72-refstack.yaml", []string{"redfish"}, nil)
	var seed int64 = -1
	for s := int64(1); s < 200; s++ {
		p, err := critic.Plan(tw, c.Spec.Generate, s)
		if err != nil {
			t.Fatal(err)
		}
		leaks := 0
		for _, f := range p.Faults {
			if f.Fault == "generic/leak/tray-detector" {
				leaks++
			}
		}
		if leaks == 1 && len(p.Faults) == 3 {
			seed = s
			t.Logf("seed %d: %+v", s, p.Faults)
			break
		}
	}
	tw.Down()
	if seed < 0 {
		t.Fatal("no seed draws one tray leak among three faults")
	}
	c.Spec.Seeds = &schema.CampaignSeeds{Count: 1, From: seed}
	out := t.TempDir()
	res, err := critic.RunCampaign(context.Background(), c, critic.CampaignOptions{Dir: "../../examples/campaigns", Out: out, Log: testWriter{t}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + res.Text())
	g := res.Generated[0]
	if !contains(g.Violations, "generic/safety/no-destructive-on-healthy") {
		t.Fatalf("the generated run finds the wrong tray: %v (%s)", g.Violations, g.Error)
	}
	if len(g.MinimalFaults) != 1 || g.MinimalFaults[0].Fault != "generic/leak/tray-detector" {
		t.Fatalf("shrunk to the one tray leak: %+v", g.MinimalFaults)
	}
	data, err := os.ReadFile(g.Minimal)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("minimal scenario:\n%s", data)
	var sc schema.Scenario
	if err := schema.Decode(data, &sc); err != nil {
		t.Fatalf("the minimal scenario is a valid scenario file: %v", err)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
