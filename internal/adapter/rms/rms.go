// Package rms is the Rack Management Service adapter (docs/spec/05-interfaces.md — Fabric
// managers): NVIDIA's RMS gRPC API (`rack_manager.RackManager` and
// `rack_manager_v2.RackManagerV2`) for the estate's NVL racks, served once for the estate on the
// service network over TLS, as NICo's component manager calls it (`nico-api.rms.apiUrl`).
//
// The bindings are generated from nv-rms-client's Apache-2.0 protos (rmspb, NOTICE). The scope
// is what NICo calls (D25), which is what NICo's rms-mock serves, plus the single-node power and
// device-info reads, ListRacks and ExecuteColdReboot: inventory (placement of compute trays,
// switch trays and power shelves), power (engine.Power, logged with protocol "rms" and the
// stack as the source, so an RMS power request and a Redfish reset are the same canonical
// request), firmware objects and switch system images (engine.UpdateFirmware: staged, and
// applied as each component's applyOn says), switch certificates, the scale-up fabric manager
// (an elected primary per rack, its topology and static configuration), NVOS passwords (the
// tray's accounts, which the NVUE surface also authenticates against) and factory resets. Every
// other RPC answers UNIMPLEMENTED, as RMS does for an RPC it does not serve.
//
// Nodes are matched to the model as rms-mock matches them: by BMC MAC first, then the host
// (NVOS) MAC, the BMC address and the host address; the twin also accepts a node id that names a
// device (its path, rack/name, serial or BMC MAC). node_id and rack_id are echoed, never
// interpreted otherwise. Credentials a request carries are checked against the device's accounts.
package rms

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
	"github.com/BudEcosystem/dc-twin/internal/adapter/rms/rmspb"
	"github.com/BudEcosystem/dc-twin/internal/adapter/rms/rmspb/rmsv2pb"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Name is the adapter's name.
const Name = "rms"

// Protocol is the protocol name requests are logged under.
const Protocol = "rms"

// Port is the port RMS answers on at its site address.
const Port = 443

// ServiceFile is where the adapter writes how to reach it, in the state directory.
const ServiceFile = "rms.json"

// Output names a stack's outputs may set: the site address RMS takes on the service network,
// and the version GetVersion reports.
const (
	OutputSiteAddress = "rms.siteAddress"
	OutputVersion     = "rms.version"
)

// DefaultVersion is what GetVersion reports: the API the bindings were generated from.
const DefaultVersion = "dc-twin RMS (rack_manager API, nv-rms-client 0.0.14 at 966d7a7)"

// Service is how a stack reaches the twin's RMS.
type Service struct {
	URL       string   `json:"url"`               // https://127.0.0.1:<port> on this host
	SiteURL   string   `json:"siteUrl,omitempty"` // https://<service address>, through the site gateway
	Hostnames []string `json:"hostnames"`         // names the certificate carries
	CA        string   `json:"ca"`
	Racks     []string `json:"racks"`
}

// Adapter serves RMS for the estate.
type Adapter struct {
	rmspb.UnimplementedRackManagerServer
	rt      *adapter.Runtime
	version string
	site    string
	siteIP  string
	addr    string
	gs      *grpc.Server
	nodes   []*node
	racks   []*model.Rack
	rackIDs map[*model.Rack]string
	jobs    *tracker
	fab     *fabric
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	cert    *tls.Certificate
}

// v2 serves RackManagerV2 for the same adapter (its one RPC shares a name with a V1 RPC).
type v2 struct {
	rmsv2pb.UnimplementedRackManagerV2Server
	a *Adapter
}

func init() {
	twin.Register(Name, func() adapter.Adapter { return &Adapter{} })
}

func (a *Adapter) Name() string { return Name }

// node is a device RMS manages.
type node struct {
	dev    *model.Device
	rack   *model.Rack
	typ    rmspb.NodeType
	kind   string // compute, switch, powershelf
	bmcMAC net.HardwareAddr
	hosts  []net.HardwareAddr
}

