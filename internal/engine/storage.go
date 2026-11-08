package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/selector"
)

// driveSerial is the serial of the drive now in a bay: a replacement's own, else the first's.
func (e *Engine) driveSerial(c *model.Component) string {
	if c.State.Serial != "" {
		return c.State.Serial
	}
	return c.Drive.Serial
}

// Storage behaviours (docs/reviews/2026-11-08-storage-deep-dive.md 5.1, 5.2): what a drive holds,
// its sanitisation, technician acts on drives, and storage controllers' logical volumes. Every
// projection of a drive (Redfish, IPMI, the guest's NVMe device, exporters, impersonated agents)
// reads DriveStatus, so they agree.

// Contents a drive can hold.
const (
	ContentsEmpty     = "empty"
	ContentsInstalled = "installed"
	ContentsData      = "data"
	ContentsErased    = "erased"
	ContentsErasing   = "erasing"
)

var errNoDrive = fmt.Errorf("%w: no such drive", ErrPrecondition)

// NVMe SMART critical-warning bits (NVMe Base 2.0, Get Log Page 02h, byte 0).
const (
	SMARTSpare       = 1 << 0 // available spare below threshold
	SMARTTemperature = 1 << 1
	SMARTReliability = 1 << 2 // NVM subsystem reliability degraded
	SMARTReadOnly    = 1 << 3
	SMARTBackup      = 1 << 4 // volatile memory backup failed
)

// DriveStatus is a drive as every interface reports it: the model's facts, its contents, and what
// its active faults make of it.
type DriveStatus struct {
	Present          bool
	Health, State    string
	Contents         string
	CriticalWarning  int
	FailurePredicted bool
	ReadOnly         bool
	EraseFails       bool
	PercentageUsed   float64
	AvailableSpare   float64
	LifeLeftPercent  float64
	TemperatureC     float64
	MediaErrors      float64
	PowerOnHours     float64
	// Erasing is a secure erase in progress, Percent how far it is.
	Erasing bool
	Percent int
	// Serial and EUI64 are the identity of the drive in the bay now (a replacement has its own).
	Serial, EUI64 string
	// Replaced is how many times the drive was replaced.
	Replaced int
}

// componentSets merges the effect values of a component's active faults.
func (e *Engine) componentSets(c *model.Component) map[string]any {
	out := map[string]any{}
	for _, f := range e.faultsOnComponent(c) {
		for k, v := range f.Sets {
			out[k] = v
		}
	}
	return out
}

// ComponentSets returns the effect values of a component's active faults.
func (e *Engine) ComponentSets(c *model.Component) map[string]any {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.componentSets(c)
}

func num(v any, def float64) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		if n, err := strconv.ParseFloat(x, 64); err == nil {
			return n
		}
	}
	return def
}

func truthy(v any) bool { return v == true || fmt.Sprint(v) == "true" }

func (e *Engine) driveStatus(c *model.Component) DriveStatus {
	sets := e.componentSets(c)
	s := DriveStatus{Present: !c.State.Pulled, Health: e.componentHealth(c), State: e.componentState(c),
		Contents: c.State.Contents, PercentageUsed: 2, AvailableSpare: 100, LifeLeftPercent: 98, TemperatureC: 38,
		Serial: e.driveSerial(c), EUI64: c.Drive.EUI64, Replaced: c.State.Replaced}
	if c.State.EUI64 != "" {
		s.EUI64 = c.State.EUI64
	}
	if v, ok := sets["drive.present"]; ok && !truthy(v) {
		s.Present = false
	}
	if !s.Present {
		s.State = "Absent"
	}
	s.CriticalWarning = int(num(sets["smart.criticalWarning"], 0))
	s.FailurePredicted = truthy(sets["drive.failurePredicted"])
	s.ReadOnly = truthy(sets["drive.readOnly"])
	s.EraseFails = truthy(sets["drive.eraseFails"])
	s.PercentageUsed = num(sets["drive.percentageUsed"], s.PercentageUsed)
	s.AvailableSpare = num(sets["drive.availableSpare"], s.AvailableSpare)
	s.LifeLeftPercent = num(sets["drive.lifeLeftPercent"], 100-s.PercentageUsed)
	if s.LifeLeftPercent < 0 {
		s.LifeLeftPercent = 0
	}
	s.TemperatureC = num(sets["drive.temperatureC"], s.TemperatureC+10*c.Device.State.Utilisation)
	s.MediaErrors = c.State.Counters["mediaErrors"]
	if !e.started.IsZero() {
		s.PowerOnHours = 1200 + e.now.Sub(e.started).Hours()
	}
	if s.ReadOnly {
		s.CriticalWarning |= SMARTReadOnly
	}
	if s.FailurePredicted {
		s.CriticalWarning |= SMARTReliability
	}
	if c.State.Contents == ContentsErasing {
		s.Erasing = true
		for _, t := range c.Device.State.Tasks {
			if t.Canonical == "erase.start" && t.State == "Running" && strings.HasSuffix(t.Message, " "+c.Name) {
				s.Percent = t.Percent
			}
		}
	}
	return s
}

