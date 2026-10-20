package engine

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/expr"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Errors a power request can return; adapters map them to their protocol's error shapes.
var (
	ErrNoDevice     = errors.New("no such device")
	ErrNoInput      = errors.New("the device has no input power")
	ErrPrecondition = errors.New("precondition not met")
	ErrBMCDown      = errors.New("the management controller is not available")
	ErrUnsupported  = errors.New("unsupported request")
)

// Request is a protocol request translated by an adapter, or an act.
type Request struct {
	Path      string
	Canonical string
	Params    map[string]any
	Source    schema.EventSource
	Protocol  string
	Method    string
	Target    string
}

func (e *Engine) logRequest(r Request, status int) uint64 {
	kind := "request"
	if r.Source.Class == "user" || r.Source.Class == "scenario" {
		kind = "act"
	}
	return e.commit(schema.Event{Kind: kind, Source: r.Source, Path: r.Path,
		Request: &schema.EventRequest{Protocol: r.Protocol, Method: r.Method, Target: r.Target, Canonical: r.Canonical, Status: status}})
}

// LogRead records a read request (inventory.read, health.read …) by its canonical name.
func (e *Engine) LogRead(r Request, status int) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.logRequest(r, status)
}

func behaviour(d *model.Device, kind, key string, def float64) float64 {
	if b, ok := d.Profile.Spec.Behaviours[kind]; ok {
		if v, ok := b[key]; ok {
			switch x := v.(type) {
			case float64:
				return x
			case int:
				return float64(x)
			}
		}
	}
	return def
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

// Power performs a power request: power.on, power.off.graceful, power.off.force,
// power.restart, power.cycle, or ac.cycle (an AC cycle, as a technician or AuxPowerReset).
func (e *Engine) Power(r Request) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[r.Path]
	if d == nil {
		return ErrNoDevice
	}
	if r.Source.Class == "stack" && d.HasBMC() && !e.bmcAvailable(d) {
		return ErrBMCDown
	}
	cause := e.logRequest(r, 0)
	canonical := r.Canonical
	if canonical == "power.restart" && r.Params["graceful"] == true {
		canonical = "power.restart.graceful"
	}
	return e.powerLocked(d, canonical, r.Source, cause)
}

func (e *Engine) powerLocked(d *model.Device, canonical string, src schema.EventSource, cause uint64) error {
	if s := e.substrates[d.Path]; s != nil && canonical != "ac.cycle" {
		return e.substratePower(d, s, canonical, src, cause)
	}
	switch canonical {
	case "power.on":
		// a power-on from Off is a cold boot: every device on the host comes up from reset, so
		// a fault that clears on device.reset clears as it does on a restart or a power cycle
		// (found by medik8s's fence agents, whose default reboot is an off and an on)
		return e.powerOn(d, src, cause, true)
	case "power.off.graceful":
		e.powerOff(d, src, cause, true, nil)
	case "power.off.force":
		e.powerOff(d, src, cause, false, nil)
	case "power.cycle":
		if d.State.Power == model.Off {
			return e.powerOn(d, src, cause, true)
		}
		e.powerOff(d, src, cause, false, func() { _ = e.powerOn(d, behaviourSrc, cause, true) })
	case "power.restart", "power.restart.graceful":
		if d.State.Power == model.Off {
			return e.powerOn(d, src, cause, true)
		}
		if d.State.Power == model.On && d.IsHost() {
			e.warmReset(d, src, cause, canonical == "power.restart.graceful")
			return nil
		}
		e.powerOff(d, src, cause, canonical == "power.restart.graceful", func() { _ = e.powerOn(d, behaviourSrc, cause, true) })
	case "ac.cycle":
		e.acCycle(d, src, cause)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupported, canonical)
	}
	return nil
}

func (e *Engine) setPower(d *model.Device, to string, src schema.EventSource, cause uint64) uint64 {
	from := d.State.Power
	if from == to {
		return cause
	}
	d.State.Power = to
	d.State.PowerSince = e.now
	sev := ""
	return e.stateEvent(d.Path, "power", from, to, src, cause, sev)
}

func (e *Engine) rackEnv(r *model.Rack) map[string]any {
	if r == nil {
		return map[string]any{"name": "", "devices": []any{}, "power": map[string]any{"sourcesOn": 0, "minSources": 0}}
	}
	devs := []any{}
	for _, d := range r.Devices {
		devs = append(devs, e.deviceMini(d))
	}
	on, min := 0, 0
	for _, in := range r.Inputs {
		on += e.deliveringSources(in)
		min += in.MinSources
	}
	return map[string]any{"name": r.Name, "path": r.Path, "format": r.FormatName, "devices": devs,
		"power": map[string]any{"sourcesOn": on, "minSources": min}}
}

