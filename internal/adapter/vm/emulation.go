package vm

import (
	"fmt"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// A DPU's NVMe emulation at vm (docs/reviews/2026-11-08-storage-deep-dive.md 5.3, P5): a vm host
// whose DPU emulates NVMe gets spare PCIe root ports at power-on, and each function its DPU
// presents is hot-plugged into one as a QEMU NVMe controller whose namespace is an NBD export
// the storage adapter's stand-in serves (internal/adapter/storage/snap.go), as SNAP hot-plugs
// through the DPU's emulated PCIe switch. The controller's PCI id is QEMU's, not NVIDIA's.

// EmulationPorts is how many functions a vm host's DPUs can present at once.
const EmulationPorts = 4

func emulationPort(slot int) string { return fmt.Sprintf("rpemu%d", slot) }
func emulationID(slot int) string   { return fmt.Sprintf("emu%d", slot) }

// emulatingDPU reports whether a device has a DPU that emulates NVMe.
func emulatingDPU(d *model.Device) bool {
	for _, ch := range d.Children {
		if ch.BaseClass != "dpu" {
			continue
		}
		if b, ok := ch.Profile.Spec.Behaviours["nvmeEmulation"]; ok && b["functions"] != nil {
			return true
		}
	}
	return false
}

// emulationArgs are the spare root ports (chassis 96 onwards; a host's drives take 64 onwards).
func emulationArgs() []string {
	var a []string
	for i := 0; i < EmulationPorts; i++ {
		a = append(a, "-device", fmt.Sprintf("pcie-root-port,id=%s,chassis=%d,slot=0", emulationPort(i), 96+i))
	}
	return a
}

// AttachEmulatedNVMe hot-plugs an emulated function into a running vm host: an NVMe controller
// with the function's serial on spare port slot, its namespace the NBD export at socket, in
// blocks of blockSize bytes.
func (a *Adapter) AttachEmulatedNVMe(path string, slot int, socket, serial string, blockSize int) error {
	m := a.machines[path]
	if m == nil {
		return fmt.Errorf("%s is not a vm host of this adapter", path)
	}
	if slot < 0 || slot >= EmulationPorts {
		return fmt.Errorf("%s: no spare port %d (it has %d)", path, slot, EmulationPorts)
	}
	pm := m.running()
	if pm == nil {
		return fmt.Errorf("%s is off", path)
	}
	id := emulationID(slot)
	if _, err := pm.QMP.Execute("blockdev-add", map[string]any{"driver": "nbd", "node-name": id + "-disk",
		"server": map[string]any{"type": "unix", "path": socket}, "export": "volume"}); err != nil {
		return fmt.Errorf("blockdev-add %s: %w", id, err)
	}
	if _, err := pm.QMP.Execute("device_add", map[string]any{"driver": "nvme", "id": id, "bus": emulationPort(slot), "serial": serial,
		"drive": id + "-disk", "logical_block_size": blockSize, "physical_block_size": blockSize}); err != nil {
		_, _ = pm.QMP.Execute("blockdev-del", map[string]any{"node-name": id + "-disk"})
		return fmt.Errorf("device_add %s: %w", id, err)
	}
	m.driveMu.Lock()
	if m.emu == nil {
		m.emu = map[int]string{}
	}
	m.emu[slot] = serial
	m.driveMu.Unlock()
	return nil
}

// DetachEmulatedNVMe takes a function's controller off a running host's bus (the guest's PCIe
// hot-plug releases it) and drops its NBD node.
func (a *Adapter) DetachEmulatedNVMe(path string, slot int) error {
	m := a.machines[path]
	if m == nil {
		return fmt.Errorf("%s is not a vm host of this adapter", path)
	}
	m.driveMu.Lock()
	delete(m.emu, slot)
	m.driveMu.Unlock()
	pm := m.running()
	if pm == nil {
		return nil // gone with the machine
	}
	id := emulationID(slot)
	if _, err := pm.QMP.Execute("device_del", map[string]any{"id": id}); err != nil && !strings.Contains(err.Error(), "not found") {
		return err
	}
	var err error
	for i := 0; i < 50; i++ { // the node is in use until the guest has let the controller go
		if _, err = pm.QMP.Execute("blockdev-del", map[string]any{"node-name": id + "-disk"}); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// EmulatedNVMe is what is plugged into a running host's spare ports, by slot (serials); empty
// for a host that is off or has restarted since (a power-on starts with none).
func (a *Adapter) EmulatedNVMe(path string) map[int]string {
	m := a.machines[path]
	out := map[int]string{}
	if m == nil || m.running() == nil {
		return out
	}
	m.driveMu.Lock()
	for k, v := range m.emu {
		out[k] = v
	}
	m.driveMu.Unlock()
	return out
}

// Running reports whether the vm host at path is running.
func (a *Adapter) Running(path string) bool {
	m := a.machines[path]
	return m != nil && m.running() != nil
}
