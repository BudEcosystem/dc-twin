package netsim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// A vm host's NIC on the loopback substrate (D48). A `vm` host's virtio NIC speaks Ethernet
// frames; the loopback substrate has no segment for them, and on macOS the twin cannot make a
// TAP. So the twin is the switch port the NIC is cabled to, in userspace, and does for the NIC's
// frames what the model's switch and the site's routed network do on a site:
//
//   - DHCP (UDP to port 67) goes to the twin relay of the segment the port is cabled to, entering
//     at this port: the relay inserts the circuit id and remote id the cabling implies and link
//     selection, as it does for a BMC, and forwards to the segment's server (the stack's, for a
//     `dhcp: stack` network). The relay's replies come out of the port as frames from the
//     segment gateway: broadcast when the client asked, else to its hardware address at yiaddr,
//     as RFC 2131 4.1 has a relay deliver them.
//   - The segment gateway is a router: a userspace network stack (gVisor's netstack, the TAP
//     substrate's) holds the gateway address with the switch's MAC, answers ARP and ping for it,
//     and routes the guest's TCP to what a site routes it to: an estate endpoint at its site
//     address (a BMC's Redfish at its lease, the same routes the site gateway serves), or a
//     stack service at the site address the stack's outputs name (`siteServices`), which runs on
//     this host. UDP to a stack service is routed the same way. Nothing else is routed: the guest
//     reaches no other address, the twin's control plane or the internet (15).
//
// What it is not: the router terminates TCP and opens a new connection to the service (a
// transparent proxy, not a forwarding plane), so a service sees its peer as this host's loopback
// address, not the guest's lease; a DHCP client's unicast renewal is relayed like its broadcast
// (as the twin's own clients' are on this substrate); there is no other host on the segment; and
// IPv6, multicast and ICMP other than echo to the gateway are dropped. The report says so.

// PortRelay is the twin relay of the segment a host's port is cabled to, as that port sees it:
// the dhcp adapter implements it (package dhcp, plugPort).
type PortRelay interface {
	// Send relays a client message (the UDP payload of a BOOTREQUEST) that arrived on the port.
	Send(payload []byte) error
	// Receive blocks for the next reply the relay delivers out of the port; ErrClosed once closed.
	Receive(b []byte) (int, error)
	// Gateway is the segment's gateway and prefix, and the MAC it answers with (the switch's).
	Gateway() (netip.Prefix, net.HardwareAddr)
	// Server says what answers the segment's DHCP, for reports.
	Server() string
	Close() error
}

// RelayDirectory plugs a host's port (its ref, "device:port") into the twin relay of its
// segment, or says why it cannot.
type RelayDirectory func(ref string) (PortRelay, error)

// ErrNoRelays is PlugRelay's error while no DHCP adapter has registered its relays.
var ErrNoRelays = errors.New("no DHCP adapter runs, so no twin relay serves the segment")

// relayDirs holds each substrate's relay directory. The DHCP adapter and the vm adapter share a
// substrate (a Net) and nothing else; a directory keyed by the Net joins them without either
// importing the other.
var relayDirs sync.Map // *Net -> RelayDirectory

// SetRelayDirectory registers the DHCP adapter's relays with the substrate (nil removes them).
func (n *Net) SetRelayDirectory(d RelayDirectory) {
	if d == nil {
		relayDirs.Delete(n)
		return
	}
	relayDirs.Store(n, d)
}

// PlugRelay plugs a host's port into the twin relay of its segment.
func (n *Net) PlugRelay(ref string) (PortRelay, error) {
	v, ok := relayDirs.Load(n)
	if !ok {
		return nil, ErrNoRelays
	}
	return v.(RelayDirectory)(ref)
}

// Service is a stack service at the site address a site routes to it, and where it runs on this
// host.
type Service struct {
	Name  string
	Proto string // tcp | udp
	Site  netip.AddrPort
	Local string
}

func (s Service) String() string {
	p := ""
	if s.Proto == "udp" {
		p = "udp:"
	}
	return fmt.Sprintf("%s@%s%s=%s", s.Name, p, s.Site, s.Local)
}

