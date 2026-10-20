// Package vm is the `vm` fidelity adapter (docs/spec/07-fidelity.md): each device at `vm`
// fidelity is a real virtual machine behind its emulated BMC. The BMC's protocols (Redfish,
// IPMI) still come from the model; what the machine is — powered or not, booted or not — is
// observed from the substrate (D19). The adapter hands the engine's power requests to QEMU and
// reports what QEMU says back as committed `state` events with source class `observer`.
//
// The machine is derived from the model: CPUs and memory from the profile's fidelity.vm.cost
// (or the estate's spec.adapters.vm, labelled), UEFI firmware with a per-device NVRAM file
// kept in the state directory, SMBIOS types 0–3 from the identity the device's Redfish tree
// serves (manufacturer, model, serial, UUID, part number, the uefi firmware version), virtio
// NICs with the MACs of the host's ports, a swtpm when the profile has a TPM and swtpm is
// installed, and the serial console logged to a file the control API serves.
package vm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Name is the adapter's name.
const Name = "vm"

const observer = "qemu"

func init() { twin.Register(Name, func() adapter.Adapter { return New() }) }

// Adapter runs the estate's `vm` devices as QEMU machines.
type Adapter struct {
	rt       *adapter.Runtime
	log      *slog.Logger
	opts     schema.VMOptions
	machines map[string]*machine
	order    []string
	uplink   *netsim.Uplink // the router's interface on the stack's routed site network, if any (D48)
	workers  sync.WaitGroup // one per machine: carries out requests in order
	wg       sync.WaitGroup // observers: QMP and process watchers, console followers
	stopped  chan struct{}
	stopOnce sync.Once
}

// New returns an adapter that has not started.
func New() *Adapter { return &Adapter{machines: map[string]*machine{}, stopped: make(chan struct{})} }

func (a *Adapter) Name() string { return Name }

// plan is what a device asks of the substrate, before anything runs.
type plan struct {
	arch, firmware string
	tpm            bool
	ramGB, vcpu    float64
}

