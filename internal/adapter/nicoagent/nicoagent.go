// Package nicoagent impersonates the software NICo runs on the machines it manages, for machines
// at `model` fidelity, which run no OS (D55). This is IMPERSONATION (D24, 07 — Impersonation): an
// estate enables it by naming the stack's impersonators, and the run is labelled impersonated.
//
//   - `nico-dpu-agent`, for every BlueField DPU at `model`: the DPU's UEFI HTTP boot (boot.go),
//     NICo's DPU image (carbide.efi) reporting the DPU's hardware, and the installed DPU OS's
//     forge-dpu-agent taking its instructions, reporting the managed host's network configuration
//     applied, and answering its host's DHCP from that configuration, as the forge-dhcp-server it
//     runs does (agent.go, dpuserve.go).
//   - `nico-scout`, for every host at `model` (a compute tray or a server): the host's UEFI HTTP
//     boot, through its DPU when the DPU is impersonated too and else on its provisioning port,
//     and NICo's discovery agent, scout, reporting the host's hardware (its TPM's EK certificate,
//     its NICs, its GPUs with the NVLink placement NMX-C reports) and answering NICo's
//     instructions (host.go).
//
// What it does not: run any of that software, configure the ConnectX or HBN, wipe disks or run
// validation tests, or report anything NICo did not configure. Everything it says about a machine
// is what the machine's BMC serves (serials, MACs, firmware, read from the redfish adapter) or
// what the model derives for it as the twin's other interfaces do (NVLink GUIDs, placement), and
// everything it does is on the machine's own network path: a userspace NIC at the machine's
// switch port (guest.go), so NICo's DHCP server, PXE service and API see its MAC and address, as
// they would a real machine's.
//
// It follows each machine's power: a power-on or a reset (from NICo, through the BMC) starts a
// boot, a power-off ends what runs. Nothing changes model state except a machine's boot progress,
// which it commits as an observer (engine.SetBooted), so the BMC's BootProgress says OSRunning
// only while the stand-in's software runs.
package nicoagent

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Name is the adapter's registered name; DPUAgent and Scout are the impersonators the stack lists.
const (
	Name     = "nicoagent"
	DPUAgent = "nico-dpu-agent"
	Scout    = "nico-scout"
	dialect  = "nvidia/bluefield"
)

var src = schema.EventSource{Class: "observer", Principal: Name}

func init() { twin.Register(Name, func() adapter.Adapter { return New() }) }

// Adapter runs a stand-in for every model machine whose impersonator the estate enables.
type Adapter struct {
	rt       *adapter.Runtime
	forge    *Forge
	rootCA   *x509.CertPool
	inactive string
	enabled  map[string]bool
	dpus     map[string]*dpu
	hosts    map[string]*host
	machines map[string]*machine
	order    []string
	uplink   *netsim.Uplink
	ownLink  bool
	upMu     sync.Mutex
	subID    int
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	// The agents' polling intervals (fields so a test can shorten them): forge-dpu-agent's and
	// scout's defaults are of this order.
	controlEvery, networkEvery, discoveryRetry time.Duration
	// bootRetry is how long a machine's firmware waits after a network boot that failed (no
	// DHCP answer, a boot service down) before it tries again, as UEFI cycles its boot options
	bootRetry time.Duration
}

// New returns an unstarted adapter.
func New() *Adapter {
	return &Adapter{dpus: map[string]*dpu{}, hosts: map[string]*host{}, machines: map[string]*machine{}, enabled: map[string]bool{},
		controlEvery: 10 * time.Second, networkEvery: 30 * time.Second, discoveryRetry: 30 * time.Second, bootRetry: 30 * time.Second}
}

func (a *Adapter) Name() string { return Name }

// impersonators are the ones the estate enables on its stacks (D24).
func impersonators(est *schema.Estate) map[string]bool {
	out := map[string]bool{}
	if est == nil {
		return out
	}
	for _, s := range est.Spec.Stacks {
		for _, i := range s.Impersonators {
			out[i] = true
		}
	}
	return out
}

func isDPU(d *model.Device) bool {
	return d.HasBMC() && d.Dialect == dialect && d.BaseClass == "dpu" && d.Fidelity == "model"
}

func isHost(d *model.Device) bool { return d.IsHost() && d.HasBMC() && d.Fidelity == "model" }