// ParseServices reads a stack's `siteServices` output: entries separated by commas or white
// space, each `name@[udp:]site-ip:port=host:port` (TCP unless udp: is given), e.g.
// `pxe@10.90.0.17:80=127.0.0.1:18080`.
func ParseServices(s string) ([]Service, error) {
	var out []Service
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		name, rest, ok := strings.Cut(f, "@")
		if !ok || name == "" {
			return nil, fmt.Errorf("siteServices entry %q: want name@[udp:]site-ip:port=host:port", f)
		}
		site, local, ok := strings.Cut(rest, "=")
		if !ok {
			return nil, fmt.Errorf("siteServices entry %q: no =host:port", f)
		}
		svc := Service{Name: name, Proto: "tcp", Local: local}
		if strings.HasPrefix(site, "udp:") {
			svc.Proto, site = "udp", strings.TrimPrefix(site, "udp:")
		} else {
			site = strings.TrimPrefix(site, "tcp:")
		}
		ap, err := netip.ParseAddrPort(site)
		if err != nil || !ap.Addr().Is4() {
			return nil, fmt.Errorf("siteServices entry %q: the site address must be an IPv4 address and port", f)
		}
		if _, _, err := net.SplitHostPort(local); err != nil {
			return nil, fmt.Errorf("siteServices entry %q: %v", f, err)
		}
		svc.Site = ap
		out = append(out, svc)
	}
	return out, nil
}

// Flow is one connection (TCP) or datagram flow (UDP) the guest opened through the port's
// gateway, or one it was refused: what an observer records.
type Flow struct {
	Proto    string
	Src, Dst netip.AddrPort
	Service  string // the stack service's name, "estate endpoint", or "" when refused
	Local    string // where it was routed on this host
	Refused  string // why it was not routed
	Closed   bool
	Up, Down int64 // bytes from the guest, and to it (on close)
}

// Cable is a NIC's cable as its switch port sees it: the NIC's frames (qemu.Cable on a vm host).
type Cable interface {
	ReadFrame(b []byte) (int, error)
	WriteFrame(b []byte) error
}

// HostPortConfig is a host port's switch port and gateway.
type HostPortConfig struct {
	Ref string
	MAC net.HardwareAddr // the guest NIC's (the model's)
	// Plug plugs the port into its segment's relay; nil when the port's network has no DHCP. It
	// is retried on the guest's DHCP until it succeeds (the DHCP adapter may start later).
	Plug     func() (PortRelay, error)
	Services []Service
	// Routes is the substrate's site routes (estate endpoints at their site addresses).
	Routes func() map[string]string
	// Observe is told about every flow, and OnDHCP about every DHCP message through the port
	// ("request" from the guest, "reply" to it, with its UDP payload).
	Observe func(Flow)
	OnDHCP  func(dir string, payload []byte)
	// Uplink is the router's interface on a routed site network, if the stack has one: what the
	// guest sends there is forwarded at L3, its source address intact (uplink.go).
	Uplink *Uplink
	Log    *slog.Logger
}

// HostPort is a host NIC's switch port, segment gateway and router.
type HostPort struct {
	cfg  HostPortConfig
	cab  Cable
	st   *stack.Stack
	link *channel.Endpoint

	mu      sync.Mutex
	relay   PortRelay
	plugErr string
	gw      netip.Prefix
	gwMAC   net.HardwareAddr
	udp     map[udpKey]*udpFlow
	closed  chan struct{}
	once    sync.Once

	// counters for the report
	DHCPRequests, DHCPReplies, TCPFlows, TCPRefused, UDPFlows, UDPRefused, Dropped atomic.Int64
}

type udpKey struct {
	src  netip.AddrPort
	site netip.Addr
}

type udpFlow struct {
	hp   *HostPort
	key  udpKey
	conn *net.UDPConn
	svc  string
	up   atomic.Int64
	down atomic.Int64
	last atomic.Int64 // unix nanoseconds of the last datagram either way
}

const hostNIC tcpip.NICID = 1

