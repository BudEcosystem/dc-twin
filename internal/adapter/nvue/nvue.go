// Package nvue is the NVUE REST adapter of NVLink switch trays (docs/spec/05-interfaces.md —
// Network: NVLink switch trays): NVOS is emulated at its NVUE REST surface because no runnable
// NVOS image is public (Q11).
//
// Its scope is what NICo reads (D25), which is what NICo's bmc-mock NVUE mock serves: the
// GET-only `nv show` trees under /nvue_v1 — system (identity, version, health, reboot reason),
// platform (identity, environment: fans, temperatures, leak sensors, LEDs), interface (the NVLink
// ports and the management interface), cluster apps and SDN partitions — behind nginx's Basic
// authentication. Every value that the model has comes from the model: the hostname and serial
// from the tray's identity, the release from its nvos firmware, NVLink port state from
// engine.PortUp of both ends of each lane, health from the engine's rollup and active faults,
// readings from the tray's sensors. NVOS users are the tray BMC's accounts, so a credential
// rotation through Redfish rotates them here too.
//
// Where NVOS answers (D46). NVOS is reached at its management port's address, the lease of the
// tray's mgmt0, not at the BMC's: NICo's health collector reads NVUE at https://<NVOS IP> and its
// component manager resolves a switch's NVOS endpoint from the NVOS interface's address. The dhcp
// adapter runs NVOS's DHCP client and commits its lease as a state event on the port's path
// (field lease); this adapter follows those events and routes <lease>:443 through the site
// gateway to the tray's NVUE, and the NVOS host name nvos-<serial>:443 with it, while NVOS runs,
// that is while the tray is powered on. When the tray leaves On both routes go, so the gateway
// answers as for any address nothing holds; when it is on again they come back at the address
// NVOS held, which its DHCP client asks for again (a lease at another address moves them; NVOS's
// boot time is not modelled). Each tray's loopback endpoint (Endpoints) stays open for direct
// use and answers nothing while the tray is off, as before.
package nvue

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Protocol is the protocol name requests are logged under.
const Protocol = "nvue"

// Class is the device class served.
const Class = "switch-tray"

// Port is NVUE's port at NVOS's address (NICo's health collector's default).
const Port = 443

// Adapter serves NVUE REST on every NVLink switch tray.
type Adapter struct {
	rt      *adapter.Runtime
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	servers []*server
	byDev   map[string]*server // tray path -> server
	byPort  map[string]*server // NVOS management port ref -> server
	subID   int
}

func init() {
	twin.Register("nvue", func() adapter.Adapter { return &Adapter{} })
}

func (a *Adapter) Name() string { return "nvue" }

// server is one tray's NVOS.
type server struct {
	a    *Adapter
	dev  *model.Device
	port string // the NVOS management port's ref ("" when the profile has none)
	addr string
	srv  *http.Server

	mu         sync.Mutex
	siteIP     string // NVOS's address: its management port's lease, as committed
	routed     string // the site address routed to this server now ("" while NVOS is down)
	hostRouted bool
	certs      map[string]*tls.Certificate // by the site address they name ("" for none)
}

// Start listens on every switch tray, then follows NVOS's leases and the trays' power.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	a.byDev, a.byPort = map[string]*server{}, map[string]*server{}
	var trays []*model.Device
	rt.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if d.BaseClass == Class {
				trays = append(trays, d)
			}
		}
	})
	for _, d := range trays {
		s := &server{a: a, dev: d, certs: map[string]*tls.Certificate{}}
		if ps := twin.NVOSPorts(d); len(ps) > 0 {
			s.port = ps[0].Ref
		}
		ln, err := rt.Net.Listen(d.Path, "nvue")
		if err != nil {
			a.Stop()
			return err
		}
		s.addr = ln.Addr().String()
		rt.Engine.SetEndpoint(d.Path, "nvue", s.addr)
		// issued now so a CA problem fails the start; certificates naming a site address are
		// issued when NVOS first holds it
		if _, err := s.certificate(nil); err != nil {
			ln.Close()
			a.Stop()
			return err
		}
		s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(discard{}, "", 0)}
		tl := tls.NewListener(ln, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.certificate})
		go s.srv.Serve(tl)
		a.mu.Lock()
		a.servers = append(a.servers, s)
		a.byDev[d.Path] = s
		if s.port != "" {
			a.byPort[s.port] = s
		}
		a.mu.Unlock()
	}
	// subscribed before reading the leases already committed (the dhcp adapter may have started
	// first), so none falls between
	ctx, a.cancel = context.WithCancel(ctx)
	a.done = make(chan struct{})
	id, ch := rt.Engine.Subscribe(4096)
	a.mu.Lock()
	a.subID = id
	a.mu.Unlock()
	a.catchUp()
	go a.watch(ctx, ch)
	return nil
}

