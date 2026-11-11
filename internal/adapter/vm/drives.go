package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
)

// A `vm` host's drives (docs/reviews/2026-11-08-storage-deep-dive.md 5.1): each of the model's
// drives is an NVMe controller in the guest, in bay order (the guest's nvme<bay>), on a sparse
// backing file in the device's state directory that keeps what the guest wrote across power
// cycles. The machine follows the model while it runs: a drive that fails, is pulled or goes
// missing leaves the guest's PCIe bus; one inserted or replaced joins it, empty; a fault's SMART
// critical-warning bits are raised in the drive's SMART log. An out-of-band erase (the engine
// allows it with the host off) empties the backing.

// defaultDriveBytes backs a drive whose profile gives no size.
const defaultDriveBytes = 64 << 30

type vmDrive struct {
	comp     *model.Component
	bay      int
	path     string
	size     int64
	attached bool   // in the running guest's PCIe tree
	removing bool   // device_del sent; DEVICE_DELETED awaited
	hasNS    bool   // the bay's subsystem holds the drive's namespace (and block node)
	warn     int    // the SMART critical-warning bits last set
	contents string // the model's contents as last seen
	zero     bool   // empty the backing once no guest has it open
	// removedAt is when the guest last released the drive
	removedAt time.Time
}

// settle is how long a bay stays empty after the guest releases a drive before another is
// plugged into it: Linux finishes tearing the old controller's subsystem down meanwhile.
const settle = 3 * time.Second

// planDrives makes the backing of every drive of the device (sparse, created once).
func (m *machine) planDrives(d *model.Device) error {
	if m.a.opts.Drives == "off" {
		return nil
	}
	for _, c := range d.Drives() {
		size := c.Drive.CapacityBytes
		if size <= 0 {
			size = defaultDriveBytes
		}
		dr := &vmDrive{comp: c, bay: len(m.drives), path: filepath.Join(m.dir, "drive-"+c.Name+".raw"), size: size, contents: c.State.Contents}
		got, err := qemu.CreateBacking(dr.path, size)
		if err != nil {
			return fmt.Errorf("drive %s: %w", c.Name, err)
		}
		if got != size {
			dr.size = got
			m.a.cappedMu.Lock()
			m.a.capped = append(m.a.capped, fmt.Sprintf("%s: drive %s is %d bytes in the guest, not its %d: the host's file system holds no larger file for its backing", d.Path, c.Name, got, size))
			m.a.cappedMu.Unlock()
		}
		m.drives = append(m.drives, dr)
	}
	return nil
}

func (dr *vmDrive) qemu(absent bool, st engine.DriveStatus) qemu.Drive {
	c := dr.comp
	fw := c.Drive.Firmware
	if fw == "" {
		fw = "1.0.0" // as the BMC reports it (Redfish Revision)
	}
	return qemu.Drive{Path: dr.path, Serial: st.Serial, Model: c.Model, Firmware: fw, EUI64: st.EUI64, Absent: absent}
}

// wantDrive is whether a drive is in its bay and answering: present and not failed.
func wantDrive(st engine.DriveStatus) bool { return st.Present && st.State != "UnavailableOffline" }

// driveConfig is the machine's drives at power-on, with what each one holds settled first.
func (m *machine) driveConfig() []qemu.Drive {
	m.driveMu.Lock()
	defer m.driveMu.Unlock()
	var out []qemu.Drive
	for _, dr := range m.drives {
		st := m.a.rt.Engine.DriveStatus(dr.comp)
		m.settleContents(dr, st)
		if dr.zero {
			m.zeroDrive(dr)
		}
		want := wantDrive(st)
		dr.attached, dr.removing, dr.warn, dr.hasNS = want, false, 0, want
		out = append(out, dr.qemu(!want, st))
	}
	return out
}

// settleContents notes a change of what the model says the drive holds: erased or empty (an
// erase, a replacement) means its backing is emptied before a guest sees it again.
func (m *machine) settleContents(dr *vmDrive, st engine.DriveStatus) {
	if st.Contents == dr.contents {
		return
	}
	prev := dr.contents
	dr.contents = st.Contents
	blank := func(c string) bool { return c == engine.ContentsErased || c == engine.ContentsEmpty }
	if prev != "" && blank(st.Contents) && !blank(prev) {
		dr.zero = true
	}
}

