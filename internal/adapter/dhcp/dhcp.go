// Package dhcp is the twin's addressing and network-boot adapter (docs/spec/05-interfaces.md —
// Boot and addressing): a userspace DHCP client for every BMC port and for every NVLink switch
// tray's NVOS management port, the twin relay of every model-switch segment, the twin site DHCP
// for `dhcp: twin` networks, and the boot walker that walks PXE, iPXE and UEFI HTTP boot on
// behalf of `model` hosts.
//
// The relay is the centre of it. Stacks such as NICo accept relayed DHCP only (docs/stacks/
// nvidia-dsx-os.md), so every request reaches a server through the relay of the segment its port
// is cabled to, carrying what the model's cabling implies: option 82 circuit id
// "<rack>-<switch>:<port>" and remote id (the switch's MAC, as text), and RFC 3527 link selection
// naming the segment's gateway. A `dhcp: stack` network's relays forward to the stack's server; a
// `dhcp: twin` network's to the twin site DHCP, which the fidelity report labels as the twin's.
//
// Segments are addressed by a pure function of the hall layout (segments.go), so a rack added
// to the estate never renumbers another. Every BMC lease is committed through the engine
// (SetLease), and every boot-walk step through SetBooted; the adapter changes no model field
// itself. The model holds one lease per device, the BMC's, so NVOS's lease is committed as a
// state event on its port's path (nvos.go, D46).
package dhcp

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
	"github.com/insomniacslk/dhcp/dhcpv4"
)

func init() { twin.Register("dhcp", func() adapter.Adapter { return New() }) }

// defaultVendorClass is option 60 for a BMC whose dialect names none. It is the twin's name, so a
// stack that keys on option 60 can tell such an endpoint apart; the report counts them.
const defaultVendorClass = "dc-twin"

// Adapter is the DHCP adapter.
type Adapter struct {
	rt     *adapter.Runtime
	ctx    context.Context
	cancel context.CancelFunc

	// retransmit is the first retransmission wait (RFC 2131: 4 s, doubling to 64 s).
	retransmit time.Duration
	// leaseTime is what the twin site DHCP grants.
	leaseTime time.Duration
	// stagger spreads the first DISCOVERs of a start over a window, by a hash of each MAC, so a
	// thousand endpoints do not all transmit in one instant (a hall's BMCs coming up do not).
	stagger time.Duration

	mu        sync.Mutex
	nets      map[string]*netState
	endpoints map[string]*endpoint // device path -> BMC endpoint
	walkEPs   map[string]*endpoint // port ref -> boot walker endpoint
	walks     map[string]*walkRecord
	walkStop  map[string]context.CancelFunc
	leasedBy  map[string]string // device path -> server class that leased it
	skipped   map[string]int    // why a BMC port has no DHCP client -> count
	classes   map[string]int    // "dialect" | "profile" | "default" -> endpoints
	closers   []io.Closer
	stopped   atomic.Bool

	// NVOS (nvos.go): one client per switch tray NVOS management port, run while the tray is on.
	// nosByDev is written once in Start and read only after.
	nosEPs     map[string]*endpoint   // port ref -> NVOS endpoint
	nosByDev   map[string][]*endpoint // tray path -> its NVOS endpoints
	nosLeases  map[string]string      // port ref -> the address NVOS holds (committed as an event)
	nosSkipped map[string]int         // why an NVOS port has no DHCP client -> count

	// plugged are vm host ports plugged into their segment's relay for their guest's own DHCP
	// client (hostport.go, D48), by port ref
	plugged map[string]*endpoint
	// upstreams are the relay legs to stack servers, by server address (one per server)
	upstreams map[string]*upstream

	sent, acks, renewals atomic.Int64
}

// netState is one DHCP network at run time.
type netState struct {
	net      *model.Network
	plan     *addressing
	segs     map[*model.Rack]*segment
	order    []*segment
	up       *upstream
	server   *twinServer // dhcp: twin
	idle     string      // why the network serves nothing (dhcp: stack without a server)
	bmcPorts int
	nosPorts int // NVOS management ports on it
}

// New returns the adapter with RFC defaults.
func New() *Adapter {
	return &Adapter{retransmit: 4 * time.Second, leaseTime: time.Hour, stagger: 2 * time.Second}
}

func (a *Adapter) Name() string { return "dhcp" }