// NewHostPort starts a host port on a cable (the NIC's frames). The gateway goes live when the
// port is plugged into its segment's relay, which names it.
func NewHostPort(cab Cable, cfg HostPortConfig) (*HostPort, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	hp := &HostPort{cfg: cfg, cab: cab, udp: map[udpKey]*udpFlow{}, closed: make(chan struct{})}
	hp.st = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, icmp.NewProtocol4},
	})
	hp.link = channel.New(512, 1500, "")
	if terr := hp.st.CreateNIC(hostNIC, ethernet.New(hp.link)); terr != nil {
		return nil, fmt.Errorf("%s: gateway NIC: %s", cfg.Ref, terr)
	}
	// the gateway routes: it takes packets for any address (promiscuous) and answers from the
	// address the guest addressed (spoofing), which is what makes it the site's router
	hp.st.SetPromiscuousMode(hostNIC, true)
	hp.st.SetSpoofing(hostNIC, true)
	hp.st.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: hostNIC}})
	sack := tcpip.TCPSACKEnabled(true)
	hp.st.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	fwd := tcp.NewForwarder(hp.st, 0, 256, hp.forwardTCP)
	hp.st.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	if cfg.Plug != nil {
		hp.plug()
	}
	go hp.fromGuest()
	go hp.toGuest()
	go hp.expireUDP()
	return hp, nil
}

// plug connects the port to its segment's relay, once.
func (hp *HostPort) plug() PortRelay {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	if hp.relay != nil || hp.cfg.Plug == nil {
		return hp.relay
	}
	r, err := hp.cfg.Plug()
	if err != nil {
		hp.plugErr = err.Error()
		return nil
	}
	hp.relay, hp.plugErr = r, ""
	gw, mac := r.Gateway()
	if gw.IsValid() && gw.Addr().Is4() {
		awp := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(gw.Addr().As4()), PrefixLen: gw.Bits()}
		if terr := hp.st.AddProtocolAddress(hostNIC, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: awp}, stack.AddressProperties{}); terr == nil {
			hp.gw = gw
		}
	}
	if len(mac) == 6 {
		hp.gwMAC = mac
		hp.link.SetLinkAddress(tcpip.LinkAddress(mac))
	}
	go hp.fromRelay(r)
	return r
}

// Gateway is the segment gateway the port answers as (invalid until plugged).
func (hp *HostPort) Gateway() netip.Prefix {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	return hp.gw
}

// Relay says what serves the port's DHCP, or why nothing does.
func (hp *HostPort) Relay() (server, why string) {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	if hp.relay != nil {
		return hp.relay.Server(), ""
	}
	if hp.cfg.Plug == nil {
		return "", "its network has no DHCP"
	}
	return "", hp.plugErr
}

// fromGuest dispatches the guest's frames: DHCP to the relay, UDP to the stack's services, ARP,
// TCP and ICMP to the gateway's stack; anything else is dropped.
func (hp *HostPort) fromGuest() {
	buf := make([]byte, 65536)
	for {
		n, err := hp.cab.ReadFrame(buf)
		if err != nil {
			return
		}
		frame := buf[:n]
		if n < ethHeader {
			hp.Dropped.Add(1)
			continue
		}
		if hp.routed(frame) {
			continue
		}
		if f, ok := ParseUDPFrame(frame); ok {
			if f.DstPort == 67 {
				hp.dhcpRequest(f)
			} else {
				hp.routeUDP(f)
			}
			continue
		}
		switch binary.BigEndian.Uint16(frame[12:14]) {
		case 0x0806:
		case 0x0800:
			if !hp.admitIPv4(frame[ethHeader:]) {
				hp.Dropped.Add(1)
				continue
			}
		default:
			hp.Dropped.Add(1) // IPv6, LLDP …: nothing on this segment speaks them
			continue
		}
		if !hp.Gateway().IsValid() {
			hp.Dropped.Add(1)
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), frame...))})
		hp.link.InjectInbound(0, pkt)
		pkt.DecRef()
	}
}