// DriveStatus returns what every interface reports of a drive.
func (e *Engine) DriveStatus(c *model.Component) DriveStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.driveStatus(c)
}

// bootable reports whether a host can boot from its boot drive: present, not failed, holding an
// installed OS. A host without drives boots as the profile always has (a network-installed OS
// the model does not track).
func (e *Engine) bootable(d *model.Device) (bool, string) {
	bd := d.BootDrive()
	if bd == nil {
		return true, ""
	}
	if hw := e.controllerVolumeBootable(d, bd); hw != "" {
		return false, hw
	}
	s := e.driveStatus(bd)
	switch {
	case !s.Present:
		return false, "boot drive " + bd.Name + " absent"
	case s.State == "UnavailableOffline":
		return false, "boot drive " + bd.Name + " failed"
	case s.Contents != ContentsInstalled:
		return false, "no operating system on " + bd.Name + " (" + s.Contents + ")"
	}
	return true, ""
}

// controllerVolumeBootable explains why a boot drive behind a RAID controller cannot be booted
// from: its volume is failed, or it is in no volume at all on a controller that has volumes.
func (e *Engine) controllerVolumeBootable(d *model.Device, bd *model.Component) string {
	if bd.Drive == nil || bd.Drive.Controller == nil {
		return ""
	}
	for _, v := range d.State.Volumes {
		for _, m := range v.Drives {
			if m == bd.Name && v.Pending != "create" {
				if h, _, _ := e.volumeStatus(d, &v); h == model.Critical {
					return "volume " + v.Name + " failed"
				}
				return ""
			}
		}
	}
	return ""
}

func (e *Engine) driveOf(d *model.Device, name string) *model.Component {
	for _, c := range d.Components {
		if c.Kind == "drive" && (c.Name == name || c.Path == name) {
			return c
		}
	}
	return nil
}

