package engine

import (
	"fmt"
	"slices"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// A DPU's NVMe emulation (docs/reviews/2026-11-08-storage-deep-dive.md 5.3, P5): a BlueField whose
// profile can emulate NVMe (behaviours.nvmeEmulation) presents NVMe functions to its host, each
// backed by a storage system's volume that the DPU reaches as an NVMe-oF host, as DOCA SNAP does
// for DPF's DPUVolumeAttachment. Attaching maps the volume to the DPU's host NQN in the storage
// system; detaching unmaps it. The state is owned; what the host sees of it is the adapters'.

// EmulationView is a DPU's NVMe emulation as the API reports it.
type EmulationView struct {
	Device    string
	Enabled   bool
	HostNQN   string
	Max       int
	Functions []model.EmulatedFunction
}

// DPUHostNQN is the NQN a DPU connects to storage targets as: NVMe's UUID form, from its identity.
func DPUHostNQN(d *model.Device) string {
	return "nqn.2014-08.org.nvmexpress:uuid:" + d.Identity.UUID
}

func (e *Engine) emulationDPU(r Request) (*model.Device, int, error) {
	d, err := e.dev(r)
	if err != nil {
		return nil, 0, err
	}
	max := int(behaviour(d, "nvmeEmulation", "functions", 0))
	if d.BaseClass != "dpu" || max == 0 {
		return nil, 0, fmt.Errorf("%w: %s (%s) does not emulate NVMe", ErrUnsupported, d.Path, d.ProfileName)
	}
	if d.State.Emulation == nil {
		d.State.Emulation = &model.NVMeEmulation{}
	}
	return d, max, nil
}

// Emulation reports a DPU's NVMe emulation.
func (e *Engine) Emulation(path string) (EmulationView, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return EmulationView{}, ErrNoDevice
	}
	max := int(behaviour(d, "nvmeEmulation", "functions", 0))
	if d.BaseClass != "dpu" || max == 0 {
		return EmulationView{}, fmt.Errorf("%w: %s does not emulate NVMe", ErrUnsupported, path)
	}
	v := EmulationView{Device: path, HostNQN: DPUHostNQN(d), Max: max}
	if em := d.State.Emulation; em != nil {
		v.Enabled = em.Enabled
		v.Functions = append(v.Functions, em.Functions...)
	}
	return v, nil
}

// SetNVMeEmulation turns a DPU's NVMe emulation on or off (nvconfig NVME_EMULATION_ENABLE); off
// is refused while functions are attached.
func (e *Engine) SetNVMeEmulation(r Request, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, _, err := e.emulationDPU(r)
	if err != nil {
		e.logRequest(r, httpStatus(r, 400))
		return err
	}
	em := d.State.Emulation
	if !on && len(em.Functions) > 0 {
		e.logRequest(r, httpStatus(r, 409))
		return fmt.Errorf("%w: %d emulated functions are attached; detach them first", ErrPrecondition, len(em.Functions))
	}
	c := e.logRequest(r, httpStatus(r, 200))
	if em.Enabled != on {
		e.stateEvent(d.Path, "nvmeEmulation", em.Enabled, on, r.Source, c, "")
		em.Enabled = on
	}
	return nil
}

// AttachEmulatedVolume presents a storage system's volume to the DPU's host as an NVMe function.
func (e *Engine) AttachEmulatedVolume(r Request, system, volume string) (model.EmulatedFunction, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, max, err := e.emulationDPU(r)
	fail := func(status int, err error) (model.EmulatedFunction, error) {
		e.logRequest(r, httpStatus(r, status))
		return model.EmulatedFunction{}, err
	}
	if err != nil {
		return fail(400, err)
	}
	em := d.State.Emulation
	if !em.Enabled {
		return fail(409, fmt.Errorf("%w: %s's NVMe emulation is off", ErrPrecondition, d.Path))
	}
	if len(em.Functions) >= max {
		return fail(409, fmt.Errorf("%w: %s presents its %d functions already", ErrPrecondition, d.Path, max))
	}
	s, err := e.system(system)
	if err != nil {
		return fail(404, err)
	}
	v := findVolume(s, volume)
	if v == nil {
		return fail(404, fmt.Errorf("%w: volume %s of %s", ErrNoStorageObject, volume, s.Name))
	}
	for _, f := range em.Functions {
		if f.System == s.Path && f.Volume == v.ID {
			return fail(409, fmt.Errorf("%w: volume %s is attached to %s as function %d", ErrExists, v.Name, d.Path, f.ID))
		}
	}
	nqn := DPUHostNQN(d)
	if !slices.Contains(s.State.Hosts, nqn) {
		s.State.Hosts = append(s.State.Hosts, nqn)
	}
	if !slices.Contains(v.Hosts, nqn) {
		v.Hosts = append(v.Hosts, nqn)
	}
	em.Seq++
	f := model.EmulatedFunction{ID: em.Seq, System: s.Path, Volume: v.ID, HostNQN: nqn,
		Serial: fmt.Sprintf("SNAP%s%02d", d.Identity.Serial, em.Seq), Created: e.now}
	em.Functions = append(em.Functions, f)
	c := e.logRequest(r, httpStatus(r, 201))
	e.commit(schema.Event{Kind: "state", Source: r.Source, Path: d.Path, Cause: c,
		State: &schema.EventState{Field: fmt.Sprintf("nvmeEmulation.function.%d", f.ID), From: nil, To: s.Name + "/" + v.Name},
		Data:  map[string]any{"system": s.Path, "volume": v.ID, "hostNQN": nqn, "serial": f.Serial}})
	e.objectEvent(r, s, c, "volume", v.ID, "", "mapped", map[string]any{"host": nqn, "dpu": d.Path})
	return f, nil
}

// DetachEmulatedVolume removes an emulated function and unmaps its volume from the DPU.
func (e *Engine) DetachEmulatedVolume(r Request, id int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, _, err := e.emulationDPU(r)
	if err != nil {
		e.logRequest(r, httpStatus(r, 400))
		return err
	}
	em := d.State.Emulation
	i := slices.IndexFunc(em.Functions, func(f model.EmulatedFunction) bool { return f.ID == id })
	if i < 0 {
		e.logRequest(r, httpStatus(r, 404))
		return fmt.Errorf("%w: %s has no emulated function %d", ErrNoStorageObject, d.Path, id)
	}
	f := em.Functions[i]
	em.Functions = slices.Delete(em.Functions, i, i+1)
	if s := e.M.StorageBy[f.System]; s != nil {
		if v := findVolume(s, f.Volume); v != nil {
			shared := false
			for _, g := range em.Functions {
				shared = shared || (g.System == f.System && g.Volume == f.Volume)
			}
			if !shared {
				v.Hosts = slices.DeleteFunc(v.Hosts, func(h string) bool { return h == f.HostNQN })
			}
		}
	}
	c := e.logRequest(r, httpStatus(r, 204))
	e.commit(schema.Event{Kind: "state", Source: r.Source, Path: d.Path, Cause: c,
		State: &schema.EventState{Field: fmt.Sprintf("nvmeEmulation.function.%d", f.ID), From: f.Volume, To: nil}})
	return nil
}

// EmulatedFunctions lists every attached function of every DPU, by DPU path.
func (e *Engine) EmulatedFunctions() map[string][]model.EmulatedFunction {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := map[string][]model.EmulatedFunction{}
	for _, d := range e.M.Devices {
		if em := d.State.Emulation; em != nil && len(em.Functions) > 0 {
			out[d.Path] = append([]model.EmulatedFunction{}, em.Functions...)
		}
	}
	return out
}