func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	a.enabled = impersonators(rt.Engine.M.Estate)
	if !a.enabled[DPUAgent] && !a.enabled[Scout] {
		a.inactive = "the estate enables neither impersonator " + DPUAgent + " nor " + Scout + " (spec.stacks[].impersonators)"
		return nil
	}
	var dpus, hosts []*model.Device
	rt.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			switch {
			case a.enabled[DPUAgent] && isDPU(d):
				dpus = append(dpus, d)
			case a.enabled[Scout] && isHost(d):
				hosts = append(hosts, d)
			}
		}
	})
	if len(dpus)+len(hosts) == 0 {
		a.inactive = "no BlueField DPU or host at model fidelity for the enabled impersonators"
		return nil
	}
	f, err := LoadForge(rt.Reg)
	if err != nil {
		return fmt.Errorf("nicoagent: %w", err)
	}
	a.forge = f
	if s := strings.TrimSpace(rt.Outputs["nicoRootCA"]); s != "" {
		pem, err := os.ReadFile(s)
		if err != nil {
			return fmt.Errorf("nicoagent: the stack's nicoRootCA output: %w", err)
		}
		a.rootCA = x509.NewCertPool()
		a.rootCA.AppendCertsFromPEM(pem)
	}
	dir := filepath.Join(rt.StateDir, Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	newMachine := func(d *model.Device, kind string) *machine {
		m := &machine{a: a, dev: d, path: d.Path, kind: kind, forge: f, arch: archOf(d),
			statePath: filepath.Join(dir, strings.ReplaceAll(d.Path, "/", "_")+".json"), calls: map[string]int{}, failures: map[string]string{}}
		a.machines[d.Path] = m
		a.order = append(a.order, d.Path)
		return m
	}
	for _, d := range dpus {
		b := &dpu{machine: newMachine(d, "dpu")}
		b.run = b.runDPU
		a.dpus[d.Path] = b
	}
	for _, d := range hosts {
		h := &host{machine: newMachine(d, "host")}
		h.run = h.runHost
		// the host's DPU, when this adapter runs it: the host's network is through it
		for _, ch := range d.Children {
			if b := a.dpus[ch.Path]; b != nil {
				h.dpu = b
				break
			}
		}
		a.hosts[d.Path] = h
		// the boot walker leaves the host to the stand-in (a walk would be a second firmware)
		rt.ClaimBoot(d.Path, Name)
	}
	sort.Strings(a.order)
	ctx, a.cancel = context.WithCancel(ctx)
	id, ch := rt.Engine.Subscribe(8192)
	a.subID = id
	a.wg.Add(1)
	go a.watch(ctx, ch)
	// a machine on at start boots once the twin is up (its NIC needs the dhcp adapter's relays
	// and its identity the redfish adapter, which may start after this one)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		for _, p := range a.order {
			m := a.machines[p]
			on := false
			rt.Engine.Read(func(*model.Model) { on = m.dev.State.Power == model.On })
			if on {
				m.boot(ctx, "initialState")
			}
		}
	}()
	return nil
}

// archOf is a machine's UEFI architecture: a BlueField's and a Grace tray's aarch64, a profile's
// vm architecture when it gives one, else x86_64.
func archOf(d *model.Device) string {
	if d.BaseClass == "dpu" || d.BaseClass == "compute-tray" {
		return "aarch64"
	}
	if v, ok := d.Profile.Spec.Fidelity["vm"]["arch"].(string); ok && v == "aarch64" {
		return "aarch64"
	}
	return "x86_64"
}

// watch follows the machines' power: on or reset boots, off stops.
func (a *Adapter) watch(ctx context.Context, ch <-chan schema.Event) {
	defer a.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				a.rt.Engine.Incident(Name, "event subscription dropped: the NICo agent stand-ins fell behind the log")
				return
			}
			m := a.machines[ev.Path]
			if m == nil || ev.State == nil {
				continue
			}
			to, _ := ev.State.To.(string)
			switch {
			case ev.State.Field == "power" && to == model.On:
				m.boot(ctx, "power on")
			case ev.State.Field == "boot" && to == "reset":
				m.boot(ctx, "reset")
			case ev.State.Field == "power" && (to == model.Off || to == model.PoweringOff):
				m.stop()
			}
		}
	}
}

func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	if a.rt != nil && a.subID != 0 {
		a.rt.Engine.Unsubscribe(a.subID)
	}
	for _, m := range a.machines {
		m.stop()
		m.closeNIC()
	}
	a.wg.Wait()
	if a.ownLink && a.uplink != nil {
		a.uplink.Close()
	}
	return nil
}

// portConfig is the switch port a machine's NIC is cabled to, with the stack's site services and
// the routed site network, if the stack has one (D48), so NICo sees the machine's own address as
// the source of what it sends. plug is what answers its DHCP.
func (a *Adapter) portConfig(ref string, plug func() (netsim.PortRelay, error)) netsim.HostPortConfig {
	return netsim.HostPortConfig{Ref: ref, Services: a.rt.Net.Services(), Routes: a.rt.Net.Routes, Log: a.rt.Log,
		Uplink: a.siteUplink(), Plug: plug}
}

