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

// Links lists the cabling, each link with both ends and whether it is up (a pulled cable is
// down), in id order. sel, when set, keeps the links with an end on a device it names.
func (e *Engine) Links(sel string) ([]map[string]any, error) {
	var s *selector.Selector
	if sel != "" {
		var err error
		if s, err = selector.Parse(sel); err != nil {
			return nil, err
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	end := func(p *model.Port) map[string]any {
		if p == nil {
			return nil
		}
		out := map[string]any{"ref": p.Ref, "port": p.Name, "kind": p.Kind, "role": p.Role, "speed": p.Speed, "network": p.Network}
		if p.Device != nil {
			out["device"] = p.Device.Path
			if r := p.Device.RackOf(); r != nil {
				out["rack"] = r.Name
			}
			out["class"] = p.Device.Class
		}
		return out
	}
	on := func(p *model.Port) bool { return p != nil && p.Device != nil && s.MatchPath(p.Device.Path) }
	out := []map[string]any{}
	for _, l := range e.M.Links {
		if s != nil && !on(l.A) && !on(l.B) {
			continue
		}
		out = append(out, map[string]any{"id": l.ID, "kind": l.Kind, "fabric": l.Fabric, "up": l.Up, "a": end(l.A), "b": end(l.B)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
	return out, nil
}

// Facility is the facility as the console draws it: feeds with the load each carries, cooling
// loops with their CDU's readings, and every rack's inputs, feeds and loop.
func (e *Engine) Facility() map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	feedW := map[string]float64{}
	racks := []any{}
	for _, r := range e.M.Racks {
		capW, byFeed := 0.0, map[string]float64{}
		inputs := []any{}
		for _, in := range r.Inputs {
			capW += in.CapacityW
			for _, s := range in.Sources {
				if fn, ok := in.SourceFeed[s.Path]; ok {
					w := e.sourceOutput(s)
					feedW[fn] += w
					byFeed[fn] += w
				}
			}
			inputs = append(inputs, map[string]any{"name": in.Name, "kind": in.Kind, "capacityW": in.CapacityW, "redundancy": in.Redundancy,
				"minSources": in.MinSources, "sources": len(in.Sources), "delivering": e.deliveringSources(in), "loadW": round(e.inputLoad(in))})
		}
		// a rack without feeds of its own draws from the estate's, as the compiler assigned them
		feeds := append([]string{}, r.Feeds...)
		for k, v := range byFeed {
			byFeed[k] = round(v)
			if len(r.Feeds) == 0 {
				feeds = append(feeds, k)
			}
		}
		sort.Strings(feeds)
		loop, row := "", ""
		if r.Loop != nil {
			loop = r.Loop.Name
		}
		if r.Row != nil {
			row = r.Row.Name
		}
		racks = append(racks, map[string]any{"name": r.Name, "path": r.Path, "row": row, "position": r.Position, "format": r.FormatName,
			"powerW": round(e.rackPower(r)), "capacityW": capW, "feeds": feeds, "feedW": byFeed, "loop": loop, "inputs": inputs, "isolated": r.Isolated})
	}
	feeds := []any{}
	for _, f := range e.M.Feeds {
		feeds = append(feeds, map[string]any{"name": f.Name, "path": f.Path, "capacityW": f.CapacityW, "up": f.Up, "loadW": round(feedW[f.Name])})
	}
	sort.Slice(feeds, func(i, j int) bool {
		return feeds[i].(map[string]any)["name"].(string) < feeds[j].(map[string]any)["name"].(string)
	})
	loops := []any{}
	for _, lp := range e.M.Loops {
		names, kw := []string{}, 0.0
		for _, r := range lp.Racks {
			names = append(names, r.Name)
			kw += e.rackPower(r) / 1000
		}
		v := map[string]any{"name": lp.Name, "profile": lp.Profile, "racks": names, "capacityKW": lp.CapacityKW, "flow": lp.Flow,
			"supplyC": round(lp.SupplyC), "loadKW": round(kw)}
		if len(e.M.Halls) > 0 {
			if cdu := e.M.DeviceBy[e.M.Halls[0].Path+"/facility/"+lp.Name]; cdu != nil {
				v["device"] = cdu.Path
				v["health"] = e.deviceHealth(cdu)
				v["sensors"] = e.sensorsLocked(cdu)
			}
		}
		loops = append(loops, v)
	}
	sort.Slice(loops, func(i, j int) bool {
		return loops[i].(map[string]any)["name"].(string) < loops[j].(map[string]any)["name"].(string)
	})
	return map[string]any{"feeds": feeds, "power": e.powerChainView(), "loops": loops, "racks": racks}
}