func sourceOf(class, server string) schema.EventSource {
	return schema.EventSource{Class: class, Principal: server}
}

var walkerSrc = schema.EventSource{Class: "twin", Principal: "boot-walker"}

// Start plans every DHCP network's segments, opens the servers, relays and endpoints, and
// registers the engine hooks: OnBMCUp restarts a BMC's client, OnBoot runs the boot walker. The
// switch trays' power, which starts and halts NVOS's clients, is followed through the engine's
// committed events (watchNVOS).
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.nets, a.endpoints, a.walkEPs = map[string]*netState{}, map[string]*endpoint{}, map[string]*endpoint{}
	a.walks, a.walkStop, a.leasedBy = map[string]*walkRecord{}, map[string]context.CancelFunc{}, map[string]string{}
	a.skipped, a.classes = map[string]int{}, map[string]int{}
	a.nosEPs, a.nosByDev, a.nosLeases, a.nosSkipped = map[string]*endpoint{}, map[string][]*endpoint{}, map[string]string{}, map[string]int{}
	a.plugged = map[string]*endpoint{}
	a.upstreams = map[string]*upstream{}

	var site string
	var bmcs []*model.Device
	var nvos []*model.Port
	rt.Engine.Read(func(m *model.Model) {
		site = m.Site
		segs := PlanSegments(m)
		nosSwitches := twin.NOSSwitches(m)
		for name, n := range m.Networks {
			if n.DHCP != "twin" && n.DHCP != "stack" {
				continue
			}
			ns := &netState{net: n, plan: planAddressing(m, n), segs: map[*model.Rack]*segment{}}
			for _, s := range segs {
				if s.Network != name {
					continue
				}
				sg := &segment{Segment: s, network: n}
				for _, p := range s.Switches {
					sg.switches = append(sg.switches, m.DeviceBy[p])
					if name := nosSwitches[m.DeviceBy[p]]; name != "" && rt.Net.L2 != nil {
						sg.nos = name // its NOS relays it; the twin runs no relay there
					}
				}
				ns.segs[m.RackBy[s.Rack]] = sg
				ns.order = append(ns.order, sg)
			}
			a.nets[name] = ns
		}
		for _, d := range m.Devices {
			if d.HasBMC() {
				bmcs = append(bmcs, d)
			}
			nvos = append(nvos, twin.NVOSPorts(d)...)
		}
	})
	names := make([]string, 0, len(a.nets))
	for n := range a.nets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := a.openNetwork(site, a.nets[name]); err != nil {
			a.Stop()
			return fmt.Errorf("network %s: %w", name, err)
		}
	}
	for _, d := range bmcs {
		ep := a.bmcEndpoint(d)
		if ep == nil {
			continue
		}
		if err := ep.open("dhcp"); err != nil {
			a.Stop()
			return err
		}
		a.endpoints[d.Path] = ep
	}
	for _, p := range nvos {
		ep := a.nvosEndpoint(p)
		if ep == nil {
			continue
		}
		if err := ep.open("dhcp-nvos:" + p.Name); err != nil {
			a.Stop()
			return err
		}
		a.nosEPs[p.Ref] = ep
		a.nosByDev[p.Device.Path] = append(a.nosByDev[p.Device.Path], ep)
	}
	// a vm host's NICs are plugged into their segments' relays by the vm adapter (D48)
	rt.Net.SetRelayDirectory(a.plugPort)
	rt.Engine.AddHooks(engine.Hooks{
		OnBMCUp: []func(d *model.Device){a.onBMCUp},
		OnBoot:  []func(d *model.Device, target string){a.onBoot},
	})
	for _, ep := range a.endpoints {
		a.startEndpoint(ep, a.spread(ep.mac))
	}
	if len(a.nosEPs) > 0 {
		// subscribed before the first look at the trays' power, so no power-on falls between
		id, ch := rt.Engine.Subscribe(4096)
		go a.watchNVOS(a.ctx, id, ch)
		for _, eps := range a.nosByDev {
			a.nvosPower(eps, true)
		}
	}
	return nil
}

// spread is a MAC's deterministic place in the start window.
func (a *Adapter) spread(mac net.HardwareAddr) time.Duration {
	if a.stagger <= 0 {
		return 0
	}
	h := fnv.New64a()
	h.Write(mac)
	return time.Duration(h.Sum64() % uint64(a.stagger))
}

