// Package nmxc is the NMX-C adapter (docs/spec/05-interfaces.md — Fabric managers): the NVLink
// controller's gRPC API (`nmx_c.NMX_Controller`) for every NVLink scale-up domain whose racks
// hold NVLink switch trays, served on each switch tray of the domain over TLS on port 9370 at the
// tray's site address, as NICo reaches it (crates/nvlink-manager: https://<nvos-ip>:9370).
//
// The interface definition is NVIDIA's proprietary nmx_c.proto. It is not in this repository and
// nothing generated from it is (D13): `dctwin assets fetch` obtains it from NICo at a pinned
// commit (packs/nvidia/pack.yaml), the adapter compiles it at start into descriptors
// (bufbuild/protocompile) and serves the service from them with dynamic messages, and without it
// the adapter refuses to start and says how to obtain it.
//
// Its scope is what NICo calls (D25): Hello, the GFM SDK reads (domain properties and state,
// compute nodes, GPUs, switch nodes and switches, connections, topology, state reports),
// partitions (count, ids, info, create, delete, add and remove GPUs), FactoryReset, and
// Subscribe. Everything the model has comes from the model: compute nodes are the domain's
// compute trays, GPUs its member components with a fabric GUID that is identity/v1 material,
// switch nodes its switch trays and their NVLink switch ASICs, connections the model's NVLink
// lanes with the state engine.PortUp gives both ends, and health the engine's. Partitions are the
// controller's own state, kept here and persisted in the state directory.
//
// Subscribe is the twin's answer to Q8 (NICo's nmxc-mock returns UNIMPLEMENTED for it). The
// stream carries, after a SubscriptionResponse: HealthStateChanged when the health of a GPU,
// compute node, switch or switch node changes; an FmEvent topology change when an NVLink lane or
// a tray's power changes; an FmEvent control-plane state change when the domain forms, degrades
// or unforms; an FmEvent partition change when a partition's health changes; the
// Create/Delete/UpdatePartitionResponse of another client's partition change (and the
// requester's own with notify_on_self_change); and a DomainStateInfo heartbeat every
// heart_beat_rate seconds when one is asked for. Changes are observed from the engine's committed
// events and coalesced over 100 ms, one notification per kind per window.
package nmxc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Name is the adapter's name.
const Name = "nmxc"

// Protocol is the protocol name requests are logged under.
const Protocol = "nmx-c"

// Port is NMX-C's gRPC port on a switch tray (NICo's NMX_C_DEFAULT_GRPC_PORT).
const Port = 9370

// EndpointsFile is where the adapter writes its endpoints in the state directory.
const EndpointsFile = "nmxc-endpoints.json"

// Endpoint is one switch tray's controller, as a client reaches it.
type Endpoint struct {
	Domain     string `json:"domain"`
	DomainUUID string `json:"domainUuid"`
	Device     string `json:"device"`
	URL        string `json:"url"`               // https://127.0.0.1:<port> on this host
	SiteURL    string `json:"siteUrl,omitempty"` // https://<tray site address>:9370, through the site gateway
	Authority  string `json:"authority"`         // a name the certificate carries
	CA         string `json:"ca"`                // the estate CA that issued it
	Manager    bool   `json:"manager"`           // the domain's manager tray
}

type config struct {
	authority        string
	factoryPartition bool
}

// Adapter serves NMX-C for every NVLink domain.
type Adapter struct {
	rt      *adapter.Runtime
	ps      *Protoset
	seed    int64
	cfg     config
	mu      sync.Mutex
	domains []*domain
	servers []*trayServer
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	dirty   map[*domain]bool
	kickC   chan struct{}
}

func init() {
	twin.Register(Name, func() adapter.Adapter { return &Adapter{} })
}

func (a *Adapter) Name() string { return Name }

// domain is one NVLink domain's controller state, shared by the trays that serve it.
type domain struct {
	*shape
	a           *Adapter
	mu          sync.Mutex
	tab         *table
	file        string
	subs        map[*subscriber]bool
	last        *snapshot
	topoChanged time.Time
	app         string // the manager tray's controller instance
}