// SecureErase sanitises a drive out of band (erase.start, Drive.SecureErase): a task that runs for
// the drive's erase time and leaves it erased, or ends in an exception when an erase-fails fault
// holds. Kind is the Redfish SanitizationType: CryptographicErase and BlockErase take the
// behaviour's eraseSeconds, Overwrite eraseSecondsPerTB for each pass.
func (e *Engine) SecureErase(r Request, drive, kind string, passes int) (model.Task, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return model.Task{}, err
	}
	if r.Source.Class == "stack" && d.HasBMC() && !e.bmcAvailable(d) {
		return model.Task{}, ErrBMCDown
	}
	c := e.driveOf(d, drive)
	if c == nil {
		return model.Task{}, errNoDrive
	}
	st := e.driveStatus(c)
	switch {
	case !st.Present:
		return model.Task{}, fmt.Errorf("%w: drive %s is absent", ErrPrecondition, c.Name)
	case st.State == "UnavailableOffline":
		return model.Task{}, fmt.Errorf("%w: drive %s has failed", ErrPrecondition, c.Name)
	case st.Erasing:
		return model.Task{}, fmt.Errorf("%w: drive %s is already being erased", ErrPrecondition, c.Name)
	case d.Fidelity == "vm" && d.State.Power != model.Off:
		// the backing is the running guest's disk: the out-of-band erase needs the host off
		return model.Task{}, fmt.Errorf("%w: power the host off before erasing %s out of band", ErrPrecondition, c.Name)
	}
	if kind == "" {
		kind = "CryptographicErase"
	}
	if passes < 1 {
		passes = 1
	}
	takes := behaviour(d, "storage", "eraseSeconds", 8)
	if kind == "Overwrite" {
		tb := float64(c.Drive.CapacityBytes) / 1e12
		takes = behaviour(d, "storage", "eraseSecondsPerTB", 60) * tb * float64(passes)
	}
	cause := e.logRequest(r, httpStatus(r, 202))
	t := model.Task{ID: strconv.Itoa(len(d.State.Tasks) + 1), State: "Running", Started: e.now, Canonical: "erase.start",
		Message: kind + " of " + c.Name}
	d.State.Tasks = append(d.State.Tasks, t)
	idx := len(d.State.Tasks) - 1
	before := c.State.Contents
	c.State.Contents = ContentsErasing
	e.stateEvent(c.Path, "contents", before, ContentsErasing, behaviourSrc, cause, "")
	// progress in tenths, so a client polling the task sees it move
	for i := 1; i < 10; i++ {
		pct := i * 10
		e.after(secs(takes*float64(i)/10), "erase-progress", func() {
			if d.State.Tasks[idx].State == "Running" {
				d.State.Tasks[idx].Percent = pct
			}
		})
	}
	e.after(secs(takes), "erase", func() {
		task := &d.State.Tasks[idx]
		task.Ends, task.Percent = e.now, 100
		if e.driveStatus(c).EraseFails {
			task.State = "Exception"
			task.Message = kind + " of " + c.Name + " failed"
			c.State.Contents = before
			e.stateEvent(c.Path, "contents", ContentsErasing, before, behaviourSrc, cause, "Critical")
			e.addLog(d, "Critical", fmt.Sprintf("The secure erase of drive %s failed.", c.Name), "StorageDevice.1.2.DriveEraseFailed", "SEL", cause)
			return
		}
		task.State = "Completed"
		c.State.Contents = ContentsErased
		e.stateEvent(c.Path, "contents", ContentsErasing, ContentsErased, behaviourSrc, cause, "")
		e.addLog(d, "OK", fmt.Sprintf("Drive %s was erased (%s).", c.Name, kind), "StorageDevice.1.2.DriveEraseCompleted", "SEL", cause)
	})
	return t, nil
}

// SetDriveContents records what a drive holds after software wrote it (an installer, an
// impersonated agent's cleanup): installed, data, empty or erased.
func (e *Engine) SetDriveContents(compPath, contents string, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.componentByPath(compPath)
	if c == nil || c.Kind != "drive" {
		return fmt.Errorf("no drive %s", compPath)
	}
	switch contents {
	case ContentsEmpty, ContentsInstalled, ContentsData, ContentsErased:
	default:
		return fmt.Errorf("contents %q: empty, installed, data or erased", contents)
	}
	if c.State.Contents == contents {
		return nil
	}
	old := c.State.Contents
	c.State.Contents = contents
	e.stateEvent(c.Path, "contents", old, contents, src, 0, "")
	return nil
}

// PullDrive and InsertDrive are a technician taking a drive out of its bay and putting it back
// (drive.pulled, drive.inserted): the drive is absent from every interface while it is out.
func (e *Engine) PullDrive(compPath string, src schema.EventSource) error {
	return e.seatDrive(compPath, true, src)
}

func (e *Engine) InsertDrive(compPath string, src schema.EventSource) error {
	return e.seatDrive(compPath, false, src)
}

func (e *Engine) seatDrive(compPath string, pulled bool, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.componentByPath(compPath)
	if c == nil || c.Kind != "drive" {
		return fmt.Errorf("no drive %s", compPath)
	}
	if c.State.Pulled == pulled {
		return nil
	}
	msg, sev := "insert drive", ""
	if pulled {
		msg, sev = "pull drive", "Warning"
	}
	cause := e.commit(schema.Event{Kind: "act", Source: src, Path: c.Path, Message: msg})
	c.State.Pulled = pulled
	e.stateEvent(c.Path, "present", pulled, !pulled, src, cause, sev)
	d := c.Device
	if pulled {
		e.addLog(d, "Warning", fmt.Sprintf("Drive %s was removed.", c.Name), "StorageDevice.1.2.DriveRemoved", "SEL", cause)
	} else {
		e.addLog(d, "OK", fmt.Sprintf("Drive %s was inserted.", c.Name), "StorageDevice.1.2.DriveInserted", "SEL", cause)
		e.startRebuild(d, c, cause)
	}
	e.rollup(cause)
	return nil
}