func planOf(d *model.Device) (plan, error) {
	p := plan{arch: qemu.HostArch(), firmware: "uefi"}
	spec, ok := d.Profile.Spec.Fidelity["vm"]
	if !ok {
		return p, fmt.Errorf("profile %s has no fidelity.vm", d.ProfileName)
	}
	if s, _ := spec["arch"].(string); s != "" {
		p.arch = s
	}
	if s, _ := spec["firmware"].(string); s != "" {
		p.firmware = s
	}
	p.tpm, _ = spec["tpm"].(bool)
	if c, ok := spec["cost"].(map[string]any); ok {
		p.ramGB, _ = num(c["ramGB"])
		p.vcpu, _ = num(c["vcpu"])
	}
	if p.firmware != "uefi" {
		return p, fmt.Errorf("fidelity.vm.firmware %q: the substrate boots UEFI only", p.firmware)
	}
	return p, nil
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// Accepts reports whether a device can run as a QEMU machine on this host (adapter.Substrate).
func (a *Adapter) Accepts(est *schema.Estate, d *model.Device, allowTCG bool) error {
	p, err := planOf(d)
	if err != nil {
		return err
	}
	if _, err := qemu.Find(p.arch, est.Spec.Adapters.VM.QEMU, allowTCG); err != nil {
		return err
	}
	if d.OS == "twin" {
		g, ok := Guests[p.arch]
		if !ok {
			return fmt.Errorf("os: twin needs the twin's guest image, and none is pinned for %s", p.arch)
		}
		if err := g.Present(GuestDir(twin.AssetsDir(), est.Spec.Adapters.VM.Guest, g)); err != nil {
			return err
		}
	}
	return nil
}

// machine is one device's virtual machine and its observer.
type machine struct {
	a      *Adapter
	path   string
	slug   string
	os     string
	plan   plan
	inst   *qemu.Installation
	guest  *GuestImage
	initrd string
	dir    string // persistent state: NVRAM, console log, TPM state
	run    string // sockets
	reqs   chan request

	mu        sync.Mutex
	cur       *qemu.Machine // nil when off
	cause     uint64        // the request whose effect the next observation reports
	restart   bool          // power on again when the process exits (graceful restart)
	reset     bool          // the next power-on follows a restart or cycle
	exited    chan struct{} // closed once the current process's exit has been observed
	bootFloor int64         // console offset below which lines say nothing about this boot
	tpm       *exec.Cmd
	lastArgs  string
	starts    int
	nvram     *qemu.NVRAM
	cpus, mem int
	nics      []*nic // the host's ports, cabled to the twin's switch ports (network.go, D48)
	tpmCerts  string // what manufacturing the TPM gave it (tpm.go), or why it has no certificates
	nv        nvramState
}

type request struct {
	canonical string
	cause     uint64
}

// Start plans a machine for every `vm` device up claimed for this adapter (every one another
// substrate does not run), attaches each as its device's power substrate and powers on those the
// estate starts on.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt, a.log = rt, rt.Log
	est := rt.Engine.M.Estate
	if est != nil {
		a.opts = est.Spec.Adapters.VM
	}
	var devs []*model.Device
	initialOn := map[string]bool{}
	rt.Engine.Read(func(m *model.Model) {
		nos := twin.NOSSwitches(m) // a real NOS's switches are `vm` by their NOS (D21), not machines here
		for _, d := range m.Devices {
			// a device another substrate claims (a BlueField's Arm, the bluefield adapter's) is
			// that adapter's to plan, not this one's
			if _, ok := nos[d]; !ok && d.Fidelity == "vm" && rt.Runs(a.Name(), d.Path) {
				devs = append(devs, d)
				initialOn[d.Path] = d.State.Power != model.Off && d.State.AC
			}
		}
	})
	svcs, err := a.services()
	if err != nil {
		return fmt.Errorf("the stack's %s output: %w", siteServicesOutput, err)
	}
	if len(devs) > 0 {
		if a.uplink, err = a.siteNetwork(); err != nil {
			return fmt.Errorf("the stack's %s output: %w", siteNetworkOutput, err)
		}
	}
	for _, d := range devs {
		m, err := a.plan(d)
		if err == nil {
			err = m.cableNICs(d, svcs)
		}
		if err != nil {
			a.closeAll()
			return fmt.Errorf("%s: %w", d.Path, err)
		}
		a.machines[d.Path] = m
		a.order = append(a.order, d.Path)
	}
	for _, path := range a.order {
		m := a.machines[path]
		if err := rt.Engine.AttachSubstrate(path, observer, a); err != nil {
			return err
		}
		a.workers.Add(1)
		a.wg.Add(2)
		go m.work()
		go m.watchConsole()
		go m.watchNVRAM()
	}
	for _, path := range a.order {
		if initialOn[path] {
			_ = rt.Engine.Power(engine.Request{Path: path, Canonical: "power.on", Protocol: "twin", Target: "initialState",
				Source: schema.EventSource{Class: "twin", Principal: "vm"}})
		}
	}
	return nil
}

func (a *Adapter) plan(d *model.Device) (*machine, error) {
	p, err := planOf(d)
	if err != nil {
		return nil, err
	}
	inst, err := qemu.Find(p.arch, a.opts.QEMU, a.rt.AllowTCG)
	if err != nil {
		return nil, err
	}
	slug := strings.ReplaceAll(d.Path, "/", "_")
	m := &machine{a: a, path: d.Path, slug: slug, os: d.OS, plan: p, inst: inst, reqs: make(chan request, 64),
		dir: filepath.Join(a.rt.StateDir, "vm", slug)}
	m.run = runDir(m.dir, a.rt.StateDir+"|"+d.Path)
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.run, 0o700); err != nil {
		return nil, err
	}
	if d.OS == "twin" {
		g, ok := Guests[p.arch]
		if !ok {
			return nil, fmt.Errorf("no twin guest image is pinned for %s", p.arch)
		}
		gdir := GuestDir(a.rt.Assets, a.opts.Guest, g)
		if err := g.Present(gdir); err != nil {
			return nil, err
		}
		initrd, err := BuildInitrd(g, gdir)
		if err != nil {
			return nil, err
		}
		m.guest, m.initrd = &g, initrd
	}
	m.cpus, m.mem = int(p.vcpu), int(p.ramGB*1024)
	if a.opts.CPUs > 0 {
		m.cpus = a.opts.CPUs
	}
	if a.opts.MemoryMB > 0 {
		m.mem = a.opts.MemoryMB
	}
	if m.cpus <= 0 {
		m.cpus = 1
	}
	if m.mem <= 0 {
		m.mem = 1024
	}
	return m, nil
}

