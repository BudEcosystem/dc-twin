package bluefield

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// A BlueField at `vm` fidelity has an Arm: a QEMU machine (aarch64, edk2) that runs what a
// BlueField's Arm runs, and nothing the twin wrote in its place. Its BMC is still the model's
// (fronted here); its Arm is observed (D19). The Arm is this adapter's substrate for the DPU
// (adapter.Substrate, engine.Substrate): the engine hands it the DPU's power requests, and it
// reports what QEMU says back as observations.
//
//   - Powered on with nothing installed, the Arm runs its UEFI firmware against an empty NVMe
//     disk, as a BlueField with an empty eMMC does.
//   - A BFB pushed through the BMC (UpdateService.SimpleUpdate to DPU_OS) is saved as the BMC
//     streams it; when the whole stream has gone through, the BMC resets the Arm into it, and the
//     Arm boots the stream's own installer: its Linux image and initramfs, its boot arguments, and
//     the bf.cfg that came after the last image as /etc/bf.cfg (armdisk.go). The installer is
//     NVIDIA's, unmodified: it partitions the disk, writes the DPU OS and its boot loader, runs the
//     bf.cfg's hooks (DPF's write the DPU's cloud-init), and reboots.
//   - From then on every reset boots GRUB's default entry from the disk: the kernel, initrd and
//     command line GRUB would boot, read from the installed root file system, so a change the DPU
//     OS makes to its boot configuration (DPF's agent rewrites the kernel command line and
//     reboots) takes effect at the next reset as on a BlueField.
//
// What the twin adds, because QEMU's `virt` machine is not a BlueField, each said in the
// fidelity report:
//   - A UART where the kernel's early console is. BlueField kernels print their first lines to
//     a PL011 at the BlueField's address (0x01000000 in the BFB's GRUB default, 0x13010000 in
//     DPF's flavour), which on `virt` is the firmware flash or a PCIe hole: the kernel waits on
//     its first character forever (Q1 traced it). `virt`'s PL011 is at 0x09000000, so the twin
//     puts `earlycon=pl011,0x9000000` first on the command line. Linux takes the first earlycon
//     and ignores the rest, and GRUB's command line follows verbatim, so /proc/cmdline still
//     carries every parameter the DPU OS configured (DPF's agent checks them).
//   - The out-of-band port's name. A BlueField's mlxbf_gige driver names its OOB port oob_net0
//     (DPF's cloud-init brings it up by that name); the Arm's OOB NIC is virtio. After the
//     installer finishes, the twin adds a systemd .link file matching the NIC's MAC to the
//     installed root file system (armdisk.go), once, while the disk is at rest and clean.
//   - The out-of-band address (armdhcp.go): the Arm's NIC is cabled to the DPU's management
//     switch port, which it shares with the BMC as on a BlueField; the twin relay serves one
//     client per port, so the Arm's DHCP is answered on its cable with an address in the BMC's
//     segment that the site DHCP never hands out.
//   - Reboots. QEMU runs with -no-reboot, so a reset the guest asks for ends the process and the
//     twin starts the next boot (GRUB's entry, re-read): the observation is a reset, not a power
//     cycle.
//
// What the Arm does not have: the ConnectX-7 (p0/p1, their representors and SFs: no QEMU
// device), rshim and its tmfifo network to the host, the boot FIFO (the bf.cfg archive stands in
// for it at installation only), IPMI towards the BMC, the eMMC, and BlueField's ACPI tables. Those
// the DPU OS finds missing, it reports as missing; nothing answers in their place.

const armObserver = "bluefield-arm"

// virtEarlycon is QEMU virt's PL011.
const virtEarlycon = "earlycon=pl011,0x9000000"

// tcgCPU is the CPU the Arm runs on under TCG: an Armv8.2 core like BlueField-3's Cortex-A78,
// with the 64 KB translation granule the 64K-page BFBs need. QEMU's `-cpu max` leaves edk2
// (AAVMF 2025.11) printing nothing past its banner on QEMU 10.2's TCG (Q1).
const tcgCPU = "neoverse-n1"

// armDiskBytes is the Arm's NVMe disk, sparse: a 64K-page DPU OS installs into 6.5 GiB of it.
const armDiskBytes = 40 << 30

// armInstallerMB is the least memory a BFB's installer installs with: it unpacks its 1.6 GB
// initramfs into a RAM file system of half the machine's memory, and a 64K-page BFB's needs 8 GB
// (Q1: 5 and 6 GB fail). The installer boots with at least this; the DPU OS with the profile's
// cost (or the estate's spec.adapters.vm), which is less.
const armInstallerMB = 8192