// Start inventories the NVL racks and serves RMS.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	a.version = rt.Outputs[OutputVersion]
	if a.version == "" {
		a.version = DefaultVersion
	}
	a.rackIDs = map[*model.Rack]string{}
	rt.Engine.Read(func(m *model.Model) {
		a.site = m.Site
		names := map[string]int{}
		for _, r := range m.Racks {
			if nvl.RackScale(r) {
				names[r.Name]++
			}
		}
		for _, r := range m.Racks {
			if !nvl.RackScale(r) {
				continue
			}
			a.racks = append(a.racks, r)
			a.rackIDs[r] = r.Name
			if names[r.Name] > 1 {
				a.rackIDs[r] = strings.TrimPrefix(r.Path, m.Site+"/")
			}
			for _, d := range r.Devices {
				if n := newNode(r, d); n != nil {
					a.nodes = append(a.nodes, n)
				}
			}
		}
		a.siteIP = serviceAddress(m, rt.Outputs[OutputSiteAddress])
	})
	if len(a.racks) == 0 {
		return nil
	}
	sort.Slice(a.nodes, func(i, j int) bool { return a.nodes[i].dev.Path < a.nodes[j].dev.Path })
	a.jobs = newTracker(rt.Engine)
	var err error
	if a.fab, err = loadFabric(rt.StateDir); err != nil {
		return err
	}
	ln, err := rt.Net.Listen(a.site, "rms")
	if err != nil {
		return err
	}
	a.addr = ln.Addr().String()
	host, _, _ := net.SplitHostPort(a.siteIP)
	var ips []string
	if host != "" {
		ips = []string{host}
	}
	cert, err := rt.Net.Certificate("rms", a.hostnames(), ips)
	if err != nil {
		ln.Close()
		return err
	}
	a.cert = &cert
	a.gs = grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 5 * time.Second, PermitWithoutStream: true}),
		grpc.UnaryInterceptor(a.intercept))
	rmspb.RegisterRackManagerServer(a.gs, a)
	rmsv2pb.RegisterRackManagerV2Server(a.gs, &v2{a: a})
	reflection.Register(a.gs)
	go a.gs.Serve(ln)
	for _, h := range a.hostnames() {
		if h != "localhost" {
			rt.Net.Route(fmt.Sprintf("%s:%d", h, Port), a.addr)
		}
	}
	if a.siteIP != "" {
		rt.Net.Route(a.siteIP, a.addr)
	}
	ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.jobs.settle()
			}
		}
	}()
	if err := a.writeService(); err != nil {
		rt.Log.Warn("rms: service file not written", "err", err)
	}
	return nil
}

func (a *Adapter) hostnames() []string {
	return []string{"rms", "rms." + strings.ToLower(a.site), "localhost"}
}

// serviceAddress is RMS's site address: the stack's output when it sets one, else a fixed
// host on the estate's service network (the twentieth address), port 443.
func serviceAddress(m *model.Model, out string) string {
	if out != "" {
		if _, _, err := net.SplitHostPort(out); err != nil {
			return net.JoinHostPort(out, fmt.Sprint(Port))
		}
		return out
	}
	var names []string
	for n := range m.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		nw := m.Networks[n]
		if nw.Role != "service" || nw.CIDR == "" {
			continue
		}
		_, ipn, err := net.ParseCIDR(nw.CIDR)
		if err != nil || ipn.IP.To4() == nil {
			continue
		}
		ip := ipn.IP.To4()
		host := net.IPv4(ip[0], ip[1], ip[2], ip[3]+20)
		if ipn.Contains(host) {
			return net.JoinHostPort(host.String(), fmt.Sprint(Port))
		}
	}
	return ""
}