func (d *domain) uids() []uint64 {
	out := make([]uint64, 0, len(d.gpus))
	for _, g := range d.gpus {
		out = append(out, g.uid)
	}
	return out
}

func (d *domain) saveLocked() error { return d.tab.save(d.file) }

func (d *domain) observe() *snapshot { return d.shape.observe(d.a.rt.Engine) }

// trayServer is one switch tray's NMX-C.
type trayServer struct {
	a       *Adapter
	d       *domain
	dev     *model.Device
	appUUID string
	addr    string
	gs      *grpc.Server
	gate    *gate
	mu      sync.Mutex
	siteIP  string
	certs   map[string]*tls.Certificate
	// NMX-C runs on NVOS, so it also answers at NVOS's address: the lease of the tray's NVOS
	// management port (D46), which NICo's nvlink-manager dials. The BMC's address stays routed
	// for clients configured with it.
	nvosPort, nvosIP string
}

// Start compiles the interface definition and serves every NVLink domain's controller.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	a.dirty = map[*domain]bool{}
	a.kickC = make(chan struct{}, 1)
	a.cfg = config{authority: rt.Outputs["nmxc.authority"], factoryPartition: !strings.EqualFold(rt.Outputs["nmxc.defaultPartition"], "false")}
	var doms []*model.Domain
	rt.Engine.Read(func(m *model.Model) {
		a.seed = m.Seed
		doms = nvl.ScaleUpDomains(m)
	})
	if len(doms) == 0 {
		return nil
	}
	ps, err := LoadProto(rt.Reg)
	if err != nil {
		return err
	}
	a.ps = ps
	ctx, a.cancel = context.WithCancel(ctx)
	ids := identity.New(a.seed)
	for _, dom := range doms {
		var sh *shape
		rt.Engine.Read(func(m *model.Model) { sh = newShape(m, a.seed, dom) })
		d := &domain{shape: sh, a: a, subs: map[*subscriber]bool{}, topoChanged: rt.Engine.Now(), app: ids.UUID(dom.Manager, "nmx-c", 0)}
		if rt.StateDir != "" {
			d.file = filepath.Join(rt.StateDir, "nmxc-"+strings.ReplaceAll(dom.Path, "/", "_")+".json")
		}
		if d.tab, err = loadTable(d.file, d.uids(), a.cfg.factoryPartition); err != nil {
			a.Stop()
			return fmt.Errorf("nmxc %s: %w", dom.Path, err)
		}
		_ = d.saveLocked()
		d.last = d.observe()
		a.domains = append(a.domains, d)
		for _, tray := range sh.trays {
			s := &trayServer{a: a, d: d, dev: tray, certs: map[string]*tls.Certificate{},
				appUUID: ids.UUID(tray.Path, "nmx-c", 0)}
			if err := s.start(); err != nil {
				a.Stop()
				return err
			}
			a.servers = append(a.servers, s)
		}
	}
	a.wg.Add(1)
	go func() { defer a.wg.Done(); a.watch(ctx) }()
	if err := a.writeEndpoints(); err != nil {
		rt.Log.Warn("nmxc: endpoints file not written", "err", err)
	}
	return nil
}

func (s *trayServer) start() error {
	a := s.a
	ln, err := a.rt.Net.Listen(s.dev.Path, "nmx-c")
	if err != nil {
		return err
	}
	s.addr = ln.Addr().String()
	s.gate = &gate{Listener: ln, open: s.powered, conns: map[*trackedConn]bool{}}
	a.rt.Engine.Read(func(*model.Model) { s.siteIP = s.dev.Endpoint.SiteIP })
	s.route("", s.siteIP)
	if ps := twin.NVOSPorts(s.dev); len(ps) > 0 {
		s.nvosPort = ps[0].Ref
		for _, ev := range a.rt.Engine.Events(0, func(e schema.Event) bool {
			return e.State != nil && e.State.Field == "lease" && e.Path == s.nvosPort
		}) {
			s.setNVOS(fmt.Sprint(ev.State.To))
		}
	}
	a.rt.Net.Route(hostname(s.dev)+fmt.Sprintf(":%d", Port), s.addr)
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.certificate})
	s.gs = grpc.NewServer(grpc.Creds(creds),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}))
	s.gs.RegisterService(s.serviceDesc(), s)
	// Server reflection, from the same compiled definition, for tooling (grpcurl).
	registerReflection(s.gs, reflection.ServerOptions{Services: s.gs, DescriptorResolver: a.ps.Files})
	go s.gs.Serve(s.gate)
	return nil
}

