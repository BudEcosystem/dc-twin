// Package bluefield fronts every BMC of the nvidia/bluefield dialect with what a BlueField DPU's
// BMC (OpenBMC's bmcweb on the DPU's own BMC SoC) does beyond the Redfish engine's generic
// service, for the stacks that provision DPUs through it: NVIDIA DOCA Platform Framework (DPF) in
// zero-trust mode, and NICo's DPU path. It owns the BMC's TLS identity and certificate store
// (CertificateService.GenerateCSR and ReplaceCertificate, the Manager's Truststore, client
// certificates mapped to accounts by CommonName: mutual TLS), the BlueField OEM resources whose
// writes change state (HostRshim.Set, BmcRShim, Mode.Set, SOC.ForceReset), ResetToDefaults, and
// the update paths a BlueField BMC serves itself: a BFB pushed to the DPU with
// UpdateService.SimpleUpdate (fetched from the ImageURI, its boot stream checked image by image,
// a task whose progress is the transfer), and a BMC firmware package pushed to the HttpPushUri
// (a DSP0267 PLDM package whose image names the version the BMC reports).
//
// Everything else is the Redfish engine's: the front forwards each request it does not answer to
// the redfish adapter's service for the same BMC, over TLS verified against the estate CA, and
// answers successful writes as bmcweb does (200 with a Base Success message where the engine
// answers 204). It takes the BMC's site routes (its lease and host name at :443) over from the
// redfish adapter, so a stack reaching the BMC through the site gateway meets one BMC.
//
// Why a front and not code in the redfish engine (D51): the engine serves one TLS identity per
// BMC from a listener it owns and has no hook through which a dialect can answer a request (D25
// names a DialectHandler for "OEM behaviour data cannot express"; it is not built). The front is
// that handler, outside the engine. When the engine grows the hook (a per-dialect handler consulted
// before dispatch, and a per-BMC tls.Config callback), this package's handlers move behind it and
// the route takeover and the forwarding hop go away; the fidelity report says it is a front.
//
// Nothing here changes model state except through the engine: requests are logged by canonical
// name (LogRead), firmware staging is the engine's UpdateFirmware, power and BMC resets are the
// engine's, and the BlueField-internal facts a BMC keeps on its own flash (truststore, installed
// certificate, rshim owner, the BFB transfer) are committed as state events on the device so the
// log carries their causes. Private keys are held in memory only, as a BMC holds them in its
// secure storage; a pending CSR key is dropped when the BMC reboots, as on a real BMC.
package bluefield

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Dialect is the Redfish dialect this adapter fronts.
const Dialect = "nvidia/bluefield"

func init() { twin.Register("bluefield", func() adapter.Adapter { return New() }) }

// Adapter fronts every nvidia/bluefield BMC of the estate.
type Adapter struct {
	rt      *adapter.Runtime
	eng     *engine.Engine
	fronts  map[string]*front // by device path
	order   []string
	servers []*http.Server
	subID   int
	cancel  context.CancelFunc
	// fetch is the BMCs' outbound HTTP(S) client for SimpleUpdate image URIs. It dials directly
	// from the twin, as a BMC on the management network reaches the stack's image server.
	fetch    *http.Client
	estateCA *x509.CertPool
	mu       sync.Mutex
	unrouted map[string]bool // fronts whose redfish service never appeared

	// arms are the BlueFields at vm fidelity, whose Arm is a QEMU machine (arm.go), and
	// services the stack's site services (siteServices) their segment gateway routes to.
	arms     map[string]*arm
	services []netsim.Service
	workers  sync.WaitGroup // one per Arm: carries out its requests in order
	wg       sync.WaitGroup // console observers
	stopped  chan struct{}
	stopOnce sync.Once
}

