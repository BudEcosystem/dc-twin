package vm

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/insomniacslk/dhcp/dhcpv4"
)

// A vm host's NICs are on the twin's segments (D48). Each of the host's ports (hostPorts) is a
// virtio NIC whose QEMU netdev is a `stream` socket the twin holds (qemu.Cable): the far end of
// the NIC's cable is the switch port the model cables it to, served in userspace by
// netsim.HostPort. The guest's DHCP enters the twin relay of the port's segment there, so a
// `dhcp: stack` network's server sees the host's own firmware and OS ask, with the circuit id the
// cabling implies; the segment gateway routes the guest's TCP (and UDP) to the stack's services
// at the site addresses its outputs name (`siteServices`) and to estate endpoints at theirs. The
// MACs are the model's. Nothing else is reachable.
//
// The networking is observed, not owned: every lease the relay delivers to the guest and every
// connection the guest opens through the gateway is committed as an `observe` event on the
// port's path (field `lease`, field `flow`), with the machine's last power request as its cause.

// siteServicesOutput is the stack output naming its services' site addresses; siteNetworkOutput
// names a routed site network (a Linux namespace where services that need the guest's own address
// run, netsim.Uplink).
const (
	siteServicesOutput = "siteServices"
	siteNetworkOutput  = "siteNetwork"
)

// uplinkMAC is the router's MAC on the routed site network: locally administered, the twin's.
var uplinkMAC = net.HardwareAddr{0x02, 0xdc, 0x7e, 0x00, 0x00, 0x01}

// siteNetwork starts the router's interface on the stack's routed site network, if it names one.
func (a *Adapter) siteNetwork() (*netsim.Uplink, error) {
	s := strings.TrimSpace(a.rt.Outputs[siteNetworkOutput])
	if s == "" {
		return nil, nil
	}
	n, err := netsim.ParseSiteNetwork(s)
	if err != nil {
		return nil, err
	}
	return netsim.NewUplink(n, uplinkMAC, a.log), nil
}

// netObserver is the event principal for what the switch port observes of the guest.
const netObserver = "vm-network"

// nic is one host port's NIC: the QEMU end of its cable and the twin's switch port.
type nic struct {
	m       *machine
	port    *model.Port
	peer    string // the switch port it is cabled to ("<switch>:<port>"), or "" when uncabled
	network string
	cable   *qemu.Cable
	hp      *netsim.HostPort

	mu      sync.Mutex
	lease   string
	boot    string // the boot file of the last ACK (NICo names one only to an HTTPClient)
	server  string
	leases  int
	flows   map[string]int // service -> connections
	refuse  map[string]int // why -> refused flows
	classes map[string]int // the guest's DHCP vendor classes (option 60) -> requests
	seq     []string       // the vendor classes of the guest's DHCP requests, in order (the first 256)
}

// services reads the stack's site services from its outputs.
func (a *Adapter) services() ([]netsim.Service, error) {
	s := a.rt.Outputs[siteServicesOutput]
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	return netsim.ParseServices(s)
}

// cableNICs gives each of a machine's host ports its cable and switch port. The sockets are in
// the machine's short run directory; they outlive power cycles (each new QEMU process connects).
func (m *machine) cableNICs(d *model.Device, svcs []netsim.Service) error {
	for i, p := range hostPorts(d) {
		c, err := qemu.ListenCable(filepath.Join(m.run, fmt.Sprintf("nic%d.sock", i)))
		if err != nil {
			m.closeNICs()
			return err
		}
		n := &nic{m: m, port: p, network: p.Network, cable: c, flows: map[string]int{}, refuse: map[string]int{}, classes: map[string]int{}}
		if p.Link != nil {
			if peer := p.Link.Peer(p); peer != nil && peer.Device != nil {
				n.peer = peer.Device.Name + ":" + peer.Name
			}
		}
		mac, _ := net.ParseMAC(p.MAC)
		ref := p.Ref
		cfg := netsim.HostPortConfig{Ref: ref, MAC: mac, Services: svcs, Routes: m.a.rt.Net.Routes, Log: m.a.log,
			Observe: n.flow, OnDHCP: n.dhcp, Uplink: m.a.uplink}
		if p.Link != nil {
			cfg.Plug = func() (netsim.PortRelay, error) { return m.a.rt.Net.PlugRelay(ref, nil) }
		}
		hp, err := netsim.NewHostPort(c, cfg)
		if err != nil {
			c.Close()
			m.closeNICs()
			return err
		}
		n.hp = hp
		m.nics = append(m.nics, n)
	}
	return nil
}