func (m *machine) zeroDrive(dr *vmDrive) {
	if err := qemu.ZeroBacking(dr.path, dr.size); err != nil {
		m.observe("drive."+dr.comp.Name, "zero-failed", fmt.Sprintf("drive %s: emptying its backing failed: %v", dr.comp.Name, err), 0, nil)
		return
	}
	dr.zero = false
	m.observe("drive."+dr.comp.Name, "empty", fmt.Sprintf("drive %s (bay %d): its backing emptied (%s); every block reads zero", dr.comp.Name, dr.bay, dr.contents), 0,
		map[string]any{"backing": dr.path})
}

// syncDrives brings a running machine's drives in line with the model: plugs, unplugs, SMART
// bits; with the machine off, it empties the backing an erase left for emptying.
func (m *machine) syncDrives() {
	m.driveMu.Lock()
	defer m.driveMu.Unlock()
	if len(m.drives) == 0 {
		return
	}
	pm := m.running()
	smart := m.inst.NVMeProps()["smart_critical_warning"]
	for _, dr := range m.drives {
		st := m.a.rt.Engine.DriveStatus(dr.comp)
		m.settleContents(dr, st)
		if pm == nil {
			if dr.zero {
				m.zeroDrive(dr)
			}
			continue
		}
		want := wantDrive(st)
		switch {
		case dr.removing:
			// waiting for the guest to let go (driveDeleted)
		case dr.attached && (!want || dr.zero):
			// failed, pulled or missing, or swapped for a new (empty) one: off the bus first
			if err := pm.UnplugDrive(dr.bay); err != nil {
				m.observe("drive."+dr.comp.Name, "unplug-failed", fmt.Sprintf("drive %s: device_del: %v", dr.comp.Name, err), 0, nil)
				continue
			}
			dr.removing = true
			m.observe("drive."+dr.comp.Name, "removing", fmt.Sprintf("drive %s (bay %d) leaves the guest's PCIe bus (%s); the guest is asked to release it", dr.comp.Name, dr.bay, st.State), 0, nil)
		case !dr.attached && want:
			// the guest's driver needs a moment after a removal before the bay fills again
			if wait := settle - time.Since(dr.removedAt); wait > 0 {
				time.AfterFunc(wait, m.syncDrives)
				continue
			}
			// no controller exposes the namespace now: what a replacement holds (nothing) is set
			// before one does
			if dr.zero {
				m.zeroDrive(dr)
			}
			if err := pm.PlugDrive(dr.bay, dr.qemu(false, st), !dr.hasNS); err != nil {
				m.observe("drive."+dr.comp.Name, "plug-failed", fmt.Sprintf("drive %s: %v", dr.comp.Name, err), 0, nil)
				continue
			}
			dr.attached, dr.warn, dr.hasNS = true, 0, true
			m.observe("drive."+dr.comp.Name, "inserted", fmt.Sprintf("drive %s (bay %d) joins the guest's PCIe bus as %s", dr.comp.Name, dr.bay, qemu.DriveID(dr.bay)), 0, nil)
		}
		if dr.attached && !dr.removing && smart && dr.warn != st.CriticalWarning {
			if err := pm.SetSMARTWarning(dr.bay, st.CriticalWarning); err == nil {
				m.observe("drive."+dr.comp.Name, "smart", fmt.Sprintf("drive %s: SMART critical warning 0x%02x", dr.comp.Name, st.CriticalWarning), 0,
					map[string]any{"criticalWarning": st.CriticalWarning})
				dr.warn = st.CriticalWarning
			}
		}
	}
}

// driveDeleted is the guest releasing a drive (QMP DEVICE_DELETED for nvme<bay>): its block node
// goes, and a replacement waiting for the bay goes in.
func (m *machine) driveDeleted(pm *qemu.Machine, device string) {
	if !strings.HasPrefix(device, "nvme") || strings.Contains(device, "-") {
		return
	}
	m.driveMu.Lock()
	var hit *vmDrive
	for _, dr := range m.drives {
		if qemu.DriveID(dr.bay) == device {
			hit = dr
		}
	}
	if hit != nil {
		// the namespace stays on the bay's subsystem, for the controller plugged in next
		hit.attached, hit.removing, hit.removedAt = false, false, time.Now()
		m.observe("drive."+hit.comp.Name, "removed", fmt.Sprintf("drive %s (bay %d) is off the guest's PCIe bus (QMP DEVICE_DELETED)", hit.comp.Name, hit.bay), 0, nil)
	}
	m.driveMu.Unlock()
	if hit != nil {
		m.syncDrives()
	}
}

// drivesOff is the machine's process gone: nothing is attached until the next power-on.
func (m *machine) drivesOff() {
	m.driveMu.Lock()
	for _, dr := range m.drives {
		dr.attached, dr.removing, dr.hasNS = false, false, false
	}
	m.driveMu.Unlock()
	m.syncDrives()
}