// runDir is a directory for a machine's sockets short enough for a unix socket path.
func runDir(preferred, key string) string {
	if len(filepath.Join(preferred, "console.sock")) <= 100 {
		return preferred
	}
	sum := sha256.Sum256([]byte(key))
	name := "dctwin-vm-" + hex.EncodeToString(sum[:5])
	for _, base := range []string{os.TempDir(), "/tmp"} {
		if p := filepath.Join(base, name); len(filepath.Join(p, "console.sock")) <= 100 {
			return p
		}
	}
	return filepath.Join("/tmp", name)
}

// Power is the engine's hand-off (engine.Substrate): it queues the request for the machine's
// worker, which carries requests out in order.
func (a *Adapter) Power(path, canonical string, cause uint64) error {
	m := a.machines[path]
	if m == nil {
		return engine.ErrNoDevice
	}
	select {
	case m.reqs <- request{canonical, cause}:
		return nil
	default:
		return errors.New("the machine's request queue is full")
	}
}

func (m *machine) observe(field, to, msg string, cause uint64, data map[string]any) {
	_ = m.a.rt.Engine.Observe(engine.Observation{Path: m.path, Observer: observer, Field: field, To: to, Message: msg, Cause: cause, Data: data})
}

func (m *machine) work() {
	defer m.a.workers.Done()
	for {
		select {
		case <-m.a.stopped:
			return
		case r := <-m.reqs:
			if err := m.handle(r); err != nil {
				m.a.rt.Engine.Commit(schema.Event{Kind: "twin", Severity: "incident", Source: schema.EventSource{Class: "twin", Principal: "vm"},
					Path: m.path, Cause: r.cause, Message: fmt.Sprintf("%s: %s: %v", m.path, r.canonical, err)})
			}
		}
	}
}