func (m *machine) closeNICs() {
	for _, n := range m.nics {
		if n.hp != nil {
			n.hp.Close()
		}
		n.cable.Close()
	}
}

func (n *nic) cause() uint64 {
	n.m.mu.Lock()
	defer n.m.mu.Unlock()
	return n.m.cause
}

func (n *nic) commit(field, to, msg string, data map[string]any) {
	n.m.a.rt.Engine.Commit(schema.Event{Kind: "observe", Source: schema.EventSource{Class: "observer", Principal: netObserver},
		Path: n.port.Ref, State: &schema.EventState{Field: field, To: to}, Cause: n.cause(), Message: msg, Data: data})
}

// dhcp observes the guest's DHCP through the port: each ACK the relay delivers is the guest's
// lease (an event when its address changes), and each request's vendor class says what asked
// (the firmware's PXE or HTTP boot client, iPXE, or the OS).
func (n *nic) dhcp(dir string, payload []byte) {
	msg, err := dhcpv4.FromBytes(payload)
	if err != nil {
		return
	}
	if dir == "request" {
		// what asked: the firmware's PXE (PXEClient) or HTTP boot (HTTPClient) client, iPXE, the OS
		class := msg.ClassIdentifier()
		if class == "" {
			class = "(none)"
		}
		n.mu.Lock()
		n.classes[class]++
		if len(n.seq) < 256 {
			n.seq = append(n.seq, class)
		}
		n.mu.Unlock()
		return
	}
	if msg.MessageType() != dhcpv4.MessageTypeAck {
		return
	}
	ip := msg.YourIPAddr.String()
	server := ""
	if sid := msg.ServerIdentifier(); sid != nil {
		server = sid.String()
	}
	// an event when the address or the boot file changes: the firmware's PXE client, its HTTP
	// boot client and iPXE usually renew the same address, and what each was told to boot is the
	// evidence a stack's boot path is read from
	boot := msg.BootFileName
	if boot == "" {
		boot = msg.BootFileNameOption() // option 67, where an HTTP boot URL is usually given
	}
	n.mu.Lock()
	changed := ip != n.lease || boot != n.boot
	n.lease, n.boot, n.server = ip, boot, server
	n.leases++
	n.mu.Unlock()
	if !changed {
		return
	}
	data := map[string]any{"ip": ip, "server": server, "network": n.network, "mac": n.port.MAC}
	if boot != "" {
		data["bootFile"] = boot
	}
	if s := msg.ServerIPAddr; s != nil && !s.IsUnspecified() {
		data["nextServer"] = s.String()
	}
	if r := msg.Router(); len(r) > 0 {
		data["router"] = r[0].String()
	}
	n.commit("lease", ip, fmt.Sprintf("the guest's DHCP client on %s (%s) was leased %s on %s by %s, relayed by the twin relay at %s",
		n.port.Name, n.port.MAC, ip, n.network, server, n.peer), data)
}

// flow observes a connection the guest opened through the gateway, or was refused.
func (n *nic) flow(f netsim.Flow) {
	if f.Closed {
		return
	}
	n.mu.Lock()
	if f.Refused != "" {
		n.refuse[f.Refused]++
	} else {
		n.flows[f.Service]++
	}
	n.mu.Unlock()
	data := map[string]any{"proto": f.Proto, "src": f.Src.String(), "dst": f.Dst.String()}
	to := f.Proto + " " + f.Dst.String()
	if f.Refused != "" {
		data["refused"] = f.Refused
		n.commit("flow", to+" refused", fmt.Sprintf("the guest on %s opened %s %s → %s: refused (%s)", n.port.Name, f.Proto, f.Src, f.Dst, f.Refused), data)
		return
	}
	data["service"], data["local"] = f.Service, f.Local
	n.commit("flow", to, fmt.Sprintf("the guest on %s opened %s %s → %s (%s)", n.port.Name, f.Proto, f.Src, f.Dst, f.Service), data)
}

// netdev is the -netdev value for a NIC.
func (n *nic) netdev() string { return qemu.StreamNetdev(n.cable.Path) }