// siteUplink is the router's interface on the stack's routed site network: the vm adapter's
// when it runs (one link to the namespace), else this adapter's own.
func (a *Adapter) siteUplink() *netsim.Uplink {
	a.upMu.Lock()
	defer a.upMu.Unlock()
	if a.uplink != nil {
		return a.uplink
	}
	// the namespace's services reach estate endpoints through the router too (NICo's
	// nvlink-manager the switch trays' NMX-C at their NVOS leases)
	defer func() { a.uplink.SetSiteRoutes(a.rt.Net.Routes) }()
	if a.rt.Peer != nil {
		if v, ok := a.rt.Peer("vm").(interface{ SiteUplink() *netsim.Uplink }); ok {
			if u := v.SiteUplink(); u != nil {
				a.uplink = u
				return u
			}
		}
	}
	s := strings.TrimSpace(a.rt.Outputs["siteNetwork"])
	if s == "" {
		return nil
	}
	n, err := netsim.ParseSiteNetwork(s)
	if err != nil {
		a.rt.Engine.Incident(Name, "the stack's siteNetwork output: "+err.Error())
		return nil
	}
	a.uplink, a.ownLink = netsim.NewUplink(n, net.HardwareAddr{0x02, 0xdc, 0x7e, 0x00, 0x00, 0x02}, a.rt.Log), true
	return a.uplink
}

func bmcPort(d *model.Device) *model.Port {
	if p := d.PortBy["bmc0"]; p != nil {
		return p
	}
	for _, p := range d.Ports {
		if p.Role == "management" {
			return p
		}
	}
	return nil
}

// Report is the fidelity report's contribution: impersonated, with what each machine reached.
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: Name}
	if a.inactive != "" {
		r.Lines = []string{"inactive: " + a.inactive}
		return r
	}
	if len(a.dpus) > 0 {
		r.Impersonated = append(r.Impersonated, DPUAgent+" (NICo's DPU image and forge-dpu-agent, for "+fmt.Sprint(len(a.dpus))+" BlueFields at model fidelity)")
	}
	if len(a.hosts) > 0 {
		r.Impersonated = append(r.Impersonated, Scout+" (NICo's scout, for "+fmt.Sprint(len(a.hosts))+" hosts at model fidelity)")
	}
	reached := map[string]int{}
	var lines []string
	for _, p := range a.order {
		m := a.machines[p]
		m.mu.Lock()
		reached[m.kind+" "+firstOf(m.reached, "not booted")]++
		lines = append(lines, fmt.Sprintf("%s: %s", p, m.summary()))
		m.mu.Unlock()
	}
	var parts []string
	for k, v := range reached {
		parts = append(parts, fmt.Sprintf("%d %s", v, k))
	}
	sort.Strings(parts)
	r.Lines = append([]string{"reached: " + strings.Join(parts, ", ")}, lines...)
	if a.uplink == nil {
		r.Lines = append(r.Lines, "addressing: no routed site network; the machines' TCP reaches the stack's services from this host's loopback address, so a service that identifies a client by its source address (NICo's PXE service and DiscoverMachine) does not know them")
	}
	r.IsNot = []string{
		"No NICo software runs on the machines: its DPU image, forge-dpu-agent and scout are impersonated by their calls to nico-api; no DPU OS is installed, no HBN, OVS or ConnectX configuration is applied, no disk is wiped and no validation test runs, and the network status reported is NICo's own configuration echoed back",
		"A DPU's hardware is a BlueField-3's (CPU, eMMC) with the identity its BMC serves; a host's is its BMC's identity, its NICs and GPUs from the model, and a TPM EK certificate the estate CA signs (not a TCG EK certificate); no PCI, NVMe, memory or LLDP inventory",
		"The firmware boots the network first and then its disk, whatever boot order the BMC holds",
	}
	return r
}

// machine is one machine's stand-in: what a DPU's and a host's have in common.
type machine struct {
	a         *Adapter
	dev       *model.Device
	path      string
	kind      string // dpu | host
	arch      string
	statePath string
	forge     *Forge
	nic       *nic
	mac       net.HardwareAddr
	sm        smbios
	run       func(ctx context.Context, why string)
	agentUp   atomic.Bool

	prepMu sync.Mutex

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	boots    int
	reached  string // the furthest a boot got
	last     string // the last boot's end
	lease    string
	fetches  int
	calls    map[string]int
	failures map[string]string
	syncedV  string
	notes    []string
}

// boot starts the machine's boot, ending any that runs.
func (m *machine) boot(ctx context.Context, why string) {
	m.stop()
	m.mu.Lock()
	bctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.cancel, m.done = cancel, done
	m.boots++
	m.mu.Unlock()
	m.a.wg.Add(1)
	go func() {
		defer m.a.wg.Done()
		defer close(done)
		m.run(bctx, why)
	}()
}