func (m *machine) running() *qemu.Machine {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

func (m *machine) handle(r request) error {
	cur := m.running()
	m.mu.Lock()
	m.cause = r.cause
	if r.canonical != "power.on" {
		m.restart = false
	}
	m.mu.Unlock()
	switch r.canonical {
	case "power.on":
		if cur != nil {
			return nil
		}
		return m.start(false)
	case "power.off.force":
		if cur != nil {
			cur.Quit(5 * time.Second)
			m.waitExitObserved()
		}
	case "power.off.graceful":
		if cur != nil {
			// the ACPI power button: the guest decides, and a guest that ignores it stays on
			if err := cur.Powerdown(); err != nil && !m.gone(cur) {
				return err
			}
		}
	case "power.restart":
		if cur != nil && m.desiredBootOrder() != nil {
			// a boot order set through Redfish is pending: the store can be written only while
			// QEMU does not hold it, so the restart is a cold one (firmware.go)
			cur.Quit(5 * time.Second)
			m.waitExitObserved()
			return m.start(true)
		}
		if cur != nil {
			if err := cur.Reset(); err == nil || !m.gone(cur) {
				return err
			}
		}
		return m.start(true)
	case "power.restart.graceful":
		if cur != nil {
			m.mu.Lock()
			m.restart = true
			m.mu.Unlock()
			if err := cur.Powerdown(); err == nil || !m.gone(cur) {
				return err
			}
			m.mu.Lock()
			m.restart = false
			m.mu.Unlock()
		}
		return m.start(true)
	case "power.cycle":
		if cur != nil {
			cur.Quit(5 * time.Second)
			m.waitExitObserved()
		}
		return m.start(true)
	default:
		return fmt.Errorf("unsupported request %s", r.canonical)
	}
	return nil
}

// gone reports whether a machine's process has ended (the guest powered itself off while a
// request for it was on its way), after waiting for the observer to report it.
func (m *machine) gone(cur *qemu.Machine) bool {
	select {
	case <-cur.Exited():
		m.waitExitObserved()
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

func (m *machine) waitExitObserved() {
	m.mu.Lock()
	ch := m.exited
	m.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
		}
	}
}

// config derives the machine from the model as it is now: a firmware update applied since the
// last power-on shows in the next boot's SMBIOS.
func (m *machine) config() qemu.Config {
	var c qemu.Config
	m.a.rt.Engine.Read(func(mm *model.Model) {
		d := mm.DeviceBy[m.path]
		manuf, product, part := "", "", ""
		if id := d.Profile.Spec.Identity; id != nil {
			manuf, product, part = id.Manufacturer, id.Model, id.PartNumber
		}
		uefi := ""
		if fw := d.State.Firmware["uefi"]; fw != nil {
			uefi = fw.Version
		} else if fw := d.State.Firmware["bios"]; fw != nil {
			uefi = fw.Version
		}
		c = qemu.Config{Name: d.Path, Arch: m.plan.arch, Accel: m.inst.Accel, CPUs: m.cpus, MemoryMB: m.mem, Binary: m.inst.Binary,
			Code: m.inst.Code, VarsTemplate: m.inst.VarsTemplate, Vars: filepath.Join(m.dir, "efivars.fd"),
			UUID: d.Identity.UUID, RunDir: m.run, Console: filepath.Join(m.dir, "console.log")}
		c.SMBIOS = []qemu.SMBIOS{
			{Type: 0, Fields: [][2]string{{"vendor", manuf}, {"version", uefi}, {"uefi", "on"}}},
			{Type: 1, Fields: [][2]string{{"manufacturer", manuf}, {"product", product}, {"serial", d.Identity.Serial}, {"uuid", d.Identity.UUID}, {"sku", part}, {"family", d.Class}}},
			{Type: 2, Fields: [][2]string{{"manufacturer", manuf}, {"product", product}, {"serial", d.Identity.Serial}}},
			{Type: 3, Fields: [][2]string{{"manufacturer", manuf}, {"serial", d.Identity.Serial}, {"sku", part}}},
		}
	})
	// each NIC's cable is the twin's switch port for it (D48)
	for _, n := range m.nics {
		c.NICs = append(c.NICs, qemu.NIC{MAC: n.port.MAC, Netdev: n.netdev()})
	}
	if m.guest != nil {
		c.Kernel = filepath.Join(GuestDir(m.a.rt.Assets, m.a.opts.Guest, *m.guest), m.guest.Kernel)
		c.Initrd = m.initrd
		c.Append = "console=" + m.guest.Console + " " + GuestCmdline
	}
	return c
}

// hostPorts are the Ethernet ports the host's OS sees: its own, other than the BMC's, and
// those of sub-devices that are not machines of their own (a DPU or NIC at model fidelity
// presents its ports to the host as PCIe functions).
func hostPorts(d *model.Device) []*model.Port {
	var out []*model.Port
	bmc := compile.BMCPort(d)
	add := func(dev *model.Device, skip *model.Port) {
		for _, p := range dev.Ports {
			if p.Kind != "ethernet" || p.MAC == "" || p == skip || p.Role == "management" {
				continue
			}
			out = append(out, p)
		}
	}
	if d.HasBMC() {
		add(d, bmc)
	} else {
		add(d, nil)
	}
	for _, ch := range d.Children {
		if ch.Fidelity != "vm" {
			add(ch, compile.BMCPort(ch))
		}
	}
	return out
}

// start powers the machine on: swtpm first when it has one, then QEMU paused, then the
// observer attaches and the guest is let run. Power is reported On when QEMU says it resumed.
func (m *machine) start(reset bool) error {
	m.applyBootOrder()
	cfg := m.config()
	tpm, err := m.startTPM()
	if err != nil {
		return err
	}
	if tpm != "" {
		cfg.TPM = tpm
	}
	pm, err := qemu.Start(cfg)
	if err != nil {
		m.stopTPM()
		return err
	}
	m.mu.Lock()
	m.cur, m.reset, m.exited = pm, reset, make(chan struct{})
	m.starts++
	m.lastArgs = cfg.CommandLine()
	if st, err := os.Stat(cfg.Console); err == nil {
		m.bootFloor = st.Size()
	}
	cause := m.cause
	m.mu.Unlock()
	m.observe("qemu.process", "started", fmt.Sprintf("QEMU %s started (pid %d, -accel %s)", m.inst.Version, pm.PID(), cfg.Accel), cause,
		map[string]any{"commandLine": m.lastArgs, "pid": pm.PID(), "qemuVersion": pm.QMP.Version})
	m.a.wg.Add(1)
	go m.watch(pm)
	if err := pm.Cont(); err != nil {
		pm.Quit(5 * time.Second)
		return err
	}
	return nil
}

// watch is the observer of one QEMU process: QMP events while it runs, its exit when it ends.
func (m *machine) watch(pm *qemu.Machine) {
	defer m.a.wg.Done()
	resumed := false
	var shutdown map[string]any
	for ev := range pm.QMP.Events() {
		m.mu.Lock()
		cause := m.cause
		m.mu.Unlock()
		data := map[string]any{"qmp": ev.Name}
		for k, v := range ev.Data {
			data[k] = v
		}
		switch ev.Name {
		case "RESUME":
			if resumed {
				continue
			}
			resumed = true
			m.mu.Lock()
			reset := m.reset
			m.reset = false
			m.mu.Unlock()
			data["reset"] = reset
			m.observe("power", model.On, "QEMU: the machine is running (QMP RESUME)", cause, data)
		case "POWERDOWN":
			m.observe("acpi.powerButton", "pressed", "QEMU: the ACPI power button was pressed (QMP POWERDOWN); the guest decides", cause, data)
		case "RESET":
			if st, err := os.Stat(pm.Cfg.Console); err == nil {
				m.mu.Lock()
				m.bootFloor = st.Size()
				m.mu.Unlock()
			}
			why := fmt.Sprint(ev.Data["reason"])
			m.observe("reset", why, "QEMU: the machine was reset (QMP RESET, "+why+")", cause, data)
		case "SHUTDOWN":
			shutdown = ev.Data
			m.observe("qemu.shutdown", fmt.Sprint(ev.Data["reason"]), fmt.Sprintf("QEMU: shutdown (QMP SHUTDOWN, %v)", ev.Data["reason"]), cause, data)
		}
	}
	<-pm.Exited()
	m.mu.Lock()
	if m.cur == pm {
		m.cur = nil
	}
	restart := m.restart
	m.restart = false
	cause := m.cause
	exited := m.exited
	m.mu.Unlock()
	m.stopTPM()
	data := map[string]any{"exit": pm.ExitError()}
	reason := "the process exited"
	if shutdown != nil {
		reason = fmt.Sprint(shutdown["reason"])
		data["reason"], data["guest"] = shutdown["reason"], shutdown["guest"]
	}
	if nv, err := qemu.ReadNVRAM(pm.Cfg.Vars); err == nil {
		m.mu.Lock()
		m.nvram = nv
		m.mu.Unlock()
		data["nvramBootOrder"] = nv.BootOrder
	}
	m.observe("power", model.Off, "QEMU: the machine is off ("+reason+")", cause, data)
	if st, err := os.Stat(pm.Cfg.Vars); err == nil {
		m.observeNVRAM(st.ModTime())
	}
	if nv := m.nvramSnapshot(); nv != nil {
		var entries []string
		for _, e := range nv.Entries {
			entries = append(entries, e.ID+" "+e.Description)
		}
		m.observe("nvram.bootOrder", strings.Join(nv.BootOrder, ","), "UEFI NVRAM read on power-off: BootOrder "+strings.Join(entries, ", "), cause,
			map[string]any{"bootOrder": nv.BootOrder, "bootNext": nv.BootNext, "variables": nv.Variables})
	}
	if exited != nil {
		close(exited)
	}
	if restart {
		select {
		case m.reqs <- request{"power.on", cause}:
		default:
		}
	}
}

func (m *machine) nvramSnapshot() *qemu.NVRAM {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nvram
}

// watchConsole is the boot observer: it follows the serial console and reports the boot
// stage the guest's own output shows — the kernel starting, then the OS up (the twin guest's
// ready line, or a login prompt).
func (m *machine) watchConsole() {
	defer m.a.wg.Done()
	path := filepath.Join(m.dir, "console.log")
	var off int64
	var partial string
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-m.a.stopped:
			return
		case <-tick.C:
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		st, _ := f.Stat()
		if st != nil && st.Size() < off {
			off = 0
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			f.Close()
			continue
		}
		rd := bufio.NewReader(f)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				partial += line
				off += int64(len(line))
				break
			}
			start := off - int64(len(partial))
			off += int64(len(line))
			line, partial = partial+line, ""
			m.consoleLine(start, strings.TrimRight(line, "\r\n"))
		}
		f.Close()
	}
}