// powered reports whether the tray runs NVOS: NMX-C runs on the tray's CPU, so with the tray off
// nothing answers on its address.
func (s *trayServer) powered() bool {
	on := false
	s.a.rt.Engine.Read(func(*model.Model) { on = s.dev.State.AC && s.dev.State.Power == model.On })
	return on
}

func (s *trayServer) nvos() string {
	v := ""
	s.a.rt.Engine.Read(func(*model.Model) {
		if fw := s.dev.State.Firmware["nvos"]; fw != nil {
			v = fw.Version
		}
	})
	return v
}

// certificate issues (once per site address) a certificate from the estate CA naming the tray's
// NVOS host name, its site address and the configured authority.
func (s *trayServer) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	// one certificate names both addresses NMX-C answers at (a client dialling an IP literal sends
	// no server name to choose by): the BMC's and NVOS's
	s.mu.Lock()
	var ips []string
	for _, ip := range []string{s.siteIP, s.nvosIP} {
		if ip != "" {
			ips = append(ips, ip)
		}
	}
	key := strings.Join(ips, ",")
	c := s.certs[key]
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	names := []string{hostname(s.dev), "nmx-c", "localhost"}
	if s.a.cfg.authority != "" {
		names = append(names, s.a.cfg.authority)
	}
	cert, err := s.a.rt.Net.Certificate(hostname(s.dev), names, ips)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.certs[key] = &cert
	s.mu.Unlock()
	return &cert, nil
}

func (s *trayServer) route(old, ip string) {
	if old != "" {
		s.a.rt.Net.Unroute(fmt.Sprintf("%s:%d", old, Port))
	}
	if ip != "" {
		s.a.rt.Net.Route(fmt.Sprintf("%s:%d", ip, Port), s.addr)
	}
}

func (s *trayServer) setSite(ip string) {
	s.mu.Lock()
	old := s.siteIP
	s.siteIP = ip
	s.mu.Unlock()
	if old != ip {
		s.route(old, ip)
		_ = s.a.writeEndpoints()
	}
}

// setNVOS routes NVOS's address (its management port's lease) to this tray's NMX-C.
func (s *trayServer) setNVOS(ip string) {
	s.mu.Lock()
	old := s.nvosIP
	s.nvosIP = ip
	s.mu.Unlock()
	if old == ip {
		return
	}
	if old != "" {
		s.a.rt.Net.Unroute(fmt.Sprintf("%s:%d", old, Port))
	}
	if ip != "" {
		s.a.rt.Net.Route(fmt.Sprintf("%s:%d", ip, Port), s.addr)
	}
}

// hostname is NVOS's, as the NVUE adapter spells it.
func hostname(d *model.Device) string { return "nvos-" + strings.ToLower(d.Identity.Serial) }

// Stop closes every endpoint, ends every subscription and persists partitions.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	for _, s := range a.servers {
		s.gs.Stop()
		s.mu.Lock()
		if s.siteIP != "" {
			a.rt.Net.Unroute(fmt.Sprintf("%s:%d", s.siteIP, Port))
		}
		if s.nvosIP != "" {
			a.rt.Net.Unroute(fmt.Sprintf("%s:%d", s.nvosIP, Port))
		}
		s.mu.Unlock()
		a.rt.Net.Unroute(hostname(s.dev) + fmt.Sprintf(":%d", Port))
	}
	a.wg.Wait()
	for _, d := range a.domains {
		d.mu.Lock()
		_ = d.saveLocked()
		d.mu.Unlock()
	}
	return nil
}

