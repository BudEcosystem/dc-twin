package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/selector"
)

// ActiveFault is one injected fault and what it contributes to derived state. Faults never
// reach into a protocol: adapters see their effects through the model (08).
type ActiveFault struct {
	ID string
	// Group is the id the fault was injected under when one inject named several targets (each
	// then gets "<group>-<n>"); clearing the group's id clears them all.
	Group      string
	Fault      string
	Title      string
	Target     string
	Device     *model.Device
	Component  *model.Component
	Port       *model.Port
	Params     map[string]any
	Since      time.Time
	Seq        uint64
	Spec       *schema.FaultSpec
	Source     schema.EventSource
	Parent     string // a propagated fault's parent id
	Health     string
	State      string
	Sets       map[string]any
	PortsDown  []*model.Port
	LinksDown  []*model.Link
	NotApplied []string // effects that do not apply at this fidelity (kernelLog, shim at model)
	clearTimer uint64
}

// FaultView is what the API and scenarios see of an active fault.
type FaultView struct {
	ID, Fault, Title, Target, Since, Parent string
	NotApplied                              []string
}

func (f *ActiveFault) View() FaultView {
	return FaultView{ID: f.ID, Fault: f.Fault, Title: f.Title, Target: f.Target, Since: f.Since.Format(time.RFC3339), Parent: f.Parent, NotApplied: f.NotApplied}
}

func (e *Engine) faultsOn(d *model.Device) []*ActiveFault {
	var out []*ActiveFault
	for _, f := range e.faults {
		if f.Device == d && f.Component == nil && f.Port == nil {
			out = append(out, f)
		}
	}
	return out
}

func (e *Engine) faultsOnComponent(c *model.Component) []*ActiveFault {
	var out []*ActiveFault
	for _, f := range e.faults {
		if f.Component == c {
			out = append(out, f)
		}
	}
	return out
}

// faultsTouching returns every fault on a device, its components or its ports.
func (e *Engine) faultsTouching(d *model.Device) []*ActiveFault {
	var out []*ActiveFault
	for _, f := range e.faults {
		if f.Device == d {
			out = append(out, f)
		}
	}
	return out
}

// Inject applies a fault from a catalogue to every target the selector names.
func (e *Engine) Inject(sel, faultID string, params map[string]any, id string, src schema.EventSource) ([]FaultView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fs, err := e.injectLocked(sel, faultID, params, id, src, 0)
	var out []FaultView
	for _, f := range fs {
		out = append(out, f.View())
	}
	return out, err
}