// openNetwork opens a network's server leg and one relay per segment.
func (a *Adapter) openNetwork(site string, ns *netState) error {
	n := ns.net
	path := site + "/networks/" + n.Name
	var server *net.UDPAddr
	label := ""
	switch n.DHCP {
	case "twin":
		c, err := a.rt.Net.ListenUDP(path, "dhcp-server")
		if err != nil {
			return err
		}
		a.closers = append(a.closers, c)
		server = c.LocalAddr().(*net.UDPAddr)
		ns.server = newTwinServer(a, ns.plan, c, ns.order)
		label = fmt.Sprintf("the twin site DHCP (server id %s)", ns.plan.serverID())
	case "stack":
		addr, from := n.Server, "estate networks."+n.Name+".server"
		if addr == "" {
			addr, from = a.rt.Outputs["dhcpServer"], "the stack's dhcpServer output"
		}
		if addr == "" {
			ns.idle = "dhcp: stack, but neither the estate (networks." + n.Name + ".server) nor the stack's outputs (dhcpServer) name a DHCP server"
			return nil
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "67")
		}
		ua, err := net.ResolveUDPAddr("udp4", addr)
		if err != nil {
			return fmt.Errorf("dhcp server %s: %w", addr, err)
		}
		server = ua
		label = fmt.Sprintf("the stack's DHCP at %s (%s)", addr, from)
	}
	// Networks relayed to the same stack server share one upstream leg: a server that answers
	// relays at one fixed port (NICo's dhcp-server --relay-response-port) can serve them all. The
	// circuit ids and MACs the replies are matched by are unique across networks.
	shared := n.DHCP == "stack" && a.upstreams[server.String()] != nil
	if shared {
		ns.up = a.upstreams[server.String()]
		ns.up.label = strings.Replace(ns.up.label, "(estate networks.", "(estate networks."+n.Name+", networks.", 1)
	} else {
		conn, err := a.upstreamConn(path, server)
		if err != nil {
			return err
		}
		a.closers = append(a.closers, conn)
		ns.up = newUpstream(n.Name, conn, server, label, n.DHCP == "twin")
		if n.DHCP == "stack" {
			a.upstreams[server.String()] = ns.up
		}
	}
	for _, sg := range ns.order {
		if sg.nos != "" {
			continue // a real NOS relays this segment (the TAP substrate wired its ports in)
		}
		owner := sg.Rack
		if len(sg.Switches) > 0 {
			owner = sg.Switches[0]
		}
		c, err := a.rt.Net.ListenUDP(owner, "dhcp-relay:"+n.Name)
		if err != nil {
			return err
		}
		a.closers = append(a.closers, c)
		sg.relay = newRelay(sg, ns.up, c)
		if a.rt.Net.L2 != nil && sg.Gateway.IsValid() && len(sg.switches) > 0 {
			// on the TAP substrate the relay is also on the segment's bridge, at its gateway
			gw, err := a.rt.Net.L2.Gateway(n.Name, sg.Rack, netip.PrefixFrom(sg.Gateway, sg.Prefix.Bits()), switchMAC(sg.switches[0]))
			if err != nil {
				return fmt.Errorf("segment %s on %s: %w", sg.Rack, n.Name, err)
			}
			sg.relay.gw = gw
		}
	}
	// serve only once the network is wired, so no server or relay reads a half-built segment
	if ns.server != nil {
		go ns.server.serve()
	}
	if !shared {
		go ns.up.serve()
	}
	for _, sg := range ns.order {
		if sg.relay == nil {
			continue
		}
		go sg.relay.serve()
		if sg.relay.gw != nil {
			go sg.relay.serveL2()
		}
	}
	return nil
}

// upstreamConn opens the relay's server-facing socket on the local address that routes to the
// server: giaddr must be an address the server can answer.
func (a *Adapter) upstreamConn(path string, server *net.UDPAddr) (*net.UDPConn, error) {
	local := net.ParseIP(a.rt.Net.BindIP)
	if c, err := net.DialUDP("udp4", nil, server); err == nil {
		local = c.LocalAddr().(*net.UDPAddr).IP
		c.Close()
	}
	if local.Equal(net.ParseIP(a.rt.Net.BindIP)) {
		return a.rt.Net.ListenUDP(path, "dhcp-relay-upstream")
	}
	return net.ListenUDP("udp4", &net.UDPAddr{IP: local})
}