func (m *machine) stop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (m *machine) closeNIC() {
	if m.nic != nil {
		m.nic.close()
	}
}

// netbootRetrying is the firmware's network boot, tried again after each failure (an exit or a
// boot is an answer, and ends it) until the boot ends.
func (m *machine) netbootRetrying(ctx context.Context) bootOutcome {
	for {
		out := m.netboot(ctx, m.step)
		if ctx.Err() != nil || out.kernel != "" || out.exited {
			return out
		}
		m.note("network boot: " + out.failed + "; the firmware tries again")
		select {
		case <-ctx.Done():
			return out
		case <-time.After(m.a.bootRetry):
		}
	}
}

// retry runs f until it succeeds or the boot ends, as the machine's software retries nico-api.
func (m *machine) retry(ctx context.Context, f func() error) {
	for {
		err := f()
		if err == nil || ctx.Err() != nil {
			return
		}
		m.note(err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.a.discoveryRetry):
		}
	}
}

func (m *machine) step(s string) {
	m.mu.Lock()
	if rank(s) > rank(m.reached) {
		m.reached = s
	}
	m.mu.Unlock()
	m.a.rt.Engine.SetBooted(m.path, s, src)
}

func rank(s string) int {
	return map[string]int{stepDHCP: 1, stepHTTP: 2, stepIPXEDHCP: 3, stepScript: 4, stepBooted: 5, stepOS: 6, "discovered": 7, "synced": 8}[s]
}

// reach records a milestone past the boot (discovered, synced) without changing boot progress.
func (m *machine) reach(s string) {
	m.mu.Lock()
	if rank(s) > rank(m.reached) {
		m.reached = s
	}
	m.mu.Unlock()
}

func (m *machine) end(s string) {
	m.mu.Lock()
	m.last = s
	m.mu.Unlock()
}

func (m *machine) note(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.notes) == 0 || m.notes[len(m.notes)-1] != s {
		m.notes = append(m.notes, s)
		if len(m.notes) > 20 {
			m.notes = m.notes[1:]
		}
	}
	m.a.rt.Engine.Commit(schema.Event{Kind: "observe", Source: src, Path: m.path, Message: "NICo " + m.kind + " stand-in: " + s})
}

func (m *machine) leased(l *lease) {
	m.mu.Lock()
	changed := m.lease != l.addr.Addr().String()
	m.lease = l.addr.Addr().String()
	m.mu.Unlock()
	if changed {
		field := "oob.lease"
		if m.kind == "host" {
			field = "host.lease"
		}
		m.a.rt.Engine.Commit(schema.Event{Kind: "observe", Source: src, Path: m.path, State: &schema.EventState{Field: field, To: l.addr.Addr().String()},
			Message: fmt.Sprintf("the %s's NIC (%s) was leased %s by %s", m.kind, m.mac, l.addr.Addr(), l.server),
			Data:    map[string]any{"ip": l.addr.Addr().String(), "server": l.server, "mac": m.mac.String()}})
	}
}

func (m *machine) fetched(string, int64) {
	m.mu.Lock()
	m.fetches++
	m.mu.Unlock()
}

func (m *machine) called(method string) {
	m.mu.Lock()
	m.calls[method]++
	delete(m.failures, method)
	m.mu.Unlock()
}

func (m *machine) callFailed(method string, err error) {
	m.mu.Lock()
	m.failures[method] = grpcReason(err)
	m.mu.Unlock()
}

// synced records that the agent reported a network configuration version applied.
func (m *machine) synced(version string) {
	m.mu.Lock()
	first := m.syncedV != version
	m.syncedV = version
	m.mu.Unlock()
	m.reach("synced")
	if first {
		m.a.rt.Engine.Commit(schema.Event{Kind: "observe", Source: src, Path: m.path,
			Message: "NICo dpu stand-in: reported network configuration " + version + " applied, DPU healthy"})
	}
}

// summary is the machine's line in the report (m.mu held).
func (m *machine) summary() string {
	s := fmt.Sprintf("%d boot(s), last: %s", m.boots, firstOf(m.last, "none"))
	if m.lease != "" {
		s += "; address " + m.lease
	}
	if len(m.calls) > 0 {
		var cs []string
		for k, v := range m.calls {
			cs = append(cs, fmt.Sprintf("%s %d", k, v))
		}
		sort.Strings(cs)
		s += "; calls: " + strings.Join(cs, ", ")
	}
	if len(m.failures) > 0 {
		var fs []string
		for k, v := range m.failures {
			fs = append(fs, k+": "+v)
		}
		sort.Strings(fs)
		s += "; failing: " + strings.Join(fs, "; ")
	}
	if n := len(m.notes); n > 0 {
		s += "; last note: " + m.notes[n-1]
	}
	return s
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func baseName(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}
