package engine

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/expr"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Sensor readings are derived state (D27): computed on read from committed state and the clock,
// following their targets with a first-order lag. Only threshold crossings are committed.

const ambientC = 25.0

func (e *Engine) tau() time.Duration { return time.Duration(30 * float64(time.Second) * e.scale) }

func (e *Engine) offset(d *model.Device, sensor string) float64 {
	off := 0.0
	for _, f := range e.faultsTouching(d) {
		for _, k := range []string{"sensorOffset." + sensor, "device.sensorOffset." + sensor} {
			if v, ok := f.Sets[k]; ok {
				n, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
				off += n
			}
		}
	}
	return off
}

func (e *Engine) override(d *model.Device, sensor string) (float64, bool) {
	for _, f := range e.faultsTouching(d) {
		if v, ok := f.Sets["sensor."+sensor]; ok {
			n, err := strconv.ParseFloat(fmt.Sprint(v), 64)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func (e *Engine) coolant(d *model.Device) float64 {
	lp := e.loopOf(d)
	if lp == nil {
		return 30
	}
	t := lp.SupplyC
	if lp.Profile != "" {
		if cdu := e.M.DeviceBy[e.M.Halls[0].Path+"/facility/"+lp.Name]; cdu != nil {
			for _, f := range e.faultsTouching(cdu) {
				if v, ok := f.Sets["loop.supplyOffsetC"]; ok {
					n, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
					t += n
				}
			}
		}
	}
	if at, lost := e.flowLost[lp.Name]; lost {
		mins := e.now.Sub(at).Minutes() / e.scale
		t += math.Min(35, 3*mins)
	}
	return t
}

func (e *Engine) inlet(d *model.Device) float64 {
	if d.Profile.Spec.Cooling.Kind == "liquid" {
		return e.coolant(d) + e.offset(d, "inlet")
	}
	return ambientC + e.offset(d, "inlet")
}

func (e *Engine) load(d *model.Device) float64 {
	p := d.Profile.Spec.Power
	if p.MaxW <= p.IdleW {
		return 0
	}
	return math.Max(0, (e.draw(d)-p.IdleW)/(p.MaxW-p.IdleW))
}

// target is where a sensor is heading now.
func (e *Engine) target(d *model.Device, s schema.SensorSpec) float64 {
	if v, ok := e.override(d, s.Name); ok {
		return v
	}
	on := d.State.Power == model.On
	switch s.Source {
	case "power":
		return e.draw(d)
	case "output-power":
		return e.sourceOutput(d)
	case "output-current":
		return e.sourceOutput(d) / (math.Sqrt(3) * 400)
	case "inlet":
		return e.inlet(d)
	case "coolant-in":
		return e.coolant(d) + e.offset(d, s.Name)
	case "accelerator-max", "cpu-max", "asic-max":
		rise := 8.0
		if on {
			k := 50.0
			if d.Profile.Spec.Cooling.Kind == "liquid" {
				k = 35
			}
			rise = 15 + k*e.load(d)
		}
		return e.inlet(d) + rise + e.offset(d, s.Name)
	case "exhaust":
		return e.inlet(d) + 12*e.load(d)
	case "loop-supply":
		if lp := e.M.Loops[d.Name]; lp != nil {
			return e.coolant(&model.Device{Rack: firstRack(lp), Profile: d.Profile})
		}
		return 30
	case "loop-return":
		if lp := e.M.Loops[d.Name]; lp != nil {
			kw := 0.0
			for _, r := range lp.Racks {
				kw += e.rackPower(r) / 1000
			}
			ret := e.coolant(&model.Device{Rack: firstRack(lp), Profile: d.Profile}) + 10*kw/math.Max(1, lp.CapacityKW)*1.2
			return ret
		}
		return 40
	case "loop-flow":
		if lp := e.M.Loops[d.Name]; lp != nil {
			if !lp.Flow {
				return 0
			}
			return 1.5 * lp.CapacityKW
		}
		return 0
	case "tray-leak-detector", "leak-point", "sensor-fault":
		return 0
	case "node-load":
		if n := e.M.PowerBy[d.Path]; n != nil {
			return e.nodeLoad(n)
		}
		return 0
	case "ups-charge":
		if n := e.M.PowerBy[d.Path]; n != nil {
			return 100 * e.chargeNow(n)
		}
		return 0
	case "ups-runtime":
		if n := e.M.PowerBy[d.Path]; n != nil {
			return e.upsRuntimeLeft(n)
		}
		return 0
	}
	return 0
}

func firstRack(lp *model.Loop) *model.Rack {
	if len(lp.Racks) > 0 {
		return lp.Racks[0]
	}
	return nil
}

func lagged(s schema.SensorSpec) bool {
	return s.Quantity == "temperature"
}

func (e *Engine) updateAnchors() {
	for _, d := range e.M.Devices {
		for _, s := range d.Profile.Spec.Sensors {
			if !lagged(s) {
				continue
			}
			key := d.Path + "#" + s.Name
			t := e.target(d, s)
			a, ok := e.anchors[key]
			if !ok {
				e.anchors[key] = anchor{v0: t, target: t, t0: e.now}
				continue
			}
			if math.Abs(a.target-t) > 0.01 {
				e.anchors[key] = anchor{v0: e.valueAt(a), target: t, t0: e.now}
			}
		}
	}
}

func (e *Engine) valueAt(a anchor) float64 {
	dt := e.now.Sub(a.t0)
	if dt <= 0 {
		return a.v0
	}
	return a.target + (a.v0-a.target)*math.Exp(-float64(dt)/float64(e.tau()))
}

// sensorValue is a sensor's derived reading now.
func (e *Engine) sensorValue(d *model.Device, s schema.SensorSpec) float64 {
	if v, ok := e.override(d, s.Name); ok {
		return v
	}
	if lagged(s) {
		if a, ok := e.anchors[d.Path+"#"+s.Name]; ok {
			return round(e.valueAt(a))
		}
	}
	return round(e.target(d, s))
}

func round(v float64) float64 { return math.Round(v*100) / 100 }

// Sensor returns a named sensor's reading and its threshold state.
func (e *Engine) Sensor(d *model.Device, name string) (float64, string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, s := range d.Profile.Spec.Sensors {
		if s.Name == name {
			v := e.sensorValue(d, s)
			return v, thresholdState(s, v), true
		}
	}
	return 0, "", false
}

// Sensors returns every sensor of a device.
func (e *Engine) Sensors(d *model.Device) map[string]float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.sensorsLocked(d)
}

func (e *Engine) sensorsLocked(d *model.Device) map[string]float64 {
	out := map[string]float64{}
	for _, s := range d.Profile.Spec.Sensors {
		out[s.Name] = e.sensorValue(d, s)
	}
	return out
}

func thresholdState(s schema.SensorSpec, v float64) string {
	switch {
	case s.Quantity == "leak" || s.Quantity == "state":
		if v >= 1 {
			return model.Critical
		}
	case s.Crit != nil && v >= *s.Crit:
		return model.Critical
	case s.Warn != nil && v >= *s.Warn:
		return model.Warning
	}
	return model.OK
}

func (e *Engine) evalThresholds() {
	for _, d := range e.M.Devices {
		for _, s := range d.Profile.Spec.Sensors {
			if s.Warn == nil && s.Crit == nil && s.Quantity != "leak" && s.Quantity != "state" {
				continue
			}
			key := d.Path + "#" + s.Name
			st := thresholdState(s, e.sensorValue(d, s))
			prev, ok := e.sensorSt[key]
			if !ok {
				prev = model.OK
			}
			if st != prev {
				e.sensorSt[key] = st
				e.stateEvent(d.Path, "sensor."+s.Name, prev, st, behaviourSrc, 0, st)
			}
		}
	}
}

func (e *Engine) componentHealth(c *model.Component) string {
	h := model.OK
	for _, f := range e.faultsOnComponent(c) {
		h = model.WorstHealth(h, f.Health)
	}
	if c.Sub != nil {
		h = model.WorstHealth(h, e.deviceHealth(c.Sub))
	}
	return h
}

func (e *Engine) componentState(c *model.Component) string {
	for _, f := range e.faultsOnComponent(c) {
		if f.State == "offline" {
			return "UnavailableOffline"
		}
	}
	if c.Device.State.Power != model.On {
		return "StandbyOffline"
	}
	return "Enabled"
}

func (e *Engine) deviceHealth(d *model.Device) string {
	return e.deviceHealthExcept(d, "")
}

// deviceHealthExcept rolls up a device's health leaving out components of one kind ("" leaves
// out none).
func (e *Engine) deviceHealthExcept(d *model.Device, kind string) string {
	h := model.OK
	for _, f := range e.faultsOn(d) {
		h = model.WorstHealth(h, f.Health)
	}
	for _, c := range d.Components {
		if kind != "" && c.Kind == kind {
			continue
		}
		ch := model.OK
		for _, f := range e.faultsOnComponent(c) {
			ch = model.WorstHealth(ch, f.Health)
		}
		h = model.WorstHealth(h, ch)
	}
	for _, s := range d.Profile.Spec.Sensors {
		if st, ok := e.sensorSt[d.Path+"#"+s.Name]; ok {
			h = model.WorstHealth(h, st)
		}
	}
	if n := e.M.PowerBy[d.Path]; n != nil {
		h = model.WorstHealth(h, chainHealth(n.Mode))
	}
	return h
}

// Health returns a device's rolled-up health.
func (e *Engine) Health(d *model.Device) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.deviceHealth(d)
}

// HealthExcept returns a device's health without the components of one kind, so a consumer
// can tell a host that is Critical from a host with a Critical accelerator.
func (e *Engine) HealthExcept(d *model.Device, kind string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.deviceHealthExcept(d, kind)
}

// ComponentHealth returns a component's health and Redfish state.
func (e *Engine) ComponentHealth(c *model.Component) (string, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.componentHealth(c), e.componentState(c)
}

// rollup commits health changes of components and devices (cause-chained).
func (e *Engine) rollup(cause uint64) {
	for _, d := range e.M.Devices {
		for _, c := range d.Components {
			h := model.OK
			for _, f := range e.faultsOnComponent(c) {
				h = model.WorstHealth(h, f.Health)
			}
			if prev := e.health[c.Path]; prev != h && !(prev == "" && h == model.OK) {
				e.health[c.Path] = h
				e.stateEvent(c.Path, "health", prev, h, behaviourSrc, cause, h)
			}
		}
		h := e.deviceHealth(d)
		if prev := e.health[d.Path]; prev != h && !(prev == "" && h == model.OK) {
			e.health[d.Path] = h
			e.stateEvent(d.Path, "health", prev, h, behaviourSrc, cause, h)
		}
	}
}

// evalDomains evaluates each domain's formation rule over its racks.
func (e *Engine) evalDomains(cause uint64) {
	for _, dom := range e.M.Domains {
		st := "formed"
		if dom.Formation != "" {
			env := e.rackEnv(dom.Racks[0])
			if len(dom.Racks) > 1 {
				devs := []any{}
				for _, r := range dom.Racks {
					for _, d := range r.Devices {
						devs = append(devs, e.deviceMini(d))
					}
				}
				env["devices"] = devs
			}
			ok, err := expr.Bool(expr.EnvRack, dom.Formation, map[string]any{"rack": env}, nil)
			if err != nil || !ok {
				st = "unformed"
			}
		}
		if st == "formed" {
			for _, m := range dom.Members {
				if c := e.componentByPath(m); c != nil && (e.componentHealth(c) == model.Critical || c.Device.State.Power != model.On) {
					st = "degraded"
					break
				}
			}
		}
		if prev := e.domains[dom.Path]; prev != st {
			e.domains[dom.Path] = st
			dom.State = st
			if prev != "" {
				sev := ""
				if st != "formed" {
					sev = model.Warning
				}
				e.stateEvent(dom.Path, "domain", prev, st, behaviourSrc, cause, sev)
			}
		}
	}
}

func (e *Engine) componentByPath(path string) *model.Component {
	for i := len(path) - 1; i > 0; i-- {
		if path[i] == '/' {
			if d := e.M.DeviceBy[path[:i]]; d != nil {
				for _, c := range d.Components {
					if c.Path == path {
						return c
					}
				}
				return nil
			}
		}
	}
	return nil
}

// DomainState returns a domain's committed state.
func (e *Engine) DomainState(path string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.domains[path]
}
