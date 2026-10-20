package dhcp

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/insomniacslk/dhcp/dhcpv4"
)

// segment is a planned Segment at run time: the model switches behind it and its relay.
type segment struct {
	Segment
	network  *model.Network
	switches []*model.Device
	relay    *relay
	// nos names the real NOS serving the segment on the TAP substrate: it relays, the twin
	// does not (relay is nil).
	nos string
}

// relay is the twin relay of one model-switch segment (D21: a model switch's relay inserts the
// circuit id and remote id the cabling implies, and link selection). Endpoints send their
// "broadcasts" to its downstream socket, which stands in for the segment: the substrate has no
// L2, so a frame's ingress port is known from which endpoint socket sent it, the way a switch
// knows the port a frame arrived on, never from anything the client claims.
//
// On the TAP substrate the relay is also on the segment itself (gw): a NIC on the segment's
// bridge holding the segment gateway with the switch's MAC. It hears a BMC's broadcast there and
// knows the port it came in on from the bridge's forwarding database, as a switch does (and as
// SONiC's patched dhcrelay does for a VLAN's member ports); it answers with frames to the
// client's hardware address, or broadcast when the client asked.
type relay struct {
	seg  *segment
	up   *upstream
	down *net.UDPConn
	gw   netsim.L2Gateway

	mu      sync.RWMutex
	ports   map[netip.AddrPort]*endpoint
	l2ports map[string]*endpoint // port ref -> endpoint on the TAP substrate

	requests, replies, unknownIngress atomic.Int64
}

// upstream is a network's relay-to-server leg: one socket, as a relay has one address toward
// its servers. Every segment's relay forwards through it.
//
// giaddr is this socket's own address, not the segment's gateway. On a site the relay's
// client-facing address is both the reply route and the segment selector; on the loopback
// substrate no per-segment address is routable, so giaddr carries the reply route and RFC
// 3527's link-selection sub-option carries the segment, which is exactly the split RFC 3527
// was written for. The socket is not on port 67, so the relay adds RFC 8357's relay source
// port sub-option, and a server that implements it (Kea does) answers this port.
type upstream struct {
	network string
	conn    *net.UDPConn
	server  *net.UDPAddr
	label   string // what the server is, for the report
	twin    bool   // the server is the twin site DHCP
	giaddr  net.IP

	mu        sync.RWMutex
	byCircuit map[string]*endpoint
	byMAC     map[string]*endpoint

	requests, replies, noEcho, unmatched, foreign atomic.Int64
}

func newUpstream(network string, conn *net.UDPConn, server *net.UDPAddr, label string, twin bool) *upstream {
	u := &upstream{network: network, conn: conn, server: server, label: label, twin: twin,
		byCircuit: map[string]*endpoint{}, byMAC: map[string]*endpoint{}}
	u.giaddr = conn.LocalAddr().(*net.UDPAddr).IP.To4()
	_ = conn.SetReadBuffer(4 << 20)
	return u
}

func (u *upstream) attach(ep *endpoint) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.byCircuit[ep.circuit] = ep
	u.byMAC[ep.mac.String()] = ep
}

func (u *upstream) detach(ep *endpoint) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.byCircuit[ep.circuit] == ep {
		delete(u.byCircuit, ep.circuit)
	}
	if u.byMAC[ep.mac.String()] == ep {
		delete(u.byMAC, ep.mac.String())
	}
}

func (u *upstream) send(m *dhcpv4.DHCPv4) {
	u.requests.Add(1)
	_, _ = u.conn.WriteToUDP(marshal(m), u.server)
}

// serve takes the server's replies and hands each to the port it is for: by the circuit id
// the server echoed (RFC 3046 requires the echo, and it is how a relay picks the interface),
// else by chaddr, which the report counts.
func (u *upstream) serve() {
	buf := make([]byte, 4096)
	for {
		n, from, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !from.IP.Equal(u.server.IP) {
			u.foreign.Add(1)
			continue
		}
		m, err := dhcpv4.FromBytes(buf[:n])
		if err != nil || m.OpCode != dhcpv4.OpcodeBootReply {
			continue
		}
		var ep *endpoint
		u.mu.RLock()
		if rai := m.Options.Get(optRAI); rai != nil {
			ep = u.byCircuit[string(subOptions(rai)[subCircuitID])]
		} else {
			u.noEcho.Add(1)
		}
		if ep == nil {
			ep = u.byMAC[m.ClientHWAddr.String()]
		}
		u.mu.RUnlock()
		if ep == nil {
			u.unmatched.Add(1)
			continue
		}
		u.replies.Add(1)
		m.Options.Del(optRAI) // the relay strips option 82 before the client sees the reply
		ep.seg.relay.deliver(ep, m)
	}
}