// The installer's RAM is a file on disk (QEMU memory-backend-file, shared), deleted when the
// installer has finished. Measured on an x86_64 host under TCG, the installer alone in a 9G memory
// cap (the lab's rule for one job): with anonymous guest RAM the installer's QEMU holds the
// guest's memory (the installer's page cache fills it while it extracts the DPU OS), its own copy
// of the 1.6 GB initramfs that -initrd loads, and the translation cache, and the cap killed it
// while it extracted the DPU OS, at 8.99 GiB with 8 GB of guest RAM and at 9,206 MiB with 7.5 GB.
// File-backed, the installer finished (1,040 s, against 777 s uncapped) with QEMU at 5,665 MiB
// resident of which 2,066 MiB anonymous: guest pages count against the cap too, but the kernel
// writes them back to the file instead of killing QEMU.
const armInstallerRAMFile = "installer-ram"

// tcgCacheMB caps TCG's translation cache. QEMU's default on a 64-bit host is up to 1 GiB, which a
// long installer fills and which counts against the host's memory as much as guest RAM does.
const tcgCacheMB = 256

type armRequest struct {
	canonical string
	cause     uint64
}

// arm is one BlueField's Arm.
type arm struct {
	a      *Adapter
	f      *front
	dev    *model.Device
	path   string
	dir    string
	run    string
	inst   *qemu.Installation
	cpus   int
	mem    int
	mac    net.HardwareAddr
	macSrc string
	disk   string
	cable  *qemu.Cable
	hp     *netsim.HostPort
	reqs   chan armRequest

	mu         sync.Mutex
	relay      *armRelay
	cur        *qemu.Machine
	kind       string // what cur runs: firmware | installer | os
	cause      uint64
	stopping   bool   // the twin is ending cur (a request), not the guest
	shutdown   string // the last QMP SHUTDOWN reason of cur
	stream     string // a whole pushed boot stream awaiting installation
	streamNote string
	versions   map[string]string // the stream's `info` inventory (BF3_UEFI and the like)
	capsule    string            // the boot firmware staged by the last installer, for the report
	installed  bool
	linkNote   string
	floor      int64 // console offset where cur's output begins
	seen       map[string]bool
	cmdline    string
	history    []string
	starts     map[string]int
	flows      map[string]int
	refused    map[string]int
	closed     bool
}

// armEligible says whether a device is a BlueField whose Arm this adapter runs at vm fidelity.
func armEligible(d *model.Device) bool {
	return d.HasBMC() && d.Dialect == Dialect && d.BaseClass == "dpu"
}