// catchUp applies every NVOS lease committed so far, in order, then routes each tray as its
// power says.
func (a *Adapter) catchUp() {
	for _, ev := range a.rt.Engine.Events(0, func(e schema.Event) bool {
		return e.State != nil && e.State.Field == "lease" && a.byPort[e.Path] != nil
	}) {
		to, _ := ev.State.To.(string)
		a.byPort[ev.Path].setSite(to)
	}
	for _, s := range a.servers {
		s.sync()
	}
}

// watch follows NVOS's leases and its tray's power. A subscription dropped for falling behind
// the log is renewed and the leases read again.
func (a *Adapter) watch(ctx context.Context, ch <-chan schema.Event) {
	defer close(a.done)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				id, c := a.rt.Engine.Subscribe(4096)
				a.mu.Lock()
				a.subID = id
				a.mu.Unlock()
				ch = c
				a.catchUp()
				continue
			}
			if ev.Kind == "request" || ev.State == nil {
				continue
			}
			switch ev.State.Field {
			case "lease":
				if s := a.byPort[ev.Path]; s != nil {
					to, _ := ev.State.To.(string)
					s.setSite(to)
				}
			case "power", "ac":
				if s := a.byDev[ev.Path]; s != nil {
					s.sync()
				}
			}
		}
	}
}

// powered reports whether NVOS runs: it runs on the tray's CPU, so only while the tray is on.
func (s *server) powered() bool {
	on := false
	s.a.rt.Engine.Read(func(*model.Model) { on = s.dev.State.AC && s.dev.State.Power == model.On })
	return on
}

// setSite records NVOS's address and routes it if NVOS runs.
func (s *server) setSite(ip string) {
	s.mu.Lock()
	s.siteIP = ip
	s.mu.Unlock()
	s.sync()
}

// sync routes NVOS's address and host name to this server while NVOS runs, and unroutes them
// while it does not.
func (s *server) sync() {
	up := s.powered()
	nw := s.a.rt.Net
	host := fmt.Sprintf("%s:%d", hostname(s.dev), Port)
	s.mu.Lock()
	defer s.mu.Unlock()
	want := ""
	if up {
		want = s.siteIP
	}
	if want != s.routed {
		if s.routed != "" {
			nw.Unroute(fmt.Sprintf("%s:%d", s.routed, Port))
		}
		if want != "" {
			nw.Route(fmt.Sprintf("%s:%d", want, Port), s.addr)
		}
		s.routed = want
	}
	if up != s.hostRouted {
		if up {
			nw.Route(host, s.addr)
		} else {
			nw.Unroute(host)
		}
		s.hostRouted = up
	}
}