func (e *Engine) injectLocked(selStr, faultID string, params map[string]any, id string, src schema.EventSource, cause uint64) ([]*ActiveFault, error) {
	ref, ok := e.Reg.Faults[faultID]
	if !ok {
		return nil, fmt.Errorf("fault %q is in no pinned catalogue", faultID)
	}
	sel, err := selector.Parse(selStr)
	if err != nil {
		return nil, err
	}
	t := ref.Spec.Target
	type target struct {
		d    *model.Device
		c    *model.Component
		p    *model.Port
		path string
	}
	var targets []target
	switch {
	case t.Port:
		for r, p := range e.M.Ports {
			if sel.MatchPort(r) {
				targets = append(targets, target{d: p.Device, p: p, path: r})
			}
		}
	case t.Component != "":
		for _, d := range e.M.Devices {
			for _, c := range d.Components {
				if c.Kind != t.Component || (t.Vendor != "" && c.Vendor != t.Vendor) {
					continue
				}
				if sel.MatchPath(c.Path) {
					targets = append(targets, target{d: d, c: c, path: c.Path})
				}
			}
		}
	default:
		for _, d := range e.M.Devices {
			if !sel.MatchPath(d.Path) || !sel.MatchAttrs(d.Attr) {
				continue
			}
			if t.Device != "" && d.BaseClass != t.Device {
				continue
			}
			switch t.DeviceWith {
			case "bmc":
				if !d.HasBMC() {
					continue
				}
			case "firmware":
				if len(d.Profile.Spec.Firmware) == 0 {
					continue
				}
			case "leak-detector":
				if !hasSensorSource(d, "tray-leak-detector") {
					continue
				}
			case "breaker":
				if e.M.PowerBy[d.Path] == nil {
					continue
				}
			}
			targets = append(targets, target{d: d, path: d.Path})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].path < targets[j].path })
	if len(targets) == 0 {
		return nil, fmt.Errorf("fault %s: selector %q matches no %s", faultID, selStr, describeTarget(t))
	}
	catalogue := ref.Catalogue.Metadata.Name
	var out []*ActiveFault
	for i, tg := range targets {
		if !allowed(tg.d, catalogue) {
			return out, fmt.Errorf("fault %s refused on %s: profile %s does not list catalogue %s (a strict allow-list)", faultID, tg.path, tg.d.ProfileName, catalogue)
		}
		fid := id
		if fid == "" || len(targets) > 1 {
			e.faultSeq++
			fid = fmt.Sprintf("f%d", e.faultSeq)
			if id != "" {
				fid = fmt.Sprintf("%s-%d", id, i+1)
			}
		}
		group := ""
		if id != "" && len(targets) > 1 {
			group = id
		}
		f := &ActiveFault{ID: fid, Group: group, Fault: faultID, Title: ref.Spec.Title, Target: tg.path, Device: tg.d, Component: tg.c, Port: tg.p,
			Params: resolveParams(ref.Spec, params), Since: e.now, Spec: ref.Spec, Source: src, Sets: map[string]any{}}
		f.Seq = e.commit(schema.Event{Kind: "fault", Source: src, Path: tg.path, Cause: cause, Severity: "Warning",
			Message: "inject " + faultID + ": " + ref.Spec.Title, Data: map[string]any{"fault": faultID, "id": fid, "params": f.Params}})
		e.faults = append(e.faults, f)
		e.applyEffects(f)
		if ref.Spec.Clears.After != "" {
			if d, err := time.ParseDuration(ref.Spec.Clears.After); err == nil {
				ff := f
				f.clearTimer = e.after(d, "fault-clear", func() { e.clearLocked(ff, "after", behaviourSrc, 0) })
			}
		}
		out = append(out, f)
	}
	e.rollup(out[0].Seq)
	e.evalDomains(out[0].Seq)
	return out, nil
}

func describeTarget(t schema.FaultTarget) string {
	switch {
	case t.Port:
		return "port"
	case t.Component != "":
		return "component of kind " + t.Component
	case t.Device != "":
		return "device of class " + t.Device
	case t.DeviceWith == "breaker":
		return "power-chain node (UPS, busway or panel)"
	case t.DeviceWith != "":
		return "device with a " + t.DeviceWith
	}
	return "device"
}

func allowed(d *model.Device, catalogue string) bool {
	for _, c := range d.Profile.Spec.Faults {
		if c == catalogue {
			return true
		}
	}
	return false
}

func hasSensorSource(d *model.Device, src string) bool {
	for _, s := range d.Profile.Spec.Sensors {
		if s.Source == src {
			return true
		}
	}
	return false
}

func resolveParams(f *schema.FaultSpec, given map[string]any) map[string]any {
	out := map[string]any{}
	for k, decl := range f.Params {
		out[k] = decl.Default
	}
	for k, v := range given {
		out[k] = v
	}
	return out
}

var paramRe = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

func (f *ActiveFault) subst(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	out := paramRe.ReplaceAllStringFunc(s, func(m string) string {
		k := m[1 : len(m)-1]
		if pv, ok := f.Params[k]; ok {
			return fmt.Sprint(pv)
		}
		switch k {
		case "component":
			if f.Component != nil {
				return f.Component.Name
			}
		case "index":
			if f.Component != nil {
				return strconv.Itoa(f.Component.Index)
			}
		case "pci":
			if f.Component != nil {
				return fmt.Sprintf("0000:%02x:00.0", 0x18+f.Component.Index*0x10)
			}
		}
		return m
	})
	if n, err := strconv.ParseFloat(out, 64); err == nil && out != s {
		return n
	}
	return out
}

