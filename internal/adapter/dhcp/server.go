package dhcp

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// twinServer is the twin site DHCP (03 — Addressing, `dhcp: twin`): for an estate with no stack
// serving DHCP, a labelled server the twin provides. It is a real DHCPv4 server on its own
// socket, reached only through the twin relays, and like the relayed-only servers stacks run
// (NICo's Kea hook) it answers relayed requests only. It selects a segment by link selection,
// else giaddr, and hands each port's own client the address its cabling implies (staticOffset);
// a request from a port it cannot place, or from a second client behind a port (a BlueField's
// Arm behind its BMC's), gets the first free address of a dynamic range instead.
type twinServer struct {
	a     *Adapter
	plan  *addressing
	conn  *net.UDPConn
	id    net.IP
	lease time.Duration

	segs []*segment // this network's segments

	mu     sync.Mutex
	byMAC  map[string]netip.Addr // chaddr -> address bound or offered
	byAddr map[netip.Addr]string

	offers, acks, naks, unrelayed, unplaced atomic.Int64
}

func newTwinServer(a *Adapter, plan *addressing, conn *net.UDPConn, segs []*segment) *twinServer {
	_ = conn.SetReadBuffer(4 << 20)
	return &twinServer{a: a, plan: plan, conn: conn, id: plan.serverID().AsSlice(), lease: a.leaseTime, segs: segs,
		byMAC: map[string]netip.Addr{}, byAddr: map[netip.Addr]string{}}
}

func (s *twinServer) serve() {
	buf := make([]byte, 4096)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		m, err := dhcpv4.FromBytes(buf[:n])
		if err != nil || m.OpCode != dhcpv4.OpcodeBootRequest {
			continue
		}
		if reply := s.handle(m); reply != nil {
			// a relayed reply goes back to the relay that sent it (its giaddr, on its port)
			_, _ = s.conn.WriteToUDP(marshal(reply), from)
		}
	}
}

// segmentOf picks the segment a relayed request belongs to: RFC 3527 link selection, else
// giaddr.
func (s *twinServer) segmentOf(m *dhcpv4.DHCPv4) (*segment, string) {
	var sel netip.Addr
	var circuit string
	if rai := m.Options.Get(optRAI); rai != nil {
		so := subOptions(rai)
		if ls := so[subLinkSelection]; len(ls) == 4 {
			sel = netip.AddrFrom4([4]byte(ls))
		}
		circuit = string(so[subCircuitID])
	}
	if !sel.IsValid() {
		if a, ok := netip.AddrFromSlice(m.GatewayIPAddr.To4()); ok {
			sel = a
		}
	}
	for _, sg := range s.segs {
		if sg.Prefix.IsValid() && sg.Prefix.Contains(sel) {
			// in a shared CIDR every segment matches: the circuit id picks the rack's
			if s.plan.scheme == schemeShared && circuit != "" && !sg.hasCircuit(circuit) {
				continue
			}
			return sg, circuit
		}
	}
	return nil, circuit
}

func (sg *segment) hasCircuit(c string) bool {
	sg.relay.up.mu.RLock()
	defer sg.relay.up.mu.RUnlock()
	for _, ep := range sg.relay.up.byCircuit[c] {
		return ep.seg == sg // every client behind a port is on the port's segment
	}
	return false
}

func (s *twinServer) handle(m *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	if m.GatewayIPAddr == nil || m.GatewayIPAddr.IsUnspecified() {
		s.unrelayed.Add(1)
		return nil
	}
	sg, circuit := s.segmentOf(m)
	if sg == nil {
		s.unplaced.Add(1)
		return nil
	}
	mac := m.ClientHWAddr.String()
	switch m.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		ip, ok := s.allocate(sg, mac, circuit)
		if !ok {
			s.unplaced.Add(1)
			return nil
		}
		s.offers.Add(1)
		return s.reply(m, sg, dhcpv4.MessageTypeOffer, ip)
	case dhcpv4.MessageTypeRequest:
		if sid := m.ServerIdentifier(); sid != nil && !sid.Equal(s.id) {
			s.release(mac) // the client chose another server's offer
			return nil
		}
		want := m.RequestedIPAddress()
		if want == nil || want.IsUnspecified() {
			want = m.ClientIPAddr // RENEWING / REBINDING
		}
		ip, ok := s.allocate(sg, mac, circuit)
		w, _ := netip.AddrFromSlice(want.To4())
		if !ok || w != ip {
			s.naks.Add(1)
			s.release(mac)
			return s.reply(m, sg, dhcpv4.MessageTypeNak, netip.Addr{})
		}
		s.acks.Add(1)
		return s.reply(m, sg, dhcpv4.MessageTypeAck, ip)
	case dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline:
		s.release(mac)
	}
	return nil
}