// replacedDrive is a new drive in a failed one's bay: empty, present, and rebuilt into any volume
// the old one belonged to.
func (e *Engine) replacedDrive(c *model.Component, cause uint64) {
	old := c.State.Contents
	c.State.Contents, c.State.Pulled = ContentsEmpty, false
	// the replacement keeps the bay's identity (its serial and namespace identifiers): at vm the
	// bay's NVMe subsystem, whose serial a controller must match, outlives the drive in it
	e.stateEvent(c.Path, "contents", old, ContentsEmpty, behaviourSrc, cause, "")
	e.startRebuild(c.Device, c, cause)
}

// ---- volumes

// raidRule is a RAID level's member count rule and how many members it may lose.
type raidRule struct {
	min, mult, tolerate int
	usable              func(n int, member int64) int64
}

var raidRules = map[string]raidRule{
	"RAID0":  {1, 1, 0, func(n int, m int64) int64 { return int64(n) * m }},
	"RAID1":  {2, 2, 1, func(n int, m int64) int64 { return m }},
	"RAID5":  {3, 1, 1, func(n int, m int64) int64 { return int64(n-1) * m }},
	"RAID6":  {4, 1, 2, func(n int, m int64) int64 { return int64(n-2) * m }},
	"RAID10": {4, 2, 1, func(n int, m int64) int64 { return int64(n/2) * m }},
	"RAID50": {6, 2, 1, func(n int, m int64) int64 { return int64(n-2) * m }},
	"RAID60": {8, 2, 2, func(n int, m int64) int64 { return int64(n-4) * m }},
}