// NIC describes one machine NIC for tests and the inspector.
type NIC struct {
	Port, MAC, Network, Peer string
	Socket                   string
	Linked                   bool
	Gateway                  string
	Server, NoRelay          string
	Lease                    string
	Leases                   int
	Flows                    map[string]int
	Refused                  map[string]int
	Classes                  map[string]int
	Requests                 []string // the vendor class of each DHCP request, in order (the first 256)
	DHCPRequests, Replies    int64
}

// NICs lists a machine's NICs.
func (a *Adapter) NICs(path string) []NIC {
	m := a.machines[path]
	if m == nil {
		return nil
	}
	var out []NIC
	for _, n := range m.nics {
		server, why := n.hp.Relay()
		n.mu.Lock()
		x := NIC{Port: n.port.Ref, MAC: n.port.MAC, Network: n.network, Peer: n.peer, Socket: n.cable.Path, Linked: n.cable.Linked(),
			Server: server, NoRelay: why, Lease: n.lease, Leases: n.leases, Flows: map[string]int{}, Refused: map[string]int{}, Classes: map[string]int{},
			DHCPRequests: n.hp.DHCPRequests.Load(), Replies: n.hp.DHCPReplies.Load()}
		for k, v := range n.flows {
			x.Flows[k] = v
		}
		for k, v := range n.refuse {
			x.Refused[k] = v
		}
		for k, v := range n.classes {
			x.Classes[k] = v
		}
		x.Requests = append([]string(nil), n.seq...)
		n.mu.Unlock()
		if g := n.hp.Gateway(); g.IsValid() {
			x.Gateway = g.String()
		}
		out = append(out, x)
	}
	return out
}

// networkReport is the network part of the adapter's report: each NIC's segment, relay and
// gateway, and what the guest reached.
func (a *Adapter) networkReport() (lines []string) {
	svcs, _ := a.services()
	for _, path := range a.order {
		for _, x := range a.NICs(path) {
			s := fmt.Sprintf("network: %s (%s) cabled to %s on %s", x.Port, x.MAC, orNone(x.Peer), orNone(x.Network))
			if x.Server != "" {
				s += fmt.Sprintf("; DHCP relayed by the segment's twin relay to %s (%d requests, %d replies", x.Server, x.DHCPRequests, x.Replies)
				if x.Lease != "" {
					s += ", leased " + x.Lease
				}
				s += ")"
				if len(x.Classes) > 0 {
					var c []string
					for k, v := range x.Classes {
						c = append(c, fmt.Sprintf("%s %d", k, v))
					}
					sort.Strings(c)
					s += "; the guest asked as " + strings.Join(c, ", ")
				}
			} else {
				s += "; no DHCP: " + orNone(x.NoRelay)
			}
			if x.Gateway != "" {
				s += "; the twin is its gateway " + x.Gateway
			}
			if len(x.Flows) > 0 {
				var f []string
				for k, v := range x.Flows {
					f = append(f, fmt.Sprintf("%s %d", k, v))
				}
				sort.Strings(f)
				s += "; connections: " + strings.Join(f, ", ")
			}
			if n := sum(x.Refused); n > 0 {
				s += fmt.Sprintf("; %d refused (no route)", n)
			}
			lines = append(lines, s)
		}
	}
	if u := a.uplink; u != nil {
		state := "linked"
		if !u.Linked() {
			state = "NOT linked"
		}
		lines = append(lines, fmt.Sprintf("network: the gateway routes %s at L3, without NAT, to the stack's routed site network at %s (%s, gateway %s): %d packets there, %d back to guests, %d dropped for an unresolved neighbour",
			u.Net.Prefix, u.Net.Link, state, u.Net.Gateway, u.ToSite.Load(), u.FromSite.Load(), u.Unresolved.Load()))
	}
	if len(svcs) > 0 {
		var names []string
		for _, s := range svcs {
			names = append(names, s.String())
		}
		lines = append(lines, "network: the gateway routes to the stack's services at the site addresses its outputs name (siteServices): "+strings.Join(names, ", ")+"; and to estate endpoints at their site addresses")
	} else {
		lines = append(lines, "network: the stack's outputs name no siteServices, so the gateway routes only to estate endpoints at their site addresses")
	}
	return lines
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