func (e *Engine) applyEffects(f *ActiveFault) {
	level := f.Device.Fidelity
	for _, eff := range f.Spec.Effects {
		for verb, body := range eff {
			m, _ := body.(map[string]any)
			if at, ok := m["appliesAt"].([]any); ok && !containsAny(at, level) {
				f.NotApplied = append(f.NotApplied, fmt.Sprintf("%s (applies at %v; device is %s)", verb, at, level))
				continue
			}
			switch verb {
			case "set":
				for k, v := range m {
					v = f.subst(v)
					switch k {
					case "health":
						f.Health = fmt.Sprint(v)
					case "state":
						f.State = fmt.Sprint(v)
					default:
						k2 := fmt.Sprint(f.subst(k))
						if strings.HasPrefix(k2, "firmware.") && k2 != "firmware.failNext" {
							comp := strings.TrimPrefix(k2, "firmware.")
							if fw := f.Device.State.Firmware[comp]; fw != nil {
								old := fw.Version
								fw.Version = fmt.Sprint(v)
								e.stateEvent(f.Device.Path, "firmware."+comp, old, fw.Version, schema.EventSource{Class: "twin", Principal: "fault"}, f.Seq, "Warning")
							}
							continue
						}
						if k2 == "account.passwordChangeRequired" {
							for i := range f.Device.State.Accounts {
								if f.Device.State.Accounts[i].User == f.Device.Identity.BMCUser {
									f.Device.State.Accounts[i].PasswordChangeRequired = true
								}
							}
							e.stateEvent(f.Device.Path, "account.passwordChangeRequired", false, true, schema.EventSource{Class: "twin", Principal: "fault"}, f.Seq, "Warning")
							continue
						}
						if k2 == "account.rotate" {
							for i := range f.Device.State.Accounts {
								if f.Device.State.Accounts[i].User == f.Device.Identity.BMCUser {
									f.Device.State.Accounts[i].Password = fmt.Sprintf("oob-%d-%s", f.Seq, f.Device.Identity.BMCPassword[:6])
								}
							}
							e.stateEvent(f.Device.Path, "account.password", nil, "[rotated out of band]", schema.EventSource{Class: "twin", Principal: "fault"}, f.Seq, "Warning")
							continue
						}
						if strings.HasPrefix(k2, "loop.") {
							if lp := e.loopOf(f.Device); lp != nil {
								if k2 == "loop.flow" {
									lp.Flow = v == true || fmt.Sprint(v) == "true"
									if !lp.Flow {
										e.flowLost[lp.Name] = e.now
									}
								}
							}
						}
						f.Sets[k2] = v
					}
				}
			case "counter":
				for k, v := range m {
					n := 1.0
					if s, ok := f.subst(v).(string); ok {
						n, _ = strconv.ParseFloat(strings.TrimPrefix(s, "+"), 64)
					} else if x, ok := f.subst(v).(float64); ok {
						n = x
					}
					if f.Component != nil {
						f.Component.State.Counters[k] += n
					} else {
						f.Device.State.Counters[k] += n
					}
				}
			case "ports":
				owner, _ := m["owner"].(string)
				kind, _ := m["kind"].(string)
				var cands []*model.Port
				if owner == "self" && f.Component != nil {
					cands = f.Component.Ports
				} else {
					cands = f.Device.Ports
				}
				idx := -1
				if iv, ok := m["index"]; ok {
					if n, err := strconv.Atoi(fmt.Sprint(f.subst(iv))); err == nil {
						idx = n
					}
				}
				for i, p := range cands {
					if kind != "" && p.Kind != kind {
						continue
					}
					if idx >= 0 && i != idx {
						continue
					}
					f.PortsDown = append(f.PortsDown, p)
				}
				e.stateEvent(f.Target, "ports", nil, fmt.Sprintf("%d ports down", len(f.PortsDown)), schema.EventSource{Class: "twin", Principal: "fault"}, f.Seq, "Warning")
			case "link":
				if f.Port != nil && f.Port.Link != nil && fmt.Sprint(m["state"]) == "down" {
					l := f.Port.Link
					if l.Up {
						l.Up = false
						f.LinksDown = append(f.LinksDown, l)
						e.stateEvent(l.ID, "link", "up", "down", schema.EventSource{Class: "twin", Principal: "fault"}, f.Seq, "Warning")
					}
				}
			case "kernelLog", "shim":
				// applies only inside a vm host (appliesAt): there the in-host agent delivers a
				// kernelLog line into the guest's kernel log (internal/inhost); at model it is
				// recorded on the model only
				if f.Device != nil && f.Device.Fidelity == "vm" && verb == "kernelLog" {
					break
				}
				f.NotApplied = append(f.NotApplied, verb+" (recorded on the model only)")
			case "redfish.logEntry":
				msg := fmt.Sprint(f.subst(m["message"]))
				e.addLog(f.Device, fmt.Sprint(m["severity"]), msg, fmt.Sprint(m["messageId"]), "SEL", f.Seq)
			case "redfish.property":
				f.Sets["redfish:"+fmt.Sprint(m["path"])+"#"+fmt.Sprint(m["property"])] = f.subst(m["value"])
			case "bms":
				point := fmt.Sprint(m["point"])
				if bm := f.Device.Profile.Spec.BMS; bm != nil {
					for _, p := range bm.Points {
						if p.Name == point {
							v := f.subst(m["value"])
							if v == true {
								v = 1.0
							} else if v == false {
								v = 0.0
							}
							f.Sets["sensor."+p.Sensor] = v
						}
					}
				}
			case "power.loss":
				defer e.recomputePower(f.Seq)
			case "propagate":
				e.propagate(f, fmt.Sprint(body))
			case "attestation":
				f.Sets["attestation"] = m["result"]
			default:
				f.NotApplied = append(f.NotApplied, "unknown effect "+verb)
			}
		}
	}
}

