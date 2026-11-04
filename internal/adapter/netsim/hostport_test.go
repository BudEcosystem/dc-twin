package netsim

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
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
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// memCable is a NIC's cable in memory: what the guest writes, the port reads, and back.
type memCable struct {
	toPort, toGuest chan []byte
	done            chan struct{}
	once            sync.Once
}

func newMemCable() *memCable {
	return &memCable{toPort: make(chan []byte, 256), toGuest: make(chan []byte, 256), done: make(chan struct{})}
}

func (c *memCable) ReadFrame(b []byte) (int, error) {
	select {
	case f := <-c.toPort:
		return copy(b, f), nil
	case <-c.done:
		return 0, io.EOF
	}
}

func (c *memCable) WriteFrame(b []byte) error {
	select {
	case c.toGuest <- append([]byte(nil), b...):
	case <-c.done:
	}
	return nil
}

func (c *memCable) close() { c.once.Do(func() { close(c.done) }) }

// fakeRelay is a segment's relay as the port sees it.
type fakeRelay struct {
	sent    chan []byte
	replies chan []byte
	closed  chan struct{}
	once    sync.Once
}

func (r *fakeRelay) Send(p []byte) error { r.sent <- append([]byte(nil), p...); return nil }
func (r *fakeRelay) Receive(b []byte) (int, error) {
	select {
	case m := <-r.replies:
		return copy(b, m), nil
	case <-r.closed:
		return 0, ErrClosed
	}
}
func (r *fakeRelay) Gateway() (netip.Prefix, net.HardwareAddr) {
	return netip.MustParsePrefix("10.40.2.1/24"), net.HardwareAddr{0x02, 0, 0, 0, 0, 0x01}
}
func (r *fakeRelay) Server() string { return "the stack's DHCP at 127.0.0.1:67" }
func (r *fakeRelay) Close() error   { r.once.Do(func() { close(r.closed) }); return nil }

var guestMAC = net.HardwareAddr{0x3c, 0x6d, 0x66, 0, 0, 0x07}

// bootp builds a minimal BOOTP message: op, flags, yiaddr, chaddr and the magic cookie.
func bootp(op byte, broadcast bool, yiaddr netip.Addr) []byte {
	m := make([]byte, 240)
	m[0], m[1], m[2] = op, 1, 6
	if broadcast {
		binary.BigEndian.PutUint16(m[10:12], 0x8000)
	}
	if yiaddr.IsValid() {
		a := yiaddr.As4()
		copy(m[16:20], a[:])
	}
	copy(m[28:34], guestMAC)
	copy(m[236:240], []byte{99, 130, 83, 99})
	return m
}

func startPort(t *testing.T, svcs []Service, routes map[string]string) (*memCable, *fakeRelay, *HostPort, chan Flow) {
	t.Helper()
	cab := newMemCable()
	relay := &fakeRelay{sent: make(chan []byte, 8), replies: make(chan []byte, 8), closed: make(chan struct{})}
	flows := make(chan Flow, 64)
	hp, err := NewHostPort(cab, HostPortConfig{Ref: "site-1/a03/host:p0", MAC: guestMAC, Services: svcs,
		Plug:    func() (PortRelay, error) { return relay, nil },
		Routes:  func() map[string]string { return routes },
		Observe: func(f Flow) { flows <- f }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hp.Close(); cab.close() })
	return cab, relay, hp, flows
}

