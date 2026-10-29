package nicoagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
)

// The DPU's out-of-band NIC (oob_net0), as the stand-in's software sees it. A BlueField's Arm
// reaches the site through its OOB port, which shares the BMC's switch port (D52); the stand-in
// has no QEMU NIC, so the twin gives it a userspace one: a network stack (gVisor's netstack,
// the one netsim's gateway uses) with the MAC the BMC serves for oob0, cabled in memory to a
// netsim.HostPort at the BMC's switch port. So the stand-in's traffic takes the path a booted
// DPU's does: its DHCP enters the twin relay of that port as a second client behind it (the
// stack's DHCP server sees the OOB MAC with the port's circuit id), and its TCP leaves through
// the segment gateway from its own lease, routed at L3 with that source address where the stack
// runs on a routed site network (netsim.Uplink) and proxied from this host's loopback address
// where it does not.

// memCable is the in-memory cable between the guest NIC and its switch port: frames the guest
// sends are read by the port (netsim.Cable), frames the port writes reach the guest.
type memCable struct {
	toPort  chan []byte
	toGuest func([]byte)
	closed  chan struct{}
	once    sync.Once
}

var errCableClosed = errors.New("cable closed")

func (c *memCable) ReadFrame(b []byte) (int, error) {
	select {
	case f := <-c.toPort:
		return copy(b, f), nil
	case <-c.closed:
		return 0, errCableClosed
	}
}

func (c *memCable) WriteFrame(b []byte) error {
	select {
	case <-c.closed:
		return errCableClosed
	default:
	}
	c.toGuest(append([]byte(nil), b...))
	return nil
}

func (c *memCable) send(f []byte) {
	select {
	case c.toPort <- f:
	case <-c.closed:
	default: // a full queue drops the frame, as a NIC's ring does
	}
}

func (c *memCable) close() { c.once.Do(func() { close(c.closed) }) }

const guestNIC tcpip.NICID = 1

// nic is the guest's NIC and network stack.
type nic struct {
	mac   net.HardwareAddr
	cable *memCable
	port  *netsim.HostPort
	st    *stack.Stack
	link  *channel.Endpoint

	dhcp chan []byte // DHCP replies to the guest's port 68

	mu     sync.Mutex
	addr   netip.Prefix
	router netip.Addr
}

// newNIC makes a NIC with mac cabled to a switch port that cfg describes.
func newNIC(mac net.HardwareAddr, cfg netsim.HostPortConfig) (*nic, error) {
	n := &nic{mac: mac, dhcp: make(chan []byte, 16)}
	n.cable = &memCable{toPort: make(chan []byte, 256), closed: make(chan struct{}), toGuest: n.fromPort}
	n.st = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	n.link = channel.New(512, 1500, tcpip.LinkAddress(mac))
	if terr := n.st.CreateNIC(guestNIC, ethernet.New(n.link)); terr != nil {
		return nil, fmt.Errorf("guest NIC: %s", terr)
	}
	cfg.MAC = mac
	hp, err := netsim.NewHostPort(n.cable, cfg)
	if err != nil {
		return nil, err
	}
	n.port = hp
	go n.toPortLoop()
	return n, nil
}

// fromPort takes a frame the switch port delivered: DHCP to the guest's client, the rest to its
// network stack.
func (n *nic) fromPort(f []byte) {
	if u, ok := netsim.ParseUDPFrame(f); ok && u.DstPort == 68 {
		select {
		case n.dhcp <- u.Payload:
		default:
		}
		return
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(f)})
	n.link.InjectInbound(0, pkt)
	pkt.DecRef()
}

func (n *nic) toPortLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-n.cable.closed; cancel() }()
	for {
		pkt := n.link.ReadContext(ctx)
		if pkt == nil {
			return
		}
		var b []byte
		for _, s := range pkt.AsSlices() {
			b = append(b, s...)
		}
		pkt.DecRef()
		n.cable.send(b)
	}
}

func (n *nic) close() {
	n.cable.close()
	if n.port != nil {
		n.port.Close()
	}
	n.st.Close()
}

// lease is what a DHCPACK gave the guest.
type lease struct {
	ack    *dhcpv4.DHCPv4
	offer  *dhcpv4.DHCPv4
	addr   netip.Prefix
	router netip.Addr
	server string
}