// armPlan is the machine a BlueField's profile asks for: its fidelity.vm architecture and cost,
// replaced by the estate's spec.adapters.vm when it gives them.
func armPlan(est *schema.Estate, d *model.Device) (arch string, cpus, memMB int, err error) {
	spec, ok := d.Profile.Spec.Fidelity["vm"]
	if !ok {
		return "", 0, 0, fmt.Errorf("profile %s has no fidelity.vm", d.ProfileName)
	}
	arch, _ = spec["arch"].(string)
	if arch == "" {
		arch = "aarch64"
	}
	if c, ok := spec["cost"].(map[string]any); ok {
		if v, ok := num(c["ramGB"]); ok {
			memMB = int(v * 1024)
		}
		if v, ok := num(c["vcpu"]); ok {
			cpus = int(v)
		}
	}
	if est != nil {
		if est.Spec.Adapters.VM.MemoryMB > 0 {
			memMB = est.Spec.Adapters.VM.MemoryMB
		}
		if est.Spec.Adapters.VM.CPUs > 0 {
			cpus = est.Spec.Adapters.VM.CPUs
		}
	}
	return arch, max(cpus, 1), max(memMB, 1024), nil
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

// Accepts runs a BlueField's Arm at vm fidelity where QEMU can run its guest and the tools the
// Arm's boot needs are installed (adapter.Substrate).
func (a *Adapter) Accepts(est *schema.Estate, d *model.Device, allowTCG bool) error {
	if !armEligible(d) {
		return fmt.Errorf("the bluefield adapter runs only BlueField DPUs (%s is %s)", d.Path, d.BaseClass)
	}
	arch, _, _, err := armPlan(est, d)
	if err != nil {
		return err
	}
	if _, err := qemu.Find(arch, est.Spec.Adapters.VM.QEMU, allowTCG); err != nil {
		return err
	}
	if _, err := debugfsPath(); err != nil {
		return err
	}
	if _, err := exec.LookPath("xz"); err != nil {
		return errors.New("xz is not installed: a BFB's Linux image is LZMA-compressed")
	}
	return nil
}

// newArm plans a BlueField's Arm. The machine starts when the DPU powers on.
func (a *Adapter) newArm(f *front) (*arm, error) {
	d := f.dev
	arch, cpus, mem, err := armPlan(a.eng.M.Estate, d)
	if err != nil {
		return nil, err
	}
	var qemuDir string
	if a.eng.M.Estate != nil {
		qemuDir = a.eng.M.Estate.Spec.Adapters.VM.QEMU
	}
	inst, err := qemu.Find(arch, qemuDir, a.rt.AllowTCG)
	if err != nil {
		return nil, err
	}
	slug := strings.ReplaceAll(d.Path, "/", "_")
	m := &arm{a: a, f: f, dev: d, path: d.Path, inst: inst, cpus: cpus, mem: mem, reqs: make(chan armRequest, 64),
		dir: filepath.Join(a.rt.StateDir, "bluefield", slug), starts: map[string]int{}, flows: map[string]int{},
		refused: map[string]int{}, seen: map[string]bool{}}
	m.run = shortDir(m.dir, a.rt.StateDir+"|"+d.Path)
	for _, p := range []string{m.dir, m.run} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return nil, err
		}
	}
	m.disk = filepath.Join(m.dir, "nvme.raw")
	if _, err := os.Stat(m.disk); os.IsNotExist(err) {
		fh, err := os.Create(m.disk)
		if err != nil {
			return nil, err
		}
		err = fh.Truncate(armDiskBytes)
		fh.Close()
		if err != nil {
			return nil, err
		}
	} else if _, err := rootPartition(m.disk); err == nil {
		m.installed = true // an earlier run of this state directory installed a DPU OS
	}
	m.mac, m.macSrc = a.armMAC(f)
	if m.cable, err = qemu.ListenCable(filepath.Join(m.run, "oob.sock")); err != nil {
		return nil, err
	}
	var svcs []netsim.Service
	if s := strings.TrimSpace(a.rt.Outputs["siteServices"]); s != "" {
		if svcs, err = netsim.ParseServices(s); err != nil {
			m.cable.Close()
			return nil, fmt.Errorf("the stack's siteServices output: %w", err)
		}
	}
	bmcPort := m.bmcPort()
	ref := d.Path + ":oob"
	if bmcPort != nil {
		ref = bmcPort.Ref
	}
	m.hp, err = netsim.NewHostPort(m.cable, netsim.HostPortConfig{Ref: ref, MAC: m.mac, Plug: m.plug, Services: svcs,
		Routes: a.rt.Net.Routes, Observe: m.flow, Log: a.rt.Log})
	if err != nil {
		m.cable.Close()
		return nil, err
	}
	return m, nil
}

