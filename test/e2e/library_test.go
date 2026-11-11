package e2e

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/critic"
)

// The scenario library (docs/spec/17-critic.md — The scenario library): every stack-neutral
// scenario, in an order that leaves each the estate it needs, judged by the critic.

const lib = "../../examples/scenarios/library/"

var libraryOrder = []struct {
	path string
	want string // the judgement's verdict against the clean reference stack
}{
	{"ingest/discovery.yaml", "passed"},
	{"bmc/unreachable.yaml", "passed"},
	{"fabric/switch-tray-failure.yaml", "passed"}, // before any tray is shut down: it expects the domain formed again
	{"leak/tray-detector.yaml", "passed"},
	{"leak/rack-point.yaml", "passed"},
	{"compound/leak-and-bmc.yaml", "passed"},
	{"thermal/cdu-flow-loss.yaml", "passed"},
	{"accelerator/xid-79.yaml", "not-run"},         // the reference stack maps no node alias
	{"lifecycle/release-sanitise.yaml", "not-run"}, // nor allocation
	{"power/feed-loss.yaml", "passed"},
	{"leak/isolation.yaml", "passed"}, // last: it leaves the rack isolated
}

func TestScenarioLibraryDecodes(t *testing.T) {
	files, _ := filepath.Glob(lib + "*/*.yaml")
	if len(files) != len(libraryOrder)+1 {
		t.Fatalf("%d library scenarios, the test orders %d (+ storage)", len(files), len(libraryOrder))
	}
	for _, f := range files {
		load(t, f)
	}
}

// TestScenarioLibraryOnTheReferenceStack runs the whole library against the clean reference
// stack: every scenario passes or is not-run for an alias the stack does not map, and no rule is
// violated. Then, on a stack with the wrong-tray defect, the scenarios that leak a tray fail and
// the critic names the healthy tray it shut down.
func TestScenarioLibraryOnTheReferenceStack(t *testing.T) {
	if testing.Short() {
		t.Skip("the library takes about ten minutes against the reference stack")
	}
	bin := buildRefstack(t)
	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		tw, _ := upWithRefstack(t, bin, "")
		res, err := critic.DefaultPolicy(tw)
		if err != nil {
			t.Fatal(err)
		}
		for _, sc := range libraryOrder {
			result, j, err := critic.RunScenario(context.Background(), tw, load(t, lib+sc.path), critic.RunOptions{Policy: res, Subject: "refstack"})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: scenario %s (%s)\n%s", sc.path, result.Verdict, result.Reason, j.Text())
			if j.Verdict != sc.want {
				for _, s := range result.Steps {
					t.Logf("  step %d %s %s %v", s.Index, s.Kind, s.Status, s.Detail)
				}
				t.Errorf("%s: judgement %s, want %s", sc.path, j.Verdict, sc.want)
			}
			if len(j.Violations) != 0 {
				t.Errorf("%s: the clean stack breaks no rule: %s", sc.path, ruleSet(j))
			}
		}
	})
	for _, p := range []string{"leak/tray-detector.yaml", "compound/leak-and-bmc.yaml"} {
		p := p
		t.Run("wrong-tray/"+p, func(t *testing.T) {
			t.Parallel()
			tw, _ := upWithRefstack(t, bin, "wrong-tray") // a fresh estate: the mutant leaves a healthy tray off
			res, err := critic.DefaultPolicy(tw)
			if err != nil {
				t.Fatal(err)
			}
			result, j, err := critic.RunScenario(context.Background(), tw, load(t, lib+p), critic.RunOptions{Policy: res, Subject: "refstack+wrong-tray"})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s\n%s", p, j.Text())
			got := ruleSet(j)
			if j.Verdict != "failed" || got != "["+safetyHealthy+" "+trayForbids+"]" {
				for _, s := range result.Steps {
					t.Logf("  step %d %s %s %v", s.Index, s.Kind, s.Status, s.Detail)
				}
				t.Errorf("%s: want failed with %s and %s, got %s %s", p, safetyHealthy, trayForbids, j.Verdict, got)
			}
		})
	}
}

// TestScenarioLibraryTwinOnly runs the library's twin-only scenario on a storage estate.
func TestScenarioLibraryTwinOnly(t *testing.T) {
	tw := up(t, "../../examples/estates/storage-lab.yaml", []string{"redfish"}, nil)
	res, _ := critic.Resolve()
	result, j, err := critic.RunScenario(context.Background(), tw, load(t, lib+"storage/drive-failure.yaml"), critic.RunOptions{Policy: res})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", j.Text())
	if result.Verdict != "passed" || j.Verdict != "passed" || len(j.Violations) != 0 || len(j.Faults) != 1 {
		t.Fatalf("storage/drive-failure: %s %s, %d violations, %d faults", result.Verdict, j.Verdict, len(j.Violations), len(j.Faults))
	}
}
