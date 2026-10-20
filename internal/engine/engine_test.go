package engine

import (
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

var user = schema.EventSource{Class: "user", Principal: "test"}
var stack = schema.EventSource{Class: "stack", Principal: "test"}

func virtualNVL(t *testing.T) *Engine {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../examples/estates/gb300-nvl72-rack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	est.Spec.Clock, est.Spec.Stacks = "virtual", nil
	est.Spec.Fidelity.Overrides = nil
	for i := range est.Spec.Fabrics {
		est.Spec.Fabrics[i].NOS = "none"
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "aarch64"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(res.Model, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const tray7 = "site-1/hall-a/a03/compute-tray-07"

func TestPowerBehaviourTakesTime(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	if err := e.Power(Request{Path: tray7, Canonical: "power.off.graceful", Source: stack}); err != nil {
		t.Fatal(err)
	}
	if d.State.Power != model.PoweringOff {
		t.Fatalf("graceful off should take time, got %s", d.State.Power)
	}
	e.Advance(11 * time.Second)
	if d.State.Power != model.Off {
		t.Fatalf("after offSeconds: %s", d.State.Power)
	}
	if err := e.Power(Request{Path: tray7, Canonical: "power.on", Source: stack}); err != nil {
		t.Fatal(err)
	}
	e.Advance(41 * time.Second)
	if d.State.Power != model.On {
		t.Fatalf("after onSeconds: %s", d.State.Power)
	}
}

func TestBusbarBelowMinSourcesDropsTrays(t *testing.T) {
	e := virtualNVL(t)
	// three shelves fail: 8 - 3 = 5 < minSources 6
	for _, s := range []string{"01", "02", "03"} {
		if _, err := e.Inject("a03/power-shelf-"+s, "generic/power/output-loss", nil, "", user); err != nil {
			t.Fatal(err)
		}
	}
	d := e.M.DeviceBy[tray7]
	if d.State.AC || d.State.Power != model.Off {
		t.Fatalf("tray should have lost power: ac=%v power=%s", d.State.AC, d.State.Power)
	}
	if err := e.Power(Request{Path: tray7, Canonical: "power.on", Source: user}); err == nil {
		t.Fatal("power on without input must be refused")
	}
	if _, err := e.Clear("a03/power-shelf-01", "", user); err != nil {
		t.Fatal(err)
	}
	e.Advance(2 * time.Minute)
	if !d.State.AC || !d.State.BMCUp {
		t.Fatalf("power should return to the tray's BMC: ac=%v bmc=%v", d.State.AC, d.State.BMCUp)
	}
	if d.State.Power != model.Off {
		t.Fatalf("the host stays off after power returns, got %s", d.State.Power)
	}
}

func TestFeedLossOnNPlusN(t *testing.T) {
	e := virtualNVL(t)
	// feed A serves shelves 1-4: 4 remain, below minSources 6 -> trays drop
	if err := e.SetFeed("A", false, user); err != nil {
		t.Fatal(err)
	}
	if e.M.DeviceBy[tray7].State.AC {
		t.Fatal("losing one feed of an N+N busbar with minSources 6 of 8 must drop the trays")
	}
}

func TestXidRollsUpAndClearsOnACCycle(t *testing.T) {
	e := virtualNVL(t)
	if _, err := e.Inject(tray7+"/gpu-2", "nvidia/xid/xid-79", nil, "x", user); err != nil {
		t.Fatal(err)
	}
	d := e.M.DeviceBy[tray7]
	if e.Health(d) != model.Critical {
		t.Fatal("device health should be Critical")
	}
	if len(d.State.Log) == 0 {
		t.Fatal("xid should add a SEL entry")
	}
	dom := e.Domains()
	if len(dom) == 0 {
		t.Fatal("no domains")
	}
	// a reboot does not clear an Xid 79; an AC cycle does
	e.Power(Request{Path: tray7, Canonical: "power.restart", Source: stack})
	e.Advance(time.Minute)
	if len(e.Faults(tray7+"/**")) != 1 {
		t.Fatal("xid-79 must survive a reboot")
	}
	e.Power(Request{Path: tray7, Canonical: "ac.cycle", Source: user})
	e.Advance(5 * time.Minute)
	if n := len(e.Faults(tray7 + "/**")); n != 0 {
		t.Fatalf("xid-79 should clear on AC cycle, %d remain", n)
	}
}

func TestSwitchTrayFailureUnformsDomain(t *testing.T) {
	e := virtualNVL(t)
	var dom string
	for _, d := range e.Domains() {
		if d["kind"] == "scale-up" {
			dom = d["path"].(string)
		}
	}
	if e.DomainState(dom) != "formed" {
		t.Fatalf("domain should start formed, got %q", e.DomainState(dom))
	}
	if _, err := e.Inject("a03/switch-tray-04", "nvidia/nvlink/switch-tray-failure", nil, "", user); err != nil {
		t.Fatal(err)
	}
	if e.DomainState(dom) != "unformed" {
		t.Fatalf("domain should unform, got %q", e.DomainState(dom))
	}
}

func TestAllowList(t *testing.T) {
	e := virtualNVL(t)
	if _, err := e.Inject("a03/power-shelf-01", "nvidia/xid/xid-79", nil, "", user); err == nil {
		t.Fatal("a fault outside the profile's catalogues must be refused")
	}
}

func TestSensorsFollowLoad(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	e.Advance(time.Minute)
	v1, _, _ := e.Sensor(d, "gpu-max")
	e.SetUtilisation(tray7, 1.0, user)
	e.Advance(3 * time.Minute)
	v2, _, _ := e.Sensor(d, "gpu-max")
	if v2 <= v1 {
		t.Fatalf("gpu temperature should rise with load: %v -> %v", v1, v2)
	}
	if _, err := e.Inject(tray7, "generic/leak/tray-detector", nil, "", user); err != nil {
		t.Fatal(err)
	}
	e.Advance(time.Second)
	if v, st, _ := e.Sensor(d, "leak"); v != 1 || st != model.Critical {
		t.Fatalf("leak sensor %v %s", v, st)
	}
}

// A warm restart keeps the host On while it resets: a reset-cleared fault clears, POST runs, the
// OS boots again, and the power state never leaves On (a BMC's ForceRestart, GracefulRestart or
// IPMI reset). A power-off then power-on is a cold boot and resets too. A power cycle passes
// through Off.
func TestWarmRestartKeepsPowerOn(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	e.Advance(10 * time.Minute) // the tray is up and booted
	if d.State.Power != model.On || d.State.Booted != "os" {
		t.Fatalf("start: %s %q", d.State.Power, d.State.Booted)
	}
	since := d.State.PowerSince
	for _, canonical := range []string{"power.restart", "power.restart.graceful"} {
		if _, err := e.Inject(tray7+"/gpu-1", "nvidia/xid/xid-48", nil, "x48", user); err != nil {
			t.Fatal(err)
		}
		if err := e.Power(Request{Path: tray7, Canonical: canonical, Source: stack}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 600; i++ { // ten minutes in one-second steps: power must stay On throughout
			e.Advance(time.Second)
			if d.State.Power != model.On || !d.State.PowerSince.Equal(since) {
				t.Fatalf("%s: power %s since %v during the restart", canonical, d.State.Power, d.State.PowerSince)
			}
		}
		if d.State.Booted != "os" || len(e.Faults(tray7+"/**")) != 0 {
			t.Fatalf("%s: booted %q, faults %v", canonical, d.State.Booted, e.Faults(tray7+"/**"))
		}
	}
	// an off and an on: a cold boot, which resets the GPUs as well
	if _, err := e.Inject(tray7+"/gpu-1", "nvidia/xid/xid-48", nil, "x48", user); err != nil {
		t.Fatal(err)
	}
	e.Power(Request{Path: tray7, Canonical: "power.off.force", Source: stack})
	e.Advance(time.Minute)
	e.Power(Request{Path: tray7, Canonical: "power.on", Source: stack})
	e.Advance(10 * time.Minute)
	if d.State.Power != model.On || len(e.Faults(tray7+"/**")) != 0 {
		t.Fatalf("off and on: power %s, faults %v", d.State.Power, e.Faults(tray7+"/**"))
	}
	// a power cycle passes through Off
	e.Power(Request{Path: tray7, Canonical: "power.cycle", Source: stack})
	e.Advance(time.Second)
	if d.State.Power == model.On {
		t.Fatalf("power.cycle stayed On")
	}
}

// An update staged on a BMC's own firmware applies when the BMC reboots (Manager.Reset), as on a
// real BMC, not only at the host's next reset.
func TestBMCFirmwareAppliesOnBMCReset(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	e.Advance(10 * time.Minute)
	before := d.State.Firmware["bmc"].Version
	if _, err := e.UpdateFirmware(Request{Path: tray7, Canonical: "firmware.update.simple", Source: stack}, []string{"bmc"}, "99.0.0"); err != nil {
		t.Fatal(err)
	}
	e.Advance(10 * time.Minute) // the update task completes; the bmc firmware applies on reset
	if v := d.State.Firmware["bmc"].Version; v != before && v != "99.0.0" {
		t.Fatalf("unexpected bmc version %q", v)
	}
	if d.State.Firmware["bmc"].Version == "99.0.0" {
		t.Skip("this profile's bmc firmware applies immediately; nothing to reset")
	}
	if err := e.ResetBMC(Request{Path: tray7, Canonical: "bmc.reset", Source: stack}); err != nil {
		t.Fatal(err)
	}
	e.Advance(10 * time.Minute)
	if v := d.State.Firmware["bmc"].Version; v != "99.0.0" || d.State.Firmware["bmc"].Staged != "" || d.State.Power != model.On {
		t.Fatalf("after a BMC reset: version %q staged %q power %s", v, d.State.Firmware["bmc"].Staged, d.State.Power)
	}
}