// New returns an unstarted adapter.
func New() *Adapter {
	return &Adapter{fronts: map[string]*front{}, unrouted: map[string]bool{}, arms: map[string]*arm{}, stopped: make(chan struct{}),
		fetch: &http.Client{Transport: &http.Transport{
			Proxy: nil, // a BMC has no proxy configured; the stack's image server is on its network
			// The BMC fetches the image without verifying the server: SimpleUpdate over HTTPS on
			// BlueField BMC firmware takes no CA, and whether it checks the server against its
			// Truststore is not documented. The task's messages record whether the server's chain
			// verifies against the Truststore, so a run shows it either way.
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see above
			ResponseHeaderTimeout: 30 * time.Second,
		}}}
}

func (a *Adapter) Name() string { return "bluefield" }

func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt, a.eng = rt, rt.Engine
	a.estateCA = x509.NewCertPool()
	a.estateCA.AppendCertsFromPEM(rt.Net.CAPEM)
	var devs []*model.Device
	rt.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if d.HasBMC() && d.Dialect == Dialect {
				devs = append(devs, d)
			}
		}
	})
	sort.Slice(devs, func(i, j int) bool { return devs[i].Path < devs[j].Path })
	spec, _ := rt.Reg.Dialect(Dialect)
	if spec == nil {
		return fmt.Errorf("bluefield: dialect %s is not in the registry", Dialect)
	}
	ctx, a.cancel = context.WithCancel(ctx)
	for _, d := range devs {
		f, err := newFront(a, d, spec)
		if err != nil {
			return err
		}
		l, err := rt.Net.Listen(d.Path, "bluefield")
		if err != nil {
			return err
		}
		tl := tls.NewListener(l, &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: f.tlsConfig})
		srv := &http.Server{Handler: f, ReadHeaderTimeout: 30 * time.Second}
		go srv.Serve(tl)
		f.addr = l.Addr().String()
		a.servers = append(a.servers, srv)
		a.fronts[d.Path] = f
		a.order = append(a.order, d.Path)
		rt.Engine.SetEndpoint(d.Path, "bluefield", f.addr)
	}
	if s := strings.TrimSpace(rt.Outputs["siteServices"]); s != "" {
		svcs, err := netsim.ParseServices(s)
		if err != nil {
			return fmt.Errorf("bluefield: the stack's siteServices output: %w", err)
		}
		a.services = svcs
	}
	// A BMC fetches an image from where its network routes the ImageURI's address: a stack
	// service at its site address, an estate endpoint, or (a loopback URL) this host.
	if tr, ok := a.fetch.Transport.(*http.Transport); ok {
		tr.DialContext = a.dialSite
	}
	if err := a.startArms(); err != nil {
		return err
	}
	id, ch := rt.Engine.Subscribe(4096)
	a.subID = id
	go a.watch(ctx, ch)
	go a.keepRoutes(ctx)
	return nil
}

// startArms gives every BlueField at vm fidelity its Arm (arm.go), as its power substrate.
func (a *Adapter) startArms() error {
	var initialOn []string
	for _, p := range a.order {
		f := a.fronts[p]
		if f.dev.Fidelity != "vm" || !armEligible(f.dev) {
			continue
		}
		m, err := a.newArm(f)
		if err != nil {
			a.closeArms()
			return fmt.Errorf("bluefield: %s: %w", p, err)
		}
		f.arm = m
		a.arms[p] = m
	}
	if len(a.arms) == 0 {
		return nil
	}
	// The site gateway routes stack processes to estate endpoints; a stack service at its site
	// address is reached there too, as the Arm reaches it through its segment gateway, so DPF
	// and a DPU name the DPU cluster's API (and the bfb-registry) by one address.
	for _, s := range a.services {
		if s.Proto == "tcp" {
			a.rt.Net.Route(s.Site.String(), s.Local)
		}
	}
	for _, p := range a.order {
		m := a.arms[p]
		if m == nil {
			continue
		}
		on := false
		a.eng.Read(func(*model.Model) { on = m.dev.State.Power != model.Off })
		if err := a.eng.AttachSubstrate(p, armObserver, a); err != nil {
			a.closeArms()
			return err
		}
		a.workers.Add(1)
		a.wg.Add(1)
		go m.work()
		go m.watchConsole()
		if on {
			initialOn = append(initialOn, p)
		}
	}
	for _, p := range initialOn {
		_ = a.eng.Power(engine.Request{Path: p, Canonical: "power.on", Protocol: "twin", Target: "initialState",
			Source: schema.EventSource{Class: "twin", Principal: "bluefield"}})
	}
	return nil
}