// acquire runs DHCP (DISCOVER, OFFER, REQUEST, ACK) with the client options mods add, retrying
// each exchange as a firmware does, and configures the stack with the address it is given.
func (n *nic) acquire(ctx context.Context, tries int, mods ...dhcpv4.Modifier) (*lease, error) {
	var last error
	for i := 0; i < tries; i++ {
		l, err := n.exchange(ctx, mods...)
		if err == nil {
			n.configure(l)
			return l, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		last = err
	}
	return nil, last
}

var errNAK = errors.New("the server answered NAK")

func (n *nic) exchange(ctx context.Context, mods ...dhcpv4.Modifier) (*lease, error) {
	disc, err := dhcpv4.NewDiscovery(n.mac, append([]dhcpv4.Modifier{dhcpv4.WithBroadcast(true)}, mods...)...)
	if err != nil {
		return nil, err
	}
	n.drain()
	n.broadcast(disc)
	offer, err := n.wait(ctx, disc.TransactionID, dhcpv4.MessageTypeOffer, 4*time.Second)
	if err != nil {
		return nil, fmt.Errorf("no DHCPOFFER: %w", err)
	}
	req, err := dhcpv4.NewRequestFromOffer(offer, append([]dhcpv4.Modifier{dhcpv4.WithBroadcast(true)}, mods...)...)
	if err != nil {
		return nil, err
	}
	n.broadcast(req)
	ack, err := n.wait(ctx, req.TransactionID, dhcpv4.MessageTypeAck, 4*time.Second)
	if err != nil {
		return nil, fmt.Errorf("no DHCPACK: %w", err)
	}
	ip, ok := netip.AddrFromSlice(ack.YourIPAddr.To4())
	if !ok {
		return nil, errors.New("the DHCPACK gave no address")
	}
	bits := 24
	if m := ack.SubnetMask(); m != nil {
		bits, _ = m.Size()
	}
	l := &lease{ack: ack, offer: offer, addr: netip.PrefixFrom(ip, bits)}
	if r := ack.Router(); len(r) > 0 {
		l.router, _ = netip.AddrFromSlice(r[0].To4())
	}
	if sid := ack.ServerIdentifier(); sid != nil {
		l.server = sid.String()
	}
	return l, nil
}

func (n *nic) drain() {
	for {
		select {
		case <-n.dhcp:
		default:
			return
		}
	}
}

func (n *nic) broadcast(m *dhcpv4.DHCPv4) {
	n.cable.send(netsim.UDPFrame{SrcMAC: n.mac, DstMAC: netsim.BroadcastMAC, SrcIP: netip.IPv4Unspecified(),
		DstIP: netip.AddrFrom4([4]byte{255, 255, 255, 255}), SrcPort: 68, DstPort: 67, Payload: m.ToBytes()}.Marshal())
}

func (n *nic) wait(ctx context.Context, xid dhcpv4.TransactionID, want dhcpv4.MessageType, d time.Duration) (*dhcpv4.DHCPv4, error) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-n.cable.closed:
			return nil, errCableClosed
		case <-t.C:
			return nil, errors.New("timed out")
		case b := <-n.dhcp:
			m, err := dhcpv4.FromBytes(b)
			if err != nil || m.TransactionID != xid || m.ClientHWAddr.String() != n.mac.String() {
				continue
			}
			switch m.MessageType() {
			case want:
				return m, nil
			case dhcpv4.MessageTypeNak:
				return nil, errNAK
			}
		}
	}
}

// configure gives the stack the leased address and a default route through the router (or the
// segment gateway the port answers as, when the server named none).
func (n *nic) configure(l *lease) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.addr == l.addr && n.router == l.router {
		return
	}
	if n.addr.IsValid() {
		_ = n.st.RemoveAddress(guestNIC, tcpip.AddrFrom4(n.addr.Addr().As4()))
	}
	awp := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(l.addr.Addr().As4()), PrefixLen: l.addr.Bits()}
	_ = n.st.AddProtocolAddress(guestNIC, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: awp}, stack.AddressProperties{})
	gw := l.router
	if !gw.IsValid() {
		gw = n.port.Gateway().Addr()
	}
	routes := []tcpip.Route{{Destination: subnetOf(l.addr), NIC: guestNIC}}
	if gw.IsValid() {
		routes = append(routes, tcpip.Route{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(gw.As4()), NIC: guestNIC})
	}
	n.st.SetRouteTable(routes)
	n.addr, n.router = l.addr, l.router
}

func subnetOf(p netip.Prefix) tcpip.Subnet {
	m := p.Masked()
	s, _ := tcpip.NewSubnet(tcpip.AddrFrom4(m.Addr().As4()), tcpip.MaskFromBytes(net.CIDRMask(p.Bits(), 32)))
	return s
}

// dial opens a TCP connection from the guest's address. The address must be an IPv4 address
// and port: the stand-in has no resolver, and NICo hands its clients addresses.
func (n *nic) dial(ctx context.Context, _, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() {
		return nil, fmt.Errorf("%s: the DPU's network has no resolver; NICo must name its services by IPv4 address", address)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}
	return gonet.DialContextTCP(ctx, n.st, tcpip.FullAddress{NIC: guestNIC, Addr: tcpip.AddrFrom4(ip.As4()), Port: uint16(p)}, ipv4.ProtocolNumber)
}

func (n *nic) address() netip.Prefix {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.addr
}
