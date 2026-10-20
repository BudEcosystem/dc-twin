// Package metrics is the in-host metrics exporter projection at `model` fidelity: for every
// model host whose profile names an exporter (`metrics.exporter: dcgm | amd | gaudi`), an HTTP
// /metrics endpoint in the Prometheus text format with the real exporter's metric names, label
// names and label order (docs/spec/06-packs-and-stacks.md — The same slots, every vendor: the
// Metrics row).
//
// This is IMPERSONATION (D24, 07 — Impersonation): at `model` no host runs, so DCGM-exporter,
// the AMD Device Metrics Exporter and the Gaudi metric exporter cannot run; the twin answers in
// their place. Every value is the model's and nothing is measured.
package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Name is the adapter's registered name.
const Name = "metrics"

func init() {
	twin.Register(Name, func() adapter.Adapter { return New() })
}

// Adapter serves one exporter endpoint per host.
type Adapter struct {
	rt        *adapter.Runtime
	endpoints []*Endpoint
	servers   []*http.Server
	routes    []string
	unknown   []string
	mu        sync.Mutex
	scrapes   int
}

// Endpoint is one host's exporter.
type Endpoint struct {
	Device   *model.Device
	Exporter *Exporter
	Node     string // the hostname label: the host's Node name in the kwok projection
	Local    string // host:port on this machine
	Site     string // the site name the gateway routes: <node>:<the exporter's default port>

	tempSensor string
	idleFrac   float64
	accels     []*accelID
}

// accelID is an accelerator's fixed identity, derived once (identity/v1 material for the
// component's path, so it is stable across up/down).
type accelID struct {
	c      *model.Component
	index  int
	uuid   string
	serial string
	memMiB float64 // 0 when the profile does not say
}

// New returns an unstarted adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return Name }

// NodeName must equal kwok.NodeName (a test holds them together): "<rack>-<device>" as a
// DNS-1123 name. dcgm-exporter's hostname label is the node name in Kubernetes (NODE_NAME).
func NodeName(d *model.Device) string {
	n := d.Name
	if r := d.RackOf(); r != nil {
		n = r.Name + "-" + d.Name
	}
	n = notDNS.ReplaceAllString(strings.ToLower(n), "-")
	n = strings.Trim(n, "-.")
	if len(n) > 253 {
		n = strings.Trim(n[:253], "-.")
	}
	return n
}

var notDNS = regexp.MustCompile(`[^a-z0-9.-]+`)

// Start opens an endpoint per model host whose profile names a known exporter.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	var seed int64
	var hosts []*model.Device
	rt.Engine.Read(func(m *model.Model) {
		seed = m.Seed
		for _, d := range m.Devices {
			if d.IsHost() && d.Fidelity == "model" && d.Profile.Spec.Metrics != nil {
				hosts = append(hosts, d)
			}
		}
	})
	id := identity.New(seed)
	unknown := map[string]bool{}
	for _, d := range hosts {
		x := Exporters[d.Profile.Spec.Metrics.Exporter]
		if x == nil {
			key := fmt.Sprintf("profile %s names exporter %q, which the twin does not project; not served", d.ProfileName, d.Profile.Spec.Metrics.Exporter)
			if !unknown[key] {
				unknown[key] = true
				a.unknown = append(a.unknown, key)
			}
			continue
		}
		ep := &Endpoint{Device: d, Exporter: x, Node: NodeName(d)}
		for _, s := range d.Profile.Spec.Sensors {
			if s.Source == "accelerator-max" {
				ep.tempSensor = s.Name
			}
		}
		if p := d.Profile.Spec.Power; p.MaxW > 0 {
			ep.idleFrac = p.IdleW / p.MaxW
		}
		for _, c := range d.Components {
			if c.Kind != "accelerator" {
				continue
			}
			ai := &accelID{c: c, index: c.Index,
				uuid:   id.UUID(c.Path, d.Identity.ProfileMajor, c.Index),
				serial: serialOf(id, d, x.serialFormat, c)}
			ai.memMiB = c.MemoryMiB
			ep.accels = append(ep.accels, ai)
		}
		l, err := rt.Net.Listen(d.Path, "metrics")
		if err != nil {
			a.Stop()
			return fmt.Errorf("metrics: %s: %w", d.Path, err)
		}
		ep.Local = l.Addr().String()
		ep.Site = fmt.Sprintf("%s:%d", ep.Node, x.Port)
		rt.Net.Route(ep.Site, ep.Local)
		a.routes = append(a.routes, ep.Site)
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", a.handler(ep))
		srv := &http.Server{Handler: mux}
		go srv.Serve(l)
		a.servers = append(a.servers, srv)
		a.endpoints = append(a.endpoints, ep)
	}
	rt.Log.Info("metrics: exporter endpoints up", "endpoints", len(a.endpoints))
	return nil
}

// Endpoints lists the served endpoints, in model path order.
func (a *Adapter) Endpoints() []*Endpoint { return append([]*Endpoint{}, a.endpoints...) }

