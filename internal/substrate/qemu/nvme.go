package qemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// A host's drives (docs/reviews/2026-11-08-storage-deep-dive.md 5.1): each an NVMe controller
// with one namespace on a PCIe root port of its own, so it can leave and rejoin the bus while the
// machine runs (a failed or pulled drive, a replacement). The controller carries the drive's
// serial, model and firmware strings (QEMU 11.1 and later), the namespace its EUI-64; the backing
// is a sparse raw file holding what the guest wrote. Each bay has an NVMe subsystem of its own,
// which keeps the namespace while the controller is off the bus: a controller plugged back in
// finds it at start, as at power-on, rather than a namespace being added to a running controller.

// Drive is one of a machine's NVMe drives.
type Drive struct {
	Path     string // the sparse raw backing file
	Serial   string
	Model    string
	Firmware string
	// EUI64 is the namespace's IEEE extended unique identifier, 16 hex digits.
	EUI64 string
	// Absent leaves the drive's bay empty at power-on (its root port is still there to plug into).
	Absent bool
}

// NVMeProps are the properties this QEMU's nvme device takes (`-device nvme,help`): `model` and
// `firmware-version` from 11.1, `ns-mgmt` from the twin's namespace-management patch
// (scripts/qemu), `smart_critical_warning` from 6.0.
func (in *Installation) NVMeProps() map[string]bool {
	nvmePropsMu.Lock()
	defer nvmePropsMu.Unlock()
	if p, ok := nvmeProps[in.Binary]; ok {
		return p
	}
	p := map[string]bool{}
	out, _ := exec.Command(in.Binary, "-machine", "none", "-device", "nvme,help").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if k, _, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.Contains(k, " ") {
			p[k] = true
		}
	}
	nvmeProps[in.Binary] = p
	return p
}

var (
	nvmePropsMu sync.Mutex
	nvmeProps   = map[string]map[string]bool{}
)

// DriveID is the QEMU id of the drive in bay i: its controller, nvme<i>; its namespace and block
// node derive from it.
func DriveID(i int) string { return fmt.Sprintf("nvme%d", i) }

func rootPort(i int) string { return fmt.Sprintf("rpnvme%d", i) }

func subsysID(i int) string { return fmt.Sprintf("nvmesubsys%d", i) }

// blockdev is the drive's block node as -blockdev and blockdev-add take it. Both nodes unmap:
// QEMU drops a write-zeroes' may-unmap at a node opened with discard=ignore (-blockdev's default),
// and the zeroes are then allocated, so a guest's NVMe Format or Write Zeroes (NICo scout's
// sanitise formats every drive) would fill the host's disk with the drive's whole capacity.
func (d Drive) blockdev(id string) map[string]any {
	return map[string]any{"driver": "raw", "node-name": id + "-disk", "discard": "unmap",
		"file": map[string]any{"driver": "file", "filename": d.Path, "discard": "unmap"}}
}

func eui64(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(s), "0x"), 16, 64)
	return n
}

// controller is the drive's nvme device's properties, the ones this QEMU takes.
func (d Drive) controller(id string, props map[string]bool) map[string]any {
	c := map[string]any{"driver": "nvme", "id": id, "bus": rootPort(idx(id)), "serial": d.Serial, "subsys": subsysID(idx(id))}
	if props["model"] && d.Model != "" {
		c["model"] = d.Model
	}
	if props["firmware-version"] && d.Firmware != "" {
		c["firmware-version"] = d.Firmware
	}
	return c
}

func (d Drive) namespace(id string) map[string]any {
	ns := map[string]any{"driver": "nvme-ns", "id": id + "-ns1", "drive": id + "-disk", "nsid": 1, "bus": id}
	if e := eui64(d.EUI64); e != 0 {
		ns["eui64"] = e
	}
	return ns
}

func idx(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "nvme"))
	return n
}

// deviceArg renders a device's properties as -device takes them.
func deviceArg(p map[string]any) string {
	parts := []string{fmt.Sprint(p["driver"])}
	for _, k := range []string{"id", "bus", "subsys", "drive", "nsid", "serial", "model", "firmware-version", "eui64"} {
		if v, ok := p[k]; ok {
			if k == "eui64" {
				parts = append(parts, fmt.Sprintf("eui64=0x%016x", v))
				continue
			}
			parts = append(parts, k+"="+esc(fmt.Sprint(v)))
		}
	}
	return strings.Join(parts, ",")
}

// driveArgs are the command-line arguments for the machine's drives: a root port for every bay,
// and a controller and namespace in each bay that is not empty.
func (c Config) driveArgs() []string { return DriveArgs(c.Drives, c.NVMeProps) }