// routed forwards a guest's IPv4 packet to the routed site network at L3 when its destination
// is there (and is not a service the stack runs on this host, which the proxy serves).
func (hp *HostPort) routed(frame []byte) bool {
	u := hp.cfg.Uplink
	if u == nil || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 || len(frame) < ethHeader+ipv4Header {
		return false
	}
	ip := frame[ethHeader:]
	dst := netip.AddrFrom4([4]byte(ip[16:20]))
	if !u.Routes(dst) {
		return false
	}
	if ihl := int(ip[0]&0x0f) * 4; (ip[9] == 6 || ip[9] == 17) && len(ip) >= ihl+4 {
		proto := map[byte]string{6: "tcp", 17: "udp"}[ip[9]]
		ap := netip.AddrPortFrom(dst, binary.BigEndian.Uint16(ip[ihl+2:ihl+4]))
		for _, s := range hp.cfg.Services {
			if s.Proto == proto && s.Site == ap {
				return false
			}
		}
		if ip[9] == 17 && binary.BigEndian.Uint16(ip[ihl+2:ihl+4]) == 67 {
			return false // DHCP is the relay's
		}
	}
	u.forward(ip)
	return true
}

// deliverIPv4 sends a routed packet out of the port to the guest.
func (hp *HostPort) deliverIPv4(ip []byte) {
	hp.mu.Lock()
	gwMAC := hp.gwMAC
	hp.mu.Unlock()
	f := make([]byte, ethHeader+len(ip))
	copy(f[0:6], hp.cfg.MAC)
	copy(f[6:12], gwMAC)
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	copy(f[ethHeader:], ip)
	_ = hp.cab.WriteFrame(f)
}

// dhcpACK returns yiaddr when a BOOTREPLY is a DHCPACK (option 53 = 5).
func dhcpACK(m []byte) (netip.Addr, bool) {
	for i := 240; i < len(m); {
		code := m[i]
		if code == 255 {
			break
		}
		if code == 0 {
			i++
			continue
		}
		if i+1 >= len(m) {
			break
		}
		l := int(m[i+1])
		if i+2+l > len(m) {
			break
		}
		if code == 53 && l == 1 {
			if m[i+2] != 5 {
				return netip.Addr{}, false
			}
			yi := netip.AddrFrom4([4]byte(m[16:20]))
			return yi, !yi.IsUnspecified()
		}
		i += 2 + l
	}
	return netip.Addr{}, false
}

// admitIPv4 keeps the router honest in promiscuous mode: TCP is routed or refused by the
// forwarder, but an ICMP echo is answered only for the gateway, where a promiscuous stack would
// answer it for any address.
func (hp *HostPort) admitIPv4(ip []byte) bool {
	if len(ip) < ipv4Header || ip[0]>>4 != 4 {
		return false
	}
	switch ip[9] {
	case 6: // TCP
		return true
	case 1: // ICMP
		dst := netip.AddrFrom4([4]byte(ip[16:20]))
		return dst == hp.Gateway().Addr()
	}
	return false
}

// toGuest writes what the gateway's stack sends (ARP, TCP, ICMP) to the guest.
func (hp *HostPort) toGuest() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-hp.closed; cancel() }()
	for {
		pkt := hp.link.ReadContext(ctx)
		if pkt == nil {
			return
		}
		var b []byte
		for _, s := range pkt.AsSlices() {
			b = append(b, s...)
		}
		pkt.DecRef()
		_ = hp.cab.WriteFrame(b)
	}
}

func (hp *HostPort) dhcpRequest(f UDPFrame) {
	r := hp.plug()
	if r == nil {
		hp.Dropped.Add(1)
		return
	}
	hp.DHCPRequests.Add(1)
	if hp.cfg.OnDHCP != nil {
		hp.cfg.OnDHCP("request", f.Payload)
	}
	_ = r.Send(f.Payload)
}