// certificate issues (once per site address) a certificate from the estate CA naming NVOS's
// host name and the address NVOS holds.
func (s *server) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	ip := s.siteIP
	c := s.certs[ip]
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	var ips []string
	if ip != "" {
		ips = []string{ip}
	}
	cert, err := s.a.rt.Net.Certificate(hostname(s.dev), []string{hostname(s.dev), "localhost"}, ips)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.certs[ip] = &cert
	s.mu.Unlock()
	return &cert, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Stop closes every endpoint and takes NVOS's routes down.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
		<-a.done
		a.mu.Lock()
		id := a.subID
		a.mu.Unlock()
		a.rt.Engine.Unsubscribe(id)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.servers {
		_ = s.srv.Close()
		s.mu.Lock()
		if s.routed != "" {
			a.rt.Net.Unroute(fmt.Sprintf("%s:%d", s.routed, Port))
			s.routed = ""
		}
		if s.hostRouted {
			a.rt.Net.Unroute(fmt.Sprintf("%s:%d", hostname(s.dev), Port))
			s.hostRouted = false
		}
		s.mu.Unlock()
	}
	return nil
}

// Endpoints returns each tray's NVUE endpoint (host:port on this host), by device path.
func (a *Adapter) Endpoints() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for _, s := range a.servers {
		out[s.dev.Path] = s.addr
	}
	return out
}

// SiteAddresses returns, by device path, the NVOS address each tray answers NVUE at through the
// site gateway now: its management port's lease while the tray is on; absent otherwise.
func (a *Adapter) SiteAddresses() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for _, s := range a.servers {
		s.mu.Lock()
		if s.routed != "" {
			out[s.dev.Path] = s.routed
		}
		s.mu.Unlock()
	}
	return out
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	a.mu.Lock()
	n := len(a.servers)
	leased, routed, noPort := 0, 0, 0
	for _, s := range a.servers {
		s.mu.Lock()
		if s.siteIP != "" {
			leased++
		}
		if s.routed != "" {
			routed++
		}
		if s.port == "" {
			noPort++
		}
		s.mu.Unlock()
	}
	a.mu.Unlock()
	r := adapter.Report{Adapter: "nvue", Served: n}
	r.Lines = []string{
		fmt.Sprintf("NVUE REST (GET /nvue_v1: %s) on %d NVLink switch trays, Basic auth with each tray's BMC accounts", strings.Join(routeNames(), ", "), n),
		"NVLink ports acp1..acpN are the tray's NVLink Switch lanes in model order: acp(72*i+l+1) is nvswitch-i-nvl-l",
		fmt.Sprintf("NVOS addresses: %d of %d trays' NVOS management ports (mgmt0) hold a lease (the dhcp adapter's NVOS client), %d answer NVUE at <lease>:%d and nvos-<serial>:%d through the site gateway now; a tray that is not on has neither route",
			leased, n, routed, Port, Port),
	}
	if noPort > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("NVOS addresses: %d trays' profiles have no NVOS management port; they answer at their loopback endpoint only", noPort))
	}
	r.IsNot = []string{
		"NVUE GET surfaces only: no configuration writes, revisions or nv config apply (405, as NICo's mock answers).",
		"No SSH/SFTP surface (switch/nvos-ssh): RMS's own drive of the tray is not served; the rms adapter applies its firmware, image, certificate and password jobs to the model directly.",
		"No NVOS image runs: NVOS users are the tray BMC's accounts; NVOS answers nothing while the tray is powered off, and its boot time after power-on is not modelled.",
		"NVOS's address is its mgmt0 lease from the dhcp adapter: without that adapter, or on a management network without DHCP, NVUE answers at its loopback endpoint and at nvos-<serial> through the gateway only.",
		"No NVLink data path or link training: an NVLink port is up when the model's lane is cabled and both ends are up.",
		"Fans are not modelled (the tray profile is liquid-cooled); temperature sensors ASIC<n> report the model's asic-max reading and LEAKAGE-<n> the tray's leak sensors; the capture's other sensors, the fnm fabric-management ports and LED timing are not served.",
		"Cluster apps and the SDN default partition are projected from the rack's scale-up domain; NMX-C's own partitions are the NMX-C adapter's, not NVUE's.",
	}
	return r
}

// hostname is NVOS's: nvos-<serial>, as NICo's mock spells it.
func hostname(d *model.Device) string { return "nvos-" + strings.ToLower(d.Identity.Serial) }