// followDrives keeps every machine's drives in line with the model: a committed change on one of
// a host's drives (a fault, an act, its contents) resyncs that host.
func (a *Adapter) followDrives(ctx context.Context) {
	defer a.wg.Done()
	id, ch := a.rt.Engine.Subscribe(1024)
	defer func() { a.rt.Engine.Unsubscribe(id) }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopped:
			return
		case ev, ok := <-ch:
			if !ok {
				id, ch = a.rt.Engine.Subscribe(1024)
				continue
			}
			if m := a.driveOwner(ev); m != nil {
				m.syncDrives()
			}
		}
	}
}

func (a *Adapter) driveOwner(ev schema.Event) *machine {
	if ev.Kind != "fault" && ev.Kind != "state" && ev.Kind != "act" {
		return nil
	}
	for _, path := range a.order {
		m := a.machines[path]
		if len(m.drives) == 0 || !strings.HasPrefix(ev.Path, path+"/") {
			continue
		}
		for _, dr := range m.drives {
			if ev.Path == dr.comp.Path {
				return m
			}
		}
	}
	return nil
}

// driveReport is the drives' part of the fidelity report.
func (a *Adapter) driveReport() (lines, isNot []string) {
	n, hosts := 0, 0
	var props map[string]bool
	for _, path := range a.order {
		m := a.machines[path]
		if len(m.drives) == 0 {
			continue
		}
		hosts++
		n += len(m.drives)
		props = m.inst.NVMeProps()
	}
	if n == 0 {
		return nil, nil
	}
	lines = append(lines, fmt.Sprintf("drives: %d NVMe drive(s) on %d host(s), each a QEMU nvme controller and namespace on its own root port, on a sparse backing in the state directory", n, hosts))
	if !props["model"] {
		isNot = append(isNot, "This QEMU's nvme device takes no model or firmware strings (QEMU 11.1 does): every drive's model reads \"QEMU NVMe Ctrl\" in the guest.")
	}
	if !props["ns-mgmt"] {
		isNot = append(isNot, "This QEMU's NVMe controller advertises namespace management and implements neither create nor delete, and reports no total capacity: a stack that rebuilds namespaces when it sanitises a drive (NICo's scout) fails; scripts/qemu/build.sh builds one that does (S-15).")
	}
	if runtime.GOOS != "linux" {
		isNot = append(isNot, "On this host QEMU writes a guest's zeroes into the drive's backing rather than unmapping it: a guest that formats or sanitises its drives (NICo's scout) allocates each drive's whole capacity on the host's disk.")
	}
	a.cappedMu.Lock()
	isNot = append(isNot, a.capped...)
	a.cappedMu.Unlock()
	isNot = append(isNot, "A drive's PCI id is QEMU's (Red Hat 1b36:0010), not the drive vendor's; its SMART log carries the model's critical-warning bits while spare, wear and media-error counts stay zero.",
		"A replacement drive keeps its bay's serial and namespace identifiers: the bay's NVMe subsystem, whose serial a controller must match, outlives the drive in it.",
		"Every drive is NVMe in the guest, whatever protocol the profile gives it; at vm the guest's writes to a drive are the substrate's, and the model's record of its contents changes only by the twin's acts (an erase, a replacement).")
	return lines, isNot
}

// DriveObs is a vm host's drive as its machine has it now.
type DriveObs struct {
	Name     string
	Bay      int
	Attached bool // in the running guest's PCIe tree
	Warning  int  // the SMART critical-warning bits last set in its SMART log
	Backing  string
}

// Drives reports the drives of the vm host at path.
func (a *Adapter) Drives(path string) []DriveObs {
	m := a.machines[path]
	if m == nil {
		return nil
	}
	m.driveMu.Lock()
	defer m.driveMu.Unlock()
	var out []DriveObs
	for _, dr := range m.drives {
		out = append(out, DriveObs{Name: dr.comp.Name, Bay: dr.bay, Attached: dr.attached && !dr.removing, Warning: dr.warn, Backing: dr.path})
	}
	return out
}

// HMP runs a QEMU human-monitor command on the running machine at path (diagnostics: "info qtree").
func (a *Adapter) HMP(path, command string) (string, error) {
	m := a.machines[path]
	if m == nil {
		return "", fmt.Errorf("no machine %s", path)
	}
	pm := m.running()
	if pm == nil {
		return "", fmt.Errorf("%s is not running", path)
	}
	raw, err := pm.QMP.Execute("human-monitor-command", map[string]any{"command-line": command})
	if err != nil {
		return "", err
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s, nil
}