// fromRelay delivers the relay's replies out of the port, as a relay delivers on a segment.
func (hp *HostPort) fromRelay(r PortRelay) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Receive(buf)
		if err != nil {
			return
		}
		m := buf[:n]
		if len(m) < 240 || m[0] != 2 {
			continue
		}
		hp.DHCPReplies.Add(1)
		if yi, ok := dhcpACK(m); ok && hp.cfg.Uplink != nil {
			hp.cfg.Uplink.attach(yi, hp) // the routed network's answers to this lease come out here
		}
		if hp.cfg.OnDHCP != nil {
			hp.cfg.OnDHCP("reply", append([]byte(nil), m...))
		}
		hp.mu.Lock()
		gw, gwMAC := hp.gw, hp.gwMAC
		hp.mu.Unlock()
		dstMAC, dstIP := BroadcastMAC, netip.AddrFrom4([4]byte{255, 255, 255, 255})
		yi := netip.AddrFrom4([4]byte(m[16:20]))
		broadcast := binary.BigEndian.Uint16(m[10:12])&0x8000 != 0
		if !yi.IsUnspecified() && !broadcast {
			dstMAC, dstIP = net.HardwareAddr(append([]byte(nil), m[28:34]...)), yi
		}
		src := gw.Addr()
		if !src.IsValid() {
			src = netip.AddrFrom4([4]byte(m[24:28])) // giaddr
		}
		_ = hp.cab.WriteFrame(UDPFrame{SrcMAC: gwMAC, DstMAC: dstMAC, SrcIP: src, DstIP: dstIP, SrcPort: 67, DstPort: 68,
			Payload: append([]byte(nil), m...)}.Marshal())
	}
}

// route finds where a site address is served: a stack service, else an estate endpoint.
func (hp *HostPort) route(proto string, dst netip.AddrPort) (local, name, why string) {
	for _, s := range hp.cfg.Services {
		if s.Proto == proto && s.Site == dst {
			return s.Local, s.Name, ""
		}
	}
	if proto == "tcp" && hp.cfg.Routes != nil {
		if l, ok := hp.cfg.Routes()[dst.String()]; ok {
			return l, "estate endpoint", ""
		}
	}
	return "", "", "no route: " + dst.String() + " is neither a stack service the stack's outputs name (siteServices) nor an estate endpoint"
}

func (hp *HostPort) observe(f Flow) {
	if hp.cfg.Observe != nil {
		hp.cfg.Observe(f)
	}
}

func addrPort(a tcpip.Address, port uint16) netip.AddrPort {
	b := a.As4()
	return netip.AddrPortFrom(netip.AddrFrom4(b), port)
}

// forwardTCP is the router for one TCP connection the guest opened: it dials the service first,
// so a site address that routes nowhere, or a service that is down, refuses the guest's SYN with
// a reset as a closed port does.
func (hp *HostPort) forwardTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	src, dst := addrPort(id.RemoteAddress, id.RemotePort), addrPort(id.LocalAddress, id.LocalPort)
	local, name, why := hp.route("tcp", dst)
	if local == "" {
		hp.TCPRefused.Add(1)
		r.Complete(true)
		hp.observe(Flow{Proto: "tcp", Src: src, Dst: dst, Refused: why})
		return
	}
	up, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		hp.TCPRefused.Add(1)
		r.Complete(true)
		hp.observe(Flow{Proto: "tcp", Src: src, Dst: dst, Service: name, Local: local, Refused: err.Error()})
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		up.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	hp.TCPFlows.Add(1)
	hp.observe(Flow{Proto: "tcp", Src: src, Dst: dst, Service: name, Local: local})
	g := gonet.NewTCPConn(&wq, ep)
	var upN, downN int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		upN, _ = io.Copy(up, g)
		if c, ok := up.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		downN, _ = io.Copy(g, up)
		_ = g.CloseWrite()
	}()
	go func() {
		select {
		case <-hp.closed:
			up.Close()
			g.Close()
		case <-waitDone(&wg):
		}
	}()
	wg.Wait()
	up.Close()
	g.Close()
	hp.observe(Flow{Proto: "tcp", Src: src, Dst: dst, Service: name, Local: local, Closed: true, Up: upN, Down: downN})
}