func containsAny(list []any, s string) bool {
	for _, x := range list {
		if fmt.Sprint(x) == s {
			return true
		}
	}
	return false
}

func (e *Engine) loopOf(d *model.Device) *model.Loop {
	if lp, ok := e.M.Loops[d.Name]; ok {
		return lp
	}
	if r := d.RackOf(); r != nil {
		return r.Loop
	}
	return nil
}

// propagate applies the rack format's propagation rule for a facility fault.
func (e *Engine) propagate(f *ActiveFault, kind string) {
	r := f.Device.RackOf()
	if r == nil {
		return
	}
	for _, rule := range r.Format.Spec.Propagation[kind] {
		if rule.To == "" || rule.Effect == "" || !selector.Glob(rule.From, f.Device.Name) {
			continue
		}
		var parts []string
		for _, p := range strings.Fields(rule.To) {
			parts = append(parts, r.Path+"/"+p)
		}
		children, err := e.injectLocked(strings.Join(parts, " "), rule.Effect, nil, f.ID+"-prop", schema.EventSource{Class: "twin", Principal: "propagation"}, f.Seq)
		if err != nil {
			e.commit(schema.Event{Kind: "twin", Source: behaviourSrc, Path: f.Target, Message: "propagation: " + err.Error()})
		}
		for _, c := range children {
			c.Parent = f.ID
		}
	}
}

// Clear removes faults by id, or every fault on the targets a selector names.
func (e *Engine) Clear(idOrSelector string, faultID string, src schema.EventSource) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var hit []*ActiveFault
	for _, f := range e.faults {
		// an id names one fault, or the group one inject made across several targets (their
		// propagated children go with them, below). A prefix match cleared an unrelated fault
		// whose id began with this one's (found by compat/metal3).
		if f.ID == idOrSelector || (f.Group != "" && f.Group == idOrSelector) {
			hit = append(hit, f)
		}
	}
	if len(hit) == 0 {
		sel, err := selector.Parse(idOrSelector)
		if err != nil {
			return 0, err
		}
		for _, f := range e.faults {
			if (faultID == "" || f.Fault == faultID) && (sel.MatchPath(f.Target) || sel.MatchPort(f.Target)) {
				hit = append(hit, f)
			}
		}
	}
	if len(hit) == 0 {
		return 0, fmt.Errorf("no active fault matches %q", idOrSelector)
	}
	for _, f := range hit {
		if f.Spec.Clears.Manual || src.Class == "twin" {
			e.clearLocked(f, "manual", src, 0)
		} else {
			return 0, fmt.Errorf("fault %s does not clear manually; it clears on %v", f.Fault, f.Spec.Clears.Triggers())
		}
	}
	return len(hit), nil
}