func newRelay(seg *segment, up *upstream, down *net.UDPConn) *relay {
	_ = down.SetReadBuffer(1 << 20)
	return &relay{seg: seg, up: up, down: down, ports: map[netip.AddrPort]*endpoint{}, l2ports: map[string]*endpoint{}}
}

// attach plugs an endpoint into its switch port.
func (r *relay) attach(ep *endpoint) {
	r.mu.Lock()
	if ep.l2 != nil {
		r.l2ports[ep.port.Ref] = ep
	} else {
		r.ports[ep.addr] = ep
	}
	r.mu.Unlock()
	r.up.attach(ep)
}

func (r *relay) detach(ep *endpoint) {
	r.mu.Lock()
	if ep.l2 != nil {
		if r.l2ports[ep.port.Ref] == ep {
			delete(r.l2ports, ep.port.Ref)
		}
	} else if r.ports[ep.addr] == ep {
		delete(r.ports, ep.addr)
	}
	r.mu.Unlock()
	r.up.detach(ep)
}

// addr is where the segment's endpoints send.
func (r *relay) addr() netip.AddrPort { return r.down.LocalAddr().(*net.UDPAddr).AddrPort() }

// serve relays the segment's client messages upstream: hops + 1, giaddr when unset, and
// option 82 with the circuit id and remote id of the ingress port, and link selection.
func (r *relay) serve() {
	buf := make([]byte, 4096)
	for {
		n, from, err := r.down.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		r.mu.RLock()
		ep := r.ports[from]
		r.mu.RUnlock()
		if ep == nil {
			r.unknownIngress.Add(1)
			continue
		}
		m, err := dhcpv4.FromBytes(buf[:n])
		if err != nil || m.OpCode != dhcpv4.OpcodeBootRequest || m.HopCount >= 16 {
			continue
		}
		r.forward(ep, m)
	}
}

// serveL2 relays what the segment's bridge delivers to the relay's NIC: client broadcasts to
// port 67, placed on the port the bridge learned their source MAC on.
func (r *relay) serveL2() {
	buf := make([]byte, 9216)
	for {
		n, err := r.gw.ReadFrame(buf)
		if err != nil {
			return
		}
		f, ok := netsim.ParseUDPFrame(buf[:n])
		if !ok || f.DstPort != 67 || f.SrcPort != 68 {
			continue
		}
		m, err := dhcpv4.FromBytes(f.Payload)
		if err != nil || m.OpCode != dhcpv4.OpcodeBootRequest || m.HopCount >= 16 {
			continue
		}
		var ep *endpoint
		if ref, ok := r.gw.Ingress(f.SrcMAC); ok {
			r.mu.RLock()
			ep = r.l2ports[ref]
			r.mu.RUnlock()
		}
		if ep == nil {
			r.unknownIngress.Add(1)
			continue
		}
		r.forward(ep, m)
	}
}

// forward relays one client message from an endpoint's port upstream.
func (r *relay) forward(ep *endpoint, m *dhcpv4.DHCPv4) {
	r.requests.Add(1)
	m.HopCount++
	if m.GatewayIPAddr == nil || m.GatewayIPAddr.IsUnspecified() {
		m.GatewayIPAddr = r.up.giaddr
	}
	// A message that already carries option 82 came through another relay: forwarded as is.
	if !m.Options.Has(optRAI) {
		var link net.IP
		if r.seg.Gateway.IsValid() {
			link = r.seg.Gateway.AsSlice()
		}
		port := r.up.conn.LocalAddr().(*net.UDPAddr).Port
		if port == 67 {
			port = 0
		}
		m.Options[optRAI.Code()] = raiBytes(ep.circuit, ep.remote, link, port)
	}
	r.up.send(m)
}

// deliver sends a reply out of the endpoint's switch port. On the TAP substrate it is a frame
// from the segment gateway to the client: broadcast when the client set the broadcast flag (or
// there is no address to send to), else to chaddr at yiaddr, as RFC 2131 4.1 has a relay do.
func (r *relay) deliver(ep *endpoint, m *dhcpv4.DHCPv4) {
	r.replies.Add(1)
	if ep.l2 == nil || r.gw == nil {
		_, _ = r.down.WriteToUDPAddrPort(marshal(m), ep.addr)
		return
	}
	dstMAC, dstIP := netsim.BroadcastMAC, netip.AddrFrom4([4]byte{255, 255, 255, 255})
	if y, ok := netip.AddrFromSlice(m.YourIPAddr.To4()); ok && !y.IsUnspecified() && !m.IsBroadcast() {
		dstMAC, dstIP = m.ClientHWAddr, y
	}
	_ = r.gw.WriteFrame(netsim.UDPFrame{SrcMAC: r.gw.MAC(), DstMAC: dstMAC, SrcIP: r.seg.Gateway, DstIP: dstIP,
		SrcPort: 67, DstPort: 68, Payload: marshal(m)}.Marshal())
}