func (e *Engine) deviceMini(d *model.Device) map[string]any {
	return map[string]any{"path": d.Path, "name": d.Name, "class": d.Class, "health": e.deviceHealth(d),
		"power": d.State.Power, "fidelity": d.Fidelity, "profile": d.ProfileName}
}

func (e *Engine) powerOn(d *model.Device, src schema.EventSource, cause uint64, reset bool) error {
	if !d.State.AC {
		e.commit(schema.Event{Kind: "state", Source: behaviourSrc, Path: d.Path, Cause: cause, Severity: "Warning",
			Message: "power on refused: no input power"})
		return ErrNoInput
	}
	if d.State.Power == model.On || d.State.Power == model.PoweringOn {
		return nil
	}
	if req, ok := d.Profile.Spec.Behaviours["power"]["requires"].(string); ok && req != "" {
		okv, err := expr.Bool(expr.EnvBehaviour, req, map[string]any{"device": e.deviceMini(d), "rack": e.rackEnv(d.RackOf()), "estate": map[string]any{"name": e.M.Name}}, nil)
		if err != nil || !okv {
			e.commit(schema.Event{Kind: "state", Source: behaviourSrc, Path: d.Path, Cause: cause, Severity: "Warning",
				Message: fmt.Sprintf("power on refused: precondition %q not met", req)})
			return fmt.Errorf("%w: %s", ErrPrecondition, req)
		}
	}
	if s := e.substrates[d.Path]; s != nil {
		// observed power (D19): the substrate starts the machine and the observer reports it
		return e.handOff(d, s, "power.on", cause)
	}
	e.cancel(e.transition[d.Path])
	c := e.setPower(d, model.PoweringOn, src, cause)
	e.transition[d.Path] = e.after(secs(behaviour(d, "power", "onSeconds", 20)), "power-on", func() {
		if d.State.Power != model.PoweringOn {
			return
		}
		c2 := e.setPower(d, model.On, behaviourSrc, c)
		e.poweredOn(d, c2, reset)
		if e.isSource(d) {
			e.recomputePower(c2) // a shelf or PDU switched on delivers again
		}
		e.post(d, c2)
		e.rollup(c2)
		e.evalDomains(c2)
	})
	return nil
}

// warmReset is a restart that keeps the host powered (ForceRestart, GracefulRestart, IPMI's
// chassis reset): the BMC reports On throughout, as real ones do, while the host resets, applies
// its pending settings, runs POST and boots again. A graceful one lets the OS shut down first
// (the profile's power.offSeconds). Before, a restart passed through Off, and a client that reads
// the state back after a reset (MAAS's power driver) saw it off and reset again (compat/maas).
func (e *Engine) warmReset(d *model.Device, src schema.EventSource, cause uint64, graceful bool) {
	e.cancel(e.transition[d.Path])
	since := d.State.PowerSince
	from := d.State.Booted
	d.State.Booted, d.State.BootedSince = "reset", e.now
	c := e.commit(schema.Event{Kind: "state", Source: src, Path: d.Path, Cause: cause,
		State: &schema.EventState{Field: "boot", From: from, To: "reset"}, Message: "warm reset: power stays on"})
	reset := func() {
		if d.State.Power != model.On || !d.State.PowerSince.Equal(since) || d.State.Booted != "reset" {
			return
		}
		e.applyPendingOnBoot(d, c)
		e.fireTrigger(d, "device.reset", c)
		e.post(d, c)
		e.rollup(c)
		e.evalDomains(c)
	}
	if !graceful {
		reset()
		return
	}
	e.transition[d.Path] = e.after(secs(behaviour(d, "power", "offSeconds", 10)), "graceful-restart", reset)
}

// post runs a host's power-on self test for its profile's boot.postSeconds (scaled like every
// behaviour) before it boots: its OS is not up, so no kubelet posts Ready and Redfish's
// BootProgress shows POST, until the firmware hands over. A host with no POST time boots at
// once. Found by medik8s: with instant boots its Node Healthcheck ended a remediation while the
// fence agent was still confirming the power-on.
func (e *Engine) post(d *model.Device, cause uint64) {
	seconds := behaviour(d, "boot", "postSeconds", 0)
	if !d.IsHost() || seconds <= 0 {
		e.boot(d, cause)
		return
	}
	since := d.State.PowerSince
	old := d.State.Booted
	d.State.Booted, d.State.BootedSince = "post", e.now
	e.stateEvent(d.Path, "boot", old, "post", behaviourSrc, cause, "")
	e.after(secs(seconds), "post", func() {
		// a power-off, or a newer power-on, during POST ends this one
		if d.State.Power != model.On || d.State.Booted != "post" || !d.State.PowerSince.Equal(since) {
			return
		}
		e.boot(d, cause)
		e.rollup(cause)
	})
}