// shortDir is a directory for the machine's sockets short enough for a unix socket path.
func shortDir(preferred, key string) string {
	if len(filepath.Join(preferred, "console.sock")) <= 100 {
		return preferred
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(os.TempDir(), "dctwin-arm-"+hex.EncodeToString(sum[:5]))
}

func (m *arm) bmcPort() *model.Port {
	if p := m.dev.PortBy["bmc0"]; p != nil {
		return p
	}
	for _, p := range m.dev.Ports {
		if p.Role == "management" {
			return p
		}
	}
	return nil
}

// armMAC is the Arm's out-of-band MAC: what the BMC serves for the Arm's oob0 interface (the
// capture's Systems/Bluefield/EthernetInterfaces/oob0, re-identified for this device the way the
// redfish engine re-identifies every captured MAC), so the address the BMC reports is the one
// the Arm's NIC carries. Without the capture, the BMC port's MAC with its last octet incremented.
func (a *Adapter) armMAC(f *front) (net.HardwareAddr, string) {
	if dir, err := twin.AssetPath(a.rt.Reg, f.spec.Spec.Mockup); err == nil {
		b, err := os.ReadFile(filepath.Join(dir, "redfish", strings.TrimPrefix(f.system, "/redfish/"), "EthernetInterfaces", "oob0", "index.json"))
		if err == nil {
			var doc map[string]any
			if json.Unmarshal(b, &doc) == nil {
				if s, _ := doc["MACAddress"].(string); s != "" {
					mac := identity.New(a.eng.M.Seed).Scrub("mac", f.dev.Path, f.dev.Identity.ProfileMajor, s)
					if hw, err := net.ParseMAC(mac); err == nil {
						return hw, "the MAC the BMC serves for the Arm's oob0"
					}
				}
			}
		}
	}
	hw, _ := net.ParseMAC(f.dev.Identity.BMCMAC)
	if p := f.dev.PortBy["bmc0"]; p != nil {
		if m, err := net.ParseMAC(p.MAC); err == nil {
			hw = m
		}
	}
	out := append(net.HardwareAddr(nil), hw...)
	if len(out) == 6 {
		out[5]++
	}
	return out, "the BMC port's MAC + 1 (no captured oob0 interface)"
}

// plug gives the Arm's port its relay once the BMC holds a lease (netsim retries it on the
// Arm's DHCP until it succeeds).
func (m *arm) plug() (netsim.PortRelay, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.relay != nil {
		return m.relay, nil
	}
	p := m.bmcPort()
	if p == nil {
		return nil, errors.New("the DPU has no management port")
	}
	var lease *model.Lease
	var network netip.Prefix
	var dhcpMode string
	var gwMAC net.HardwareAddr
	m.a.eng.Read(func(md *model.Model) {
		if m.dev.State.Lease != nil {
			l := *m.dev.State.Lease
			lease = &l
		}
		if md.Estate != nil {
			n := md.Estate.Spec.Networks[p.Network]
			dhcpMode = n.DHCP
			network, _ = netip.ParsePrefix(n.CIDR)
		}
		if p.Link != nil {
			if q := p.Link.Peer(p); q != nil {
				gwMAC, _ = net.ParseMAC(q.MAC)
			}
		}
	})
	if dhcpMode == "stack" {
		return nil, fmt.Errorf("network %s's DHCP is the stack's, which would answer the Arm; the twin relay serves one client per switch port", p.Network)
	}
	if lease == nil || lease.IP == "" {
		return nil, errors.New("the BMC has no lease yet")
	}
	if len(gwMAC) != 6 {
		gwMAC = net.HardwareAddr{0x02, 0xdc, 0x7e, 0x00, 0x00, 0x02}
	}
	r, err := newArmRelay(m.mac, lease.IP, lease.Gateway, network, gwMAC)
	if err != nil {
		return nil, err
	}
	m.relay = r
	return r, nil
}

// flow counts what the Arm reached through its segment gateway.
func (m *arm) flow(fl netsim.Flow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fl.Closed {
		return
	}
	if fl.Refused != "" {
		m.refused[fl.Proto+" "+fl.Dst.String()]++
		return
	}
	m.flows[fl.Service+" ("+fl.Proto+" "+fl.Dst.String()+")"]++
}

// Power is the engine's hand-off (engine.Substrate): requests are carried out in order.
func (a *Adapter) Power(path, canonical string, cause uint64) error {
	m := a.arms[path]
	if m == nil {
		return engine.ErrNoDevice
	}
	select {
	case m.reqs <- armRequest{canonical, cause}:
		return nil
	default:
		return errors.New("the Arm's request queue is full")
	}
}

// pushed records a whole boot stream the BMC received: the next reset boots its installer.
func (m *arm) pushed(stream, note string, versions map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stream, m.streamNote, m.versions = stream, note, versions
}

// capsuleWritten is what a BFB's installer prints once it has written its ARM capsule (ATF and
// UEFI) to a BlueField's boot partitions, which the Arm runs from its next reset. On the Arm at vm
// the write fails (`bad block device /dev/mmcblk0`: no eMMC) and the installer reports it passed.
const capsuleWritten = "Installing ARM Capsule passed"

// capsuleVersions are the boot firmware versions a boot stream carries for a BlueField
// generation, by the profile's component: its `info` inventory's BF3_UEFI, BF3_ATF and BF3_BSP
// (what DPF reads from the same BFB and compares the BMC's DPU_UEFI and DPU_BSP with).
func capsuleVersions(versions map[string]string, gen string) map[string]string {
	out := map[string]string{}
	for comp, name := range map[string]string{"uefi": "UEFI", "atf": "ATF", "bsp": "BSP"} {
		if v := versions[gen+"_"+name]; v != "" {
			out[comp] = v
		}
	}
	return out
}

// stageCapsule stages a boot stream's boot firmware on the DPU, for the engine to apply at the
// Arm's next observed reset (applyOn: reset): the BMC's inventory then reports the BFB's versions,
// as a BlueField's does once it runs the capsule its installer wrote. Nothing is flashed.
func (m *arm) stageCapsule(versions map[string]string, cause uint64) string {
	gen := "BF3"
	if m.dev.Profile != nil && strings.Contains(m.dev.Profile.Metadata.Name, "bluefield-2") {
		gen = "BF2"
	}
	want := capsuleVersions(versions, gen)
	if len(want) == 0 {
		return "no " + gen + " boot firmware versions in the stream's inventory"
	}
	comps := make([]string, 0, len(want))
	for c := range want {
		comps = append(comps, c)
	}
	sort.Strings(comps)
	var staged []string
	m.a.eng.Seed(m.path, "the BFB's installer wrote its ARM capsule: the boot firmware it carries is staged for the Arm's next reset", func(d *model.Device) {
		for _, c := range comps {
			if fw := d.State.Firmware[c]; fw != nil {
				fw.Staged = want[c]
				staged = append(staged, c+" "+want[c])
			}
		}
	})
	if len(staged) == 0 {
		return "the profile has no uefi, atf or bsp component to stage"
	}
	return "boot firmware staged for the next reset: " + strings.Join(staged, ", ")
}

func (m *arm) observe(field, to, msg string, cause uint64, data map[string]any) {
	_ = m.a.eng.Observe(engine.Observation{Path: m.path, Observer: armObserver, Field: field, To: to, Message: msg, Cause: cause, Data: data})
}

func (m *arm) note(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = append(m.history, m.a.eng.Now().UTC().Format("15:04:05")+" "+s)
	if len(m.history) > 64 {
		m.history = m.history[len(m.history)-64:]
	}
}

func (m *arm) work() {
	defer m.a.workers.Done()
	for {
		select {
		case <-m.a.stopped:
			return
		case r := <-m.reqs:
			if err := m.handle(r); err != nil {
				m.note(r.canonical + ": " + err.Error())
				m.a.eng.Commit(schema.Event{Kind: "twin", Severity: "incident", Source: schema.EventSource{Class: "twin", Principal: "bluefield"},
					Path: m.path, Cause: r.cause, Message: fmt.Sprintf("%s: the Arm: %s: %v", m.path, r.canonical, err)})
			}
		}
	}
}

// osRunning reports whether the Arm's DPU OS has reached its login prompt in the current boot.
func (m *arm) osRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil && m.kind == "os" && m.seen["os.login"]
}