// Endpoints returns every tray's controller.
func (a *Adapter) Endpoints() []Endpoint {
	var out []Endpoint
	ca := ""
	if a.rt != nil && a.rt.StateDir != "" {
		ca = filepath.Join(a.rt.StateDir, "estate-ca.pem")
	}
	for _, s := range a.servers {
		s.mu.Lock()
		e := Endpoint{Domain: s.d.dom.Path, DomainUUID: s.d.uuid, Device: s.dev.Path, URL: "https://" + s.addr, Authority: hostname(s.dev), CA: ca,
			Manager: s.dev.Path == s.d.dom.Manager}
		if s.siteIP != "" {
			e.SiteURL = fmt.Sprintf("https://%s:%d", s.siteIP, Port)
		}
		s.mu.Unlock()
		out = append(out, e)
	}
	return out
}

func (a *Adapter) writeEndpoints() error {
	if a.rt == nil || a.rt.StateDir == "" {
		return nil
	}
	b, _ := json.MarshalIndent(a.Endpoints(), "", "  ")
	return os.WriteFile(filepath.Join(a.rt.StateDir, EndpointsFile), b, 0o644)
}

// ReadEndpoints reads the endpoints a running twin's NMX-C adapter wrote to its state directory.
func ReadEndpoints(stateDir string) ([]Endpoint, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, EndpointsFile))
	if err != nil {
		return nil, err
	}
	var out []Endpoint
	return out, json.Unmarshal(b, &out)
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: Name, Served: len(a.servers)}
	for _, d := range a.domains {
		d.mu.Lock()
		parts := len(d.tab.Partitions)
		subs := len(d.subs)
		d.mu.Unlock()
		r.Lines = append(r.Lines, fmt.Sprintf("%s (domain %s): NMX-C on %d switch trays at :%d with TLS, %d compute nodes, %d GPUs, %d switch nodes, %d NVLink switches, %d lanes, %d partitions, %d subscribers",
			d.dom.Path, d.uuid, len(d.trays), Port, len(d.nodes), len(d.gpus), len(d.sws), len(d.asics), len(d.conns), parts, subs))
	}
	if a.ps != nil {
		r.Lines = append(r.Lines,
			fmt.Sprintf("interface definition: %s obtained by `dctwin assets fetch` (asset %s, NVIDIA proprietary, not redistributed), compiled at start; RPCs served: %s; the others answer UNIMPLEMENTED", ProtoFile, Asset, strings.Join(Served(), ", ")),
			"Subscribe (Q8): SubscriptionResponse, then HealthStateChanged on GPU/compute node/switch/switch node health changes, FmEvent topology change on NVLink lane or tray power changes, FmEvent control-plane state change on domain formed/degraded/unformed, FmEvent partition change on partition health changes, other clients' partition responses, and DomainStateInfo every heart_beat_rate seconds; coalesced over 100 ms",
			"placement: chassis = the NVL rack (identity/v1 serial, chassis_id its index in the domain), slot_id = the tray's rack unit, tray_index = its index among its kind, host_id 1 on compute trays; GPU ids are module ids from 1, switch ids from 0, port numbers from 0 in model lane order; fabric GUIDs (gpu_uid, switch_uid) are identity/v1 material under NVIDIA's OUI",
			"health: a GPU is NO_NVLINK when its tray is unpowered, it is Critical or offline (Xid 79), or no lane is up; DEGRADED when some lanes are down; a switch is UNHEALTHY when its tray is unpowered or Critical (switch-tray failure), MISSING_NVLINK when a lane is down; domain formed/degraded/unformed is control plane CONFIGURED/DEGRADED/ERROR")
	}
	r.IsNot = []string{
		"No NMX-C runs: no subnet manager, global fabric manager or FIB; no routing, ALIDs or multicast groups are computed, and a partition enforces nothing because NVLink has no data path in the twin.",
		"Every switch tray of a domain answers for it; a real rack runs the controller on the fabric-manager primary RMS elects.",
		"NMX-C is reached at the switch tray's leased site address (the BMC's lease); a real tray's NVOS has a management address of its own.",
		"Subscribe carries change notifications and heartbeats, not counters or telemetry samples; InitDone is not sent; notifications are not retained for a client that reconnects.",
		"GetStaticConfig, SetStaticConfig, GetAdminState and SetAdminState answer UNIMPLEMENTED; resiliency mode and multicast limits are recorded, not applied.",
		"Domain limits the model has no source for are fixed (1024 multicast groups, as NVOS reports its default partition's limit) or 0 (ALIDs, line rate); max partitions is the GPU count.",
		"No client-certificate (mTLS) authentication: RMS's ConfigureSwitchCertificate is not enforced here.",
		"GPU fabric GUIDs are reported by no other twin surface yet (a host's NVML would report them at vm fidelity).",
		"gRPC server reflection is served from the obtained definition for tooling; a real controller may not serve it.",
	}
	return r
}