func (a *Adapter) closeArms() {
	for _, m := range a.arms {
		m.close()
	}
}

// dialSite dials an address as a host on the management network reaches it: a stack service at
// its site address, an estate endpoint at its site route, anything else directly.
func (a *Adapter) dialSite(ctx context.Context, network, addr string) (net.Conn, error) {
	for _, s := range a.services {
		if s.Proto == "tcp" && s.Site.String() == addr {
			addr = s.Local
			break
		}
	}
	if a.rt != nil {
		if l, ok := a.rt.Net.Routes()[addr]; ok {
			addr = l
		}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// keepRoutes points every site route of a fronted BMC at its front. The redfish adapter routes a
// BMC's host name when it starts and its lease whenever the lease moves; the front takes each of
// those routes over as soon as it appears, and keeps them: a route the redfish adapter re-points
// after a lease change is taken back within the period. Adapters start in the order --adapters
// names them, so the front waits for the redfish service of each BMC to exist.
func (a *Adapter) keepRoutes(ctx context.Context) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	started := time.Now()
	for {
		routes := a.rt.Net.Routes()
		for _, p := range a.order {
			f := a.fronts[p]
			up := a.rt.Net.Endpoint(p, "redfish")
			if up == "" {
				if time.Since(started) > 30*time.Second && !a.unrouted[p] {
					a.mu.Lock()
					a.unrouted[p] = true
					a.mu.Unlock()
					a.eng.Incident("bluefield", p+": no redfish service to front; enable the redfish adapter with bluefield")
				}
				continue
			}
			f.setUpstream(up)
			for site, local := range routes {
				if local == up {
					a.rt.Net.Route(site, f.addr)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watch follows the events a front acts on: its BMC rebooting (a pending CSR key is lost, as on
// a real BMC), its lease moving (the factory certificate names the new address) and the DPU
// powering off (a staged Mode.Set takes effect at a power cycle).
func (a *Adapter) watch(ctx context.Context, ch <-chan schema.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				a.eng.Incident("bluefield", "event subscription dropped: the bluefield front fell behind the log")
				return
			}
			f := a.fronts[ev.Path]
			if f == nil || ev.State == nil {
				continue
			}
			switch ev.State.Field {
			case "bmc":
				if to, _ := ev.State.To.(string); to == "Unavailable" {
					f.bmcRebooted()
				}
			case "lease":
				f.leaseMoved()
			case "power":
				if to, _ := ev.State.To.(string); to == model.Off {
					f.applyPendingMode()
				}
			}
		}
	}
}

func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.stopOnce.Do(func() { close(a.stopped) })
	a.closeArms()
	done := make(chan struct{})
	go func() { a.workers.Wait(); a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
	if a.subID != 0 {
		a.eng.Unsubscribe(a.subID)
	}
	for _, s := range a.servers {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
	}
	return nil
}

// Front returns a fronted BMC's endpoint (host:port), for tests.
func (a *Adapter) Front(path string) (string, bool) {
	f, ok := a.fronts[path]
	if !ok {
		return "", false
	}
	return f.addr, true
}

func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: "bluefield", Served: len(a.fronts)}
	r.Lines = append(r.Lines, fmt.Sprintf("bluefield: %d BlueField BMCs fronted (certificates and mutual TLS, Truststore, BlueField OEM writes, ResetToDefaults, BFB push by SimpleUpdate, BMC firmware by HttpPushUri); every other request is the redfish engine's", len(a.fronts)))
	a.mu.Lock()
	for p := range a.unrouted {
		r.Lines = append(r.Lines, "bluefield: "+p+" NOT FRONTED (no redfish service)")
	}
	a.mu.Unlock()
	for _, p := range a.order {
		f := a.fronts[p]
		for _, l := range f.reportLines() {
			r.Lines = append(r.Lines, "bluefield "+p+": "+l)
		}
		if m := a.arms[p]; m != nil {
			for _, l := range m.reportLines() {
				r.Lines = append(r.Lines, "bluefield "+p+": "+l)
			}
			r.SubstrateOwned = append(r.SubstrateOwned, "power and boot of "+p+"'s Arm observed from QEMU")
			if m.inst.Accel == "tcg" {
				r.TCG = append(r.TCG, p+"'s Arm ("+m.inst.Arch+" under TCG, -cpu "+tcgCPU+")")
			}
		}
	}
	if len(a.arms) > 0 {
		var routed []string
		for _, s := range a.services {
			if s.Proto == "tcp" {
				routed = append(routed, s.String())
			}
		}
		r.Lines = append(r.Lines, "bluefield: the site gateway also routes the stack's site services, so stack processes reach them at the site addresses the Arms reach them at: "+strings.Join(routed, ", "))
		r.IsNot = append(r.IsNot,
			"An Arm at vm is QEMU's virt machine, not a BlueField: it has no ConnectX-7 (p0/p1, representors, SFs), no rshim or tmfifo network to the host, no boot FIFO (a cpio archive carrying the bf.cfg stands in for it at installation), no IPMI towards the BMC, no eMMC and none of BlueField's ACPI tables; what the DPU OS finds missing it reports, and nothing answers in its place.",
			"An Arm at vm boots with earlycon=pl011,0x9000000 (virt's UART) ahead of GRUB's command line, which follows verbatim: a BlueField's early console address is firmware flash or a PCIe hole on virt, where the kernel's first line never ends.",
			"An Arm at vm gets its out-of-band address from the bluefield adapter on its own cable (a fixed host .9 in its BMC's segment), not from the twin relay, which serves one client per switch port; its OOB NIC is named oob_net0 by a systemd link file the twin writes into the installed root once the installer has finished.",
			"An Arm reboot is a new QEMU process booting GRUB's default entry, read from the disk; UEFI's boot manager and GRUB do not run after the installer.")
	}
	if len(a.arms) < len(a.fronts) {
		r.IsNot = append(r.IsNot, "No DPU OS runs (Q1) on a BlueField at model fidelity: a BFB pushed through the BMC is fetched and its boot stream checked image by image, and the DPU's Arm is reset into it, but no installer or DPU OS runs, so the DPU agent never starts, the DPU never joins a cluster, and DPU_OS keeps its version.")
	}
	r.IsNot = append(r.IsNot,
		"The front is a twin-side hop in front of the redfish engine's service: requests it does not answer are forwarded, so the engine logs them from the front's loopback address; on the TAP substrate a client on the BMC's own segment reaches the engine directly, without the front.",
		"Certificates are real X.509 (the BMC generates its CSR key, serves what ReplaceCertificate installs, and verifies client certificates against its Truststore); bmcweb's certificate limits other than the Truststore's ten slots are not modelled.",
		"Unsynchronised (D2): the Truststore, the installed certificate and its key, update tasks and the HostRshim, BmcRShim and Mode settings are held by the front, not the model, so they do not survive a restart of the twin and no other interface shows them; firmware versions, passwords, BMC reboots and the Arm's reset go through the engine.",
		"A BMC firmware package is read as a DSP0267 PLDM package and the version its BMC image carries is reported; its signature is not verified and nothing is flashed.",
		"At model fidelity, until a BFB is pushed, BootProgress.OemLastState is the capture's OsIsRunning, for a DPU OS the twin does not run; once the Arm is reset into a pushed BFB it is withheld, since no installer or DPU OS runs to report a state. At vm it is OsIsRunning while the Arm's DPU OS has reached its login prompt, and absent otherwise (the BMC's other states are not modelled).")
	return r
}
