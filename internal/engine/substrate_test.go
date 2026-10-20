package engine

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// fakeSubstrate records what the engine hands it, as a vm machine's worker would receive it.
type fakeSubstrate struct{ got []string }

func (f *fakeSubstrate) Power(path, canonical string, cause uint64) error {
	f.got = append(f.got, canonical)
	return nil
}

func (f *fakeSubstrate) take() []string {
	g := f.got
	f.got = nil
	return g
}

// D19: a device whose power is observed changes power only when its observer reports; the
// engine hands requests over after the checks a BMC makes, and applies what follows from an
// observation as the owned behaviour would.
func TestObservedPowerFollowsTheSubstrate(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	sub := &fakeSubstrate{}
	if err := e.AttachSubstrate(tray7, "test", sub); err != nil {
		t.Fatal(err)
	}
	if d.State.Power != model.Off || !e.ObservedPower(tray7) {
		t.Fatalf("an attached machine that is not running is Off, got %s", d.State.Power)
	}

	if err := e.Power(Request{Path: tray7, Canonical: "power.on", Source: stack}); err != nil {
		t.Fatal(err)
	}
	e.Advance(time.Minute)
	if got := sub.take(); !reflect.DeepEqual(got, []string{"power.on"}) || d.State.Power != model.Off {
		t.Fatalf("power.on is handed over and the model waits for the observer: handed %v, power %s", got, d.State.Power)
	}
	if err := e.Observe(Observation{Path: tray7, Observer: "test", Field: "power", To: model.On}); err != nil {
		t.Fatal(err)
	}
	if d.State.Power != model.On || d.State.Booted != "firmware" {
		t.Fatalf("observed On: power %s, booted %q", d.State.Power, d.State.Booted)
	}
	_ = e.Observe(Observation{Path: tray7, Observer: "test", Field: "boot", To: "os"})
	if d.State.Booted != "os" {
		t.Fatalf("observed boot: %q", d.State.Booted)
	}

	// a graceful shutdown is the guest's decision: nothing changes until it is observed
	_ = e.Power(Request{Path: tray7, Canonical: "power.off.graceful", Source: stack})
	e.Advance(time.Minute)
	if got := sub.take(); !reflect.DeepEqual(got, []string{"power.off.graceful"}) || d.State.Power != model.On {
		t.Fatalf("graceful: handed %v, power %s", got, d.State.Power)
	}

	// a forced restart is the reset line; the reset is observed, and device.reset fires
	_ = e.Power(Request{Path: tray7, Canonical: "power.restart", Source: stack})
	if got := sub.take(); !reflect.DeepEqual(got, []string{"power.restart"}) {
		t.Fatalf("restart: handed %v", got)
	}
	_ = e.Observe(Observation{Path: tray7, Observer: "test", Field: "reset", To: "host-qmp-system-reset"})
	if d.State.Power != model.On || d.State.Booted != "firmware" {
		t.Fatalf("after an observed reset: power %s, booted %q", d.State.Power, d.State.Booted)
	}
	_ = e.Observe(Observation{Path: tray7, Observer: "test", Field: "power", To: model.Off, Data: map[string]any{"reason": "guest-shutdown"}})
	if d.State.Power != model.Off || d.State.Booted != "" {
		t.Fatalf("observed Off: power %s, booted %q", d.State.Power, d.State.Booted)
	}
	var observed int
	for _, ev := range e.Events(0, nil) {
		if ev.Path == tray7 && ev.State != nil && ev.State.Field == "power" {
			if ev.Source.Class != "observer" {
				t.Fatalf("an observed device's power changed from %s: %+v", ev.Source.Class, ev)
			}
			observed++
		}
	}
	if observed != 3 {
		t.Fatalf("want 3 observed power events (attach, On, Off), got %d", observed)
	}

	// the profile's precondition is still the engine's: below minSources nothing is handed over
	for _, s := range []string{"01", "02", "03"} {
		if _, err := e.Inject("a03/power-shelf-"+s, "generic/power/output-loss", nil, "", user); err != nil {
			t.Fatal(err)
		}
	}
	if got := sub.take(); !reflect.DeepEqual(got, []string{"power.off.force"}) {
		t.Fatalf("losing input power pulls the machine's plug: handed %v", got)
	}
	if err := e.Power(Request{Path: tray7, Canonical: "power.on", Source: user}); !errors.Is(err, ErrNoInput) && !errors.Is(err, ErrPrecondition) {
		t.Fatalf("power on without input power must be refused before the substrate, got %v", err)
	}
	if got := sub.take(); len(got) != 0 {
		t.Fatalf("a refused request reached the substrate: %v", got)
	}
}