// poweredOn is what follows a host reaching On, whether the behaviour got it there (owned) or
// an observer saw it there (observed): pending BIOS settings and firmware apply, triggers
// fire, and sub-devices with input power come up.
func (e *Engine) poweredOn(d *model.Device, c uint64, reset bool) {
	e.applyPendingOnBoot(d, c)
	e.fireTrigger(d, "device.powerOn", c)
	if reset {
		e.fireTrigger(d, "device.reset", c)
	}
	for _, ch := range d.Children {
		if ch.State.AC {
			_ = e.powerOn(ch, behaviourSrc, c, reset)
		}
	}
}

// poweredOff is what follows a host reaching Off, owned or observed.
func (e *Engine) poweredOff(d *model.Device, c uint64) {
	d.State.Booted, d.State.BootedSince = "", e.now
	for _, ch := range d.Children {
		e.powerOff(ch, behaviourSrc, c, false, nil)
	}
	e.fireTrigger(d, "device.powerOff", c)
	e.rollup(c)
	e.evalDomains(c)
}

func (e *Engine) powerOff(d *model.Device, src schema.EventSource, cause uint64, graceful bool, then func()) {
	if s := e.substrates[d.Path]; s != nil {
		canonical := "power.off.force"
		if graceful {
			canonical = "power.off.graceful"
		}
		_ = e.handOff(d, s, canonical, cause)
		return
	}
	e.cancel(e.transition[d.Path])
	finish := func(c uint64) {
		c2 := e.setPower(d, model.Off, behaviourSrc, c)
		e.poweredOff(d, c2)
		if e.isSource(d) {
			e.recomputePower(c2) // a shelf or PDU switched off stops delivering
		}
		if then != nil {
			then()
		}
	}
	if d.State.Power == model.Off {
		if then != nil {
			then()
		}
		return
	}
	if !graceful {
		finish(cause)
		return
	}
	c := e.setPower(d, model.PoweringOff, src, cause)
	e.transition[d.Path] = e.after(secs(behaviour(d, "power", "offSeconds", 8)), "power-off", func() { finish(c) })
}

// acCycle removes and restores input power: the host goes off, the BMC reboots, and the host
// returns to the state it was in.
func (e *Engine) acCycle(d *model.Device, src schema.EventSource, cause uint64) {
	wasOn := d.State.Power == model.On || d.State.Power == model.PoweringOn
	e.loseAC(d, cause)
	e.after(2*time.Second, "ac-restore", func() {
		e.restoreAC(d, cause, func() {
			e.fireTrigger(d, "device.acCycle", cause)
			if wasOn {
				_ = e.powerOn(d, behaviourSrc, cause, true)
			}
		})
	})
}

func (e *Engine) loseAC(d *model.Device, cause uint64) {
	if !d.State.AC {
		return
	}
	d.State.AC = false
	e.stateEvent(d.Path, "ac", true, false, behaviourSrc, cause, "Warning")
	e.cancel(e.transition[d.Path])
	if s := e.substrates[d.Path]; s != nil {
		// the plug is pulled: the substrate stops the machine and the observer reports it off
		_ = e.handOff(d, s, "power.off.force", cause)
	} else if d.State.Power != model.Off {
		c := e.setPower(d, model.Off, behaviourSrc, cause)
		e.fireTrigger(d, "device.powerOff", c)
	}
	if d.State.BMCUp {
		d.State.BMCUp = false
		e.stateEvent(d.Path, "bmc", "Enabled", "Unavailable", behaviourSrc, cause, "")
	}
	for _, ch := range d.Children {
		e.loseAC(ch, cause)
	}
}

func (e *Engine) restoreAC(d *model.Device, cause uint64, then func()) {
	if d.State.AC {
		if then != nil {
			then()
		}
		return
	}
	d.State.AC = true
	e.stateEvent(d.Path, "ac", false, true, behaviourSrc, cause, "")
	for _, ch := range d.Children {
		e.restoreAC(ch, cause, nil)
	}
	if !d.HasBMC() {
		if then != nil {
			then()
		}
		return
	}
	e.after(secs(behaviour(d, "bmc", "bootSeconds", 30)), "bmc-boot", func() {
		if !d.State.AC {
			return
		}
		d.State.BMCUp = true
		d.State.BMCSince = e.now
		e.stateEvent(d.Path, "bmc", "Unavailable", "Enabled", behaviourSrc, cause, "")
		for _, h := range e.hooks.OnBMCUp {
			h := h
			e.pendingHooks = append(e.pendingHooks, func() { h(d) })
		}
		if then != nil {
			then()
		}
	})
}