func (m *arm) running() *qemu.Machine {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur
}

// next is what the Arm boots now: a pushed stream's installer, else the installed DPU OS, else
// its firmware alone.
func (m *arm) next() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.stream != "":
		return "installer"
	case m.installed:
		return "os"
	}
	return "firmware"
}

func (m *arm) handle(r armRequest) error {
	m.mu.Lock()
	m.cause = r.cause
	m.mu.Unlock()
	cur := m.running()
	switch r.canonical {
	case "power.on":
		if cur != nil {
			return nil
		}
		return m.start(m.next(), false)
	case "power.off.force":
		if cur != nil {
			m.end(cur)
			m.observe("power", model.Off, "the Arm's machine stopped (power off)", r.cause, nil)
		}
		return nil
	case "power.off.graceful":
		if cur != nil {
			return cur.Powerdown() // the guest decides; a shutdown ends the process (watchExit)
		}
		return nil
	case "power.restart", "power.restart.graceful", "power.cycle", "guest.reset":
		reset := (cur != nil && r.canonical != "power.cycle") || r.canonical == "guest.reset"
		if r.canonical == "power.restart.graceful" && cur != nil {
			_ = cur.Powerdown()
			select {
			case <-cur.Exited():
			case <-time.After(5 * time.Minute):
			}
		}
		if cur = m.running(); cur != nil {
			m.end(cur)
		}
		if r.canonical == "power.cycle" {
			m.observe("power", model.Off, "the Arm's machine stopped (power cycle)", r.cause, nil)
		}
		return m.start(m.next(), reset)
	}
	return fmt.Errorf("unsupported request %s", r.canonical)
}

// end stops the current machine because the twin was asked to. An installer stopped part-way
// has consumed its boot stream, as on a BlueField: the next boot is whatever the disk holds.
func (m *arm) end(cur *qemu.Machine) {
	m.mu.Lock()
	m.stopping = true
	if m.cur == cur && m.kind == "installer" && m.stream != "" {
		_ = os.Remove(m.stream)
		m.stream, m.streamNote = "", m.streamNote+" (the Arm was reset before its installer finished)"
	}
	m.mu.Unlock()
	cur.Quit(10 * time.Second)
	m.mu.Lock()
	if m.cur == cur {
		m.cur = nil
	}
	m.stopping = false
	m.mu.Unlock()
}

