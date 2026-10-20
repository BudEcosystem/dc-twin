package engine

import (
	"fmt"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Substrate carries out power requests for a device whose power is observed rather than owned
// (D19): a `vm` device's QEMU process today, a `hardware` device's own BMC later. The engine
// still decides what a BMC decides before it acts (input power, the profile's preconditions,
// whether the management controller answers), and hands the request over; the device's power
// changes only when the substrate's observer reports it through Observe.
type Substrate interface {
	// Power queues a canonical power request (power.on, power.off.graceful, power.off.force,
	// power.restart, power.restart.graceful, power.cycle). It is called with the engine's
	// lock held, so it must not block and must not call back into the engine; the change it
	// causes arrives later as an observation carrying cause.
	Power(path, canonical string, cause uint64) error
}

// Observation is a fact an observer reports about a device's substrate. It is committed with
// source class `observer`: a `state` event when it changes an observed field (power, boot), an
// `observe` event when it records a substrate fact that changes none (an ACPI power-button
// press delivered, a reset the guest asked for, the machine's command line).
type Observation struct {
	Path     string
	Observer string // the observer's principal, e.g. "qemu"
	// Field is power (To: On or Off), reset (the machine's reset line; To: the reason), boot
	// (To: the boot stage the substrate saw: firmware, os, …), or event (To: the event name).
	Field   string
	To      string
	Message string
	Cause   uint64 // the request that led to it, when the substrate knows it
	Data    map[string]any
}

// AttachSubstrate makes a device's power observed: power requests for it go to s, and its
// power and boot state change only through Observe. A machine that is not running is off, so
// the device's power is reported Off until the substrate says otherwise.
func (e *Engine) AttachSubstrate(path, observer string, s Substrate) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return ErrNoDevice
	}
	if e.substrates == nil {
		e.substrates = map[string]Substrate{}
	}
	e.substrates[path] = s
	src := schema.EventSource{Class: "observer", Principal: observer}
	if d.State.Power != model.Off {
		from := d.State.Power
		d.State.Power, d.State.PowerSince, d.State.Booted, d.State.BootedSince = model.Off, e.now, "", e.now
		e.commit(schema.Event{Kind: "state", Source: src, Path: path, State: &schema.EventState{Field: "power", From: from, To: model.Off},
			Message: "power is observed from the substrate (D19): no machine is running yet"})
		e.rollup(0)
		e.evalDomains(0)
	}
	return nil
}

// ObservedPower reports whether a device's power is observed from a substrate.
func (e *Engine) ObservedPower(path string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.substrates[path]
	return ok
}

// handOff gives a power request to the device's substrate; a substrate that cannot take it is
// a twin incident, never a device failure.
func (e *Engine) handOff(d *model.Device, s Substrate, canonical string, cause uint64) error {
	if err := s.Power(d.Path, canonical, cause); err != nil {
		e.commit(schema.Event{Kind: "twin", Severity: "incident", Source: schema.EventSource{Class: "twin", Principal: "substrate"},
			Path: d.Path, Cause: cause, Message: fmt.Sprintf("%s: the substrate did not take %s: %v", d.Path, canonical, err)})
		return err
	}
	return nil
}

// substratePower is powerLocked for a device whose power is observed.
func (e *Engine) substratePower(d *model.Device, s Substrate, canonical string, src schema.EventSource, cause uint64) error {
	switch canonical {
	case "power.on":
		return e.powerOn(d, src, cause, false)
	case "power.restart", "power.restart.graceful", "power.cycle":
		if d.State.Power == model.Off {
			// a reset of a host that is off powers it on, after the same checks as power.on
			return e.powerOn(d, src, cause, true)
		}
	case "power.off.graceful", "power.off.force":
		// handed over even when the model says Off: a start may be under way that the
		// observer has not reported yet, and the substrate knows
	default:
		return fmt.Errorf("%w: %s", ErrUnsupported, canonical)
	}
	return e.handOff(d, s, canonical, cause)
}

// Observe commits an observer's report and applies what follows from it, exactly as the
// owned behaviour would have: a host observed on gets its pending BIOS settings and its
// sub-devices powered; a host observed off loses its boot state; triggers fire.
func (e *Engine) Observe(o Observation) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[o.Path]
	if d == nil {
		return ErrNoDevice
	}
	src := schema.EventSource{Class: "observer", Principal: o.Observer}
	switch o.Field {
	case "power":
		if o.To != model.On && o.To != model.Off {
			return fmt.Errorf("observed power %q: On or Off", o.To)
		}
		if d.State.Power == o.To {
			return nil
		}
		from := d.State.Power
		d.State.Power, d.State.PowerSince = o.To, e.now
		c := e.commit(schema.Event{Kind: "state", Source: src, Path: d.Path, State: &schema.EventState{Field: "power", From: from, To: o.To},
			Cause: o.Cause, Message: o.Message, Data: o.Data})
		if o.To == model.On {
			reset, _ := o.Data["reset"].(bool)
			e.poweredOn(d, c, reset)
			e.setBootLocked(d, "firmware", src, c, "")
			e.rollup(c)
			e.evalDomains(c)
		} else {
			e.poweredOff(d, c)
		}
	case "reset":
		c := e.commit(schema.Event{Kind: "observe", Source: src, Path: d.Path, State: &schema.EventState{Field: "reset", To: o.To},
			Cause: o.Cause, Message: o.Message, Data: o.Data})
		if d.State.Power != model.On {
			return nil
		}
		e.applyPendingOnBoot(d, c)
		e.fireTrigger(d, "device.reset", c)
		e.setBootLocked(d, "firmware", src, c, "")
		e.rollup(c)
	case "boot":
		e.setBootLocked(d, o.To, src, o.Cause, o.Message)
	default:
		e.commit(schema.Event{Kind: "observe", Source: src, Path: d.Path, State: &schema.EventState{Field: o.Field, To: o.To},
			Cause: o.Cause, Message: o.Message, Data: o.Data})
	}
	return nil
}

func (e *Engine) setBootLocked(d *model.Device, to string, src schema.EventSource, cause uint64, msg string) {
	if d.State.Booted == to {
		return
	}
	from := d.State.Booted
	d.State.Booted, d.State.BootedSince = to, e.now
	e.commit(schema.Event{Kind: "state", Source: src, Path: d.Path, State: &schema.EventState{Field: "boot", From: from, To: to}, Cause: cause, Message: msg})
}