// ResetBMC reboots a management controller; the host keeps running.
func (e *Engine) ResetBMC(r Request) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[r.Path]
	if d == nil {
		return ErrNoDevice
	}
	cause := e.logRequest(r, 0)
	e.after(time.Second, "bmc-reset", func() {
		if !d.State.BMCUp {
			return
		}
		d.State.BMCUp = false
		e.stateEvent(d.Path, "bmc", "Enabled", "Unavailable", behaviourSrc, cause, "")
		e.after(secs(behaviour(d, "bmc", "bootSeconds", 30)), "bmc-boot", func() {
			d.State.BMCUp = true
			e.stateEvent(d.Path, "bmc", "Unavailable", "Enabled", behaviourSrc, cause, "")
			// a BMC boots its own staged firmware (found by DPF, compat/dpf: an operator updates a
			// BMC and resets it, not the host)
			if fw := d.State.Firmware["bmc"]; fw != nil && fw.Staged != "" {
				old := fw.Version
				fw.Version, fw.Staged = fw.Staged, ""
				e.stateEvent(d.Path, "firmware.bmc", old, fw.Version, behaviourSrc, cause, "")
				e.fireTrigger(d, "firmware.applied", cause)
			}
			e.fireTrigger(d, "bmc.reset", cause)
			for _, h := range e.hooks.OnBMCUp {
				h := h
				e.pendingHooks = append(e.pendingHooks, func() { h(d) })
			}
		})
	})
	return nil
}

// ---------------------------------------------------------------- power sources

// isSource reports whether a device is a power source (a shelf or PDU) of one of its rack's
// inputs: switching one on or off changes what the input delivers.
func (e *Engine) isSource(d *model.Device) bool {
	if r := d.RackOf(); r != nil {
		for _, in := range r.Inputs {
			for _, s := range in.Sources {
				if s == d {
					return true
				}
			}
		}
	}
	return false
}

// sourceDelivering reports whether a shelf or PDU is delivering power.
func (e *Engine) sourceDelivering(s *model.Device) bool {
	if !s.State.AC || s.State.Power != model.On {
		return false
	}
	for _, f := range e.faultsOn(s) {
		if v, ok := f.Sets["output"]; ok && fmt.Sprint(v) == "off" {
			return false
		}
	}
	return true
}

func (e *Engine) deliveringSources(in *model.PowerInput) int {
	n := 0
	for _, s := range in.Sources {
		if e.sourceDelivering(s) {
			n++
		}
	}
	return n
}

func (e *Engine) outletOff(pdu *model.Device, outlet int) bool {
	if on, ok := pdu.State.Outlets[outlet]; ok && !on {
		return true
	}
	for _, f := range e.faultsOn(pdu) {
		if v, ok := f.Sets[fmt.Sprintf("outlet.%d", outlet)]; ok && fmt.Sprint(v) == "off" {
			return true
		}
	}
	return false
}

// recomputePower evaluates every source and input (the power.loss effect): devices whose
// supply is gone lose AC and are hard off; devices whose supply returns get AC back with the
// BMC booting and the host off.
func (e *Engine) recomputePower(cause uint64) {
	// sources first: a shelf or PDU has AC when its feed is up and the rack is not isolated
	for _, r := range e.M.Racks {
		for _, in := range r.Inputs {
			for _, s := range in.Sources {
				feedUp := true
				if fn, ok := in.SourceFeed[s.Path]; ok {
					if f := e.M.Feeds[fn]; f != nil && !f.Up {
						feedUp = false
					}
				}
				want := feedUp && !r.Isolated["electrical"]
				if want && !s.State.AC {
					e.restoreAC(s, cause, nil)
					if s.State.Power == model.Off {
						// a source restores its output when power returns
						s.State.Power = model.On
						e.stateEvent(s.Path, "power", model.Off, model.On, behaviourSrc, cause, "")
					}
				} else if !want && s.State.AC {
					e.loseAC(s, cause)
				}
			}
		}
	}
	for _, r := range e.M.Racks {
		for _, in := range r.Inputs {
			ok := e.deliveringSources(in) >= in.MinSources
			if in.Kind == "busbar" && len(in.Sources) == 0 {
				ok = true
			}
			for i, d := range in.Serves {
				want := ok && !r.Isolated["electrical"]
				if in.Kind == "pdu" && ok {
					// a served device keeps power while any delivering PDU's outlet to it is on
					any := false
					for _, s := range in.Sources {
						if e.sourceDelivering(s) && !e.outletOff(s, i+1) {
							any = true
						}
					}
					want = any && !r.Isolated["electrical"]
				}
				if want && !d.State.AC {
					e.restoreAC(d, cause, nil)
				} else if !want && d.State.AC {
					e.loseAC(d, cause)
				}
			}
		}
	}
	e.rollup(cause)
	e.evalDomains(cause)
}