// The guest's DHCP enters the segment's relay at its port, and the relay's answer comes out of
// the port as a relay delivers it: to the client's hardware address at yiaddr, from the gateway.
func TestHostPortRelaysTheGuestsDHCP(t *testing.T) {
	cab, relay, hp, _ := startPort(t, nil, nil)
	discover := bootp(1, false, netip.Addr{})
	cab.toPort <- UDPFrame{SrcMAC: guestMAC, DstMAC: BroadcastMAC, SrcIP: netip.IPv4Unspecified(),
		DstIP: netip.AddrFrom4([4]byte{255, 255, 255, 255}), SrcPort: 68, DstPort: 67, Payload: discover}.Marshal()
	select {
	case got := <-relay.sent:
		if !bytes.Equal(got, discover) {
			t.Fatal("the relay got another message than the guest sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest's DISCOVER did not reach the relay")
	}
	relay.replies <- bootp(2, false, netip.MustParseAddr("10.40.2.10"))
	f := guestFrame(t, cab)
	if f.DstMAC.String() != guestMAC.String() || f.DstIP.String() != "10.40.2.10" || f.SrcIP.String() != "10.40.2.1" || f.SrcPort != 67 || f.DstPort != 68 {
		t.Fatalf("OFFER delivered as %s %s:%d → %s %s:%d", f.SrcMAC, f.SrcIP, f.SrcPort, f.DstMAC, f.DstIP, f.DstPort)
	}
	if f.SrcMAC.String() != "02:00:00:00:00:01" {
		t.Fatalf("the reply is not from the gateway's MAC: %s", f.SrcMAC)
	}
	// a client that asked for a broadcast reply gets one
	relay.replies <- bootp(2, true, netip.MustParseAddr("10.40.2.10"))
	if f := guestFrame(t, cab); f.DstMAC.String() != BroadcastMAC.String() || f.DstIP.String() != "255.255.255.255" {
		t.Fatalf("broadcast-flag reply delivered to %s %s", f.DstMAC, f.DstIP)
	}
	if hp.DHCPRequests.Load() != 1 || hp.DHCPReplies.Load() != 2 {
		t.Fatalf("counted %d requests, %d replies", hp.DHCPRequests.Load(), hp.DHCPReplies.Load())
	}
	if s, why := hp.Relay(); s == "" || why != "" {
		t.Fatalf("Relay() = %q, %q", s, why)
	}
}

// A client renews by unicast to its server's identifier (RFC 2131 4.4.5). The twin site DHCP
// takes its identifier from its network's range, which can fall inside a segment (a BlueField's
// Arm leases in its BMC's), so the client ARPs for it on the link: the gateway's stack is
// promiscuous and answers for every address there, and the renewal, UDP to port 67, reaches the
// relay like a broadcast.
func TestHostPortRelaysARenewalToAnOnLinkServer(t *testing.T) {
	cab, relay, _, _ := startPort(t, nil, nil)
	cab.toPort <- UDPFrame{SrcMAC: guestMAC, DstMAC: BroadcastMAC, SrcIP: netip.IPv4Unspecified(),
		DstIP: netip.AddrFrom4([4]byte{255, 255, 255, 255}), SrcPort: 68, DstPort: 67, Payload: bootp(1, false, netip.Addr{})}.Marshal()
	<-relay.sent
	ack := append(bootp(2, false, netip.MustParseAddr("10.40.2.10")), 53, 1, 5, 54, 4, 10, 40, 2, 2, 255)
	relay.replies <- ack
	guestFrame(t, cab)

	s := guestStack(t, cab)
	c, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 40, 2, 10}), Port: 68},
		&tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 40, 2, 2}), Port: 67}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	renew := bootp(1, false, netip.Addr{})
	copy(renew[12:16], []byte{10, 40, 2, 10}) // ciaddr
	if _, err := c.Write(renew); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-relay.sent:
		if !bytes.Equal(got, renew) {
			t.Fatal("the relay got another message than the guest's renewal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guest's unicast renewal to the on-link server 10.40.2.2 did not reach the relay")
	}
}

func guestFrame(t *testing.T, cab *memCable) UDPFrame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b := <-cab.toGuest:
			if f, ok := ParseUDPFrame(b); ok {
				return f
			}
		case <-deadline:
			t.Fatal("nothing delivered to the guest")
		}
	}
}

