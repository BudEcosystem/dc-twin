package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
)

// The firmware's boot options are the substrate's (D19). A `vm` host's UEFI firmware enumerates
// its load options itself (a PXE and an HTTP boot option per NIC and IP version, the shell) and
// keeps them, with BootOrder, in the machine's variable store. The observer reads the store
// whenever the firmware writes it, and commits what it holds as an `observe` event, field
// `nvram.bootOptions`, whose data the Redfish adapter serves as the host's BootOptions: a
// lifecycle manager that picks "UEFI HTTPv4 (MAC:…)" for the host's boot NIC finds the option
// the firmware really has. A boot order set through Redfish (the model's boot.order) is written
// into the store before the machine next starts, when it is an order of the options the store
// holds, so the firmware boots what the BMC says it will; a restart with such an order pending
// is carried out as a cold start, since QEMU holds the store while it runs.

// nvramState is the variable store as the observer last read it, and the last one it committed.
type nvramState struct {
	mtime time.Time
	last  *qemu.NVRAM
}

func (m *machine) varsPath() string { return filepath.Join(m.dir, "efivars.fd") }

// watchNVRAM follows the variable store while the adapter runs.
func (m *machine) watchNVRAM() {
	defer m.a.wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.a.stopped:
			return
		case <-t.C:
		}
		st, err := os.Stat(m.varsPath())
		if err != nil {
			continue
		}
		m.mu.Lock()
		changed := !st.ModTime().Equal(m.nv.mtime)
		m.mu.Unlock()
		if changed {
			m.observeNVRAM(st.ModTime())
		}
	}
}

// observeNVRAM reads the store and commits its boot options when they differ from the last
// committed ones.
func (m *machine) observeNVRAM(mtime time.Time) {
	nv, err := qemu.ReadNVRAM(m.varsPath())
	if err != nil {
		return
	}
	m.mu.Lock()
	m.nv.mtime = mtime
	same := m.nv.last != nil && reflect.DeepEqual(m.nv.last.BootOrder, nv.BootOrder) && reflect.DeepEqual(m.nv.last.Entries, nv.Entries) && m.nv.last.BootNext == nv.BootNext
	if !same {
		m.nv.last = nv
	}
	m.nvram = nv
	cause := m.cause
	m.mu.Unlock()
	if same || len(nv.Entries) == 0 {
		return
	}
	order := make([]any, len(nv.BootOrder))
	for i, o := range nv.BootOrder {
		order[i] = o
	}
	entries := make([]any, len(nv.Entries))
	var names []string
	for i, e := range nv.Entries {
		entries[i] = map[string]any{"id": e.ID, "description": e.Description, "active": e.Active, "devicePath": e.DevicePath, "alias": qemu.BootAlias(e.DevicePath)}
		names = append(names, e.ID+" "+e.Description)
	}
	m.observe("nvram.bootOptions", strings.Join(nv.BootOrder, ","), "UEFI NVRAM: the firmware's boot options, in BootOrder: "+strings.Join(names, ", "), cause,
		map[string]any{"bootOrder": order, "entries": entries, "bootNext": nv.BootNext})
}

// desiredBootOrder is the model's boot order when it is an order of the load options the store
// holds and differs from the store's (a boot order set through Redfish, not yet in NVRAM).
func (m *machine) desiredBootOrder() []string {
	var want []string
	m.a.rt.Engine.Read(func(mm *model.Model) {
		want = append(want, mm.DeviceBy[m.path].State.Boot.Order...)
	})
	m.mu.Lock()
	nv := m.nv.last
	m.mu.Unlock()
	if nv == nil || len(want) != len(nv.BootOrder) || reflect.DeepEqual(want, nv.BootOrder) {
		return nil
	}
	have := map[string]bool{}
	for _, id := range nv.BootOrder {
		have[id] = true
	}
	for _, id := range want {
		if !have[id] {
			return nil
		}
		delete(have, id)
	}
	return want
}

// applyBootOrder writes a pending boot order into the store before the machine starts.
func (m *machine) applyBootOrder() {
	want := m.desiredBootOrder()
	if want == nil {
		return
	}
	m.mu.Lock()
	cause := m.cause
	m.mu.Unlock()
	if err := qemu.WriteBootOrder(m.varsPath(), want); err != nil {
		m.observe("nvram.bootOrder", "not written", fmt.Sprintf("the boot order set through Redfish was not written to NVRAM: %v", err), cause, nil)
		return
	}
	m.observe("nvram.bootOrder", strings.Join(want, ","), "UEFI NVRAM: the boot order set through Redfish written before the machine starts: "+strings.Join(want, ","), cause,
		map[string]any{"bootOrder": want})
	if st, err := os.Stat(m.varsPath()); err == nil {
		m.observeNVRAM(st.ModTime())
	}
}