// SetFeed takes a facility feed down or up (an act).
func (e *Engine) SetFeed(name string, up bool, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.M.Feeds[name]
	if f == nil {
		return fmt.Errorf("no feed %q", name)
	}
	cause := e.commit(schema.Event{Kind: "act", Source: src, Path: f.Path, State: &schema.EventState{Field: "up", From: f.Up, To: up}})
	f.Up = up
	e.recomputePower(cause)
	return nil
}

// SetOutlet switches a PDU outlet (SNMP or Redfish outlet control).
func (e *Engine) SetOutlet(r Request, outlet int, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[r.Path]
	if d == nil {
		return ErrNoDevice
	}
	cause := e.logRequest(r, 0)
	d.State.Outlets[outlet] = on
	e.stateEvent(d.Path, fmt.Sprintf("outlet.%d", outlet), !on, on, r.Source, cause, "")
	e.recomputePower(cause)
	return nil
}

// Isolate applies a BMS isolation (electrical: the rack's inputs open; liquid: its flow stops).
func (e *Engine) Isolate(rackPath, kind string, on bool, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.M.RackBy[rackPath]
	if r == nil {
		for _, x := range e.M.Racks {
			if x.Name == rackPath {
				r = x
			}
		}
	}
	if r == nil {
		return fmt.Errorf("no rack %q", rackPath)
	}
	cause := e.commit(schema.Event{Kind: "state", Source: src, Path: r.Path, State: &schema.EventState{Field: "isolation." + kind, From: r.Isolated[kind], To: on}, Severity: "Warning"})
	r.Isolated[kind] = on
	if kind == "electrical" {
		e.recomputePower(cause)
	}
	return nil
}

// ---------------------------------------------------------------- derived power and sensors

// draw is a device's derived power draw in watts.
func (e *Engine) draw(d *model.Device) float64 {
	p := d.Profile.Spec.Power
	if !d.State.AC {
		return 0
	}
	switch d.State.Power {
	case model.Off:
		return p.StandbyW
	case model.PoweringOn, model.PoweringOff:
		return p.StandbyW + (p.IdleW-p.StandbyW)*0.6
	}
	w := p.IdleW + (p.MaxW-p.IdleW)*d.State.Utilisation
	for _, c := range d.Components {
		if c.MaxW <= 0 {
			continue
		}
		for _, f := range e.faultsOnComponent(c) {
			if v, ok := f.Sets["power.watts"]; ok && fmt.Sprint(v) == "0" {
				w -= c.MaxW * d.State.Utilisation
			}
		}
	}
	return math.Max(w, p.StandbyW)
}

// inputLoad is the sum of what an input's served devices draw.
func (e *Engine) inputLoad(in *model.PowerInput) float64 {
	sum := 0.0
	for _, d := range in.Serves {
		sum += e.draw(d)
		for _, ch := range d.Children {
			sum += e.draw(ch)
		}
	}
	return sum
}

// RackPower is the rack's derived input power.
func (e *Engine) RackPower(r *model.Rack) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.rackPower(r)
}

func (e *Engine) rackPower(r *model.Rack) float64 {
	sum := 0.0
	for _, d := range r.Devices {
		sum += e.draw(d)
		for _, ch := range d.Children {
			sum += e.draw(ch)
		}
	}
	return sum
}

// sourceOutput is a shelf's or PDU's share of its input's load.
func (e *Engine) sourceOutput(s *model.Device) float64 {
	r := s.RackOf()
	if r == nil || !e.sourceDelivering(s) {
		return 0
	}
	for _, in := range r.Inputs {
		for _, src := range in.Sources {
			if src == s {
				n := e.deliveringSources(in)
				if n == 0 {
					return 0
				}
				return e.inputLoad(in) / float64(n)
			}
		}
	}
	return 0
}
