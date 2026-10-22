package control

import (
	"os"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const mixedHall = "../../examples/estates/mixed-hall.yaml"

func readMixedHall(t *testing.T) []byte {
	t.Helper()
	src, err := os.ReadFile(mixedHall)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// compileOps edits mixed-hall with ops and compiles the result.
func compileOps(t *testing.T, ops ...Op) (*compile.Result, []string, string, error) {
	t.Helper()
	src := readMixedHall(t)
	out, notes, err := editOps(src, ops)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "A mixed hall") {
		t.Fatal("the edit dropped the file's comments")
	}
	var est schema.Estate
	if err := schema.Decode(out, &est); err != nil {
		t.Fatal(err)
	}
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(&est, reg, compile.Options{HostArch: "aarch64"})
	return res, notes, string(out), err
}

func rackNamed(res *compile.Result, name string) bool {
	for _, r := range res.Model.Racks {
		if r.Name == name {
			return true
		}
	}
	return false
}

func TestRackAddCompilesAndCopiesParams(t *testing.T) {
	res, notes, _, err := compileOps(t,
		Op{Op: "rack.add", Row: "b", Name: "b04", Position: 4, Format: "generic/hgx-8gpu"},
		Op{Op: "rack.add", Row: "a", Name: "a05", Position: 5, Format: "nvidia/gb200-nvl72"})
	if err != nil {
		t.Fatalf("the edited estate should compile: %v", err)
	}
	if !rackNamed(res, "b04") || !rackNamed(res, "a05") {
		t.Fatal("added racks are missing from the compiled model")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "copied from b01") {
		t.Fatalf("params should be copied from a rack of the same format (b01), and only where it has some: %v", notes)
	}
}

func TestRackAddRefusesAnOccupiedPosition(t *testing.T) {
	if _, _, _, err := compileOps(t, Op{Op: "rack.add", Row: "b", Name: "b09", Position: 1, Format: "generic/hgx-8gpu"}); err == nil {
		t.Fatal("a rack in an occupied position must be refused")
	}
}

func TestRackAddNeedsAnExistingRow(t *testing.T) {
	if _, _, err := editOps(readMixedHall(t), []Op{{Op: "rack.add", Row: "z", Name: "z01", Position: 1, Format: "generic/cpu-rack"}}); err == nil {
		t.Fatal("a row the hall does not have must be refused")
	}
}

func TestRackRemoveTakesItOutOfItsLoop(t *testing.T) {
	res, notes, _, err := compileOps(t, Op{Op: "rack.remove", Name: "a02"})
	if err != nil {
		t.Fatalf("removing a02 should compile: %v", err)
	}
	if rackNamed(res, "a02") {
		t.Fatal("a02 is still in the compiled model")
	}
	if lp := res.Model.Loops["cdu-a"]; lp == nil || len(lp.Racks) != 3 {
		t.Fatalf("a02 should have left cooling loop cdu-a: %+v", lp)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "cooling loop cdu-a") {
		t.Fatalf("notes %v", notes)
	}
}

func TestRackReplaceInOneDraft(t *testing.T) {
	res, _, _, err := compileOps(t, Op{Op: "rack.remove", Name: "b01"}, Op{Op: "rack.add", Row: "b", Name: "b01", Position: 1, Format: "generic/oam-8gpu"})
	if err != nil {
		t.Fatalf("replacing b01 should compile: %v", err)
	}
	for _, r := range res.Model.Racks {
		if r.Name == "b01" && r.FormatName != "generic/oam-8gpu" {
			t.Fatalf("b01 is %s, want generic/oam-8gpu", r.FormatName)
		}
	}
}

func TestRackRemoveRefusesAnUnknownRack(t *testing.T) {
	if _, _, err := editOps(readMixedHall(t), []Op{{Op: "rack.remove", Name: "x99"}}); err == nil {
		t.Fatal("removing a rack the estate does not have must be refused")
	}
}

func TestRackRenameFollowsEveryReference(t *testing.T) {
	// a03 and a04 are an NVL36x2 pair (each the other's peer) and both are on cooling loop cdu-a
	res, notes, out, err := compileOps(t, Op{Op: "rack.rename", Name: "a03", NewName: "a13"})
	if err != nil {
		t.Fatalf("renaming a03 should compile (peer and loop follow): %v", err)
	}
	if !rackNamed(res, "a13") || rackNamed(res, "a03") {
		t.Fatal("rename did not reach the model")
	}
	if !strings.Contains(out, "peer: a13") {
		t.Fatalf("a04's peer should now be a13:\n%s", out)
	}
	if len(notes) < 2 {
		t.Fatalf("the loop and the peer should each be noted: %v", notes)
	}
}

func TestRackMoveToAnotherRow(t *testing.T) {
	res, _, _, err := compileOps(t, Op{Op: "rack.move", Name: "c01", ToRow: "b", ToPosition: 5})
	if err != nil {
		t.Fatalf("moving c01 into row b should compile: %v", err)
	}
	for _, r := range res.Model.Racks {
		if r.Name == "c01" && (r.Row.Name != "b" || r.Position != 5) {
			t.Fatalf("c01 is at row %s position %d, want b 5", r.Row.Name, r.Position)
		}
	}
}

func TestRowAndHallIntents(t *testing.T) {
	res, _, _, err := compileOps(t,
		Op{Op: "row.add", Name: "d", Positions: 8},
		Op{Op: "rack.add", Row: "d", Name: "d01", Position: 1, Format: "generic/cpu-rack"})
	if err != nil {
		t.Fatalf("a new row with a rack should compile: %v", err)
	}
	if !rackNamed(res, "d01") {
		t.Fatal("d01 missing")
	}
	if _, _, _, err := compileOps(t, Op{Op: "row.remove", Name: "b"}); err != nil {
		t.Fatalf("removing row b and its racks should compile: %v", err)
	}
}

func TestGenericOpsSetAddRemoveMove(t *testing.T) {
	path := func(p ...any) []any { return p }
	ops := []Op{
		// a rack param by name-keyed path
		{Op: "set", Path: path("spec", "sites", map[string]any{"name": "site-1"}, "halls", map[string]any{"name": "hall-b"}, "rows",
			map[string]any{"name": "b"}, "racks", map[string]any{"name": "b01"}, "params", "servers"), Value: 2.0},
		// a fabric's select
		{Op: "set", Path: path("spec", "fabrics", map[string]any{"name": "backend-roce"}, "select"), Value: "{b02,b03}"},
		// a new network (a mapping key)
		{Op: "add", Path: path("spec", "networks", "storage"), Value: map[string]any{"role": "storage", "fabric": "frontend"}},
		// a feed's capacity
		{Op: "set", Path: path("spec", "facility", "power", "feeds", map[string]any{"name": "A"}, "capacityW"), Value: 1200000.0},
	}
	res, _, out, err := compileOps(t, ops...)
	if err != nil {
		t.Fatalf("generic ops should compile: %v", err)
	}
	if !strings.Contains(out, "servers: 2") || strings.Contains(out, "servers: 2.0") {
		t.Fatalf("a whole number should be written as an integer:\n%s", out)
	}
	if res.Model.Fabrics["backend-roce"] == nil || len(res.Model.Fabrics["backend-roce"].Racks) != 2 {
		t.Fatalf("backend-roce should select b02 and b03 only")
	}
	if res.Model.Networks["storage"] == nil {
		t.Fatal("the storage network is missing")
	}
	// remove and move on sequences
	if _, _, _, err := compileOps(t,
		Op{Op: "remove", Path: path("spec", "fabrics", map[string]any{"name": "backend-roce"})},
		Op{Op: "remove", Path: path("spec", "networks", "backend-roce")}); err != nil {
		t.Fatalf("removing a fabric and its network should compile: %v", err)
	}
	if _, _, err := editOps(readMixedHall(t), []Op{{Op: "move", Path: path("spec", "fabrics", 0), To: path("spec", "fabrics", "-")}}); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, _, err := editOps(readMixedHall(t), []Op{{Op: "set", Path: path("spec", "fabrics", map[string]any{"name": "nope"}, "select"), Value: "x"}}); err == nil {
		t.Fatal("a path through a missing element must be refused")
	}
	// appending to a list the file does not have yet makes it a sequence
	out2, _, err := editOps(readMixedHall(t), []Op{{Op: "add", Path: path("spec", "cabling", "remove", "-"), Value: "a01/pdu-a:mgmt0"},
		{Op: "add", Path: path("spec", "fidelity", "overrides", "-"), Value: map[string]any{"select": "a05/**", "level": "model"}}})
	if err != nil {
		t.Fatalf("append to a missing list: %v", err)
	}
	var est schema.Estate
	if err := schema.Decode(out2, &est); err != nil {
		t.Fatal(err)
	}
	if est.Spec.Cabling == nil || len(est.Spec.Cabling.Remove) != 1 || len(est.Spec.Fidelity.Overrides) != 1 {
		t.Fatalf("the appended lists are not sequences:\n%s", out2)
	}
}

func TestUnifiedDiff(t *testing.T) {
	d := unifiedDiff("a\nb\nc\nd\n", "a\nb\nC\nd\ne\n", 1)
	for _, want := range []string{"-c", "+C", "+e", "@@"} {
		if !strings.Contains(d, want) {
			t.Fatalf("diff lacks %q:\n%s", want, d)
		}
	}
	if unifiedDiff("same\n", "same\n", 3) != "" {
		t.Fatal("no change should give an empty diff")
	}
}

func TestClassOf(t *testing.T) {
	for _, c := range []struct {
		op   Op
		want string
	}{
		{Op{Op: "rack.add"}, "hot"},
		{Op{Op: "set", Path: []any{"spec", "facility", "power"}}, "hot"},
		{Op{Op: "set", Path: []any{"spec", "fabrics", map[string]any{"name": "frontend"}, "params"}}, "cold"},
		{Op{Op: "set", Path: []any{"spec", "fabrics", map[string]any{"name": "frontend"}, "select"}}, "hot"},
		{Op{Op: "set", Path: []any{"spec", "clock"}}, "cold"},
	} {
		if got := classOf(c.op); got != c.want {
			t.Fatalf("%+v: %s, want %s", c.op, got, c.want)
		}
	}
}

// A live server over mixed-hall: plan reports impact and layout, a stale etag is refused.
func TestPlanImpactAndEtag(t *testing.T) {
	tw, err := twin.Up(mixedHall, twin.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	s := &Server{T: tw, gen: make(chan struct{})}
	etag := fileEtag(tw)
	p, _, _, code := s.plan([]Op{{Op: "rack.add", Row: "b", Name: "b04", Position: 4, Format: "generic/hgx-8gpu"}}, etag)
	if code != 200 || !p.OK {
		t.Fatalf("plan: %d %v", code, p.Problems)
	}
	if p.Devices[1] <= p.Devices[0] || p.Diff == "" || !strings.Contains(p.YAML, "b04") || p.Layout == nil {
		t.Fatalf("plan should grow the estate and carry a diff, the YAML and a layout: %+v", p.Summary)
	}
	var b04 *RackImpact
	for i := range p.Impact.Racks {
		if p.Impact.Racks[i].Name == "b04" {
			b04 = &p.Impact.Racks[i]
		}
	}
	if b04 == nil || b04.MaxW <= 0 || b04.CapacityW <= 0 || len(b04.Feeds) != 2 {
		t.Fatalf("b04's power impact: %+v", b04)
	}
	front := false
	for _, f := range p.Impact.Fabrics {
		if f.Name == "frontend" {
			front = true
			if f.Switches[1] <= f.Switches[0] || f.Hub != "c02" {
				t.Fatalf("frontend should gain leaves for b04 and be switched in c02: %+v", f)
			}
		}
	}
	if !front || len(p.Classes) != 1 || p.Classes[0] != "hot" {
		t.Fatalf("fabrics %v classes %v", p.Impact.Fabrics, p.Classes)
	}
	if _, _, _, code := s.plan([]Op{{Op: "rack.remove", Name: "b03"}}, "stale"); code != 409 {
		t.Fatalf("a stale etag should be refused with 409, got %d", code)
	}
}