// bmcEndpoint builds the DHCP client of a device's BMC port, or records why it has none.
func (a *Adapter) bmcEndpoint(d *model.Device) *endpoint {
	p := compile.BMCPort(d)
	skip := func(why string) *endpoint {
		a.skipped[why]++
		return nil
	}
	if p == nil {
		return skip("the profile has a BMC but no management port")
	}
	ns := a.nets[p.Network]
	if ns == nil {
		return skip("the BMC port is on no network with DHCP")
	}
	ns.bmcPorts++
	if ns.idle != "" {
		return skip("network " + p.Network + " has no DHCP server")
	}
	ep := a.portEndpoint(d, p, ns, skip)
	if ep == nil {
		return nil
	}
	ep.bmc = true
	ep.dev = d
	class, src := defaultVendorClass, "default"
	if d.Profile.Spec.BMC.DHCPVendorClass != "" {
		class, src = d.Profile.Spec.BMC.DHCPVendorClass, "profile"
	} else if dl, _ := a.rt.Reg.Dialect(d.Dialect); dl != nil && dl.Spec.Identity.DHCPVendorClass != "" {
		class, src = dl.Spec.Identity.DHCPVendorClass, "dialect"
	}
	a.classes[src]++
	ep.class = class
	host := strings.ReplaceAll(strings.TrimPrefix(d.Path, rackPathOf(d)+"/"), "/", "-")
	if r := d.RackOf(); r != nil {
		host = r.Name + "-" + host
	}
	ep.opts = []dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(class)),
		dhcpv4.WithOption(dhcpv4.OptHostName(host + "-bmc")),
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier(append([]byte{1}, ep.mac...))),
		dhcpv4.WithOption(dhcpv4.OptParameterRequestList(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter,
			dhcpv4.OptionDomainNameServer, dhcpv4.OptionHostName, dhcpv4.OptionDomainName, dhcpv4.OptionNTPServers,
			dhcpv4.OptionIPAddressLeaseTime, dhcpv4.OptionServerIdentifier, dhcpv4.OptionRenewTimeValue,
			dhcpv4.OptionRebindingTimeValue)),
	}
	return ep
}

func rackPathOf(d *model.Device) string {
	if r := d.RackOf(); r != nil {
		return r.Path
	}
	return ""
}

// portEndpoint resolves a port's segment, model switch and relay facts.
func (a *Adapter) portEndpoint(d *model.Device, p *model.Port, ns *netState, skip func(string) *endpoint) *endpoint {
	if p.Link == nil {
		return skip("the port is not cabled")
	}
	peer := p.Link.Peer(p)
	sw := peer.Device
	nos := ""
	if f := a.fabricOf(p.Link); f != nil && f.NOS != "none" {
		// on the TAP substrate a BMC's port is wired into the NOS, which relays its DHCP; any
		// other port (a host's, for the boot walker) has no endpoint there
		if a.rt.Net.L2 == nil || a.rt.Net.L2.Port(p.Ref) == nil {
			return skip("the port is cabled to a real NOS (" + f.NOS + "), whose own relay serves it, and has no endpoint on the substrate")
		}
		nos = f.NOS
	}
	r := d.RackOf()
	sg := ns.segs[r]
	if nos != "" && sg != nil {
		mac, err := net.ParseMAC(p.MAC)
		if err != nil {
			return skip("the port has no MAC")
		}
		return &endpoint{a: a, device: d.Path, port: p, mac: mac, seg: sg, sw: sw, swPort: peer, nos: nos}
	}
	if r == nil || sg == nil || sg.relay == nil {
		why := "its rack has no segment on network " + ns.net.Name
		if ns.plan.capacity > 0 {
			why += " (" + ns.plan.net.CIDR + " holds too few /24s)"
		}
		return skip(why)
	}
	mac, err := net.ParseMAC(p.MAC)
	if err != nil {
		return skip("the port has no MAC")
	}
	return &endpoint{a: a, device: d.Path, port: p, mac: mac, seg: sg, sw: sw, swPort: peer,
		circuit: circuitID(sw, peer), remote: switchMAC(sw), offset: staticOffset(sg.switches, sw, peer)}
}