// guestStack is the guest's own network stack (a second netstack) on the cable, at 10.40.2.10
// with the segment gateway as its default route, as the guest's OS is once leased.
func guestStack(t *testing.T, cab *memCable) *stack.Stack {
	t.Helper()
	s := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol}})
	link := channel.New(256, 1500, tcpip.LinkAddress(guestMAC))
	if err := s.CreateNIC(1, ethernet.New(link)); err != nil {
		t.Fatal(err)
	}
	addr := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{10, 40, 2, 10}), PrefixLen: 24}
	s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: addr}, stack.AddressProperties{})
	s.SetRouteTable([]tcpip.Route{{Destination: addr.Subnet(), NIC: 1}, {Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4([4]byte{10, 40, 2, 1}), NIC: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close() })
	go func() { // the guest's NIC transmits
		for {
			pkt := link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			var b []byte
			for _, v := range pkt.AsSlices() {
				b = append(b, v...)
			}
			pkt.DecRef()
			select {
			case cab.toPort <- b:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { // and receives
		for {
			select {
			case b := <-cab.toGuest:
				pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(b)})
				link.InjectInbound(0, pkt)
				pkt.DecRef()
			case <-ctx.Done():
				return
			}
		}
	}()
	return s
}

// The segment gateway routes the guest's TCP to a stack service at the site address the stack
// names, and to an estate endpoint at its site address; anything else is refused with a reset.
func TestHostPortRoutesTheGuestsTCP(t *testing.T) {
	pxe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "#!ipxe\nchain %s\n", r.URL.Path)
	}))
	defer pxe.Close()
	bmc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "redfish") }))
	defer bmc.Close()
	svcs, err := ParseServices("pxe@10.90.0.17:80=" + pxe.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cab, _, _, flows := startPort(t, svcs, map[string]string{"10.30.2.40:443": bmc.Listener.Addr().String()})
	g := guestStack(t, cab)
	get := func(site string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ap := netip.MustParseAddrPort(site)
		c, err := gonet.DialContextTCP(ctx, g, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
		if err != nil {
			return "", err
		}
		defer c.Close()
		fmt.Fprintf(c, "GET /boot HTTP/1.0\r\nHost: %s\r\n\r\n", site)
		b, err := io.ReadAll(c)
		return string(b), err
	}
	body, err := get("10.90.0.17:80")
	if err != nil || !strings.Contains(body, "#!ipxe") {
		t.Fatalf("the PXE service at its site address: %q %v", body, err)
	}
	if f := <-flows; f.Service != "pxe" || f.Dst.String() != "10.90.0.17:80" || f.Src.Addr().String() != "10.40.2.10" {
		t.Fatalf("flow %+v", f)
	}
	body, err = get("10.30.2.40:443")
	if err != nil || !strings.Contains(body, "redfish") {
		t.Fatalf("the estate endpoint at its site address: %q %v", body, err)
	}
	if _, err := get("10.90.0.99:80"); err == nil {
		t.Fatal("an address nothing serves was reachable")
	} else if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("unrouted: %v, want connection refused", err)
	}
	for {
		select {
		case f := <-flows:
			if f.Refused != "" {
				if !strings.Contains(f.Refused, "no route") {
					t.Fatalf("refused flow %+v", f)
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the refused flow was not observed")
		}
	}
}

// UDP to a stack service is routed too, and the service's answer comes back from its site
// address.
func TestHostPortRoutesTheGuestsUDP(t *testing.T) {
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() {
		b := make([]byte, 1500)
		for {
			n, from, err := srv.ReadFromUDP(b)
			if err != nil {
				return
			}
			srv.WriteToUDP(append([]byte("answer to "), b[:n]...), from)
		}
	}()
	svcs, _ := ParseServices("dns@udp:10.90.0.53:53=" + srv.LocalAddr().String())
	cab, _, _, _ := startPort(t, svcs, nil)
	q := UDPFrame{SrcMAC: guestMAC, DstMAC: net.HardwareAddr{0x02, 0, 0, 0, 0, 0x01}, SrcIP: netip.MustParseAddr("10.40.2.10"),
		DstIP: netip.MustParseAddr("10.90.0.53"), SrcPort: 40000, DstPort: 53, Payload: []byte("query")}
	cab.toPort <- q.Marshal()
	f := guestFrame(t, cab)
	if string(f.Payload) != "answer to query" || f.SrcIP.String() != "10.90.0.53" || f.SrcPort != 53 || f.DstPort != 40000 || f.DstIP.String() != "10.40.2.10" {
		t.Fatalf("answer %q from %s:%d to %s:%d", f.Payload, f.SrcIP, f.SrcPort, f.DstIP, f.DstPort)
	}
}

func TestParseServices(t *testing.T) {
	s, err := ParseServices("pxe@10.90.0.17:80=127.0.0.1:18080, api@10.90.0.18:443=localhost:11079 dns@udp:10.90.0.53:53=127.0.0.1:15353")
	if err != nil || len(s) != 3 || s[2].Proto != "udp" || s[1].Local != "localhost:11079" || s[0].Site.Port() != 80 {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{"10.90.0.17:80=127.0.0.1:1", "pxe@host:80=127.0.0.1:1", "pxe@10.90.0.17:80", "pxe@10.90.0.17:80=nohostport"} {
		if _, err := ParseServices(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	var none []Service
	if s, err := ParseServices(""); err != nil || len(s) != len(none) {
		t.Fatal(s, err)
	}
}

// nsStack is a network namespace on a routed site network: a netstack at 10.90.0.17 whose
// default route is the twin's gateway 10.90.0.1, at the far end of the uplink's link.
func nsStack(t *testing.T) (*stack.Stack, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol}})
	link := channel.New(256, 1500, tcpip.LinkAddress(net.HardwareAddr{0x02, 0xdc, 0, 0, 0, 0x17}))
	if err := s.CreateNIC(1, ethernet.New(link)); err != nil {
		t.Fatal(err)
	}
	addr := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{10, 90, 0, 17}), PrefixLen: 24}
	s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: addr}, stack.AddressProperties{})
	s.SetRouteTable([]tcpip.Route{{Destination: addr.Subnet(), NIC: 1}, {Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4([4]byte{10, 90, 0, 1}), NIC: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); l.Close(); s.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() { // the namespace's TAP transmits onto the link
			for {
				pkt := link.ReadContext(ctx)
				if pkt == nil {
					c.Close()
					return
				}
				var b []byte
				for _, v := range pkt.AsSlices() {
					b = append(b, v...)
				}
				pkt.DecRef()
				c.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...))
			}
		}()
		var h [4]byte
		for {
			if _, err := io.ReadFull(c, h[:]); err != nil {
				return
			}
			b := make([]byte, binary.BigEndian.Uint32(h[:]))
			if _, err := io.ReadFull(c, b); err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(b)})
			link.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	return s, l.Addr().String()
}