// ---- the gate: a tray that is not powered accepts no connection and drops the ones it has

type gate struct {
	net.Listener
	open  func() bool
	mu    sync.Mutex
	conns map[*trackedConn]bool
}

type trackedConn struct {
	net.Conn
	g    *gate
	once sync.Once
}

func (g *gate) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !g.open() {
			c.Close()
			continue
		}
		tc := &trackedConn{Conn: c, g: g}
		g.mu.Lock()
		g.conns[tc] = true
		g.mu.Unlock()
		return tc, nil
	}
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.g.mu.Lock()
		delete(c.g.conns, c)
		c.g.mu.Unlock()
	})
	return c.Conn.Close()
}

// drop closes every connection (the tray lost power).
func (g *gate) drop() {
	g.mu.Lock()
	var cs []*trackedConn
	for c := range g.conns {
		cs = append(cs, c)
	}
	g.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}

// ---- the watcher: engine events -> tray power, site routes, and notifications

func (a *Adapter) kick(d *domain) {
	a.mu.Lock()
	if a.dirty != nil {
		a.dirty[d] = true
	}
	a.mu.Unlock()
	select {
	case a.kickC <- struct{}{}:
	default:
	}
}

func (a *Adapter) watch(ctx context.Context) {
	e := a.rt.Engine
	trays := map[string]*trayServer{}
	nvos := map[string]*trayServer{}
	for _, s := range a.servers {
		trays[s.dev.Path] = s
		if s.nvosPort != "" {
			nvos[s.nvosPort] = s
		}
	}
	var timer *time.Timer
	var fire <-chan time.Time
	for ctx.Err() == nil {
		id, ch := e.Subscribe(4096)
		// anything committed before subscribing is picked up by observing every domain now, and
		// by reading NVOS's leases again
		for _, d := range a.domains {
			a.kick(d)
		}
		for _, ev := range e.Events(0, func(ev schema.Event) bool {
			return ev.State != nil && ev.State.Field == "lease" && nvos[ev.Path] != nil
		}) {
			nvos[ev.Path].setNVOS(fmt.Sprint(ev.State.To))
		}
	loop:
		for {
			select {
			case <-ctx.Done():
				e.Unsubscribe(id)
				return
			case <-a.kickC:
				if fire == nil {
					timer = time.NewTimer(100 * time.Millisecond)
					fire = timer.C
				}
			case <-fire:
				fire = nil
				a.flush(trays)
			case ev, ok := <-ch:
				if !ok {
					break loop // dropped for falling behind: resubscribe
				}
				if ev.Kind == "request" {
					continue
				}
				if s := nvos[ev.Path]; s != nil && ev.State != nil && ev.State.Field == "lease" {
					s.setNVOS(fmt.Sprint(ev.State.To))
				}
				if s := trays[ev.Path]; s != nil && ev.State != nil {
					switch ev.State.Field {
					case "lease":
						s.setSite(fmt.Sprint(ev.State.To))
					case "power", "ac":
						if !s.powered() {
							s.gate.drop()
						}
					}
				}
				for _, d := range a.domains {
					if d.touches(ev.Path) {
						a.kick(d)
					}
				}
			}
		}
	}
	if timer != nil {
		timer.Stop()
	}
}

// flush observes every dirty domain and notifies its subscribers of what changed.
func (a *Adapter) flush(trays map[string]*trayServer) {
	a.mu.Lock()
	dirty := a.dirty
	a.dirty = map[*domain]bool{}
	a.mu.Unlock()
	for _, s := range trays {
		if !s.powered() {
			s.gate.drop()
		}
	}
	var ds []*domain
	for d := range dirty {
		ds = append(ds, d)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].dom.Path < ds[j].dom.Path })
	for _, d := range ds {
		d.changed(d.observe())
	}
}