func (a *Adapter) fabricOf(l *model.Link) *model.Fabric {
	var f *model.Fabric
	a.rt.Engine.Read(func(m *model.Model) { f = m.Fabrics[l.Fabric] })
	return f
}

// endpointUp: a BMC endpoint transmits while its controller is available (standby power is
// enough) and its link is up; an NVOS endpoint while its tray is powered on (NVOS runs on the
// tray's CPU) and its link is up; a host's provisioning endpoint while its port is up, which
// needs the host powered on.
func (a *Adapter) endpointUp(ep *endpoint) bool {
	if a.stopped.Load() {
		return false
	}
	if ep.bmc {
		if up, _, _ := a.rt.Engine.BMCBehaviour(ep.dev); !up {
			return false
		}
	}
	if ep.nvos && !a.nvosUp(ep.dev) {
		return false
	}
	return a.rt.Engine.PortUp(ep.port)
}

// startEndpoint (re)starts an endpoint's client, cancelling the one running. A client that
// ends by itself (its controller went down) releases its context, so running() then says so.
func (a *Adapter) startEndpoint(ep *endpoint, stagger time.Duration) {
	ep.mu.Lock()
	if ep.cancel != nil {
		ep.cancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	ep.cancel = cancel
	ep.gen++
	gen := ep.gen
	ep.mu.Unlock()
	go func() {
		ep.run(ctx, stagger)
		ep.mu.Lock()
		if ep.gen == gen && ep.cancel != nil {
			ep.cancel()
			ep.cancel = nil
		}
		ep.mu.Unlock()
	}()
}

// onBMCUp restarts a BMC's DHCP client when the engine brings its controller up (AC restored,
// a BMC reset): it asks for its previous address again (INIT-REBOOT), as a rebooted BMC does.
func (a *Adapter) onBMCUp(d *model.Device) {
	if a.stopped.Load() {
		return
	}
	a.mu.Lock()
	ep := a.endpoints[d.Path]
	a.mu.Unlock()
	if ep != nil {
		a.startEndpoint(ep, a.spread(ep.mac)/4)
	}
}

func (a *Adapter) leased(ep *endpoint) {
	class := "twin"
	if !ep.twinServed() {
		class = "stack"
	}
	a.mu.Lock()
	a.leasedBy[ep.device] = class
	a.mu.Unlock()
}

// Stop closes every socket; hooks registered with the engine become no-ops.
func (a *Adapter) Stop() error {
	a.stopped.Store(true)
	if a.cancel != nil {
		a.cancel()
	}
	if a.rt != nil && a.rt.Net != nil {
		a.rt.Net.SetRelayDirectory(nil)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.walkStop {
		c()
	}
	for _, ep := range a.endpoints {
		ep.close()
	}
	for _, ep := range a.nosEPs {
		ep.close()
	}
	for _, ep := range a.walkEPs {
		ep.close()
	}
	for _, ep := range a.plugged {
		ep.close() // the vm adapter's port sees its relay close
	}
	for _, c := range a.closers {
		c.Close()
	}
	a.closers = nil
	return nil
}

// Leases returns the device paths leased so far, by the class of server that leased them.
func (a *Adapter) Leases() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.leasedBy))
	for k, v := range a.leasedBy {
		out[k] = v
	}
	return out
}

