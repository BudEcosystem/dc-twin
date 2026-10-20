package engine

import (
	"sort"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/selector"
)

// DeviceView is a device's model state as the control API, the console and scenario CEL see
// it: owned fields, derived readings and active faults.
func (e *Engine) DeviceView(d *model.Device) map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.deviceView(d)
}

func (e *Engine) deviceView(d *model.Device) map[string]any {
	comps := []any{}
	for _, c := range d.Components {
		comps = append(comps, map[string]any{"name": c.Name, "path": c.Path, "kind": c.Kind, "model": c.Model, "vendor": c.Vendor,
			"health": e.componentHealth(c), "state": e.componentState(c), "counters": c.State.Counters})
	}
	fw := map[string]any{}
	for k, v := range d.State.Firmware {
		fw[k] = map[string]any{"version": v.Version, "baseline": v.Baseline, "staged": v.Staged}
	}
	faults := []any{}
	for _, f := range e.faultsTouching(d) {
		faults = append(faults, map[string]any{"id": f.ID, "fault": f.Fault, "target": f.Target})
	}
	rack := ""
	if r := d.RackOf(); r != nil {
		rack = r.Name
	}
	lease := ""
	if d.State.Lease != nil {
		lease = d.State.Lease.IP
	}
	sensors := map[string]any{}
	for k, v := range e.sensorsLocked(d) {
		sensors[k] = v
	}
	out := map[string]any{
		"path": d.Path, "name": d.Name, "rack": rack, "slot": d.Slot, "u": d.U, "units": d.Units,
		"class": d.Class, "profile": d.ProfileName, "fidelity": d.Fidelity, "os": d.OS, "dialect": d.Dialect,
		"generated": d.Generated,
		"power":     d.State.Power, "ac": d.State.AC, "bmc": e.bmcAvailable(d), "booted": d.State.Booted,
		"health": e.deviceHealth(d), "drawW": round(e.draw(d)),
		"identity":   map[string]any{"serial": d.Identity.Serial, "uuid": d.Identity.UUID, "bmcMAC": d.Identity.BMCMAC},
		"components": comps, "firmware": fw, "faults": faults, "sensors": sensors, "lease": lease,
		"endpoint": map[string]any{"redfish": d.Endpoint.ListenTLS, "siteIP": d.Endpoint.SiteIP, "ipmi": d.Endpoint.IPMI, "snmp": d.Endpoint.SNMP},
		"boot": map[string]any{"overrideTarget": d.State.Boot.OverrideTarget, "overrideEnabled": d.State.Boot.OverrideEnabled,
			"overrideMode": d.State.Boot.OverrideMode, "httpBootUri": d.State.Boot.HTTPBootURI, "order": d.State.Boot.Order},
		"lockdown": d.State.Lockdown, "secureBoot": d.State.SecureBoot,
	}
	if d.Parent != nil {
		out["parent"] = d.Parent.Path
	}
	if _, ok := e.substrates[d.Path]; ok {
		// these fields are the substrate's, as its observer last reported them (D19)
		out["observed"] = []string{"power", "boot"}
	}
	return out
}

// Select returns the views of devices a selector names, in path order.
func (e *Engine) Select(sel string) ([]map[string]any, error) {
	s, err := selector.Parse(sel)
	if err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []map[string]any
	for _, d := range e.M.Devices {
		if s.MatchPath(d.Path) && s.MatchAttrs(d.Attr) {
			out = append(out, e.deviceView(d))
		}
	}
	return out, nil
}

// SelectDevices returns the devices a selector names.
func (e *Engine) SelectDevices(sel string) ([]*model.Device, error) {
	s, err := selector.Parse(sel)
	if err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []*model.Device
	for _, d := range e.M.Devices {
		if s.MatchPath(d.Path) && s.MatchAttrs(d.Attr) {
			out = append(out, d)
		}
	}
	return out, nil
}

// Status summarises the estate: devices by power, health and fidelity, and incidents.
func (e *Engine) Status() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	power, health, fid := map[string]int{}, map[string]int{}, map[string]int{}
	for _, d := range e.M.Devices {
		power[d.State.Power]++
		health[e.deviceHealth(d)]++
		fid[d.Fidelity]++
	}
	doms := map[string]string{}
	for k, v := range e.domains {
		doms[k] = v
	}
	racks := []any{}
	for _, r := range e.M.Racks {
		racks = append(racks, map[string]any{"name": r.Name, "path": r.Path, "format": r.FormatName, "powerW": round(e.rackPower(r)), "isolated": r.Isolated})
	}
	feeds := map[string]bool{}
	for k, f := range e.M.Feeds {
		feeds[k] = f.Up
	}
	return map[string]any{"estate": e.M.Name, "clock": e.M.Clock, "now": e.now, "timeScale": e.scale, "devices": len(e.M.Devices),
		"links": len(e.M.Links), "power": power, "health": health, "fidelity": fid, "domains": doms, "racks": racks,
		"faults": len(e.faults), "incidents": len(e.incidents), "seq": e.seq, "feeds": feeds}
}

// Domains lists every domain with its state.
func (e *Engine) Domains() []map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []map[string]any
	for _, d := range e.M.Domains {
		out = append(out, map[string]any{"path": d.Path, "kind": d.Kind, "technology": d.Technology, "members": len(d.Members), "state": e.domains[d.Path], "manager": d.Manager})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["path"].(string) < out[j]["path"].(string) })
	return out
}