// routeUDP is the router for a guest datagram to a stack service: one host socket per flow (the
// guest's address and port, and the service's site address), and the service's answers back to
// the guest from the site address. An answer from another port of the service's host comes back
// from that port of the site address, and the guest's datagrams to it reach it: what a TFTP
// server's transfer port needs.
func (hp *HostPort) routeUDP(f UDPFrame) {
	src, dst := netip.AddrPortFrom(f.SrcIP, f.SrcPort), netip.AddrPortFrom(f.DstIP, f.DstPort)
	local, name, why := hp.route("udp", dst)
	if local == "" {
		for _, s := range hp.cfg.Services {
			if s.Proto == "udp" && s.Site.Addr() == dst.Addr() {
				h, _, _ := net.SplitHostPort(s.Local)
				local, name, why = net.JoinHostPort(h, fmt.Sprint(dst.Port())), s.Name, ""
				break
			}
		}
	}
	key := udpKey{src, dst.Addr()}
	hp.mu.Lock()
	fl := hp.udp[key]
	hp.mu.Unlock()
	if local == "" && fl == nil {
		hp.UDPRefused.Add(1)
		hp.observe(Flow{Proto: "udp", Src: src, Dst: dst, Refused: why})
		return
	}
	if fl == nil {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			hp.UDPRefused.Add(1)
			return
		}
		fl = &udpFlow{hp: hp, key: key, conn: c, svc: name}
		hp.mu.Lock()
		hp.udp[key] = fl
		hp.mu.Unlock()
		hp.UDPFlows.Add(1)
		hp.observe(Flow{Proto: "udp", Src: src, Dst: dst, Service: name, Local: local})
		go fl.read()
	}
	if local == "" {
		return
	}
	ua, err := net.ResolveUDPAddr("udp4", local)
	if err != nil {
		return
	}
	fl.last.Store(time.Now().UnixNano())
	fl.up.Add(int64(len(f.Payload)))
	_, _ = fl.conn.WriteToUDP(f.Payload, ua)
}

func (fl *udpFlow) read() {
	hp := fl.hp
	buf := make([]byte, 65536)
	for {
		n, from, err := fl.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		port := from.Port()
		for _, s := range hp.cfg.Services {
			if s.Proto == "udp" && s.Local == from.String() {
				port = s.Site.Port()
			}
		}
		fl.last.Store(time.Now().UnixNano())
		fl.down.Add(int64(n))
		hp.mu.Lock()
		gwMAC := hp.gwMAC
		hp.mu.Unlock()
		_ = hp.cab.WriteFrame(UDPFrame{SrcMAC: gwMAC, DstMAC: hp.cfg.MAC, SrcIP: fl.key.site, DstIP: fl.key.src.Addr(),
			SrcPort: port, DstPort: fl.key.src.Port(), Payload: append([]byte(nil), buf[:n]...)}.Marshal())
	}
}

// expireUDP closes UDP flows idle for two minutes.
func (hp *HostPort) expireUDP() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-hp.closed:
			return
		case <-t.C:
		}
		cut := time.Now().Add(-2 * time.Minute).UnixNano()
		hp.mu.Lock()
		for k, fl := range hp.udp {
			if fl.last.Load() < cut {
				delete(hp.udp, k)
				fl.conn.Close()
				hp.observe(Flow{Proto: "udp", Src: k.src, Dst: netip.AddrPortFrom(k.site, 0), Service: fl.svc, Closed: true, Up: fl.up.Load(), Down: fl.down.Load()})
			}
		}
		hp.mu.Unlock()
	}
}

// Services lists the stack services the port routes to, sorted, for reports.
func (hp *HostPort) Services() []string {
	var out []string
	for _, s := range hp.cfg.Services {
		out = append(out, s.String())
	}
	sort.Strings(out)
	return out
}

// Close stops the port: its relay attachment, its flows and its stack.
func (hp *HostPort) Close() error {
	hp.once.Do(func() {
		close(hp.closed)
		hp.mu.Lock()
		r := hp.relay
		for k, fl := range hp.udp {
			fl.conn.Close()
			delete(hp.udp, k)
		}
		hp.mu.Unlock()
		if r != nil {
			_ = r.Close()
		}
		if hp.cfg.Uplink != nil {
			hp.cfg.Uplink.detach(hp)
		}
		hp.link.Close()
		hp.st.Close()
	})
	return nil
}

func waitDone(wg *sync.WaitGroup) <-chan struct{} {
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	return ch
}