// Stop closes every endpoint.
func (a *Adapter) Stop() error {
	for _, s := range a.servers {
		_ = s.Close()
	}
	if a.rt != nil {
		for _, r := range a.routes {
			a.rt.Net.Unroute(r)
		}
	}
	a.servers, a.routes = nil, nil
	return nil
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: Name, Served: len(a.endpoints)}
	count := map[string]int{}
	for _, ep := range a.endpoints {
		count[ep.Exporter.ID]++
	}
	ids := make([]string, 0, len(count))
	for k := range count {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	for _, k := range ids {
		x := Exporters[k]
		r.Impersonated = append(r.Impersonated, x.Impersonator)
		r.Lines = append(r.Lines, fmt.Sprintf("metrics: %d %s endpoints (names and labels of %s %s); site names <node>:%d through the gateway",
			count[k], x.Impersonator, x.Upstream, x.Version, x.Port))
	}
	r.Lines = append(r.Lines, a.unknown...)
	if len(a.endpoints) == 0 {
		r.Lines = append(r.Lines, "metrics: no model host's profile names an exporter; nothing served")
		return r
	}
	r.IsNot = []string{
		"Values are the model's; nothing is measured.",
		"Per-accelerator temperature is the device's accelerator-max sensor for every accelerator: the model has no per-accelerator thermal sensor.",
		"Per-accelerator power is a share of the profile's power curve (component maxW at the device's idle fraction and utilisation); utilisation is the device's.",
		"Fields the model has no source for are not served: memory/HBM temperature, clocks, energy, PCIe, profiling, NVLink and xGMI traffic and error counters, and memory sizes the component's model string does not state.",
		"The last Xid is the newest Xid among the accelerator's active faults and returns to 0 when the fault clears; DCGM keeps it until the driver reloads.",
		"No driver or firmware stack is modelled: DCGM_FI_DRIVER_VERSION is absent, and the Gaudi exporter's driver, fit and spi labels are empty.",
		"ECC volatile counters are served although dcgm-exporter's default counters file comments them out.",
		"An accelerator the model holds offline (fallen off the bus) keeps only its error series, as DCGM's blank fields are skipped; the exporter answers only while its host is powered and booted, and otherwise drops the connection.",
		"HELP text for the Gaudi exporter is the twin's; its names and labels follow the exporter's source.",
	}
	return r
}

// handler serves one host's /metrics. A host that is not running has no exporter: the
// connection is dropped without an answer, as an unreachable host's scrape fails.
func (a *Adapter) handler(ep *Endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := ep.snapshot(a.rt.Engine)
		if !s.up {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					if tc, ok := conn.(*net.TCPConn); ok {
						_ = tc.SetLinger(0)
					}
					conn.Close()
					return
				}
			}
			http.Error(w, "host not running", http.StatusServiceUnavailable)
			return
		}
		a.mu.Lock()
		a.scrapes++
		a.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := ep.Exporter.render(w, s); err != nil {
			a.rt.Log.Warn("metrics: render", "device", ep.Device.Path, "err", err)
		}
	}
}

// ---------------------------------------------------------------- the model, read

// snapshot is one scrape's view of a host.
type snapshot struct {
	ep      *Endpoint
	up      bool
	util    float64
	temp    float64
	hasTemp bool
	warn    *float64
	crit    *float64
	accels  []accelSnap
}

type accelSnap struct {
	id       *accelID
	health   string
	state    string
	offline  bool
	watts    float64
	hasWatts bool
	counters map[string]float64
	xid      int
	ports    []portSnap
}

type portSnap struct {
	kind string
	up   bool
}

var xidRe = regexp.MustCompile(`(?i)\bxid[- ]?(\d+)\b`)

// xidOf is the Xid a fault stands for, from its catalogue name (nvidia/xid/xid-79) or its title
// ("... (Xid 74 on the GPU side)"); 0 when it is not an Xid.
func xidOf(f engine.FaultView) int {
	for _, s := range []string{f.Fault[strings.LastIndexByte(f.Fault, '/')+1:], f.Title} {
		if m := xidRe.FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

func (ep *Endpoint) snapshot(e *engine.Engine) *snapshot {
	d := ep.Device
	s := &snapshot{ep: ep}
	var power, booted string
	var ac bool
	counters := make([]map[string]float64, len(ep.accels))
	e.Read(func(*model.Model) {
		power, booted, ac, s.util = d.State.Power, d.State.Booted, d.State.AC, d.State.Utilisation
		for i, ai := range ep.accels {
			cp := make(map[string]float64, len(ai.c.State.Counters))
			for k, v := range ai.c.State.Counters {
				cp[k] = v
			}
			counters[i] = cp
		}
	})
	s.up = ac && power == model.On && booted == "os"
	if !s.up {
		return s
	}
	if ep.tempSensor != "" {
		if v, _, ok := e.Sensor(d, ep.tempSensor); ok {
			s.temp, s.hasTemp = v, true
		}
		for _, sp := range d.Profile.Spec.Sensors {
			if sp.Name == ep.tempSensor {
				s.warn, s.crit = sp.Warn, sp.Crit
			}
		}
	}
	faults := e.Faults(d.Path)
	for i, ai := range ep.accels {
		h, st := e.ComponentHealth(ai.c)
		as := accelSnap{id: ai, health: h, state: st, offline: st == "UnavailableOffline", counters: counters[i]}
		for _, f := range faults {
			if f.Target == ai.c.Path {
				if x := xidOf(f); x != 0 {
					as.xid = x // faults are listed in injection order: the last is the newest
				}
			}
		}
		if ai.c.MaxW > 0 && !as.offline {
			as.watts = ai.c.MaxW * (ep.idleFrac + (1-ep.idleFrac)*s.util)
			as.hasWatts = true
		}
		for _, p := range ai.c.Ports {
			as.ports = append(as.ports, portSnap{kind: p.Kind, up: e.PortUp(p)})
		}
		s.accels = append(s.accels, as)
	}
	return s
}

// serialOf is an accelerator's serial: its board's, which a superchip's GPUs share.
func serialOf(id *identity.Deriver, d *model.Device, format string, c *model.Component) string {
	src := model.SerialSource(c)
	return id.Serial(src.Path, d.Identity.ProfileMajor, format, src.Index)
}