// newNode classifies a device of an NVL rack as RMS does by node type.
func newNode(r *model.Rack, d *model.Device) *node {
	n := &node{dev: d, rack: r}
	gen := "GB200"
	f := strings.ToLower(r.FormatName + " " + d.ProfileName)
	switch {
	case strings.Contains(f, "gb300"):
		gen = "GB300"
	case strings.Contains(f, "vera-rubin"), strings.Contains(f, "vr-nvl"), strings.Contains(f, "vr200"), strings.Contains(f, "vr-"):
		gen = "VR"
	}
	vendor := ""
	if d.Profile != nil && d.Profile.Spec.Identity != nil {
		vendor = strings.ToLower(d.Profile.Spec.Identity.Manufacturer)
	}
	switch d.BaseClass {
	case "compute-tray":
		n.kind = "compute"
		n.typ = map[string]rmspb.NodeType{"GB200": rmspb.NodeType_COMPUTE_GB200_NVIDIA, "GB300": rmspb.NodeType_COMPUTE_GB300_NVIDIA, "VR": rmspb.NodeType_COMPUTE_VRNVL72_NVIDIA}[gen]
		if gen == "GB300" && strings.Contains(vendor, "lenovo") {
			n.typ = rmspb.NodeType_COMPUTE_GB300_LENOVO
		}
	case "switch-tray":
		n.kind = "switch"
		n.typ = map[string]rmspb.NodeType{"GB200": rmspb.NodeType_SWITCH_GB200_NVIDIA, "GB300": rmspb.NodeType_SWITCH_GB300_NVIDIA, "VR": rmspb.NodeType_SWITCH_VRNVL72_NVIDIA}[gen]
	case "power-shelf":
		n.kind = "powershelf"
		delta := strings.HasPrefix(d.ProfileName, "delta/") || strings.Contains(vendor, "delta")
		switch {
		case gen == "GB300" && delta:
			n.typ = rmspb.NodeType_POWERSHELF_GB300_DELTA
		case gen == "GB300":
			n.typ = rmspb.NodeType_POWERSHELF_GB300_LITEON
		case delta:
			n.typ = rmspb.NodeType_POWERSHELF_GB200_DELTA
		default:
			n.typ = rmspb.NodeType_POWERSHELF_GB200_LITEON
		}
	default:
		return nil
	}
	n.bmcMAC, _ = net.ParseMAC(d.Identity.BMCMAC)
	for _, p := range d.Ports {
		if p.Kind != "ethernet" || p.MAC == "" || strings.EqualFold(p.MAC, d.Identity.BMCMAC) || p.Role == "management" && strings.HasPrefix(p.Name, "bmc") {
			continue
		}
		if mac, err := net.ParseMAC(p.MAC); err == nil {
			n.hosts = append(n.hosts, mac)
		}
	}
	return n
}

// Stop ends the service.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	if a.gs != nil {
		a.gs.Stop()
	}
	a.wg.Wait()
	if a.rt != nil && a.addr != "" {
		for _, h := range a.hostnames() {
			a.rt.Net.Unroute(fmt.Sprintf("%s:%d", h, Port))
		}
		if a.siteIP != "" {
			a.rt.Net.Unroute(a.siteIP)
		}
	}
	return nil
}

// Address is where RMS is served on this host (host:port).
func (a *Adapter) Address() string { return a.addr }

// Service says how to reach RMS.
func (a *Adapter) Service() Service {
	s := Service{URL: "https://" + a.addr, Hostnames: a.hostnames()}
	if a.siteIP != "" {
		s.SiteURL = "https://" + a.siteIP
	}
	if a.rt != nil && a.rt.StateDir != "" {
		s.CA = filepath.Join(a.rt.StateDir, "estate-ca.pem")
	}
	for _, r := range a.racks {
		s.Racks = append(s.Racks, a.rackIDs[r])
	}
	return s
}

func (a *Adapter) writeService() error {
	if a.rt.StateDir == "" {
		return nil
	}
	b, _ := json.MarshalIndent(a.Service(), "", "  ")
	return os.WriteFile(filepath.Join(a.rt.StateDir, ServiceFile), b, 0o644)
}