// allocate returns the address for a client in a segment: the one it holds, else the one its
// port's cabling implies (for the port's own client), else the first free one from .200.
func (s *twinServer) allocate(sg *segment, mac, circuit string) (netip.Addr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ip, ok := s.byMAC[mac]; ok && sg.Prefix.Contains(ip) {
		return ip, true
	}
	if ip, ok := s.byMAC[mac]; ok {
		delete(s.byAddr, ip) // moved to another segment
		delete(s.byMAC, mac)
	}
	take := func(off int) (netip.Addr, bool) {
		ip, ok := hostAt(sg.Prefix, off)
		if !ok {
			return ip, false
		}
		if owner, used := s.byAddr[ip]; used && owner != mac {
			return ip, false
		}
		s.byMAC[mac], s.byAddr[ip] = ip, mac
		return ip, true
	}
	if s.plan.scheme != schemeShared {
		// the cabling's address belongs to the port's own client (a BMC's port to the BMC): a
		// second client behind the port (a BlueField's Arm behind its BMC's) has no offset and
		// leases from the dynamic range, whichever of the two asks first
		sg.relay.up.mu.RLock()
		ep := sg.relay.up.client(circuit, mac)
		sg.relay.up.mu.RUnlock()
		if ep != nil && ep.offset > 0 {
			if ip, ok := take(ep.offset); ok {
				return ip, true
			}
		}
	}
	size := 1 << (32 - sg.Prefix.Bits())
	for off := 200; off < size-1; off++ {
		if ip, ok := take(off); ok {
			return ip, true
		}
	}
	for off := 10; off < 200 && off < size-1; off++ {
		if ip, ok := take(off); ok {
			return ip, true
		}
	}
	return netip.Addr{}, false
}

func (s *twinServer) release(mac string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ip, ok := s.byMAC[mac]; ok {
		delete(s.byAddr, ip)
		delete(s.byMAC, mac)
	}
}

// hostAt is the address off hosts into a prefix, refusing the network and broadcast addresses.
func hostAt(p netip.Prefix, off int) (netip.Addr, bool) {
	size := 1 << (32 - p.Bits())
	if off <= 0 || off >= size-1 {
		return netip.Addr{}, false
	}
	b := p.Addr().As4()
	binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(b[:])+uint32(off))
	return netip.AddrFrom4(b), true
}

func (s *twinServer) reply(req *dhcpv4.DHCPv4, sg *segment, t dhcpv4.MessageType, ip netip.Addr) *dhcpv4.DHCPv4 {
	mods := []dhcpv4.Modifier{dhcpv4.WithMessageType(t), dhcpv4.WithOption(dhcpv4.OptServerIdentifier(s.id))}
	if t != dhcpv4.MessageTypeNak {
		mods = append(mods, dhcpv4.WithYourIP(ip.AsSlice()),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(sg.Prefix.Bits(), 32))),
			dhcpv4.WithOption(dhcpv4.OptRouter(sg.Gateway.AsSlice())),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(s.lease)),
			dhcpv4.WithOption(dhcpv4.OptRenewTimeValue(s.lease/2)),
			dhcpv4.WithOption(dhcpv4.OptRebindingTimeValue(s.lease*7/8)))
	}
	r, err := dhcpv4.NewReplyFromRequest(req, mods...) // copies giaddr, option 82 and option 61
	if err != nil {
		return nil
	}
	return r
}
