package compile

import (
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// mixedHall loads mixed-hall and lets a test change it before compiling.
func compileMixed(t *testing.T, change func(e *schema.Estate)) (*Result, error) {
	t.Helper()
	reg, err := Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := LoadEstate("../../examples/estates/mixed-hall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	change(est)
	return Compile(est, reg, Options{HostArch: "aarch64"})
}

func wantProblem(t *testing.T, err error, part string) {
	t.Helper()
	ce, ok := Fail(err)
	if !ok {
		t.Fatalf("want a refusal mentioning %q, got %v", part, err)
	}
	for _, p := range ce.Problems {
		if strings.Contains(p, part) {
			return
		}
	}
	t.Fatalf("no problem mentions %q: %v", part, ce.Problems)
}

func fabric(e *schema.Estate, name string) *schema.Fabric {
	for i := range e.Spec.Fabrics {
		if e.Spec.Fabrics[i].Name == name {
			return &e.Spec.Fabrics[i]
		}
	}
	return nil
}

func TestFabricParamsAreChecked(t *testing.T) {
	_, err := compileMixed(t, func(e *schema.Estate) { fabric(e, "frontend").Params["spines"] = 99 })
	wantProblem(t, err, "fabric frontend: parameter spines=99 out of range")
	_, err = compileMixed(t, func(e *schema.Estate) { fabric(e, "backend-ib").Params = map[string]any{"ibLeaf": "generic/server-2u"} })
	wantProblem(t, err, "is a server, the template wants a switch")
	// an undeclared template parameter is kept, with a warning
	res, err := compileMixed(t, func(e *schema.Estate) { fabric(e, "frontend").Params["underlay"] = "bgp-unnumbered" })
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(res, `parameter "underlay" is not declared by generic/clos-2tier`) {
		t.Fatalf("want a warning for the undeclared parameter: %v", res.Warnings)
	}
}

func TestSlotAcceptsUnitsAndDefaultProfiles(t *testing.T) {
	// a 2U server where the hgx format's server slot accepts rack-mounted servers: fine
	if _, err := compileMixed(t, func(e *schema.Estate) {
		e.Spec.Sites[0].Halls[0].Rows[1].Racks[0].Params = map[string]any{"server": "oem-dell/r760", "servers": 2}
	}); err != nil {
		t.Fatalf("an accepted $param profile should compile: %v", err)
	}
	// a switch as the hgx format's server: the $param profile is now checked against accepts
	_, err := compileMixed(t, func(e *schema.Estate) {
		e.Spec.Sites[0].Halls[0].Rows[1].Racks[0].Params = map[string]any{"server": "generic/mgmt-switch-1g", "servers": 2}
	})
	wantProblem(t, err, "parameter server: generic/mgmt-switch-1g is a switch")
}

func TestNamesAndSettings(t *testing.T) {
	_, err := compileMixed(t, func(e *schema.Estate) {
		rows := e.Spec.Sites[0].Halls[0].Rows
		e.Spec.Sites[0].Halls[0].Rows = append(rows, schema.Row{Name: rows[0].Name, Positions: 4})
	})
	wantProblem(t, err, "two rows named a")
	_, err = compileMixed(t, func(e *schema.Estate) {
		e.Spec.Sites[0].Halls = append(e.Spec.Sites[0].Halls, e.Spec.Sites[0].Halls[0])
	})
	wantProblem(t, err, "two halls named hall-b")
	_, err = compileMixed(t, func(e *schema.Estate) { e.Spec.Fidelity.Default = "virtual-machine" })
	wantProblem(t, err, `spec.fidelity.default "virtual-machine"`)
	_, err = compileMixed(t, func(e *schema.Estate) { e.Spec.InitialState = "standby" })
	wantProblem(t, err, `spec.initialState "standby"`)
	_, err = compileMixed(t, func(e *schema.Estate) { e.Spec.TimeScale = -1 })
	wantProblem(t, err, "spec.timeScale")
	_, err = compileMixed(t, func(e *schema.Estate) { e.Spec.Sites[0].Halls[0].Rows[0].Positions = 20 })
	wantProblem(t, err, "a row has at most 16")
}

func TestUnconnectedPortsWarn(t *testing.T) {
	res := compileFile(t, "../../examples/estates/mixed-hall.yaml")
	// b01's Dell XE9680 servers have InfiniBand scale-out NICs; backend-ib selects a0* and
	// backend-roce (Ethernet) cables none of them
	if !hasWarning(res, "rack b01: 32 infiniband scale-out ports are on no fabric") {
		t.Fatalf("want the b01 warning: %v", res.Warnings)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "rack a01:") && strings.Contains(w, "on no fabric") {
			t.Fatalf("a01 is on backend-ib; no warning expected: %s", w)
		}
	}
}

func hasWarning(res *Result, part string) bool {
	for _, w := range res.Warnings {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

func TestPowerChainIsChecked(t *testing.T) {
	top := func(e *schema.Estate) []schema.PowerNode { return e.Spec.Facility.Power.Topology }
	res, err := compileMixed(t, func(e *schema.Estate) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Model.Power) != 6 || res.Model.Power[0].Name != "ups-a" || res.Model.DeviceBy["site-1/hall-b/facility/power/rpp-a1"] == nil {
		t.Fatalf("the chain compiles to facility devices, parents first: %d nodes", len(res.Model.Power))
	}
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[2].From = "nope" })
	wantProblem(t, err, `rpp-a1: from "nope" is neither a feed nor a node`)
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[0].From = "rpp-a1" })
	wantProblem(t, err, "loops back on itself")
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[3].Serves = []string{"a01"} })
	wantProblem(t, err, "rack a01: feed A reaches it through both rpp-a1 and rpp-a2")
	if ce, _ := Fail(err); ce != nil {
		n := 0
		for _, p := range ce.Problems {
			if strings.Contains(p, "rack a01: feed A reaches it through both") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("one problem per rack, feed and pair of nodes, not per power source: %d", n)
		}
	}
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[2].RuntimeS = 60 })
	wantProblem(t, err, "runtimeS is a UPS's")
	_, err = compileMixed(t, func(e *schema.Estate) { top(e)[0].Kind = "generator" })
	wantProblem(t, err, `kind "generator"`)
	res, err = compileMixed(t, func(e *schema.Estate) { top(e)[2].CapacityW = 100000 })
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "rpp-a1: its racks draw up to")
	}
	if !found {
		t.Fatalf("a panel below its racks' max draw is warned about: %v", res.Warnings)
	}
}
