package expr

import (
	"testing"

	"github.com/google/cel-go/common/types/ref"
)

func TestScenarioFuncs(t *testing.T) {
	funcs := Funcs{
		"select":  func(arg ref.Val) ref.Val { return Native([]map[string]any{{"serial": "A"}, {"serial": "B"}}) },
		"machine": func(arg ref.Val) ref.Val { return Native(map[string]any{"state": "Ready", "serial": Arg(arg)}) },
	}
	ok, err := Bool(EnvScenario, `model.select("a03/*").all(d, accessors.machine(d.serial).state == "Ready")`, map[string]any{"model": map[string]any{}, "accessors": map[string]any{}, "events": nil, "clock": nil}, funcs)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := Check(EnvRack, `rack.devices.filter(d, d.class == "switch-tray").all(d, d.health != "Critical")`); err != nil {
		t.Fatal(err)
	}
	if err := Check(EnvRack, `rack.devices.nosuch(`); err == nil {
		t.Fatal("bad expression accepted")
	}
}