// start boots the Arm. reset says the Arm stays powered (a reset, not a power-on).
func (m *arm) start(kind string, reset bool) error {
	product := "BlueField"
	if id := m.dev.Profile.Spec.Identity; id != nil && id.Model != "" {
		product = id.Model
	}
	accel := m.inst.Accel
	if accel == "tcg" {
		accel = fmt.Sprintf("tcg,tb-size=%d", tcgCacheMB)
	}
	cfg := qemu.Config{Name: m.path, Arch: m.inst.Arch, Accel: accel, CPUs: m.cpus, MemoryMB: m.mem,
		Binary: m.inst.Binary, Code: m.inst.Code, VarsTemplate: m.inst.VarsTemplate, Vars: filepath.Join(m.dir, "vars.fd"),
		UUID: m.dev.Identity.UUID, RunDir: m.run, Console: filepath.Join(m.dir, "console.log"),
		SMBIOS: []qemu.SMBIOS{{Type: 1, Fields: [][2]string{{"manufacturer", "NVIDIA"},
			{"product", product}, {"serial", m.dev.Identity.Serial}}}},
		NICs:  []qemu.NIC{{ID: "oob", MAC: m.mac.String(), Netdev: qemu.StreamNetdev(filepath.Join(m.run, "oob.sock"))}},
		Disks: []qemu.Disk{{Path: m.disk, Format: "raw", Serial: m.dev.Identity.Serial}},
		Extra: []string{"-no-reboot"}}
	if m.inst.Accel == "tcg" && m.inst.Arch == "aarch64" {
		cfg.Extra = append(cfg.Extra, "-cpu", tcgCPU)
	}
	m.mu.Lock()
	stream, cause := m.stream, m.cause
	m.mu.Unlock()
	var what string
	switch kind {
	case "installer":
		parts, err := splitBFB(stream, filepath.Join(m.dir, "installer"))
		if err != nil {
			m.mu.Lock()
			m.stream = ""
			m.mu.Unlock()
			m.note("the pushed boot stream cannot boot: " + err.Error())
			return m.start(m.next(), reset)
		}
		args := parts.args
		if args == "" {
			args = "console=ttyAMA0 initrd=initramfs"
		}
		cfg.Kernel, cfg.Initrd, cfg.Append = parts.kernel, parts.initrd, virtEarlycon+" "+args
		cfg.MemoryMB = max(cfg.MemoryMB, armInstallerMB)
		ram := filepath.Join(m.dir, armInstallerRAMFile)
		_ = os.Remove(ram)
		cfg.Extra = append(cfg.Extra, "-object", fmt.Sprintf("memory-backend-file,id=ram0,size=%dM,mem-path=%s,share=on", cfg.MemoryMB, ram),
			"-machine", "memory-backend=ram0")
		what = fmt.Sprintf("the pushed BFB's installer (%d MB; bf.cfg: %d bytes as /etc/bf.cfg)", cfg.MemoryMB, parts.cfg)
	case "os":
		k, i, cmdline, err := installedBoot(m.disk, filepath.Join(m.dir, "boot"))
		if err != nil {
			m.mu.Lock()
			m.installed = false
			m.mu.Unlock()
			m.note("no bootable DPU OS on the disk: " + err.Error())
			return m.start(m.next(), reset)
		}
		cfg.Kernel, cfg.Initrd, cfg.Append = k, i, strings.TrimSpace(virtEarlycon+" "+cmdline)
		what = fmt.Sprintf("the installed DPU OS (GRUB's default entry, %d MB)", cfg.MemoryMB)
	default:
		what = "its UEFI firmware (no DPU OS installed)"
	}
	floor := int64(0)
	if st, err := os.Stat(cfg.Console); err == nil {
		floor = st.Size()
	}
	mach, err := qemu.Start(cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cur, m.kind, m.shutdown, m.floor, m.seen, m.cmdline = mach, kind, "", floor, map[string]bool{}, cfg.Append
	m.starts[kind]++
	m.mu.Unlock()
	go m.watchQMP(mach)
	if err := mach.Cont(); err != nil {
		mach.Quit(5 * time.Second)
		return err
	}
	data := map[string]any{"boots": kind, "commandLine": cfg.Append, "accel": cfg.Accel}
	if reset {
		m.observe("reset", "arm", "the Arm reset into "+what, cause, data)
	} else {
		m.observe("power", model.On, "the Arm's machine started: "+what, cause, data)
	}
	m.note("started " + what)
	go m.watchExit(mach, kind)
	return nil
}

// watchQMP keeps the reason QEMU gives for ending the machine.
func (m *arm) watchQMP(mach *qemu.Machine) {
	for ev := range mach.QMP.Events() {
		if ev.Name == "SHUTDOWN" {
			m.mu.Lock()
			if m.cur == mach {
				m.shutdown = fmt.Sprint(ev.Data["reason"])
			}
			m.mu.Unlock()
		}
	}
}

// watchExit follows a machine to its end: a reset the guest asked for boots the next thing (the
// installed OS after the installer); anything else the guest did is the Arm powering off.
func (m *arm) watchExit(mach *qemu.Machine, kind string) {
	<-mach.Exited()
	time.Sleep(200 * time.Millisecond) // let the QMP reader record SHUTDOWN
	m.mu.Lock()
	if m.cur != mach || m.stopping || m.closed {
		m.mu.Unlock()
		return
	}
	m.cur = nil
	reason, cause, floor := m.shutdown, m.cause, m.floor
	m.mu.Unlock()
	console := m.consoleSince(floor)
	if kind == "installer" {
		m.installerEnded(console)
	}
	if reason == "guest-reset" {
		select {
		case m.reqs <- armRequest{"guest.reset", cause}:
		default:
		}
		return
	}
	why := reason
	if why == "" {
		why = strings.TrimSpace(mach.ExitError())
	}
	m.observe("power", model.Off, "the Arm's machine ended: "+why, cause, nil)
	m.note("ended: " + why)
}

// installerEnded reads the installer's verdict from its console and readies the next boot.
func (m *arm) installerEnded(console string) {
	ok := strings.Contains(console, "INFO: Installation finished") || strings.Contains(console, "Installation finished. Rebooting")
	failed := strings.Contains(console, "ERR Installation failed") || strings.Contains(console, "Failed to install")
	m.mu.Lock()
	stream, versions := m.stream, m.versions
	m.stream = ""
	if ok && !failed {
		m.installed = true
	}
	cause := m.cause
	m.mu.Unlock()
	if stream != "" {
		_ = os.Remove(stream)
		_ = os.RemoveAll(filepath.Join(m.dir, "installer"))
	}
	_ = os.Remove(filepath.Join(m.dir, armInstallerRAMFile))
	switch {
	case ok && !failed:
		note := "oob_net0 named by a link file matching " + m.mac.String()
		if err := writeOOBLink(m.disk, m.dir, m.mac.String()); err != nil {
			note = "oob_net0 not named: " + err.Error()
		}
		capsule := "the installer did not report its ARM capsule written: no boot firmware staged"
		if strings.Contains(console, capsuleWritten) {
			capsule = m.stageCapsule(versions, cause)
		}
		m.mu.Lock()
		m.linkNote, m.capsule = note, capsule
		m.mu.Unlock()
		m.observe("arm", "installed", "the BFB's installer finished: "+note+"; "+capsule, cause, nil)
		m.note("installer finished; " + note + "; " + capsule)
	default:
		m.observe("arm", "install-failed", "the BFB's installer did not finish", cause, nil)
		m.note("installer did not finish")
	}
}

// consoleSince is the Arm's console from an offset (the current boot's output).
func (m *arm) consoleSince(off int64) string {
	f, err := os.Open(filepath.Join(m.dir, "console.log"))
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, 64<<20))
	return string(b)
}