// DriveArgs are the command-line arguments for drives (a harness that boots its own guest with the
// twin's drives uses them: compat/storage-nvme).
func DriveArgs(drives []Drive, props map[string]bool) []string {
	c := Config{NVMeProps: props}
	var a []string
	for i, d := range drives {
		id := DriveID(i)
		a = append(a, "-device", fmt.Sprintf("pcie-root-port,id=%s,chassis=%d,slot=0", rootPort(i), 64+i),
			"-device", fmt.Sprintf("nvme-subsys,id=%s,nqn=%s", subsysID(i), esc(d.NQN())))
		if d.Absent {
			continue
		}
		bd, _ := json.Marshal(d.blockdev(id))
		a = append(a, "-blockdev", string(bd), "-device", deviceArg(d.controller(id, c.NVMeProps)), "-device", deviceArg(d.namespace(id)))
	}
	return a
}

// CreateBacking makes a drive's sparse backing file of size bytes if it does not exist; an
// existing one keeps what the guest wrote.
//
// It returns the backing's size: the drive's, unless the host's file system holds no file that
// large (ext4 with 4 KiB blocks: 16 TiB), when it is the largest size the file system takes, a
// power-of-two fraction of the drive's in 4 KiB units; the guest then sees that size.
func CreateBacking(path string, size int64) (int64, error) {
	if st, err := os.Stat(path); err == nil {
		return st.Size(), nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err = f.Truncate(size); err == nil {
		return size, nil
	}
	// too large for the file system: the largest a 4 KiB-block ext4 takes, then halving
	for try := min(size, int64(16<<40)-4096); (errors.Is(err, syscall.EFBIG) || errors.Is(err, syscall.EINVAL)) && try >= 1<<30; try = (try / 2) &^ 4095 {
		if err = f.Truncate(try); err == nil {
			return try, nil
		}
	}
	os.Remove(path)
	return 0, err
}

// ZeroBacking empties a drive's backing (an erase, a new drive in the bay): every block reads
// zero and the file holds nothing. Only while no machine has it open.
func ZeroBacking(path string, size int64) error {
	if err := os.Truncate(path, 0); err != nil {
		if os.IsNotExist(err) {
			_, err := CreateBacking(path, size)
			return err
		}
		return err
	}
	return os.Truncate(path, size)
}

// NQN is the bay's subsystem NQN.
func (d Drive) NQN() string { return "nqn.2026-11.io.dctwin:" + d.Serial }

// PlugDrive puts a drive into bay i of a running machine: its controller (QMP device_add) on the
// bay's subsystem, which holds its namespace still (withNamespace false), or a namespace and its
// block node too for a bay that has never held one.
func (m *Machine) PlugDrive(i int, d Drive, withNamespace bool) error {
	id := DriveID(i)
	if withNamespace {
		if _, err := m.QMP.Execute("blockdev-add", d.blockdev(id)); err != nil {
			return fmt.Errorf("blockdev-add %s: %w", id, err)
		}
	}
	if _, err := m.QMP.Execute("device_add", d.controller(id, m.Cfg.NVMeProps)); err != nil {
		return fmt.Errorf("device_add %s: %w", id, err)
	}
	if withNamespace {
		if _, err := m.QMP.Execute("device_add", d.namespace(id)); err != nil {
			return fmt.Errorf("device_add %s-ns1: %w", id, err)
		}
	}
	return nil
}

// UnplugDrive asks the guest to release bay i (PCIe hot removal): the controller goes when the
// guest acknowledges it (QMP DEVICE_DELETED), and ReleaseDrive then drops its block node.
func (m *Machine) UnplugDrive(i int) error {
	_, err := m.QMP.Execute("device_del", map[string]any{"id": DriveID(i)})
	return err
}

// ReleaseDrive drops a drive whose controller has left the bus: its namespace (which QEMU keeps on
// the controller's subsystem, holding the block node) and then its block node.
func (m *Machine) ReleaseDrive(i int) error {
	_, _ = m.QMP.Execute("device_del", map[string]any{"id": DriveID(i) + "-ns1"})
	_, err := m.QMP.Execute("blockdev-del", map[string]any{"node-name": DriveID(i) + "-disk"})
	return err
}

// SetSMARTWarning sets the critical-warning bits of the drive in bay i's SMART log, as a running
// drive raises them (QOM smart_critical_warning).
func (m *Machine) SetSMARTWarning(i, bits int) error {
	_, err := m.QMP.Execute("qom-set", map[string]any{"path": "/machine/peripheral/" + DriveID(i), "property": "smart_critical_warning", "value": bits})
	return err
}