func (m *machine) consoleLine(at int64, line string) {
	m.mu.Lock()
	floor, on, cause := m.bootFloor, m.cur != nil, m.cause
	m.mu.Unlock()
	if at < floor || !on {
		return
	}
	line = printable(line)
	switch {
	case strings.Contains(line, "Booting Linux on physical CPU") || strings.Contains(line, "] Linux version "):
		m.observe("boot", "kernel", "console: "+line, cause, nil)
	case strings.Contains(line, GuestReady) || strings.HasSuffix(line, "login:"):
		m.observe("boot", "os", "console: "+line, cause, nil)
	case strings.Contains(line, "efi: ") && strings.Contains(line, "TPMEventLog="):
		// the firmware measured this boot into the machine's TPM and handed the OS its log
		m.observe("tpm.eventLog", "present", "console: the firmware passed a TPM event log to the OS (measured boot)", cause, nil)
	}
}

// printable drops terminal control sequences (the firmware clears the screen) from a line.
func printable(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b { // ESC [ … final byte
			i++
			if i < len(s) && s[i] == '[' {
				for i+1 < len(s) && (s[i+1] < 0x40 || s[i+1] > 0x7e) {
					i++
				}
				i++
			}
			continue
		}
		if c >= 0x20 && c != 0x7f || c == '\t' {
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------- TPM

// startTPM starts swtpm for one power-on, its state persisted in the device's directory, when
// the profile has a TPM and swtpm is installed (and the estate has not turned it off).
func (m *machine) startTPM() (string, error) {
	if !m.plan.tpm || m.a.opts.TPM == "off" {
		return "", nil
	}
	bin, err := exec.LookPath("swtpm")
	if err != nil {
		return "", nil
	}
	state := filepath.Join(m.dir, "tpm")
	if err := os.MkdirAll(state, 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.run, 0o700); err != nil {
		return "", err
	}
	if what, err := m.manufactureTPM(state); err != nil {
		m.mu.Lock()
		m.tpmCerts = "no EK certificate (" + err.Error() + ")"
		m.mu.Unlock()
	} else if what != "" {
		m.mu.Lock()
		m.tpmCerts = what
		cause := m.cause
		m.mu.Unlock()
		m.observe("tpm.manufactured", "ek-certificates", "swtpm_setup manufactured the TPM: "+what, cause, nil)
	} else {
		m.mu.Lock()
		if m.tpmCerts == "" {
			m.tpmCerts = "manufactured on an earlier start (see tpm-manufacture.log)"
		}
		m.mu.Unlock()
	}
	sock := filepath.Join(m.run, "swtpm.sock")
	_ = os.Remove(sock)
	cmd := exec.Command(bin, "socket", "--tpm2", "--tpmstate", "dir="+state, "--ctrl", "type=unixio,path="+sock,
		"--log", "file="+filepath.Join(m.dir, "swtpm.log")+",level=1", "--terminate")
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("swtpm: %w", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	for i := 0; ; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		select {
		case <-done:
			b, _ := os.ReadFile(filepath.Join(m.dir, "swtpm.log"))
			return "", fmt.Errorf("swtpm exited: %s", strings.TrimSpace(string(b)))
		case <-time.After(20 * time.Millisecond):
		}
		if i > 250 {
			_ = cmd.Process.Kill()
			return "", fmt.Errorf("swtpm did not create %s", sock)
		}
	}
	m.mu.Lock()
	m.tpm = cmd
	m.mu.Unlock()
	return sock, nil
}

func (m *machine) stopTPM() {
	m.mu.Lock()
	cmd := m.tpm
	m.tpm = nil
	m.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (m *machine) tpmAttached() bool {
	if !m.plan.tpm || m.a.opts.TPM == "off" {
		return false
	}
	_, err := exec.LookPath("swtpm")
	return err == nil
}

// ---------------------------------------------------------------- lifecycle and reports

// Stop powers every machine off (as pulling the plug does) and stops the observers. Workers
// stop first, so that no machine starts after its plug was pulled.
func (a *Adapter) Stop() error {
	a.stopOnce.Do(func() {
		close(a.stopped)
		a.workers.Wait()
		var wg sync.WaitGroup
		for _, path := range a.order {
			m := a.machines[path]
			if cur := m.running(); cur != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					cur.Quit(5 * time.Second)
					m.waitExitObserved()
				}()
			}
		}
		wg.Wait()
		a.wg.Wait()
		a.closeAll()
	})
	return nil
}

// closeAll releases what the machines hold besides their processes: swtpm, the NICs' cables
// and switch ports, and the sockets' short directories.
func (a *Adapter) closeAll() {
	if a.uplink != nil {
		a.uplink.Close()
	}
	for _, m := range a.machines {
		m.stopTPM()
		m.closeNICs()
		if m.run != m.dir {
			_ = os.RemoveAll(m.run) // the sockets' short directory outside the state directory
		}
	}
}

// ConsoleLog serves a machine's serial console transcript (adapter.Console).
func (a *Adapter) ConsoleLog(path string, from int64) ([]byte, int64, error) {
	m := a.machines[path]
	if m == nil {
		return nil, 0, fmt.Errorf("%s is not a vm device", path)
	}
	f, err := os.Open(filepath.Join(m.dir, "console.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, from, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 8<<20))
	return b, from + int64(len(b)), err
}

// Status is the machine as QEMU itself reports it (QMP query-status: running, paused,
// shutdown …), or "off" when no process runs, with the process id: what a test or an inspector
// compares the model's observed power with.
func (a *Adapter) Status(path string) (string, int, error) {
	m := a.machines[path]
	if m == nil {
		return "", 0, fmt.Errorf("%s is not a vm device", path)
	}
	cur := m.running()
	if cur == nil {
		return "off", 0, nil
	}
	st, err := cur.QMP.Status()
	return st, cur.PID(), err
}

// Machine describes one machine for tests, the inspector and the report.
type Machine struct {
	Path, OS, Arch, Accel string
	CPUs, MemoryMB        int
	CommandLine           string
	Console, NVRAM        string
	QMPSocket             string
	TPM                   bool
	Running               bool
	PID                   int
	Starts                int
	BootOrder             []string
}

// Machines lists the adapter's machines.
func (a *Adapter) Machines() []Machine {
	var out []Machine
	for _, path := range a.order {
		m := a.machines[path]
		m.mu.Lock()
		mi := Machine{Path: path, OS: m.os, Arch: m.plan.arch, Accel: m.inst.Accel, CPUs: m.cpus, MemoryMB: m.mem, CommandLine: m.lastArgs,
			Console: filepath.Join(m.dir, "console.log"), NVRAM: filepath.Join(m.dir, "efivars.fd"), QMPSocket: filepath.Join(m.run, "qmp.sock"),
			TPM: m.tpmAttached(), Running: m.cur != nil, Starts: m.starts}
		if m.cur != nil {
			mi.PID = m.cur.PID()
		}
		if m.nvram != nil {
			mi.BootOrder = m.nvram.BootOrder
		}
		m.mu.Unlock()
		out = append(out, mi)
	}
	return out
}

// Report is the adapter's contribution to the fidelity report (07).
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: Name, Served: len(a.order)}
	if len(a.order) == 0 {
		return r
	}
	var tpm, twinOS, stackOS int
	for _, mi := range a.Machines() {
		m := a.machines[mi.Path]
		size := fmt.Sprintf("%d vCPU, %d MB", mi.CPUs, mi.MemoryMB)
		if a.opts.CPUs > 0 || a.opts.MemoryMB > 0 {
			size += fmt.Sprintf(" (scaled by spec.adapters.vm; the profile asks %g vCPU, %g GB)", m.plan.vcpu, m.plan.ramGB)
		}
		boot := "boots from its firmware's boot order"
		if m.guest != nil {
			boot = "boots the twin's guest " + m.guest.Name + " through the firmware's kernel loader"
		}
		r.Lines = append(r.Lines, fmt.Sprintf("%s: QEMU %s %s -accel %s, %s, UEFI %s, NVRAM %s; os: %s, %s",
			mi.Path, m.inst.Version, mi.Arch, mi.Accel, size, filepath.Base(m.inst.Code), mi.NVRAM, mi.OS, boot))
		if mi.Accel == "tcg" {
			r.TCG = append(r.TCG, fmt.Sprintf("%s (%s guest on a %s host)", mi.Path, mi.Arch, qemu.HostArch()))
		}
		if mi.TPM {
			tpm++
			m.mu.Lock()
			if m.tpmCerts != "" {
				r.Lines = append(r.Lines, fmt.Sprintf("tpm: %s: swtpm, %s", mi.Path, m.tpmCerts))
			}
			m.mu.Unlock()
		}
		if mi.OS == "twin" {
			twinOS++
		} else {
			stackOS++
		}
	}
	n := len(a.order)
	r.Lines = append(r.Lines, a.networkReport()...)
	r.Lines = append(r.Lines,
		"console: each machine's serial port, logged and served by the control API (GET /v1/console)",
		fmt.Sprintf("boot: %d host(s) boot UEFI (QEMU's edk2); %d os: twin, %d os: stack", n, twinOS, stackOS))
	r.SubstrateOwned = []string{
		fmt.Sprintf("power observed from QEMU (QMP events and process exit) for %d host(s)", n),
		fmt.Sprintf("boot stage observed from the serial console for %d host(s)", n),
		fmt.Sprintf("UEFI NVRAM (edk2 variable store, read on power-off) for %d host(s)", n),
	}
	if tpm > 0 {
		r.SubstrateOwned = append(r.SubstrateOwned, fmt.Sprintf("swtpm state for %d host(s)", tpm))
	}
	r.IsNot = []string{
		"vm NICs are QEMU stream netdevs to the twin's switch port for each (D48), not TAPs on a bridge: the guest's DHCP enters its segment's twin relay at its port, and the segment gateway is a userspace router that terminates the guest's TCP and opens its own connection to the service (a service sees this host's loopback address, not the guest's lease); only stack services named by the stack's siteServices output and estate endpoints at their site addresses are reachable; the guest's unicast DHCP renewals are relayed like its broadcasts; the segment has no other host on it; IPv6, multicast and ICMP other than echo to the gateway are dropped.",
		"Redfish and IPMI are still the model's BMC: power, boot stage and the firmware's boot options are observed from QEMU and its NVRAM, and a boot order set through Redfish is written to NVRAM before the next start (a restart with one pending is a cold start); boot overrides (BootSourceOverride), BIOS settings and Secure Boot set through Redfish are recorded in the model and not written to the guest's firmware.",
		"os: twin boots the twin's pinned Alpine Linux image through the firmware's QEMU kernel loader (fw_cfg), not over PXE; no in-host agent or accelerator shim (Mokka) runs, and accelerators, InfiniBand ports and drives are absent from the guest's PCI tree.",
		"os: stack hosts boot from their firmware's boot order: a network boot is the firmware's own PXE or HTTP boot through the twin's segment, to whatever the stack serves; Redfish boot overrides are not applied to the firmware (above).",
		"The serial console is a stand-in for Serial over LAN: the guest's serial port logged to a file and served by the control API; IPMI SOL is not served.",
		"SMBIOS type 0 carries the profile's manufacturer and the model's uefi firmware version, but the firmware that runs is QEMU's edk2 build; the type 1 version string is QEMU's machine type (virt-<version>), which a guest can read.",
		"No x86_64 os: twin guest image is pinned: x86_64 vm hosts can run os: stack only.",
	}
	if tpm == 0 {
		r.IsNot = append(r.IsNot, "No TPM is attached (swtpm is not installed, the profile has none, or spec.adapters.vm.tpm is off).")
	} else {
		r.IsNot = append(r.IsNot, "The TPM is swtpm with persisted state, manufactured on first use with EK and platform certificates signed by the estate CA (swtpm_setup, swtpm_localca), not by a TPM vendor's CA; the twin guest's kernel (Alpine virt) has no TPM driver, so in os: twin only the firmware measures into it.")
	}
	sort.Strings(r.TCG)
	return r
}