// On a routed site network the twin's gateway forwards at L3: a service in the namespace sees the
// guest's own lease as its peer (what NICo's PXE service identifies a host by), and the TTL is
// a router's.
func TestHostPortRoutesToTheSiteNetworkWithoutNAT(t *testing.T) {
	ns, link := nsStack(t)
	ln, err := gonet.ListenTCP(ns, tcpip.FullAddress{NIC: 1, Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "peer "+r.RemoteAddr) }))
	sn, err := ParseSiteNetwork("prefix=10.90.0.0/24 gateway=10.90.0.1 link=" + link)
	if err != nil {
		t.Fatal(err)
	}
	up := NewUplink(sn, net.HardwareAddr{0x02, 0xdc, 0, 0, 0, 0x01}, nil)
	defer up.Close()
	// a service the stack runs on this host is still the proxy's, even inside the routed prefix
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api") }))
	defer api.Close()
	svcs, _ := ParseServices("api@10.90.0.18:443=" + api.Listener.Addr().String())
	cab := newMemCable()
	relay := &fakeRelay{sent: make(chan []byte, 8), replies: make(chan []byte, 8), closed: make(chan struct{})}
	hp, err := NewHostPort(cab, HostPortConfig{Ref: "h:p0", MAC: guestMAC, Services: svcs, Uplink: up,
		Plug: func() (PortRelay, error) { return relay, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer hp.Close()
	defer cab.close()
	// the guest's lease, as the relay delivers its ACK
	ack := append(bootp(2, false, netip.MustParseAddr("10.40.2.10")), 53, 1, 5, 255)
	relay.replies <- ack
	deadline := time.Now().Add(5 * time.Second)
	for (!up.Linked() || hp.DHCPReplies.Load() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	<-cab.toGuest // the ACK itself
	g := guestStack(t, cab)
	get := func(site string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ap := netip.MustParseAddrPort(site)
		c, err := gonet.DialContextTCP(ctx, g, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
		if err != nil {
			return "", err
		}
		defer c.Close()
		fmt.Fprintf(c, "GET / HTTP/1.0\r\n\r\n")
		b, err := io.ReadAll(c)
		return string(b), err
	}
	body, err := get("10.90.0.17:8080")
	if err != nil || !strings.Contains(body, "peer 10.40.2.10:") {
		t.Fatalf("the namespace's service saw %q (%v), want the guest's lease as its peer", body, err)
	}
	if body, err := get("10.90.0.18:443"); err != nil || !strings.Contains(body, "api") {
		t.Fatalf("a host service inside the routed prefix: %q %v", body, err)
	}
	if up.ToSite.Load() == 0 || up.FromSite.Load() == 0 {
		t.Fatalf("routed %d packets to the site network and %d back", up.ToSite.Load(), up.FromSite.Load())
	}
}

// A service in the namespace reaches an estate endpoint at its site address through the router,
// as NICo's nvlink-manager reaches a switch tray's NMX-C at the tray's NVOS lease: the router
// terminates the connection and opens one to where the twin serves that address; an address
// nothing serves is refused.
func TestSiteNetworkReachesEstateEndpoints(t *testing.T) {
	ns, link := nsStack(t)
	sn, err := ParseSiteNetwork("prefix=10.90.0.0/24 gateway=10.90.0.1 link=" + link)
	if err != nil {
		t.Fatal(err)
	}
	up := NewUplink(sn, net.HardwareAddr{0x02, 0xdc, 0, 0, 0, 0x01}, nil)
	defer up.Close()
	nmxc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "nmx-c") }))
	defer nmxc.Close()
	up.SetSiteRoutes(func() map[string]string { return map[string]string{"10.30.2.16:9370": nmxc.Listener.Addr().String()} })
	deadline := time.Now().Add(5 * time.Second)
	for !up.Linked() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	get := func(site string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ap := netip.MustParseAddrPort(site)
		c, err := gonet.DialContextTCP(ctx, ns, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
		if err != nil {
			return "", err
		}
		defer c.Close()
		fmt.Fprintf(c, "GET / HTTP/1.0\r\n\r\n")
		b, err := io.ReadAll(c)
		return string(b), err
	}
	if body, err := get("10.30.2.16:9370"); err != nil || !strings.Contains(body, "nmx-c") {
		t.Fatalf("the namespace reached %q (%v), want the estate endpoint", body, err)
	}
	if _, err := get("10.30.2.16:9999"); err == nil {
		t.Error("a port nothing serves at the address was not refused")
	}
	if up.EndpointFlows.Load() != 1 || up.EndpointRefused.Load() != 1 {
		t.Errorf("flows %d, refused %d", up.EndpointFlows.Load(), up.EndpointRefused.Load())
	}
}

func TestParseSiteNetwork(t *testing.T) {
	n, err := ParseSiteNetwork("prefix=10.90.0.0/24, gateway=10.90.0.1 link=127.0.0.1:17800")
	if err != nil || n.Gateway.String() != "10.90.0.1" || n.Link != "127.0.0.1:17800" {
		t.Fatal(n, err)
	}
	for _, bad := range []string{"prefix=10.90.0.0/24 gateway=10.91.0.1 link=127.0.0.1:1", "prefix=10.90.0.0/24 link=127.0.0.1:1", "gateway=10.90.0.1"} {
		if _, err := ParseSiteNetwork(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
