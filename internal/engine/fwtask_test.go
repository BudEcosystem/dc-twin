package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// A firmware update task is the BMC's: a BMC that goes down while the task runs loses it (D53).
// For every way the engine takes a BMC down, the running task ends aborted (Exception) when the
// BMC goes down, nothing is staged or applied, and once the BMC is back the task still reports
// Exception, never Running and never Completed.
func TestFirmwareTaskIsLostWhenItsBMCGoesDown(t *testing.T) {
	for _, tc := range []struct {
		name string
		down func(e *Engine) error
	}{
		{"bmc.reset", func(e *Engine) error {
			return e.ResetBMC(Request{Path: tray7, Canonical: "bmc.reset", Source: stack})
		}},
		{"ac.cycle", func(e *Engine) error {
			return e.Power(Request{Path: tray7, Canonical: "ac.cycle", Source: user})
		}},
		{"device.reseat", func(e *Engine) error { return e.Reseat(tray7, user) }},
		{"generic/bmc/unreachable", func(e *Engine) error {
			_, err := e.Inject(tray7, "generic/bmc/unreachable", nil, "hang", user)
			return err
		}},
		{"generic/bmc/reset", func(e *Engine) error {
			_, err := e.Inject(tray7, "generic/bmc/reset", nil, "selfreset", user)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := virtualNVL(t)
			d := e.M.DeviceBy[tray7]
			e.Advance(10 * time.Minute) // the tray is up and booted
			before := map[string]string{}
			for comp, fw := range d.State.Firmware {
				before[comp] = fw.Version
			}
			task, err := e.UpdateFirmware(Request{Path: tray7, Canonical: "firmware.update.simple", Source: stack}, []string{"bmc", "uefi"}, "99.0.0")
			if err != nil {
				t.Fatal(err)
			}
			e.Advance(5 * time.Second) // the task is running (it takes 20 s)
			if got := d.State.Tasks[len(d.State.Tasks)-1]; got.ID != task.ID || got.State != "Running" {
				t.Fatalf("before the BMC goes down: task %+v", got)
			}
			if err := tc.down(e); err != nil {
				t.Fatal(err)
			}
			e.Advance(2 * time.Second) // ResetBMC takes the BMC down a second after the request
			got := d.State.Tasks[len(d.State.Tasks)-1]
			if got.State != "Exception" || got.Ends.IsZero() {
				t.Fatalf("when the BMC went down: task %+v, want Exception", got)
			}
			// long past the task's 20 s and the BMC's 90 s boot; a fault that holds the BMC down is
			// cleared so the BMC can be read again
			e.Advance(10 * time.Minute)
			if len(e.Faults(tray7)) > 0 {
				if _, err := e.Clear(tray7, "", user); err != nil {
					t.Fatal(err)
				}
				e.Advance(time.Minute)
			}
			if up, _, _ := e.BMCBehaviour(d); !up {
				t.Fatalf("the BMC did not come back")
			}
			got = d.State.Tasks[len(d.State.Tasks)-1]
			if got.State != "Exception" || got.Percent == 100 {
				t.Fatalf("after the BMC is back: task %+v, want an aborted Exception", got)
			}
			for comp, fw := range d.State.Firmware {
				if fw.Staged != "" || fw.Version != before[comp] {
					t.Fatalf("%s: version %q staged %q, want %q with nothing staged", comp, fw.Version, fw.Staged, before[comp])
				}
			}
			for _, ev := range e.Events(0, nil) {
				if ev.State != nil && (ev.State.Field == "firmware.staged" || ev.State.Field == "firmware.bmc") {
					t.Fatalf("a lost task changed firmware: %+v", ev)
				}
			}
		})
	}
}

// A BMC that is down takes no update (as it takes no power request), so no task can start that
// its BMC is not there to run.
func TestFirmwareUpdateRefusedWhileBMCDown(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	e.Advance(10 * time.Minute)
	if _, err := e.Inject(tray7, "generic/bmc/unreachable", nil, "hang", user); err != nil {
		t.Fatal(err)
	}
	n := len(d.State.Tasks)
	if _, err := e.UpdateFirmware(Request{Path: tray7, Canonical: "firmware.update.simple", Source: stack}, []string{"bmc"}, "99.0.0"); !errors.Is(err, ErrBMCDown) {
		t.Fatalf("update with the BMC down: %v, want ErrBMCDown", err)
	}
	if len(d.State.Tasks) != n {
		t.Fatalf("a refused update created a task")
	}
}

// DPF's order (compat/dpf): the update task completes, DPF resets the BMC and reads the version
// again within about 100 ms, before the BMC has gone down, finds the old one and pushes the
// package again, which the BMC (still up) accepts. The first task's staged image is applied when
// the BMC reboots; the second push's task, still running when the BMC goes down, is lost with it
// and stages nothing more. The BMC comes back on the new version either way.
func TestCompletedTaskSurvivesTheResetThatFollowsIt(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	e.Advance(10 * time.Minute)
	before := d.State.Firmware["bmc"].Version
	push := func() model.Task {
		task, err := e.UpdateFirmware(Request{Path: tray7, Canonical: "firmware.update.push", Source: stack}, []string{"bmc"}, "99.0.0")
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	first := push()
	e.Advance(30 * time.Second)
	if got := d.State.Tasks[len(d.State.Tasks)-1]; got.ID != first.ID || got.State != "Completed" {
		t.Fatalf("first task: %+v", got)
	}
	if fw := d.State.Firmware["bmc"]; fw.Version != before || fw.Staged != "99.0.0" {
		t.Fatalf("after the first task: version %q staged %q", fw.Version, fw.Staged)
	}
	if err := e.ResetBMC(Request{Path: tray7, Canonical: "bmc.reset", Source: stack}); err != nil {
		t.Fatal(err)
	}
	// within the reset's first second the BMC is still up: the version is the old one, and a
	// second push is accepted
	if up, _, _ := e.BMCBehaviour(d); !up || d.State.Firmware["bmc"].Version != before {
		t.Fatalf("just after the reset request: up %v version %q", up, d.State.Firmware["bmc"].Version)
	}
	second := push()
	e.Advance(10 * time.Minute)
	tasks := map[string]string{}
	for _, task := range d.State.Tasks {
		tasks[task.ID] = task.State
	}
	if tasks[first.ID] != "Completed" || tasks[second.ID] != "Exception" {
		t.Fatalf("tasks after the reset: %v (first %s, second %s)", tasks, first.ID, second.ID)
	}
	if fw := d.State.Firmware["bmc"]; fw.Version != "99.0.0" || fw.Staged != "" {
		t.Fatalf("after the BMC's reboot: version %q staged %q, want 99.0.0 applied", fw.Version, fw.Staged)
	}
}