// ReadService reads how to reach a running twin's RMS from its state directory.
func ReadService(stateDir string) (Service, error) {
	var s Service
	b, err := os.ReadFile(filepath.Join(stateDir, ServiceFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: Name}
	if a.addr == "" {
		r.Lines = []string{"no NVL (rack-scale) rack in the estate: RMS not served"}
		return r
	}
	r.Served = 1
	kinds := map[string]int{}
	for _, n := range a.nodes {
		kinds[n.kind]++
	}
	r.Lines = []string{
		fmt.Sprintf("RMS (RackManager, RackManagerV2) at https://%s, site address %s: %d racks, %d compute trays, %d switch trays, %d power shelves", a.addr, a.siteIP, len(a.racks), kinds["compute"], kinds["switch"], kinds["powershelf"]),
		"bindings generated from nv-rms-client's Apache-2.0 protos at 966d7a7; served: " + strings.Join(served(), ", ") + "; the others answer UNIMPLEMENTED",
		"placement: slot_number = rack unit, tray_index = index among the rack's trays of that kind, chassis_sn = the NVL rack's chassis serial (the one NMX-C reports)",
		"power: ON/FORCE_ON power.on, OFF/GRACEFUL_SHUTDOWN power.off.graceful, FORCE_OFF power.off.force, RESET power.cycle, GRACEFUL_RESTART/FORCE_RESTART power.restart, through engine.Power from the stack; a node whose BMC is down is a per-node failure",
		"jobs: firmware and switch images are the engine's update tasks; certificates, fabric configuration, passwords, factory resets and cold reboots run behaviour durations scaled by spec.timeScale",
	}
	r.IsNot = []string{
		"The twin serves the RMS API itself, bound to the model; it does not run nv-rms, and RMS's own drive of the switch trays (NVUE writes, SSH/SFTP) is not exercised: switch/nvos-ssh is owed.",
		"Firmware objects are the profiles' baselines (one per NVL rack format) plus ids from rms.firmwareObjects; nothing is downloaded, and an applied SOT document sets one version per component (its Version, or per-component Components, else its Id).",
		"Switch certificates, mTLS state and the scale-up fabric configuration are this adapter's state (persisted in the state directory); NMX-C does not require the certificates and runs whether or not the fabric manager was configured.",
		"NVOS passwords are the switch tray's accounts, which its BMC and NVUE surfaces share; a factory reset clears the tray's fabric primary role and certificate state only.",
		"Jobs are kept in memory for the life of the twin (RMS keeps them 24 hours); a job id this process never issued is not found.",
		"Inventory registration (CreateNodes, UpdateNode, DeleteNode, ListNodeInventory), firmware inventories, SPDM evidence and the SDN reset are not served.",
	}
	return r
}

// ---- matching

// match finds the device a requested node names.
func (a *Adapter) match(req *rmspb.NodeInfo) (*node, error) {
	if req == nil {
		return nil, fmt.Errorf("no node")
	}
	var bmc, host *rmspb.NetworkInterface
	if req.BmcEndpoint != nil {
		bmc = req.BmcEndpoint.Interface
	}
	if req.HostEndpoint != nil {
		host = req.HostEndpoint.Interface
	}
	bmcMAC, err := parseMAC(bmc)
	if err != nil {
		return nil, err
	}
	hostMAC, err := parseMAC(host)
	if err != nil {
		return nil, err
	}
	var siteIPs map[*model.Device]string
	a.rt.Engine.Read(func(*model.Model) {
		siteIPs = map[*model.Device]string{}
		for _, n := range a.nodes {
			siteIPs[n.dev] = n.dev.Endpoint.SiteIP
		}
	})
	if bmcMAC != nil {
		for _, n := range a.nodes {
			if n.bmcMAC.String() == bmcMAC.String() {
				return n, nil
			}
		}
	}
	if hostMAC != nil {
		for _, n := range a.nodes {
			for _, h := range n.hosts {
				if h.String() == hostMAC.String() {
					return n, nil
				}
			}
		}
	}
	for _, i := range []*rmspb.NetworkInterface{bmc, host} {
		if i == nil || i.IpAddress == "" {
			continue
		}
		for _, n := range a.nodes {
			if ip := siteIPs[n.dev]; ip != "" && ip == i.IpAddress && (i == bmc || n.kind == "switch") {
				return n, nil
			}
		}
	}
	if n := a.byID(req.NodeId); n != nil {
		return n, nil
	}
	return nil, fmt.Errorf("no device in the estate matches this node")
}

// byID finds a device by an id that names it: its path, its path in the site, rack/name, serial
// or BMC MAC.
func (a *Adapter) byID(id string) *node {
	if id == "" {
		return nil
	}
	mac, _ := net.ParseMAC(id)
	for _, n := range a.nodes {
		d := n.dev
		switch {
		case id == d.Path, id == strings.TrimPrefix(d.Path, a.site+"/"), id == n.rack.Name+"/"+d.Name, id == d.Identity.Serial:
			return n
		case mac != nil && n.bmcMAC != nil && mac.String() == n.bmcMAC.String():
			return n
		}
	}
	return nil
}

func parseMAC(i *rmspb.NetworkInterface) (net.HardwareAddr, error) {
	if i == nil || i.MacAddress == "" {
		return nil, nil
	}
	m, err := net.ParseMAC(i.MacAddress)
	if err != nil {
		return nil, fmt.Errorf("%q is not a MAC address", i.MacAddress)
	}
	return m, nil
}