func (e *Engine) clearLocked(f *ActiveFault, why string, src schema.EventSource, cause uint64) {
	idx := -1
	for i, x := range e.faults {
		if x == f {
			idx = i
		}
	}
	if idx < 0 {
		return
	}
	e.faults = append(e.faults[:idx], e.faults[idx+1:]...)
	e.cancel(f.clearTimer)
	c := e.commit(schema.Event{Kind: "fault", Source: src, Path: f.Target, Cause: cause,
		Message: "clear " + f.Fault + " (" + why + ")", Data: map[string]any{"fault": f.Fault, "id": f.ID, "cleared": why}})
	for _, l := range f.LinksDown {
		l.Up = true
		e.stateEvent(l.ID, "link", "down", "up", behaviourSrc, c, "")
	}
	if lp := e.loopOf(f.Device); lp != nil {
		if _, ok := f.Sets["loop.flow"]; ok {
			lp.Flow = true
			delete(e.flowLost, lp.Name)
		}
	}
	// propagated children go with their parent
	for _, x := range append([]*ActiveFault{}, e.faults...) {
		if x.Parent == f.ID {
			e.clearLocked(x, "parent cleared", src, c)
		}
	}
	_, out := f.Sets["output"]
	_, brk := f.Sets["breaker"]
	if out || brk {
		e.recomputePower(c)
	}
	for k := range f.Sets {
		if strings.HasPrefix(k, "outlet.") {
			e.recomputePower(c)
			break
		}
	}
	e.rollup(c)
	e.evalDomains(c)
}

// fireTrigger clears faults whose catalogue entry clears on the trigger.
func (e *Engine) fireTrigger(d *model.Device, trigger string, cause uint64) {
	for _, f := range append([]*ActiveFault{}, e.faults...) {
		if f.Device != d {
			continue
		}
		for _, t := range f.Spec.Clears.Triggers() {
			if t == trigger {
				e.clearLocked(f, trigger, behaviourSrc, cause)
			}
		}
	}
}

// Faults lists active faults whose target matches the selector (all when empty).
func (e *Engine) Faults(sel string) []FaultView {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, _ := selector.Parse(sel)
	var out []FaultView
	for _, f := range e.faults {
		if sel == "" || s.MatchPath(f.Target) || s.MatchPort(f.Target) || s.MatchPath(f.Device.Path) {
			out = append(out, f.View())
		}
	}
	return out
}

// Catalogue returns the fault reference for an id.
func (e *Engine) Catalogue(id string) *pack.FaultRef { return e.Reg.Faults[id] }

// BMCBehaviour reports what faults do to a BMC's answers: availability, added latency and
// the share of requests answered with 500.
func (e *Engine) BMCBehaviour(d *model.Device) (up bool, latency time.Duration, errorPercent int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.bmcAvailable(d), e.bmcLatency(d), e.bmcErrors(d)
}

func (e *Engine) bmcAvailable(d *model.Device) bool {
	if !d.State.AC || !d.State.BMCUp {
		return false
	}
	for _, f := range e.faultsOn(d) {
		if v, ok := f.Sets["bmc"]; ok && (v == "unavailable" || v == "resetting") {
			return false
		}
	}
	return true
}

func (e *Engine) bmcLatency(d *model.Device) time.Duration {
	for _, f := range e.faultsOn(d) {
		if v, ok := f.Sets["bmc.latencyMs"]; ok {
			n, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
			return time.Duration(n) * time.Millisecond
		}
	}
	return 0
}

func (e *Engine) bmcErrors(d *model.Device) int {
	for _, f := range e.faultsOn(d) {
		if v, ok := f.Sets["bmc.errorPercent"]; ok {
			n, _ := strconv.Atoi(fmt.Sprint(v))
			return n
		}
	}
	return 0
}

// PortUp reports a port's operational state: admin up, cabled link up, not forced down by a
// fault, and its device powered (management ports need only standby power).
func (e *Engine) PortUp(p *model.Port) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.portUp(p)
}

func (e *Engine) portUp(p *model.Port) bool {
	if !p.AdminUp {
		return false
	}
	if p.Link != nil && !p.Link.Up {
		return false
	}
	for _, f := range e.faults {
		for _, x := range f.PortsDown {
			if x == p {
				return false
			}
		}
	}
	d := p.Device
	if !d.State.AC {
		return false
	}
	if p.Role != "management" && d.State.Power != model.On && d.BaseClass != "switch" {
		return false
	}
	return true
}

// FaultSets returns the effect values on a device's active faults (for adapters' overlays).
func (e *Engine) FaultSets(d *model.Device) map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string]any{}
	for _, f := range e.faultsTouching(d) {
		for k, v := range f.Sets {
			out[k] = v
		}
	}
	return out
}