// armMilestones are the console lines the Arm's observer reports, per boot kind. The getty's
// prompt is matched as `login:` alone: the kernel's messages share the console, and one printed
// between the hostname and `login:` puts the prompt at the start of a line (found by a run).
var armMilestones = map[string][][2]string{
	"installer": {{"Linux version", "installer.kernel"}, {"Installing ubuntu", "installer.started"},
		{"OS installation target", "installer.target"}, {"Running bfb_modify_os from bf.cfg", "installer.bf.cfg"},
		{"installation completed", "installer.os-written"}, {"INFO: Installation finished", "installer.finished"},
		{"ERR Installation failed", "installer.failed"}},
	"os": {{"Linux version", "os.kernel"}, {"Reached target", "os.systemd"}, {"login:", "os.login"},
		{"Cloud-init v.", "os.cloud-init"}, {"finished at", "os.cloud-init-done"}, {"Kernel panic", "os.panic"}},
}

// armReached is the milestones of a boot kind that its console text shows, in armMilestones' order.
func armReached(kind, text string) [][2]string {
	var out [][2]string
	for _, ms := range armMilestones[kind] {
		if strings.Contains(text, ms[0]) {
			out = append(out, ms)
		}
	}
	return out
}

// watchConsole reports each milestone of the current boot once, and the boot stage reaching the
// OS when its kernel starts.
func (m *arm) watchConsole() {
	defer m.a.wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.a.stopped:
			return
		case <-t.C:
		}
		m.mu.Lock()
		cur, kind, floor, cause := m.cur, m.kind, m.floor, m.cause
		m.mu.Unlock()
		if cur == nil {
			continue
		}
		for _, ms := range armReached(kind, m.consoleSince(floor)) {
			m.mu.Lock()
			fresh := m.cur == cur && !m.seen[ms[1]]
			if fresh {
				m.seen[ms[1]] = true
			}
			m.mu.Unlock()
			if !fresh {
				continue
			}
			if ms[1] == "os.kernel" {
				m.observe("boot", "os", "the DPU OS's kernel started", cause, nil)
			}
			m.observe("event", ms[1], "the Arm's console: "+ms[0], cause, nil)
		}
	}
}