// RAIDTypes are the RAID types the twin knows, for a controller that declares none.
func RAIDTypes() []string {
	var out []string
	for k := range raidRules {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// VolumeSpec is a volume create request (volume.create).
type VolumeSpec struct {
	Name, RAIDType, Controller string
	Drives                     []string
	CapacityBytes              int64
	// ApplyTime is Immediate (default) or OnReset.
	ApplyTime string
}

func (e *Engine) controllerOf(d *model.Device, name string) *model.Component {
	for _, c := range d.Components {
		if c.Kind == "storage-controller" && (c.Name == name || c.Group == name) {
			return c
		}
	}
	return nil
}

func supports(ctl *model.Component, raid string) bool {
	types := ctl.Controller.RAIDTypes
	if len(types) == 0 {
		types = RAIDTypes()
	}
	for _, t := range types {
		if t == raid {
			return true
		}
	}
	return false
}

// CreateVolume makes a logical volume over a controller's drives (volume.create). OnReset leaves
// it pending until the host's next reset, as a RAID controller applies its configuration then, and
// returns the task that completes when it does (empty when applied at once).
func (e *Engine) CreateVolume(r Request, spec VolumeSpec) (model.Volume, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, err := e.createVolume(r, spec)
	if err != nil || v.Pending == "" {
		return v, "", err
	}
	d := e.M.DeviceBy[r.Path]
	return v, e.pendingTask(d, "volume.create", "create volume "+v.ID), nil
}

// pendingTask is a task that waits for the host's next reset (OperationApplyTime OnReset).
func (e *Engine) pendingTask(d *model.Device, canonical, message string) string {
	t := model.Task{ID: strconv.Itoa(len(d.State.Tasks) + 1), State: "Pending", Started: e.now, Canonical: canonical, Message: message}
	d.State.Tasks = append(d.State.Tasks, t)
	return t.ID
}

func (e *Engine) createVolume(r Request, spec VolumeSpec) (model.Volume, error) {
	d, err := e.dev(r)
	if err != nil {
		return model.Volume{}, err
	}
	if r.Source.Class == "stack" && d.HasBMC() && !e.bmcAvailable(d) {
		return model.Volume{}, ErrBMCDown
	}
	ctl := e.controllerOf(d, spec.Controller)
	if ctl == nil {
		return model.Volume{}, fmt.Errorf("%w: no storage controller %q", ErrPrecondition, spec.Controller)
	}
	rule, ok := raidRules[spec.RAIDType]
	if !ok || !supports(ctl, spec.RAIDType) {
		return model.Volume{}, fmt.Errorf("%w: controller %s does not support %s", ErrUnsupported, ctl.Name, spec.RAIDType)
	}
	n := len(spec.Drives)
	if n < rule.min || n%rule.mult != 0 {
		return model.Volume{}, fmt.Errorf("%w: %s needs at least %d drives in multiples of %d, not %d", ErrPrecondition, spec.RAIDType, rule.min, rule.mult, n)
	}
	inUse := map[string]string{}
	for _, v := range d.State.Volumes {
		if v.Pending == "delete" {
			continue
		}
		for _, m := range v.Drives {
			inUse[m] = v.Name
		}
	}
	var member int64
	var names []string
	for _, dn := range spec.Drives {
		c := e.driveOf(d, dn)
		if c == nil {
			return model.Volume{}, fmt.Errorf("%w: no drive %s", ErrPrecondition, dn)
		}
		if c.Drive.Controller != ctl {
			return model.Volume{}, fmt.Errorf("%w: drive %s is not behind controller %s", ErrPrecondition, c.Name, ctl.Name)
		}
		if v, used := inUse[c.Name]; used {
			return model.Volume{}, fmt.Errorf("%w: drive %s is already in volume %s", ErrPrecondition, c.Name, v)
		}
		if st := e.driveStatus(c); !st.Present || st.State == "UnavailableOffline" {
			return model.Volume{}, fmt.Errorf("%w: drive %s is not usable", ErrPrecondition, c.Name)
		}
		if member == 0 || c.Drive.CapacityBytes < member {
			member = c.Drive.CapacityBytes
		}
		names = append(names, c.Name)
	}
	usable := rule.usable(n, member)
	capacity := spec.CapacityBytes
	if capacity == 0 {
		capacity = usable
	}
	if capacity > usable {
		return model.Volume{}, fmt.Errorf("%w: %s over %d drives holds %d bytes, not %d", ErrPrecondition, spec.RAIDType, n, usable, capacity)
	}
	d.State.VolumeSeq++
	v := model.Volume{ID: fmt.Sprintf("Volume%d", d.State.VolumeSeq), Name: spec.Name, RAIDType: spec.RAIDType, Controller: ctl.Name,
		Drives: names, CapacityBytes: capacity, Created: e.now}
	if v.Name == "" {
		v.Name = v.ID
	}
	cause := e.logRequest(r, httpStatus(r, 201))
	if spec.ApplyTime == "OnReset" {
		v.Pending = "create"
	}
	d.State.Volumes = append(d.State.Volumes, v)
	e.stateEvent(d.Path, "volume."+v.ID, nil, fmt.Sprintf("%s %s over %s%s", v.Name, v.RAIDType, strings.Join(v.Drives, ","), pendingNote(v.Pending)), behaviourSrc, cause, "")
	if v.Pending == "" {
		e.volumeApplied(d, &d.State.Volumes[len(d.State.Volumes)-1], cause)
	}
	return v, nil
}

func pendingNote(p string) string {
	if p == "" {
		return ""
	}
	return " (pending " + p + " on reset)"
}

// volumeApplied is a volume created on its drives: what they held is gone.
func (e *Engine) volumeApplied(d *model.Device, v *model.Volume, cause uint64) {
	for _, m := range v.Drives {
		if c := e.driveOf(d, m); c != nil && c.State.Contents != ContentsEmpty {
			old := c.State.Contents
			c.State.Contents = ContentsEmpty
			e.stateEvent(c.Path, "contents", old, ContentsEmpty, behaviourSrc, cause, "")
		}
	}
	e.addLog(d, "OK", fmt.Sprintf("Volume %s (%s) was created.", v.Name, v.RAIDType), "StorageDevice.1.2.VolumeCreated", "SEL", cause)
}

// DeleteVolume removes a logical volume (volume.delete); OnReset leaves it until the next reset
// and returns the task that completes then.
func (e *Engine) DeleteVolume(r Request, id, applyTime string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return "", err
	}
	if r.Source.Class == "stack" && d.HasBMC() && !e.bmcAvailable(d) {
		return "", ErrBMCDown
	}
	for i, v := range d.State.Volumes {
		if v.ID != id {
			continue
		}
		cause := e.logRequest(r, httpStatus(r, 204))
		if applyTime == "OnReset" && v.Pending != "create" {
			d.State.Volumes[i].Pending = "delete"
			e.stateEvent(d.Path, "volume."+v.ID, v.Name, "pending delete on reset", behaviourSrc, cause, "")
			return e.pendingTask(d, "volume.delete", "delete volume "+v.ID), nil
		}
		d.State.Volumes = append(d.State.Volumes[:i], d.State.Volumes[i+1:]...)
		e.stateEvent(d.Path, "volume."+v.ID, v.Name, nil, behaviourSrc, cause, "")
		e.addLog(d, "OK", fmt.Sprintf("Volume %s was deleted.", v.Name), "StorageDevice.1.2.VolumeDeleted", "SEL", cause)
		return "", nil
	}
	return "", fmt.Errorf("%w: no volume %s", ErrPrecondition, id)
}

// DecommissionController is an OEM controller decommission (Dell's ControllerDrivesDecommission,
// storage.decommission): its volumes are deleted and its drives erased, as a job that runs for the
// behaviour's eraseSeconds; id names the job as the BMC hands it back.
func (e *Engine) DecommissionController(r Request, controller, id string) (model.Task, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return model.Task{}, err
	}
	if r.Source.Class == "stack" && d.HasBMC() && !e.bmcAvailable(d) {
		return model.Task{}, ErrBMCDown
	}
	ctl := e.controllerOf(d, controller)
	if ctl == nil {
		return model.Task{}, fmt.Errorf("%w: no storage controller %q", ErrPrecondition, controller)
	}
	cause := e.logRequest(r, httpStatus(r, 202))
	t := model.Task{ID: id, State: "Running", Started: e.now, Canonical: r.Canonical, Message: "decommission " + ctl.Name}
	d.State.Tasks = append(d.State.Tasks, t)
	idx := len(d.State.Tasks) - 1
	e.after(secs(behaviour(d, "storage", "eraseSeconds", 8)), "decommission", func() {
		var keep []model.Volume
		for _, v := range d.State.Volumes {
			if v.Controller == ctl.Name {
				e.stateEvent(d.Path, "volume."+v.ID, v.Name, nil, behaviourSrc, cause, "")
				continue
			}
			keep = append(keep, v)
		}
		d.State.Volumes = keep
		for _, c := range ctl.Controller.Drives {
			if c.State.Contents != ContentsErased {
				old := c.State.Contents
				c.State.Contents = ContentsErased
				e.stateEvent(c.Path, "contents", old, ContentsErased, behaviourSrc, cause, "")
			}
		}
		d.State.Tasks[idx].State, d.State.Tasks[idx].Ends, d.State.Tasks[idx].Percent = "Completed", e.now, 100
		e.addLog(d, "OK", fmt.Sprintf("Controller %s was decommissioned: its volumes deleted and its drives erased.", ctl.Name), "StorageDevice.1.2.ControllerDecommissioned", "SEL", cause)
	})
	return t, nil
}