// Report states what was served, what was the twin's own, and what is not modelled (07).
func (a *Adapter) Report() adapter.Report {
	rep := adapter.Report{Adapter: "dhcp"}
	a.mu.Lock()
	defer a.mu.Unlock()
	rep.Served = len(a.endpoints) + len(a.nosEPs)
	leasedOn, nosOn := map[string]int{}, map[string]int{}
	for path, ep := range a.endpoints {
		if _, ok := a.leasedBy[path]; ok {
			leasedOn[ep.port.Network]++
		}
	}
	for ref, ep := range a.nosEPs {
		if a.nosLeases[ref] != "" {
			nosOn[ep.port.Network]++
		}
	}
	names := make([]string, 0, len(a.nets))
	for n := range a.nets {
		names = append(names, n)
	}
	sort.Strings(names)
	var relays int
	var req, rep2, noEcho, unmatched int64
	giaddrs := map[string]bool{}
	var quiet []string
	walkLeases := map[string]int{}
	for _, w := range a.walks {
		if w.lease != "" {
			walkLeases[w.network]++
		}
	}
	for _, name := range names {
		ns := a.nets[name]
		n := ns.net
		head := fmt.Sprintf("addressing: network %s (%s), dhcp %s", n.Name, n.Role, n.DHCP)
		served := fmt.Sprintf("%d of %d BMC ports leased", leasedOn[name], ns.bmcPorts)
		if ns.bmcPorts == 0 {
			served = fmt.Sprintf("no BMC is on it; %d boot-walk leases", walkLeases[name])
		}
		if ns.nosPorts > 0 {
			served += fmt.Sprintf("; %d of %d NVOS management ports leased", nosOn[name], ns.nosPorts)
		}
		switch {
		case ns.idle != "" && ns.bmcPorts == 0:
			quiet = append(quiet, n.Name)
			continue
		case ns.idle != "":
			rep.Lines = append(rep.Lines, fmt.Sprintf("%s — NOT SERVED: %s; %d BMC ports there have no lease", head, ns.idle, ns.bmcPorts))
			continue
		case n.DHCP == "twin":
			s := ns.server
			rep.Lines = append(rep.Lines, fmt.Sprintf("%s — TWIN-PROVIDED: %s, not a stack's; %s (%d offers, %d ACKs, %d NAKs; %d unrelayed requests refused)",
				head, ns.up.label, served, s.offers.Load(), s.acks.Load(), s.naks.Load(), s.unrelayed.Load()))
		default:
			rep.Lines = append(rep.Lines, fmt.Sprintf("%s — relayed to %s; %s", head, ns.up.label, served))
		}
		rep.Lines = append(rep.Lines, fmt.Sprintf("segments: network %s, %d segments: %s; gateway .1 of each", n.Name, len(ns.order), ns.plan.why))
		nosSegs, l2Relays := map[string]int{}, 0
		for _, sg := range ns.order {
			switch {
			case sg.nos != "":
				nosSegs[sg.nos]++
			case sg.relay != nil:
				relays++
				if sg.relay.gw != nil {
					l2Relays++
				}
			}
		}
		for name, c := range nosSegs {
			rep.Lines = append(rep.Lines, fmt.Sprintf("relay: network %s, %d segments relayed by a real NOS (%s) by giaddr, the segment gateway on the switch; the twin runs no relay there, and each BMC's client broadcasts on its own NIC on the TAP substrate", n.Name, c, name))
		}
		if l2Relays > 0 {
			rep.Lines = append(rep.Lines, fmt.Sprintf("relay: network %s, %d twin relays are on their segment's bridge at its gateway (TAP substrate): a BMC's broadcast reaches them over L2 and its ingress port comes from the bridge's forwarding database", n.Name, l2Relays))
		}
		for _, sg := range ns.order {
			if sg.relay != nil {
				req += sg.relay.requests.Load()
				rep2 += sg.relay.replies.Load()
			}
		}
		if ns.up != nil {
			noEcho += ns.up.noEcho.Load()
			unmatched += ns.up.unmatched.Load()
			giaddrs[ns.up.giaddr.String()] = true
		}
	}
	if len(quiet) > 0 {
		rep.Lines = append(rep.Lines, fmt.Sprintf("addressing: networks %s are dhcp: stack with no DHCP server named and no BMC on them; a host boot-walked there stops at dhcp",
			strings.Join(quiet, ", ")))
	}
	gi := make([]string, 0, len(giaddrs))
	for g := range giaddrs {
		gi = append(gi, g)
	}
	sort.Strings(gi)
	if relays > 0 {
		rep.Lines = append(rep.Lines, fmt.Sprintf("relay: %d twin relays on model switches inserted option 82 — circuit id \"<rack>-<switch>:<port>\" (ISC dhcrelay's hostname:interface), remote id (the switch's MAC as text, aa:bb:cc:dd:ee:ff), link selection (RFC 3527) naming the segment's gateway, relay source port (RFC 8357) — with giaddr %s, the relay's own loopback address, because no per-segment address is routable on this substrate; %d requests relayed, %d replies delivered",
			relays, strings.Join(gi, ", "), req, rep2))
		if noEcho > 0 || unmatched > 0 {
			rep.Lines = append(rep.Lines, fmt.Sprintf("relay: %d replies came back without option 82 (delivered by chaddr), %d matched no port", noEcho, unmatched))
		}
		rep.Lines = append(rep.Lines, fmt.Sprintf("leases: %d ACKs, %d renewals at T1 (relayed: a RENEWING client's unicast has no route on this substrate)", a.acks.Load(), a.renewals.Load()))
	}
	if a.classes["default"] > 0 {
		rep.Lines = append(rep.Lines, fmt.Sprintf("vendor class: %d BMCs sent their dialect's or profile's option 60; %d sent the default %q because their dialect names none — a stack keying on option 60 can tell those are the twin's",
			a.classes["dialect"]+a.classes["profile"], a.classes["default"], defaultVendorClass))
	}
	if a.rt != nil && a.rt.Engine.Scale() != 1 || a.rt != nil && a.rt.Engine.Virtual() {
		rep.Lines = append(rep.Lines, fmt.Sprintf("timers: DHCP retransmissions, T1 and T2 run at their protocol values on the wall clock; the estate's timeScale (%g) and a virtual clock do not apply to them", a.rt.Engine.Scale()))
	}
	for why, n := range a.skipped {
		rep.Lines = append(rep.Lines, fmt.Sprintf("no DHCP client: %d BMC ports — %s", n, why))
	}
	sort.Strings(rep.Lines[len(rep.Lines)-len(a.skipped):])
	rep.Lines = append(rep.Lines, a.nvosReport()...)
	if len(a.plugged) > 0 {
		refs := make([]string, 0, len(a.plugged))
		for ref := range a.plugged {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		rep.Lines = append(rep.Lines, fmt.Sprintf("relay: %d vm host ports plugged into their segment's relay, their guests' own DHCP clients relayed with the port's circuit id (D48): %s",
			len(refs), strings.Join(refs, ", ")))
	}
	if len(a.walks) > 0 {
		steps := map[string]int{}
		for _, w := range a.walks {
			steps[w.step]++
		}
		var parts []string
		for s, n := range steps {
			parts = append(parts, fmt.Sprintf("%d to %q", n, s))
		}
		sort.Strings(parts)
		rep.Lines = append(rep.Lines, fmt.Sprintf("boot: %d model hosts boot-walked: %s", len(a.walks), strings.Join(parts, ", ")))
	}
	l2 := "No L2: each endpoint is its own loopback UDP socket and a DHCP broadcast reaches only its segment's relay; ARP, the broadcast flag and a client's unicast renewals are not modelled (on Linux the TAP substrate adds them, D20)."
	if a.rt != nil && a.rt.Net.L2 != nil {
		l2 = "TAP substrate: BMC clients broadcast raw frames on their own NICs and take their lease's address there; a renewing client broadcasts (REBINDING-style) instead of unicasting to the server; the twin relay's leg to the server is still a host socket, so it sends link selection and its own address as giaddr. Host provisioning ports (the boot walker) stay on loopback sockets."
	}
	rep.IsNot = []string{
		l2,
		"DHCPv6 is owed: NICo ships a dhcpv6 crate and a DHCPv6 deployment guide.",
		"The boot walker implements PXE and iPXE over TFTP and plain HTTP, and UEFI HTTP boot over plain HTTP; vendor PXE extensions (option 43 menus, ProxyDHCP on port 4011), HTTPS boot and client certificates stop the walk at that step and say so.",
		"The boot walker executes nothing: it fetches what the boot script names and stops at its first boot; an NBP that is not iPXE stops the walk at its download.",
		"A host's provisioning-port lease is committed as an event only: the model holds one lease per device, and on a server that is the BMC's.",
	}
	if len(a.nosEPs) > 0 {
		rep.IsNot = append(rep.IsNot,
			"NVOS's lease is committed as a state event on its port's path (field lease), not in the model, which holds one lease per device (on a switch tray, the BMC's); the server's record of it outlives a power-off, as on a site.",
			"NVOS's boot time is not modelled: its DHCP client starts when its tray reaches On and stops the moment the tray leaves On; its DHCP client configuration (option 60, 61, the request list) is not published, so the twin's is an assumption the report states.")
	}
	return rep
}

// resolve maps a site address (host:port) to where it is served on this host, through the
// substrate's routes; anything not routed is dialled as given.
func (a *Adapter) resolve(hostport string) string {
	if local, ok := a.rt.Net.Routes()[hostport]; ok {
		return local
	}
	return hostport
}