// ConsoleLog serves a BlueField Arm's serial console (adapter.Console).
func (a *Adapter) ConsoleLog(path string, from int64) ([]byte, int64, error) {
	m := a.arms[path]
	if m == nil {
		return nil, from, engine.ErrNoDevice
	}
	f, err := os.Open(filepath.Join(m.dir, "console.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, from, err
	}
	defer f.Close()
	st, _ := f.Stat()
	if from > st.Size() || from < 0 {
		from = 0
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, from, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	return b, from + int64(len(b)), err
}

// close stops the Arm's machine and its port.
func (m *arm) close() {
	m.mu.Lock()
	m.closed = true
	cur := m.cur
	m.cur = nil
	m.mu.Unlock()
	if cur != nil {
		cur.Quit(5 * time.Second)
	}
	m.hp.Close()
	m.cable.Close()
}

// reportLines are the Arm's lines in the fidelity report. The port's relay is asked before the
// Arm's lock is taken: the port holds its own lock when it plugs (m.plug takes the Arm's).
func (m *arm) reportLines() []string {
	server, why := m.hp.Relay()
	m.mu.Lock()
	defer m.mu.Unlock()
	state := "off"
	if m.cur != nil {
		state = "running " + m.kind
	}
	tcg := ""
	if m.inst.Accel == "tcg" {
		tcg = fmt.Sprintf(", -cpu %s, translation cache %d MiB", tcgCPU, tcgCacheMB)
	}
	out := []string{fmt.Sprintf("Arm at vm: QEMU %s %s (%s%s), %d vCPUs, %d MB (the installer %d MB), NVMe %d GiB raw; %s; installed DPU OS: %v",
		m.inst.Arch, m.inst.Version, m.inst.Accel, tcg, m.cpus, m.mem, max(m.mem, armInstallerMB), armDiskBytes>>30, state, m.installed)}
	var ks []string
	for k, n := range m.starts {
		ks = append(ks, fmt.Sprintf("%s ×%d", k, n))
	}
	sort.Strings(ks)
	if len(ks) > 0 {
		out = append(out, "Arm boots: "+strings.Join(ks, ", "))
	}
	if m.cmdline != "" {
		out = append(out, "Arm's last command line: "+m.cmdline)
	}
	out = append(out, "Arm's OOB MAC "+m.mac.String()+" ("+m.macSrc+")")
	if m.relay != nil {
		req, acks := m.relay.stats()
		out = append(out, fmt.Sprintf("Arm's OOB address %s via %s: %d DHCP requests, %d ACKs — answered by the bluefield adapter on the Arm's cable, not by the twin relay (one client per switch port); the port saw %d DHCP requests, delivered %d replies, opened %d TCP flows, refused %d, dropped %d frames",
			m.relay.addr, m.relay.gw, req, acks, m.hp.DHCPRequests.Load(), m.hp.DHCPReplies.Load(), m.hp.TCPFlows.Load(), m.hp.TCPRefused.Load(), m.hp.Dropped.Load()))
	} else if why != "" {
		out = append(out, "Arm's OOB address: none ("+why+")")
	} else {
		out = append(out, "Arm's OOB DHCP: "+server)
	}
	if m.linkNote != "" {
		out = append(out, "Arm's OOB port name: "+m.linkNote)
	}
	if m.capsule != "" {
		out = append(out, "Arm's boot firmware: "+m.capsule+" (nothing is flashed: the Arm's machine runs QEMU's edk2)")
	}
	if m.streamNote != "" {
		out = append(out, "last pushed BFB: "+m.streamNote)
	}
	var fl []string
	for k, n := range m.flows {
		fl = append(fl, fmt.Sprintf("%s ×%d", k, n))
	}
	sort.Strings(fl)
	if len(fl) > 0 {
		out = append(out, "Arm reached: "+strings.Join(fl, "; "))
	}
	var rf []string
	for k, n := range m.refused {
		rf = append(rf, fmt.Sprintf("%s ×%d", k, n))
	}
	sort.Strings(rf)
	if len(rf) > 0 {
		out = append(out, "Arm refused (no route): "+strings.Join(rf, "; "))
	}
	for _, h := range m.history {
		out = append(out, "Arm: "+h)
	}
	return out
}