// applyVolumes applies pending volume changes on a host's reset, and completes their tasks.
func (e *Engine) applyVolumes(d *model.Device, cause uint64) {
	for i := range d.State.Tasks {
		t := &d.State.Tasks[i]
		if t.State == "Pending" && (t.Canonical == "volume.create" || t.Canonical == "volume.delete") {
			t.State, t.Ends, t.Percent = "Completed", e.now, 100
		}
	}
	var keep []model.Volume
	for _, v := range d.State.Volumes {
		switch v.Pending {
		case "delete":
			e.stateEvent(d.Path, "volume."+v.ID, v.Name, nil, behaviourSrc, cause, "")
			e.addLog(d, "OK", fmt.Sprintf("Volume %s was deleted.", v.Name), "StorageDevice.1.2.VolumeDeleted", "SEL", cause)
			continue
		case "create":
			v.Pending = ""
			e.stateEvent(d.Path, "volume."+v.ID, "pending create", v.Name, behaviourSrc, cause, "")
			e.volumeApplied(d, &v, cause)
		}
		keep = append(keep, v)
	}
	d.State.Volumes = keep
}

// startRebuild begins rebuilding every redundant volume a drive was returned to.
func (e *Engine) startRebuild(d *model.Device, c *model.Component, cause uint64) {
	for i, v := range d.State.Volumes {
		for _, m := range v.Drives {
			if m != c.Name || raidRules[v.RAIDType].tolerate == 0 || v.Pending != "" {
				continue
			}
			takes := secs(behaviour(d, "storage", "rebuildSeconds", 120) * e.scale)
			d.State.Volumes[i].RebuildUntil = e.now.Add(takes)
			e.stateEvent(d.Path, "volume."+v.ID, "degraded", "rebuilding", behaviourSrc, cause, "Warning")
			id := v.ID
			e.after(secs(behaviour(d, "storage", "rebuildSeconds", 120)), "rebuild", func() {
				for j := range d.State.Volumes {
					if d.State.Volumes[j].ID == id {
						d.State.Volumes[j].RebuildUntil = time.Time{}
						e.stateEvent(d.Path, "volume."+id, "rebuilding", "optimal", behaviourSrc, cause, "")
					}
				}
			})
		}
	}
}

// volumeStatus is a volume's health, Redfish state and rebuild progress (0..100, -1: none).
func (e *Engine) volumeStatus(d *model.Device, v *model.Volume) (string, string, int) {
	if v.Pending == "create" {
		return model.OK, "StandbyOffline", -1
	}
	failed := 0
	for _, m := range v.Drives {
		c := e.driveOf(d, m)
		if c == nil {
			failed++
			continue
		}
		if st := e.driveStatus(c); !st.Present || st.State == "UnavailableOffline" {
			failed++
		}
	}
	tol := raidRules[v.RAIDType].tolerate
	switch {
	case failed > tol:
		return model.Critical, "UnavailableOffline", -1
	case failed > 0:
		return model.Warning, "Degraded", -1
	}
	if !v.RebuildUntil.IsZero() && e.now.Before(v.RebuildUntil) {
		total := secs(behaviour(d, "storage", "rebuildSeconds", 120) * e.scale)
		left := v.RebuildUntil.Sub(e.now)
		pct := 100 - int(100*left/max(total, time.Second))
		return model.Warning, "Updating", min(max(pct, 0), 99)
	}
	return model.OK, "Enabled", -1
}

// VolumeView is a volume as projections read it.
type VolumeView struct {
	model.Volume
	Health, State string
	Rebuild       int
}

// Volumes returns a device's volumes and their state.
func (e *Engine) Volumes(d *model.Device) []VolumeView {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []VolumeView
	for _, v := range d.State.Volumes {
		h, st, pct := e.volumeStatus(d, &v)
		vv := VolumeView{Volume: v, Health: h, State: st, Rebuild: pct}
		vv.Drives = append([]string{}, v.Drives...)
		out = append(out, vv)
	}
	return out
}

// SelectDrives returns the drives a selector names by path ("a01/compute-tray-07/nvme-*"), or
// every drive of the devices it names.
func (e *Engine) SelectDrives(sel string) ([]*model.Component, error) {
	s, err := selector.Parse(sel)
	if err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []*model.Component
	for _, d := range e.M.Devices {
		devMatch := s.MatchPath(d.Path) && s.MatchAttrs(d.Attr)
		for _, c := range d.Components {
			if c.Kind == "drive" && (devMatch || s.MatchPath(c.Path)) {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// DriveAct is a technician's act on drives (drive.pulled, drive.inserted, component.replaced) or an
// operator's out-of-band erase.
func (e *Engine) DriveAct(sel, act string, src schema.EventSource) (int, error) {
	ds, err := e.SelectDrives(sel)
	if err != nil {
		return 0, err
	}
	if len(ds) == 0 {
		return 0, fmt.Errorf("%q names no drive", sel)
	}
	for _, c := range ds {
		switch act {
		case "pull":
			err = e.PullDrive(c.Path, src)
		case "insert":
			err = e.InsertDrive(c.Path, src)
		case "replace":
			err = e.ReplaceComponent(c.Path, src)
		case "erase":
			_, err = e.SecureErase(Request{Path: c.Device.Path, Canonical: "erase.start", Source: src, Protocol: "control"}, c.Name, "", 0)
		default:
			return 0, fmt.Errorf("drive act %q: pull, insert, replace or erase", act)
		}
		if err != nil {
			return 0, fmt.Errorf("%s: %w", c.Path, err)
		}
	}
	return len(ds), nil
}
